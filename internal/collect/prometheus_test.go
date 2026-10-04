package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePrometheus is a Prometheus instant-query API serving canned responses. It
// records what was asked for, so the request contract can be asserted and not
// just the parsing.
type fakePrometheus struct {
	mu       sync.Mutex
	paths    []string
	queries  []string
	handler  func(call int, query string) (int, string)
	requests int
}

func newFakePrometheus(t *testing.T, handler func(call int, query string) (int, string)) (*fakePrometheus, *httptest.Server) {
	t.Helper()
	f := &fakePrometheus{handler: handler}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.queries = append(f.queries, r.URL.Query().Get("query"))
		f.requests++
		call := f.requests
		f.mu.Unlock()

		status, body := f.handler(call, r.URL.Query().Get("query"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// calls returns a copy of the recorded query expressions.
func (f *fakePrometheus) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// pathsSeen returns the distinct request paths, in first-seen order.
func (f *fakePrometheus) pathsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, p := range f.paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// sampleJSON renders one entry of a vector result exactly as Prometheus does:
// the labels, then a [unix_time, "<value>"] pair whose value is a quoted string.
func sampleJSON(value string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, strconv.Quote(k)+":"+strconv.Quote(labels[k]))
	}
	return `{"metric":{` + strings.Join(parts, ",") + `},"value":[1435781451.781,` + strconv.Quote(value) + `]}`
}

// vectorJSON renders a documented /api/v1/query vector response.
func vectorJSON(entries ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		strings.Join(entries, ",") + `]}}`
}

// always returns a handler that answers every call with the same body.
func always(status int, body string) func(int, string) (int, string) {
	return func(int, string) (int, string) { return status, body }
}

// measure runs Start, calls fn, then stops and returns the report. It is the
// shape the runner uses.
func measure(t *testing.T, p *Prometheus, fn func()) Report {
	t.Helper()
	stop := p.Start(context.Background())
	fn()
	return stop()
}

func seriesNamed(t *testing.T, r Report, name string) Series {
	t.Helper()
	for _, s := range r.Series {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("report has no series %q (has %v)", name, seriesNames(r))
	return Series{}
}

func seriesNames(r Report) []string {
	out := make([]string, 0, len(r.Series))
	for _, s := range r.Series {
		out = append(out, s.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPrometheusSendsTheExpressionURLEncoded pins the request contract against
// the documented API: GET /api/v1/query?query=<expr>, with a PromQL expression
// that survives the round trip through the query string.
func TestPrometheusSendsTheExpressionURLEncoded(t *testing.T) {
	expr := `rate(http_requests_total{code=~"5.."}[5m])`
	f, srv := newFakePrometheus(t, always(200, vectorJSON(sampleJSON("1", map[string]string{}))))

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "errors", Expr: expr}},
		Interval: time.Hour,
	}
	measure(t, p, func() {})

	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("sent %d requests, want 1", len(calls))
	}
	if calls[0] != expr {
		t.Errorf("query = %q, want %q", calls[0], expr)
	}
	if paths := f.pathsSeen(); len(paths) != 1 || paths[0] != "/api/v1/query" {
		t.Errorf("paths = %v, want [/api/v1/query]", paths)
	}
}

// TestPrometheusKeepsAConfiguredPathPrefix makes sure a Prometheus behind a
// reverse proxy at /prometheus still works.
func TestPrometheusKeepsAConfiguredPathPrefix(t *testing.T) {
	f, srv := newFakePrometheus(t, always(200, vectorJSON(sampleJSON("1", map[string]string{}))))

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL + "/prometheus/",
		Queries:  []Query{{Name: "cpu", Expr: "up"}},
		Interval: time.Hour,
	}
	measure(t, p, func() {})

	if paths := f.pathsSeen(); len(paths) != 1 || paths[0] != "/prometheus/api/v1/query" {
		t.Errorf("paths = %v, want [/prometheus/api/v1/query]", paths)
	}
}

// TestPrometheusCollectsEveryQueryInConfiguredOrder checks that the report keeps
// the order the queries were written in even though they are issued
// concurrently: a report that reorders its own rows is not worth having.
func TestPrometheusCollectsEveryQueryInConfiguredOrder(t *testing.T) {
	// The first query answers slowly, so a naive append-on-arrival would put it
	// last.
	_, srv := newFakePrometheus(t, func(_ int, query string) (int, string) {
		if query == "slow" {
			time.Sleep(40 * time.Millisecond)
		}
		return 200, vectorJSON(sampleJSON("1", map[string]string{}))
	})

	p := &Prometheus{
		Source: "api",
		URL:    srv.URL,
		Queries: []Query{
			{Name: "slow", Expr: "slow"},
			{Name: "medium", Expr: "medium"},
			{Name: "fast", Expr: "fast"},
		},
		Interval: time.Hour,
	}

	report := measure(t, p, func() {})
	want := []string{"slow", "medium", "fast"}
	if got := seriesNames(report); !equalStrings(got, want) {
		t.Errorf("series = %v, want %v", got, want)
	}
	if len(report.Failures) != 0 {
		t.Errorf("unexpected failures: %v", report.Failures)
	}
}

// TestPrometheusNamesFanOutSeriesByLabel checks that a query matching several
// series does not silently collapse them into one row.
func TestPrometheusNamesFanOutSeriesByLabel(t *testing.T) {
	body := vectorJSON(
		sampleJSON("0.1", map[string]string{"__name__": "up", "job": "api", "instance": "a"}),
		sampleJSON("0.2", map[string]string{"__name__": "up", "job": "api", "instance": "b"}),
	)
	_, srv := newFakePrometheus(t, always(200, body))

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "up", Expr: "up"}},
		Interval: time.Hour,
	}

	report := measure(t, p, func() {})
	want := []string{`up{instance="a"}`, `up{instance="b"}`}
	if got := seriesNames(report); !equalStrings(got, want) {
		t.Fatalf("series = %v, want %v (job and __name__ should be dropped)", got, want)
	}
	if got := seriesNamed(t, report, `up{instance="b"}`).Samples[0].Value; got != 0.2 {
		t.Errorf("value = %v, want 0.2", got)
	}
}

// TestPrometheusNamesUnlabelledFanOutByPosition covers the pathological case
// where nothing distinguishes the series, so two rows cannot claim one name.
func TestPrometheusNamesUnlabelledFanOutByPosition(t *testing.T) {
	body := vectorJSON(
		sampleJSON("1", map[string]string{}),
		sampleJSON("2", map[string]string{}),
	)
	_, srv := newFakePrometheus(t, always(200, body))

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "count", Expr: "count(x)"}},
		Interval: time.Hour,
	}

	report := measure(t, p, func() {})
	want := []string{"count[1]", "count[2]"}
	if got := seriesNames(report); !equalStrings(got, want) {
		t.Errorf("series = %v, want %v", got, want)
	}
}

// TestPrometheusRecordsTheConfiguredUnitAndQuery keeps the report reproducible:
// a reader can rerun the same expression by hand.
func TestPrometheusRecordsTheConfiguredUnitAndQuery(t *testing.T) {
	_, srv := newFakePrometheus(t, always(200, vectorJSON(sampleJSON("1", map[string]string{}))))

	p := &Prometheus{
		Source:   "postgres",
		URL:      srv.URL,
		Queries:  []Query{{Name: "connections", Expr: "pg_stat_activity_count", Unit: "connections"}},
		Interval: time.Hour,
	}

	report := measure(t, p, func() {})
	s := seriesNamed(t, report, "connections")
	if s.Source != "postgres" || s.Unit != "connections" || s.Origin != "pg_stat_activity_count" {
		t.Errorf("series = %+v, want the configured source, unit and query", s)
	}
	if s.Label() != "postgres connections" {
		t.Errorf("Label() = %q, want %q", s.Label(), "postgres connections")
	}
}

// TestPrometheusSamplesImmediately covers a measurement shorter than the
// interval: it must still produce one reading, or a short run reports nothing.
func TestPrometheusSamplesImmediately(t *testing.T) {
	_, srv := newFakePrometheus(t, always(200, vectorJSON(sampleJSON("0.25", map[string]string{}))))

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "cpu", Expr: "cpu"}},
		Interval: time.Hour,
	}

	report := measure(t, p, func() {})
	s := seriesNamed(t, report, "cpu")
	if len(s.Samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(s.Samples))
	}
	if s.Samples[0].Value != 0.25 {
		t.Errorf("value = %v, want 0.25", s.Samples[0].Value)
	}
}

// TestPrometheusSamplesOnTheInterval checks the sampling loop actually repeats,
// and that readings land on the measurement's own timeline rather than on
// wall-clock time.
func TestPrometheusSamplesOnTheInterval(t *testing.T) {
	_, srv := newFakePrometheus(t, always(200, vectorJSON(sampleJSON("1", map[string]string{}))))

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "cpu", Expr: "cpu"}},
		Interval: 5 * time.Millisecond,
	}

	report := measure(t, p, func() { time.Sleep(80 * time.Millisecond) })
	s := seriesNamed(t, report, "cpu")
	if len(s.Samples) < 3 {
		t.Errorf("samples = %d, want several over 80ms at a 5ms interval", len(s.Samples))
	}
	if last := s.Samples[len(s.Samples)-1].OffsetMs; last < 50 {
		t.Errorf("last offset = %vms, want >= 50ms", last)
	}
}

// TestPrometheusStartDoesNotBlock is the property that keeps a wedged monitoring
// endpoint from delaying the load test itself.
func TestPrometheusStartDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, vectorJSON(sampleJSON("1", map[string]string{})))
	}))
	defer srv.Close()
	defer close(release)

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "cpu", Expr: "cpu"}},
		Interval: time.Hour,
		Timeout:  5 * time.Second,
	}

	start := time.Now()
	stop := p.Start(context.Background())
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Start blocked for %v", elapsed)
	}

	// Stopping must not wait out the wedged request either: the cancellation
	// aborts it.
	done := make(chan Report, 1)
	go func() { done <- stop() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop blocked behind the wedged request")
	}
}

// TestPrometheusRecordsCollectorFailures checks that a broken monitoring setup
// is reported rather than swallowed, and that it is not an error.
func TestPrometheusRecordsCollectorFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler func(int, string) (int, string)
		want    string
	}{
		{
			name:    "prometheus reports a query error",
			handler: always(422, `{"status":"error","errorType":"bad_data","error":"parse error at char 5"}`),
			want:    "parse error at char 5",
		},
		{
			name:    "server is not prometheus",
			handler: always(404, "404 page not found"),
			want:    "404",
		},
		{
			name:    "response is not json",
			handler: always(200, "<html>hello</html>"),
			want:    "decode response",
		},
		{
			name:    "query matched nothing",
			handler: always(200, vectorJSON()),
			want:    "matched no series",
		},
		{
			name:    "unsupported result type",
			handler: always(200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`),
			want:    "unsupported result type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFakePrometheus(t, tc.handler)
			p := &Prometheus{
				Source:   "api",
				URL:      srv.URL,
				Queries:  []Query{{Name: "cpu", Expr: "cpu"}},
				Interval: time.Hour,
			}

			report := measure(t, p, func() {})
			if len(report.Series) != 0 {
				t.Errorf("series = %v, want none", seriesNames(report))
			}
			if len(report.Failures) != 1 {
				t.Fatalf("failures = %v, want exactly one", report.Failures)
			}
			f := report.Failures[0]
			if f.Source != "api" {
				t.Errorf("source = %q, want api", f.Source)
			}
			if !strings.Contains(f.Error, "cpu") {
				t.Errorf("error %q should name the query", f.Error)
			}
			if !strings.Contains(f.Error, tc.want) {
				t.Errorf("error %q should contain %q", f.Error, tc.want)
			}
		})
	}
}

// TestPrometheusReportsAnUnreachableServer covers the common case of a typo in
// the URL: the run must carry on with a note, not an error.
func TestPrometheusReportsAnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	p := &Prometheus{
		Source:   "api",
		URL:      url,
		Queries:  []Query{{Name: "cpu", Expr: "cpu"}, {Name: "memory", Expr: "memory"}},
		Interval: time.Hour,
	}

	report := measure(t, p, func() {})
	if len(report.Series) != 0 {
		t.Errorf("series = %v, want none", seriesNames(report))
	}
	if len(report.Failures) != 2 {
		t.Fatalf("failures = %d, want one per query", len(report.Failures))
	}
}

// TestPrometheusReportsEachFailureOnce keeps a persistently broken endpoint from
// adding one line per poll.
func TestPrometheusReportsEachFailureOnce(t *testing.T) {
	_, srv := newFakePrometheus(t, always(500, "boom"))

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "cpu", Expr: "cpu"}},
		Interval: 2 * time.Millisecond,
	}

	report := measure(t, p, func() { time.Sleep(60 * time.Millisecond) })
	if len(report.Failures) != 1 {
		t.Errorf("failures = %d (%v), want 1 however many polls failed",
			len(report.Failures), report.Failures)
	}
}

// TestPrometheusRecoversFromATransientFailure checks that one bad poll does not
// condemn the whole measurement.
func TestPrometheusRecoversFromATransientFailure(t *testing.T) {
	_, srv := newFakePrometheus(t, func(call int, _ string) (int, string) {
		if call == 1 {
			return 503, "service unavailable"
		}
		return 200, vectorJSON(sampleJSON("0.75", map[string]string{}))
	})

	p := &Prometheus{
		Source:   "api",
		URL:      srv.URL,
		Queries:  []Query{{Name: "cpu", Expr: "cpu"}},
		Interval: 3 * time.Millisecond,
	}

	report := measure(t, p, func() { time.Sleep(60 * time.Millisecond) })
	if len(report.Failures) != 1 {
		t.Errorf("failures = %v, want the single transient one", report.Failures)
	}
	s := seriesNamed(t, report, "cpu")
	if len(s.Samples) < 2 {
		t.Fatalf("samples = %d, want readings from the polls that worked", len(s.Samples))
	}
	if s.Samples[0].Value != 0.75 {
		t.Errorf("value = %v, want 0.75", s.Samples[0].Value)
	}
}
