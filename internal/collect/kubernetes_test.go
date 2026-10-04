package collect

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestParseQuantity covers the suffixes the API's quantities actually use.
func TestParseQuantity(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		// What metrics-server sends: nanocores and kibibytes.
		{"12345678n", 0.012345678},
		{"1000000000n", 1},
		{"123456Ki", 123456 * 1024},
		// The rest of the documented suffix set.
		{"500m", 0.5},
		{"100u", 100e-6},
		{"1k", 1000},
		{"1K", 1000},
		{"5M", 5e6},
		{"2G", 2e9},
		{"1T", 1e12},
		{"1P", 1e15},
		{"1E", 1e18},
		{"1Mi", 1 << 20},
		{"1Gi", 1 << 30},
		{"1Ti", 1 << 40},
		{"1Pi", 1 << 50},
		{"1Ei", 1 << 60},
		// Plain numbers, fractions and exponents.
		{"3", 3},
		{"0.5", 0.5},
		{".25", 0.25},
		{"1.5e3", 1500},
		{"-2", -2},
		{"  42  ", 42},
	}

	for _, tc := range tests {
		got, err := parseQuantity(tc.in)
		if err != nil {
			t.Errorf("parseQuantity(%q): %v", tc.in, err)
			continue
		}
		if math.Abs(got-tc.want) > math.Abs(tc.want)*1e-9+1e-12 {
			t.Errorf("parseQuantity(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseQuantityRejectsGarbage: an unknown suffix is an error rather than a
// guess, because reading "5Z" as 5 would report a number that is not there.
func TestParseQuantityRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "5Z", "1.2.3", "1e", "--5", "n", "Ki"} {
		if got, err := parseQuantity(in); err == nil {
			t.Errorf("parseQuantity(%q) = %v, want an error", in, got)
		}
	}
}

// fakeKubernetes is a metrics API serving a canned pod list.
type fakeKubernetes struct {
	status  int
	body    string
	path    string
	query   string
	auth    string
	reached bool
}

func newFakeKubernetes(t *testing.T, status int, body string) (*fakeKubernetes, *httptest.Server) {
	t.Helper()
	f := &fakeKubernetes{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reached = true
		f.path = r.URL.Path
		f.query = r.URL.RawQuery
		f.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprint(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// podMetricsJSON renders a metrics API list from pod name to container usages.
func podMetricsJSON(pods map[string]map[string]map[string]string) string {
	var items []string
	for pod, containers := range pods {
		var cs []string
		for name, usage := range containers {
			pairs := make([]string, 0, len(usage))
			for k, v := range usage {
				pairs = append(pairs, strconv.Quote(k)+":"+strconv.Quote(v))
			}
			sort.Strings(pairs)
			cs = append(cs, `{"name":`+strconv.Quote(name)+`,"usage":{`+strings.Join(pairs, ",")+`}}`)
		}
		sort.Strings(cs)
		items = append(items, `{"metadata":{"name":`+strconv.Quote(pod)+`},"containers":[`+
			strings.Join(cs, ",")+`]}`)
	}
	sort.Strings(items)
	return `{"kind":"PodMetricsList","apiVersion":"metrics.k8s.io/v1beta1","items":[` +
		strings.Join(items, ",") + `]}`
}

func measureKubernetes(t *testing.T, k *Kubernetes, fn func()) Report {
	t.Helper()
	stop := k.Start(context.Background())
	fn()
	return stop()
}

// TestKubernetesRequestsTheDocumentedMetricsEndpoint pins the request contract:
// the namespaced pod-metrics path, the label selector, and the bearer token.
func TestKubernetesRequestsTheDocumentedMetricsEndpoint(t *testing.T) {
	f, srv := newFakeKubernetes(t, 200, podMetricsJSON(map[string]map[string]map[string]string{
		"api-7d9f": {"app": {"cpu": "100000000n", "memory": "100Mi"}},
	}))
	k := &Kubernetes{
		Source: "api", URL: srv.URL, Token: "secret",
		Namespace: "bench", Selector: "app=api",
		Interval: time.Hour, HTTP: srv.Client(),
	}

	measureKubernetes(t, k, func() {})

	if !f.reached {
		t.Fatal("no request reached the API server")
	}
	if f.path != "/apis/metrics.k8s.io/v1beta1/namespaces/bench/pods" {
		t.Errorf("path = %q, want the namespaced pod-metrics path", f.path)
	}
	if !strings.Contains(f.query, "labelSelector=app%3Dapi") {
		t.Errorf("query = %q, want the label selector", f.query)
	}
	if f.auth != "Bearer secret" {
		t.Errorf("Authorization = %q, want a bearer token", f.auth)
	}
}

// TestKubernetesDefaultsTheNamespace keeps a config that names no namespace
// reading from the same one kubectl would.
func TestKubernetesDefaultsTheNamespace(t *testing.T) {
	f, srv := newFakeKubernetes(t, 200, podMetricsJSON(map[string]map[string]map[string]string{
		"api": {"app": {"cpu": "1", "memory": "1Mi"}},
	}))
	k := &Kubernetes{Source: "api", URL: srv.URL, Interval: time.Hour, HTTP: srv.Client()}

	measureKubernetes(t, k, func() {})

	if !strings.HasSuffix(f.path, "/namespaces/default/pods") {
		t.Errorf("path = %q, want the default namespace", f.path)
	}
	if f.query != "" {
		t.Errorf("query = %q, want none when no selector was configured", f.query)
	}
}

// TestKubernetesSumsAcrossTheSelection: pod names are generated and mean nothing
// in a report, so the workload's total is what is reported.
func TestKubernetesSumsAcrossTheSelection(t *testing.T) {
	_, srv := newFakeKubernetes(t, 200, podMetricsJSON(map[string]map[string]map[string]string{
		"api-7d9f": {"app": {"cpu": "250000000n", "memory": "100Mi"}},
		"api-8e1c": {"app": {"cpu": "750000000n", "memory": "200Mi"}},
	}))
	k := &Kubernetes{Source: "api", URL: srv.URL, Interval: time.Hour, HTTP: srv.Client()}

	report := measureKubernetes(t, k, func() {})
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %v, want none", report.Failures)
	}

	cpu := seriesNamed(t, report, "cpu")
	if got := cpu.Samples[0].Value; math.Abs(got-1) > 1e-9 {
		t.Errorf("cpu = %v cores, want 1 (250m + 750m)", got)
	}
	if cpu.Unit != "cores" {
		t.Errorf("cpu unit = %q, want cores", cpu.Unit)
	}

	mem := seriesNamed(t, report, "memory")
	if got, want := mem.Samples[0].Value, float64(300<<20); got != want {
		t.Errorf("memory = %v bytes, want %v (100Mi + 200Mi)", got, want)
	}
	if mem.Unit != "bytes" {
		t.Errorf("memory unit = %q, want bytes", mem.Unit)
	}
}

// TestKubernetesSumsAcrossContainersInOnePod covers a pod with a sidecar.
func TestKubernetesSumsAcrossContainersInOnePod(t *testing.T) {
	_, srv := newFakeKubernetes(t, 200, podMetricsJSON(map[string]map[string]map[string]string{
		"api-1": {
			"app":     {"cpu": "100000000n", "memory": "50Mi"},
			"sidecar": {"cpu": "200000000n", "memory": "25Mi"},
		},
	}))
	k := &Kubernetes{Source: "api", URL: srv.URL, Interval: time.Hour, HTTP: srv.Client()}

	report := measureKubernetes(t, k, func() {})
	if got := seriesNamed(t, report, "cpu").Samples[0].Value; math.Abs(got-0.3) > 1e-9 {
		t.Errorf("cpu = %v cores, want 0.3 (100m + 200m)", got)
	}
	if got, want := seriesNamed(t, report, "memory").Samples[0].Value, float64(75<<20); got != want {
		t.Errorf("memory = %v bytes, want %v", got, want)
	}
}

// TestKubernetesReportsFailures covers an API server that refuses, one that does
// not know the route, and one that answers with something else entirely.
func TestKubernetesReportsFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"forbidden", 403, `{"message":"forbidden"}`, "403"},
		{"not found", 404, "404 page not found", "404"},
		{"not json", 200, "<html>hello</html>", "decode response"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFakeKubernetes(t, tc.status, tc.body)
			k := &Kubernetes{Source: "api", URL: srv.URL, Selector: "app=api",
				Interval: time.Hour, HTTP: srv.Client()}

			report := measureKubernetes(t, k, func() {})
			if len(report.Series) != 0 {
				t.Errorf("series = %v, want none", seriesNames(report))
			}
			if len(report.Failures) != 1 {
				t.Fatalf("failures = %v, want exactly one", report.Failures)
			}
			f := report.Failures[0]
			if f.Source != "api" || !strings.Contains(f.Error, tc.wantErr) {
				t.Errorf("failure = %+v, want it to mention %q", f, tc.wantErr)
			}
		})
	}
}

// TestKubernetesReportsAnEmptySelection: an empty list is a success to the API
// and almost always a wrong selector, so it is reported rather than dropped.
func TestKubernetesReportsAnEmptySelection(t *testing.T) {
	_, srv := newFakeKubernetes(t, 200, `{"kind":"PodMetricsList","items":[]}`)
	k := &Kubernetes{Source: "api", URL: srv.URL, Selector: "app=typo",
		Interval: time.Hour, HTTP: srv.Client()}

	report := measureKubernetes(t, k, func() {})
	if len(report.Series) != 0 {
		t.Errorf("series = %v, want none", seriesNames(report))
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Error, "matched no pods") {
		t.Errorf("failures = %v, want the empty selection reported", report.Failures)
	}
}

// TestKubernetesReportsABadQuantity: a quantity the parser does not understand
// is reported rather than silently read as something else.
func TestKubernetesReportsABadQuantity(t *testing.T) {
	body := `{"items":[{"metadata":{"name":"api-1"},"containers":[{"name":"app",` +
		`"usage":{"cpu":"12Z","memory":"1Mi"}}]}]}`
	_, srv := newFakeKubernetes(t, 200, body)
	k := &Kubernetes{Source: "api", URL: srv.URL, Interval: time.Hour, HTTP: srv.Client()}

	report := measureKubernetes(t, k, func() {})
	if len(report.Series) != 0 {
		t.Errorf("series = %v, want none: a half-read reading is worse than none", seriesNames(report))
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Error, "suffix") {
		t.Errorf("failures = %v, want the bad quantity reported", report.Failures)
	}
}

// TestKubernetesReportsAnUnreachableCluster: a load test must carry on with a
// note rather than fail because the cluster is not answering.
func TestKubernetesReportsAnUnreachableCluster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	k := &Kubernetes{Source: "api", URL: url, Interval: time.Hour, HTTP: srv.Client()}

	report := measureKubernetes(t, k, func() {})
	if len(report.Failures) != 1 {
		t.Errorf("failures = %v, want the unreachable cluster reported", report.Failures)
	}
}

// TestKubernetesReportsABadCAFile: a client that cannot be built says so once
// rather than once per poll.
func TestKubernetesReportsABadCAFile(t *testing.T) {
	k := &Kubernetes{
		Source: "api", URL: "https://127.0.0.1:6443", Interval: time.Hour,
		CAFile: filepath.Join(t.TempDir(), "nope.pem"),
	}

	report := measureKubernetes(t, k, func() {})
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Error, "ca_file") {
		t.Errorf("failures = %v, want the unreadable CA reported", report.Failures)
	}
}

// TestKubernetesReportsACAFileWithNoCertificates covers a file that exists but
// holds nothing usable.
func TestKubernetesReportsACAFileWithNoCertificates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	k := &Kubernetes{
		Source: "api", URL: "https://127.0.0.1:6443", Interval: time.Hour, CAFile: path,
	}

	report := measureKubernetes(t, k, func() {})
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Error, "no certificates") {
		t.Errorf("failures = %v, want the empty CA reported", report.Failures)
	}
}

// TestKubernetesTokenResolution follows the convention of a client running
// inside the cluster: an explicit value, then the environment, then the mounted
// service account token.
func TestKubernetesTokenResolution(t *testing.T) {
	t.Setenv("AUTOBENCH_K8S_TOKEN", "")

	if got := (&Kubernetes{Token: "explicit"}).token(); got != "explicit" {
		t.Errorf("token = %q, want the configured value", got)
	}

	t.Setenv("AUTOBENCH_K8S_TOKEN", "from-env")
	if got := (&Kubernetes{}).token(); got != "from-env" {
		t.Errorf("token = %q, want the env var honoured", got)
	}

	// No environment and no mounted token: empty, which is a request the API
	// server will answer with 401 and the report will say so.
	t.Setenv("AUTOBENCH_K8S_TOKEN", "")
	if got := (&Kubernetes{}).token(); got != "" {
		t.Errorf("token = %q, want empty when there is nothing to read", got)
	}
}

// TestKubernetesNameIsTheSource keeps report rows labelled by the configured
// name rather than the workload's generated one.
func TestKubernetesNameIsTheSource(t *testing.T) {
	if got := (&Kubernetes{Source: "api"}).Name(); got != "api" {
		t.Errorf("Name() = %q, want api", got)
	}
}
