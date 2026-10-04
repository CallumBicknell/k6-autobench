package collect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultKubernetesNamespace is where the metrics API is read from when the
// configuration does not say.
const DefaultKubernetesNamespace = "default"

// DefaultServiceAccountToken is where a pod finds its own API credentials.
const DefaultServiceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Kubernetes samples a workload's pods from the cluster's metrics API.
//
// It is for a target whose cluster has no Prometheus: the metrics API is what
// `kubectl top` reads, and reaching it needs only API access.
type Kubernetes struct {
	// Source identifies the workload in the report, e.g. "api".
	Source string
	// URL is the API server's base URL, e.g. https://127.0.0.1:6443.
	URL string
	// Token authenticates against the API server. Empty means
	// $AUTOBENCH_K8S_TOKEN, then a mounted service account token.
	Token string
	// Namespace to read pod metrics from. Empty means DefaultKubernetesNamespace.
	Namespace string
	// Selector picks the pods to watch, e.g. "app=api". Empty means every pod in
	// the namespace.
	Selector string
	// CAFile is a PEM bundle to verify the API server against. Empty means the
	// system roots.
	CAFile string
	// TokenPath is where a mounted service account token is read from. Empty
	// means DefaultServiceAccountToken, which is where the kubelet mounts it.
	// It is settable for the same reason System.Proc and Docker.Socket are: the
	// production path is a fixed location this test cannot write to.
	TokenPath string
	// Insecure skips verification of the API server's certificate.
	Insecure bool
	// Interval is how often the cluster is sampled.
	Interval time.Duration
	// Timeout bounds one metrics call.
	Timeout time.Duration
	// HTTP is the client used for metrics calls; nil means one built from the
	// TLS settings above.
	HTTP *http.Client
}

var _ Collector = (*Kubernetes)(nil)

// Name implements Collector.
func (k *Kubernetes) Name() string { return k.Source }

// Start begins sampling in the background and returns the function that stops
// it and reports what was seen.
func (k *Kubernetes) Start(ctx context.Context) func() Report {
	client, err := k.client()
	if err != nil {
		// A client that cannot be built will not build on the next tick either,
		// so say so once rather than once per poll.
		return func() Report {
			return Report{Failures: []Failure{{Source: k.Source, Error: err.Error()}}}
		}
	}

	return sample(ctx, k.Source, k.Interval, &kubernetesPoller{
		client:    client,
		url:       k.URL,
		token:     k.token(),
		namespace: k.Namespace,
		selector:  k.Selector,
		timeout:   k.timeout(),
	})
}

func (k *Kubernetes) timeout() time.Duration {
	if k.Timeout > 0 {
		return k.Timeout
	}
	return DefaultTimeout
}

// token resolves the API credentials, following the same convention as a client
// running inside the cluster: an explicit value, then the environment, then the
// token the kubelet mounts into every pod.
func (k *Kubernetes) token() string {
	if k.Token != "" {
		return k.Token
	}
	if v := strings.TrimSpace(os.Getenv("AUTOBENCH_K8S_TOKEN")); v != "" {
		return v
	}
	raw, err := os.ReadFile(k.tokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// tokenPath is where a mounted service account token is read from.
func (k *Kubernetes) tokenPath() string {
	if k.TokenPath != "" {
		return k.TokenPath
	}
	return DefaultServiceAccountToken
}

// client builds the API client from the TLS settings.
func (k *Kubernetes) client() (*http.Client, error) {
	if k.HTTP != nil {
		return k.HTTP, nil
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case k.Insecure:
		// The point of the setting: a cluster whose CA is not at hand is still
		// worth measuring, and the user asked for it by name.
		tlsConfig.InsecureSkipVerify = true //nolint:gosec
	case k.CAFile != "":
		pem, err := os.ReadFile(k.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s holds no certificates", k.CAFile)
		}
		tlsConfig.RootCAs = pool
	}

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
		Timeout:   k.timeout(),
	}, nil
}

// kubernetesPoller reads one cluster selection.
type kubernetesPoller struct {
	client    *http.Client
	url       string
	token     string
	namespace string
	selector  string
	timeout   time.Duration
}

// errNoPods is what a selection that matched nothing reports. An empty list is
// a success to the API and almost always means the selector is wrong, and
// silently reporting nothing would hide that.
var errNoPods = fmt.Errorf("the selector matched no pods")

// poll reads the current usage of every pod the selection matches.
func (kp *kubernetesPoller) poll(ctx context.Context) ([]reading, []pollFailure) {
	endpoint := kp.endpoint()

	list, err := kp.fetch(ctx, endpoint)
	if err != nil {
		return nil, []pollFailure{{key: kp.selector, err: err}}
	}
	if len(list.Items) == 0 {
		return nil, []pollFailure{{key: kp.selector, err: errNoPods}}
	}

	cpuCores, memoryBytes, err := list.totals()
	if err != nil {
		return nil, []pollFailure{{key: kp.selector, err: err}}
	}

	// Summed across the selection rather than reported per pod: pod names are
	// generated, so they change between runs and mean nothing in a report, and
	// the question section 33 asks is what the workload was doing. Per-pod
	// detail is what the Prometheus collector is for.
	return []reading{
		{order: 0, key: "cpu", name: "cpu", unit: "cores", origin: endpoint, value: cpuCores},
		{order: 1, key: "memory", name: "memory", unit: "bytes", origin: endpoint, value: memoryBytes},
	}, nil
}

// fetch reads one pod metrics list.
func (kp *kubernetesPoller) fetch(ctx context.Context, endpoint string) (podMetricsList, error) {
	var out podMetricsList

	ctx, cancel := context.WithTimeout(ctx, kp.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return out, err
	}
	if kp.token != "" {
		req.Header.Set("Authorization", "Bearer "+kp.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := kp.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return out, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("%s: %s", resp.Status, firstLine(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

// endpoint is the namespaced pod-metrics URL.
func (kp *kubernetesPoller) endpoint() string {
	u, err := url.Parse(kp.url)
	if err != nil {
		// Start validated the URL, so this cannot fail in practice.
		return kp.url
	}
	namespace := kp.namespace
	if namespace == "" {
		namespace = DefaultKubernetesNamespace
	}

	u.Path = strings.TrimSuffix(u.Path, "/") + "/apis/metrics.k8s.io/v1beta1/namespaces/" +
		url.PathEscape(namespace) + "/pods"

	q := url.Values{}
	if kp.selector != "" {
		q.Set("labelSelector", kp.selector)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// podMetricsList is the part of the metrics API's response this collector reads.
// Usage values are quantity strings, not numbers: see parseQuantity.
type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Containers []struct {
			Name  string            `json:"name"`
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// totals sums CPU and memory across every pod and container in the selection.
func (l podMetricsList) totals() (cpuCores, memoryBytes float64, err error) {
	for _, pod := range l.Items {
		for _, c := range pod.Containers {
			if raw, ok := c.Usage["cpu"]; ok {
				cores, qerr := parseQuantity(raw)
				if qerr != nil {
					return 0, 0, fmt.Errorf("pod %s container %s cpu: %w", pod.Metadata.Name, c.Name, qerr)
				}
				cpuCores += cores
			}
			if raw, ok := c.Usage["memory"]; ok {
				bytes, qerr := parseQuantity(raw)
				if qerr != nil {
					return 0, 0, fmt.Errorf("pod %s container %s memory: %w", pod.Metadata.Name, c.Name, qerr)
				}
				memoryBytes += bytes
			}
		}
	}
	return cpuCores, memoryBytes, nil
}

// quantityRE splits a Kubernetes quantity into its number and its suffix.
var quantityRE = regexp.MustCompile(`^([+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)([a-zA-Z]*)$`)

// quantitySuffixes are the multipliers the API's quantities use: decimal SI, its
// binary counterpart, and the "K" alias for "k".
var quantitySuffixes = map[string]float64{
	"n": 1e-9, "u": 1e-6, "m": 1e-3, "": 1,
	"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

// parseQuantity converts a Kubernetes resource quantity to a float64 in base
// units: cores for CPU, bytes for memory.
//
// The API writes these as strings with a suffix -- "12345678n" is 0.012345678
// cores, "123456Ki" is 126.5 mebibytes -- and there is no way to read them
// without knowing the suffixes. An unknown suffix is an error rather than a
// guess, because silently reading "5Z" as 5 would report a number that is not
// there.
func parseQuantity(s string) (float64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty quantity")
	}

	m := quantityRE.FindStringSubmatch(trimmed)
	if m == nil {
		return 0, fmt.Errorf("%q is not a quantity", s)
	}

	multiplier, ok := quantitySuffixes[m[2]]
	if !ok {
		return 0, fmt.Errorf("unknown quantity suffix %q", m[2])
	}

	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a quantity: %w", s, err)
	}
	return value * multiplier, nil
}
