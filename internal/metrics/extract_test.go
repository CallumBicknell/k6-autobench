package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// summaryJSON mirrors the shape of a real k6 summary payload, including k6's
// inverted "passes"/"fails" naming on rate metrics.
const summaryJSON = `{
  "metrics": {
    "http_req_duration": {
      "type": "trend",
      "contains": "time",
      "values": {
        "avg": 0.4183, "min": 0.18, "max": 1.43, "med": 0.40,
        "p(50)": 0.40, "p(95)": 0.60, "p(99)": 0.73, "p(99.9)": 0.84
      }
    },
    "http_reqs":  {"type": "counter", "contains": "default", "values": {"count": 1200, "rate": 199.96}},
    "iterations": {"type": "counter", "contains": "default", "values": {"count": 1200, "rate": 199.96}},
    "http_req_failed": {
      "type": "rate", "contains": "default",
      "values": {"rate": 0, "passes": 0, "fails": 1200}
    },
    "vus":     {"type": "gauge", "contains": "default", "values": {"value": 1, "min": 1, "max": 1}},
    "vus_max": {"type": "gauge", "contains": "default", "values": {"value": 50, "min": 50, "max": 50}}
  },
  "state": {"testRunDurationMs": 6001.35}
}`

func mustSummary(t *testing.T, raw string) *Summary {
	t.Helper()
	var s Summary
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	return &s
}

// transportFailureSummary is a k6 summary from a target that never answered: every
// request failed at the transport layer, and k6 still recorded a duration of zero
// for each one.
const transportFailureSummary = `{
  "metrics": {
    "http_req_duration": {"type": "trend", "values": {"avg": 0, "min": 0, "max": 0,
      "p(50)": 0, "p(95)": 0, "p(99)": 0}},
    "iteration_duration": {"type": "trend", "values": {"avg": 0, "min": 0, "max": 0,
      "p(50)": 0, "p(95)": 0, "p(99)": 0}},
    "http_reqs": {"type": "counter", "values": {"count": 41, "rate": 20.5}},
    "iterations": {"type": "counter", "values": {"count": 41, "rate": 20.5}},
    "http_req_failed": {"type": "rate", "values": {"rate": 1, "passes": 41, "fails": 0}},
    "vus": {"type": "gauge", "values": {"value": 1, "min": 1, "max": 1}},
    "vus_max": {"type": "gauge", "values": {"value": 10, "min": 10, "max": 10}},
    "http_req_duration{name:root}": {"type": "trend", "values": {"avg": 0, "min": 0, "max": 0,
      "p(95)": 0, "p(99)": 0}},
    "http_reqs{name:root}": {"type": "counter", "values": {"count": 41, "rate": 20.5}},
    "http_req_failed{name:root}": {"type": "rate", "values": {"rate": 1, "passes": 41, "fails": 0}},
    "ab_status_other": {"type": "counter", "values": {"count": 41, "rate": 20.5}}
  },
  "state": {"testRunDurationMs": 2000}
}`

// TestExtractDropsLatencyWhenNothingAnswered is the regression test for a
// misleading number: k6 records a duration of zero for a request that never got a
// response, and a zero latency reads as instant, which is the opposite of what
// happened. The number is dropped rather than reported.
func TestExtractDropsLatencyWhenNothingAnswered(t *testing.T) {
	m := Extract(mustSummary(t, transportFailureSummary), []float64{95, 99})

	if !m.Unanswered {
		t.Fatal("Unanswered = false, want true for a measurement with no HTTP status")
	}
	if m.HasLatency {
		t.Error("HasLatency = true, want the zero-duration samples dropped")
	}
	if len(m.Percentiles) != 0 || m.AvgMs != 0 || m.MaxMs != 0 {
		t.Errorf("latency survived: pcts=%v avg=%v max=%v", m.Percentiles, m.AvgMs, m.MaxMs)
	}
	if m.HasIterationLatency || len(m.IterationPercentiles) != 0 {
		t.Error("iteration latency survived, want it dropped too")
	}
	for _, e := range m.Endpoints {
		if e.HasLatency || len(e.Percentiles) != 0 {
			t.Errorf("endpoint %s kept its latency: %+v", e.Name, e)
		}
	}

	// The facts that explain it are kept: this is a reporting change, not a
	// forgetting of what happened.
	if m.Requests != 41 || m.FailedRequests != 41 || m.ErrorRate != 1 {
		t.Errorf("requests=%d failed=%d errorRate=%v, want 41/41/1",
			m.Requests, m.FailedRequests, m.ErrorRate)
	}
	if len(m.StatusClasses) != 1 || m.StatusClasses[0].Class != StatusClassOther {
		t.Errorf("status classes = %+v, want the transport-failure class kept", m.StatusClasses)
	}
}

// TestExtractKeepsLatencyWhenAnythingAnswered: one real response is enough to make
// the measurement a measurement.
func TestExtractKeepsLatencyWhenAnythingAnswered(t *testing.T) {
	answered := strings.Replace(transportFailureSummary,
		`"ab_status_other": {"type": "counter", "values": {"count": 41, "rate": 20.5}}`,
		`"ab_status_2xx": {"type": "counter", "values": {"count": 1, "rate": 0.5}},
    "ab_status_other": {"type": "counter", "values": {"count": 40, "rate": 20}}`, 1)

	m := Extract(mustSummary(t, answered), []float64{95, 99})
	if m.Unanswered {
		t.Error("Unanswered = true, want false when one response had a status")
	}
	if !m.HasLatency {
		t.Error("HasLatency = false, want the samples kept")
	}
}

// TestExtractDoesNotCallAnEmptyClassListUnanswered: no recorded classes means the
// workload script's counters did not fire, which is a different problem and not
// one to guess about.
func TestExtractDoesNotCallAnEmptyClassListUnanswered(t *testing.T) {
	noClasses := `{
      "metrics": {"http_reqs": {"type": "counter", "values": {"count": 5, "rate": 1}}},
      "state": {"testRunDurationMs": 5000}
    }`
	if m := Extract(mustSummary(t, noClasses), []float64{95}); m.Unanswered {
		t.Error("Unanswered = true, want false when no classes were recorded at all")
	}
}

// TestUnansweredClasses is the predicate on its own.
func TestUnansweredClasses(t *testing.T) {
	tests := []struct {
		name    string
		classes []StatusClass
		want    bool
	}{
		{"nothing recorded", nil, false},
		{"only transport failures", []StatusClass{{Class: StatusClassOther, Count: 10}}, true},
		{"one answer among many failures", []StatusClass{
			{Class: "2xx", Count: 1}, {Class: StatusClassOther, Count: 99}}, false},
		{"a 5xx storm is not unanswered", []StatusClass{{Class: "5xx", Count: 10}}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unansweredClasses(tc.classes); got != tc.want {
				t.Errorf("unansweredClasses(%+v) = %v, want %v", tc.classes, got, tc.want)
			}
		})
	}
}

func TestExtractHappyPath(t *testing.T) {
	m := Extract(mustSummary(t, summaryJSON), []float64{50, 95, 99, 99.9})

	if !m.HasLatency {
		t.Fatal("HasLatency = false, want true")
	}
	if m.Requests != 1200 || m.Iterations != 1200 {
		t.Errorf("requests/iterations = %d/%d, want 1200/1200", m.Requests, m.Iterations)
	}
	if m.RequestRate != 199.96 || m.IterationRate != 199.96 {
		t.Errorf("rates = %v/%v, want 199.96", m.RequestRate, m.IterationRate)
	}
	if m.FailedRequests != 0 || m.ErrorRate != 0 {
		t.Errorf("failed/error = %d/%v, want 0/0", m.FailedRequests, m.ErrorRate)
	}
	if m.VUs != 1 || m.MaxVUs != 50 {
		t.Errorf("vus = %d/%d, want 1/50", m.VUs, m.MaxVUs)
	}
	if m.AvgMs != 0.4183 || m.MinMs != 0.18 || m.MaxMs != 1.43 {
		t.Errorf("avg/min/max = %v/%v/%v", m.AvgMs, m.MinMs, m.MaxMs)
	}
	if m.TestDurationMs != 6001.35 {
		t.Errorf("TestDurationMs = %v, want 6001.35", m.TestDurationMs)
	}

	if got, ok := m.At(99); !ok || got != 0.73 {
		t.Errorf("At(99) = %v, %v; want 0.73, true", got, ok)
	}
	if _, ok := m.At(75); ok {
		t.Error("At(75) reported a value that was never collected")
	}
	if len(m.Percentiles) != 4 {
		t.Errorf("len(Percentiles) = %d, want 4", len(m.Percentiles))
	}
}

// TestExtractFailedRequestsUsesPasses guards against reading the "fails" key:
// for http_req_failed a *true* sample means the request failed, and k6 counts
// true samples under "passes".
func TestExtractFailedRequestsUsesPasses(t *testing.T) {
	raw := `{"metrics": {"http_req_failed": {
		"type": "rate", "contains": "default",
		"values": {"rate": 0.108, "passes": 27, "fails": 223}
	}}}`
	m := Extract(mustSummary(t, raw), nil)

	if m.FailedRequests != 27 {
		t.Errorf("FailedRequests = %d, want 27 (the true-sample count)", m.FailedRequests)
	}
	if m.ErrorRate != 0.108 {
		t.Errorf("ErrorRate = %v, want 0.108", m.ErrorRate)
	}
}

func TestExtractDroppedIterations(t *testing.T) {
	raw := `{"metrics": {"dropped_iterations": {
		"type": "counter", "contains": "default", "values": {"count": 4321, "rate": 12.5}
	}}}`
	if got := Extract(mustSummary(t, raw), nil).DroppedIterations; got != 4321 {
		t.Errorf("DroppedIterations = %d, want 4321", got)
	}
}

func TestExtractMissingMetricsAreZero(t *testing.T) {
	m := Extract(mustSummary(t, `{}`), []float64{99})

	if m.HasLatency {
		t.Error("HasLatency = true for a summary with no trend metric")
	}
	if m.Requests != 0 || m.FailedRequests != 0 || m.DroppedIterations != 0 || m.VUs != 0 {
		t.Errorf("expected zero metrics, got %+v", m)
	}
	if len(m.Percentiles) != 0 {
		t.Errorf("len(Percentiles) = %d, want 0", len(m.Percentiles))
	}
}

func TestPercentileKey(t *testing.T) {
	cases := map[float64]string{50: "p(50)", 95: "p(95)", 99.9: "p(99.9)", 99.99: "p(99.99)"}
	for in, want := range cases {
		if got := PercentileKey(in); got != want {
			t.Errorf("PercentileKey(%v) = %q, want %q", in, got, want)
		}
	}
}

// endpointsJSON is the shape k6 emits for a tagged workload: one submetric per
// request name per metric, alongside the run-level metrics.
const endpointsJSON = `{
  "metrics": {
    "http_reqs": {"type": "counter", "values": {"count": 600, "rate": 199.9}},
    "http_reqs{name:feed}":        {"type": "counter", "values": {"count": 298, "rate": 99.29}},
    "http_reqs{name:post}":        {"type": "counter", "values": {"count": 152, "rate": 50.6}},
    "http_reqs{name:like}":        {"type": "counter", "values": {"count": 90,  "rate": 29.9}},
    "http_reqs{name:create_post}": {"type": "counter", "values": {"count": 60,  "rate": 19.9}},
    "http_req_failed{name:feed}":        {"type": "rate", "values": {"rate": 0, "passes": 0, "fails": 298}},
    "http_req_failed{name:post}":        {"type": "rate", "values": {"rate": 0.0066, "passes": 1, "fails": 151}},
    "http_req_failed{name:like}":        {"type": "rate", "values": {"rate": 0, "passes": 0, "fails": 90}},
    "http_req_failed{name:create_post}": {"type": "rate", "values": {"rate": 0.05, "passes": 3, "fails": 57}},
    "http_req_duration{name:feed}": {
      "type": "trend",
      "values": {"avg": 0.96, "max": 5.61, "p(50)": 0.89, "p(95)": 1.31, "p(99)": 3.89}
    },
    "http_req_duration{name:post}": {
      "type": "trend",
      "values": {"avg": 1.02, "max": 4.10, "p(50)": 0.95, "p(95)": 1.50, "p(99)": 2.20}
    },
    "http_req_duration{name:like}": {
      "type": "trend",
      "values": {"avg": 0.88, "max": 2.90, "p(50)": 0.80, "p(95)": 1.20, "p(99)": 1.90}
    },
    "http_req_duration{expected_response:true}": {
      "type": "trend",
      "values": {"avg": 0.9, "max": 5.0, "p(95)": 1.3}
    }
  }
}`

func TestExtractEndpoints(t *testing.T) {
	m := Extract(mustSummary(t, endpointsJSON), []float64{50, 95, 99})

	if len(m.Endpoints) != 4 {
		t.Fatalf("len(Endpoints) = %d, want 4: %+v", len(m.Endpoints), m.Endpoints)
	}

	// Busiest first, so the report leads with the endpoint carrying the load.
	wantOrder := []string{"feed", "post", "like", "create_post"}
	for i, want := range wantOrder {
		if m.Endpoints[i].Name != want {
			t.Errorf("Endpoints[%d].Name = %q, want %q", i, m.Endpoints[i].Name, want)
		}
	}

	feed := m.Endpoints[0]
	if feed.Requests != 298 || feed.RequestRate != 99.29 {
		t.Errorf("feed requests = %d (%v/s), want 298 (99.29/s)", feed.Requests, feed.RequestRate)
	}
	if feed.Failed != 0 || feed.ErrorRate != 0 {
		t.Errorf("feed failed = %d (%v), want 0/0", feed.Failed, feed.ErrorRate)
	}
	if !feed.HasLatency || feed.AvgMs != 0.96 || feed.MaxMs != 5.61 {
		t.Errorf("feed latency = %+v, want avg 0.96 max 5.61", feed)
	}
	if len(feed.Percentiles) != 3 {
		t.Fatalf("feed percentiles = %+v, want 3", feed.Percentiles)
	}
	if feed.Percentiles[1].P != 95 || feed.Percentiles[1].Val != 1.31 {
		t.Errorf("feed p95 = %+v, want 1.31", feed.Percentiles[1])
	}

	// The failure count comes from "passes" for the same reason as the run-level
	// metric: a true http_req_failed sample means the request failed.
	createPost := m.Endpoints[3]
	if createPost.Failed != 3 || createPost.ErrorRate != 0.05 {
		t.Errorf("create_post failed = %d (%v), want 3 (0.05)", createPost.Failed, createPost.ErrorRate)
	}
}

// TestExtractEndpointsIgnoresExpectedResponse guards the submetric prefix match:
// http_req_duration{expected_response:true} is not a per-name breakdown.
func TestExtractEndpointsIgnoresExpectedResponse(t *testing.T) {
	raw := `{"metrics": {
		"http_req_duration{expected_response:true}": {"type": "trend", "values": {"avg": 1, "p(95)": 2}},
		"http_req_duration{name:feed}": {"type": "trend", "values": {"avg": 3, "p(95)": 4}}
	}}`
	m := Extract(mustSummary(t, raw), []float64{95})

	if len(m.Endpoints) != 1 {
		t.Fatalf("len(Endpoints) = %d, want 1: %+v", len(m.Endpoints), m.Endpoints)
	}
	if m.Endpoints[0].Name != "feed" {
		t.Errorf("Name = %q, want feed", m.Endpoints[0].Name)
	}
}

// TestExtractEndpointsAbsentWithoutSubmetrics: a workload script that does not
// tag names (or an older summary) must not invent an endpoint list.
func TestExtractEndpointsAbsentWithoutSubmetrics(t *testing.T) {
	m := Extract(mustSummary(t, summaryJSON), []float64{95})
	if len(m.Endpoints) != 0 {
		t.Errorf("Endpoints = %+v, want none", m.Endpoints)
	}
}

func TestExtractEndpointsSortedByNameOnTies(t *testing.T) {
	raw := `{"metrics": {
		"http_reqs{name:zeta}":  {"type": "counter", "values": {"count": 5, "rate": 1}},
		"http_reqs{name:alpha}": {"type": "counter", "values": {"count": 5, "rate": 1}}
	}}`
	m := Extract(mustSummary(t, raw), nil)

	if len(m.Endpoints) != 2 || m.Endpoints[0].Name != "alpha" || m.Endpoints[1].Name != "zeta" {
		t.Errorf("Endpoints = %+v, want alpha then zeta", m.Endpoints)
	}
}

// TestExtractIterationLatency: for a multi-step journey the end-to-end iteration
// time is what a user waits for, where http_req_duration covers one step of it.
func TestExtractIterationLatency(t *testing.T) {
	raw := `{"metrics": {
		"iteration_duration": {
			"type": "trend",
			"values": {"avg": 45.2, "min": 12, "max": 220.5, "p(50)": 40, "p(95)": 120, "p(99)": 190}
		}
	}}`
	m := Extract(mustSummary(t, raw), []float64{50, 95, 99})

	if !m.HasIterationLatency {
		t.Fatal("HasIterationLatency = false")
	}
	if m.IterationAvgMs != 45.2 || m.IterationMaxMs != 220.5 {
		t.Errorf("avg/max = %v/%v, want 45.2/220.5", m.IterationAvgMs, m.IterationMaxMs)
	}
	if len(m.IterationPercentiles) != 3 {
		t.Fatalf("percentiles = %+v, want 3", m.IterationPercentiles)
	}
	if m.IterationPercentiles[1].P != 95 || m.IterationPercentiles[1].Val != 120 {
		t.Errorf("p95 = %+v, want 120", m.IterationPercentiles[1])
	}
}

func TestExtractIterationLatencyAbsent(t *testing.T) {
	m := Extract(mustSummary(t, `{}`), []float64{95})
	if m.HasIterationLatency {
		t.Error("HasIterationLatency = true for a summary with no iteration_duration")
	}
	if len(m.IterationPercentiles) != 0 {
		t.Errorf("percentiles = %+v, want none", m.IterationPercentiles)
	}
}

// TestRateOverScenario guards a real false positive: k6's own rate divides by
// the whole wall time including its startup, so a short run that hit its target
// exactly reports a few percent low and looks like a saturated generator.
func TestRateOverScenario(t *testing.T) {
	raw := `{"metrics": {
		"iterations": {"type": "counter", "values": {"count": 600, "rate": 98.1}},
		"http_reqs":  {"type": "counter", "values": {"count": 2400, "rate": 392.4}}
	}}`

	m := Extract(mustSummary(t, raw), nil)
	// Before: k6's rate, diluted by its own 116ms of startup and teardown.
	if m.IterationRate != 98.1 {
		t.Fatalf("k6 rate = %v, want 98.1", m.IterationRate)
	}

	m.RateOverScenario(6 * time.Second)
	if m.IterationRate != 100 {
		t.Errorf("iteration rate = %v, want 100 over the declared 6s", m.IterationRate)
	}
	if m.RequestRate != 400 {
		t.Errorf("request rate = %v, want 400", m.RequestRate)
	}
	if m.DeclaredDurationMs != 6000 {
		t.Errorf("DeclaredDurationMs = %v, want 6000", m.DeclaredDurationMs)
	}
}

// TestRateOverScenarioIgnoresBadDuration: without a duration there is nothing to
// divide by, so k6's own rates stand.
func TestRateOverScenarioIgnoresBadDuration(t *testing.T) {
	raw := `{"metrics": {"iterations": {"type": "counter", "values": {"count": 100, "rate": 50}}}}`
	m := Extract(mustSummary(t, raw), nil)

	m.RateOverScenario(0)
	if m.IterationRate != 50 {
		t.Errorf("iteration rate = %v, want k6's 50 left alone", m.IterationRate)
	}
	if m.DeclaredDurationMs != 0 {
		t.Errorf("DeclaredDurationMs = %v, want 0", m.DeclaredDurationMs)
	}
}

// TestExtractStatusClasses: k6's http_req_failed says requests failed but not
// how. A 4xx storm and a 5xx storm mean opposite things.
func TestExtractStatusClasses(t *testing.T) {
	raw := `{"metrics": {
		"ab_status_2xx": {"type": "counter", "values": {"count": 250, "rate": 50}},
		"ab_status_5xx": {"type": "counter", "values": {"count": 750, "rate": 150}},
		"ab_status_4xx": {"type": "counter", "values": {"count": 0,   "rate": 0}}
	}}`
	m := Extract(mustSummary(t, raw), nil)

	if len(m.StatusClasses) != 2 {
		t.Fatalf("classes = %+v, want 2 (a class with no samples is dropped)", m.StatusClasses)
	}
	// Reporting order: success first, then the classes that indicate trouble.
	if m.StatusClasses[0].Class != "2xx" || m.StatusClasses[1].Class != "5xx" {
		t.Errorf("order = %s, %s; want 2xx then 5xx", m.StatusClasses[0].Class, m.StatusClasses[1].Class)
	}
	if got := m.StatusClasses[1]; got.Count != 750 || got.Rate != 150 {
		t.Errorf("5xx = %+v, want 750 at 150/s", got)
	}
	// Shares are over all responses, not just the classes present.
	if m.StatusClasses[0].Share != 0.25 || m.StatusClasses[1].Share != 0.75 {
		t.Errorf("shares = %v/%v, want 0.25/0.75",
			m.StatusClasses[0].Share, m.StatusClasses[1].Share)
	}
}

func TestExtractStatusClassesAbsent(t *testing.T) {
	m := Extract(mustSummary(t, summaryJSON), nil)
	if len(m.StatusClasses) != 0 {
		t.Errorf("classes = %+v, want none when the script records no classes", m.StatusClasses)
	}
}

// TestExtractStatusClassesOther: a transport failure reports status 0, which
// belongs to no HTTP class and must not be silently dropped.
func TestExtractStatusClassesOther(t *testing.T) {
	raw := `{"metrics": {
		"ab_status_other": {"type": "counter", "values": {"count": 12, "rate": 3}}
	}}`
	m := Extract(mustSummary(t, raw), nil)

	if len(m.StatusClasses) != 1 || m.StatusClasses[0].Class != "other" {
		t.Fatalf("classes = %+v, want a single 'other'", m.StatusClasses)
	}
	if m.StatusClasses[0].Share != 1 {
		t.Errorf("share = %v, want 1", m.StatusClasses[0].Share)
	}
}
