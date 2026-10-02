package metrics

import (
	"encoding/json"
	"testing"
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
