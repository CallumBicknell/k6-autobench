package runner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
)

// testCA is a certificate authority and the certificates it signs, made per test
// so nothing is shared between them.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "autobench-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue signs a leaf certificate, returning the PEM pair.
func (ca *testCA) issue(t *testing.T, cn string, server bool, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		for _, n := range names {
			if ip := net.ParseIP(n); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			} else {
				tmpl.DNSNames = append(tmpl.DNSNames, n)
			}
		}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("sign certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// TestRealK6PresentsAClientCertificate is the integration test for mTLS, and the
// guard that matters: the first implementation passed the certificate as a
// per-request parameter, but k6 takes it as a global option keyed by domain, so
// the server never saw it. Nothing short of running k6 against a server that
// requires one would have said so.
func TestRealK6PresentsAClientCertificate(t *testing.T) {
	k6 := requireK6(t)

	ca := newTestCA(t)
	serverCert, serverKey := ca.issue(t, "127.0.0.1", true, "127.0.0.1")
	clientCert, clientKey := ca.issue(t, "autobench-client", false)

	serverPair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}

	// A server that requires a client certificate signed by the CA, so a pass
	// cannot be a false one.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverPair},
		ClientCAs:    poolWith(ca.cert),
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	certPath := writeFile(t, "client.pem", clientCert)
	keyPath := writeFile(t, "client.key", clientKey)
	caPath := writeFile(t, "ca.pem", ca.pem)

	run := func(withCert bool) metrics.Metrics {
		t.Helper()
		spec := specFor(config.Workload{Requests: []config.Request{{Name: "root", Path: "/"}}})
		spec.BaseURL = srv.URL
		spec.Rate = 10
		spec.Duration = time.Second
		spec.CAFile = caPath
		if withCert {
			spec.ClientCertFile = certPath
			spec.ClientKeyFile = keyPath
		}

		res, err := (&Runner{K6Path: k6}).Run(context.Background(), spec)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return metrics.Extract(res.Summary, []float64{95})
	}

	t.Run("with a client certificate", func(t *testing.T) {
		m := run(true)
		if m.Requests == 0 {
			t.Fatal("no requests were made")
		}
		if m.FailedRequests != 0 {
			t.Errorf("failed requests = %d of %d, want none: the server refused the "+
				"client certificate", m.FailedRequests, m.Requests)
		}
	})

	t.Run("without one", func(t *testing.T) {
		if m := run(false); m.FailedRequests == 0 {
			t.Error("a server requiring a client certificate accepted a caller with none")
		}
	})
}

// TestTargetHost covers the hostname a client certificate is mapped to.
func TestTargetHost(t *testing.T) {
	tests := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{"https://staging.internal:8443/api", "staging.internal", false},
		{"https://127.0.0.1:8443", "127.0.0.1", false},
		{"http://localhost", "localhost", false},
		{"https://[::1]:8443", "::1", false},
		{"https:///nohost", "", true},
	}

	for _, tc := range tests {
		got, err := targetHost(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("targetHost(%q) = %q, want an error", tc.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("targetHost(%q): %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("targetHost(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// writeFile puts PEM bytes somewhere k6 can be pointed at.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// poolWith is a certificate pool holding one certificate.
func poolWith(cert *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}
