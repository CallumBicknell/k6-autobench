package results

import (
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
)

func windowConfig() *config.Config {
	cfg := &config.Config{
		Target: config.Target{BaseURL: "http://localhost:1"},
		Thresholds: config.Thresholds{
			ErrorRate: &config.ErrorRateThreshold{Max: 0.01},
			Latency: config.LatencyThresholds{
				P95: &config.LatencyThreshold{Max: config.Duration{Duration: 50 * time.Millisecond}},
			},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func window(index int, p95 float64, errorRate float64) metrics.Window {
	return metrics.Window{
		Index:      index,
		Requests:   100,
		ErrorRate:  errorRate,
		HasLatency: true,
		AvgMs:      p95 / 2,
		MaxMs:      p95,
		Percentiles: []metrics.Percentile{
			{P: 95, Val: p95},
		},
	}
}

// TestWindowFailuresCatchesASpike is the whole point of windowed validation: a
// spike in one window must surface even though the run's own p95 is fine.
func TestWindowFailuresCatchesASpike(t *testing.T) {
	windows := []metrics.Window{
		window(0, 10, 0),
		window(1, 12, 0),
		window(2, 900, 0), // the collapse
		window(3, 11, 0),
	}

	got := WindowFailures(windowConfig(), windows)
	if len(got) != 1 {
		t.Fatalf("got %d failures, want 1: %+v", len(got), got)
	}
	if got[0].Metric != "latency.p95" || got[0].Window != 2 {
		t.Errorf("failure = %+v, want latency.p95 in window 2", got[0])
	}
	if got[0].Observed != 900 {
		t.Errorf("observed = %v, want 900", got[0].Observed)
	}
	if v := VerdictForFailures(got); v != VerdictFailLatency {
		t.Errorf("verdict = %s, want FAIL_LATENCY", v)
	}
}

func TestWindowFailuresCatchesErrorSpikes(t *testing.T) {
	windows := []metrics.Window{
		window(0, 10, 0),
		window(1, 10, 0.5), // half the requests failed
	}

	got := WindowFailures(windowConfig(), windows)
	if len(got) != 1 {
		t.Fatalf("got %d failures, want 1: %+v", len(got), got)
	}
	if got[0].Metric != "error_rate" || got[0].Window != 1 {
		t.Errorf("failure = %+v, want error_rate in window 1", got[0])
	}
	// An error breach outranks a latency breach, as everywhere else.
	if v := VerdictForFailures(got); v != VerdictFailErrors {
		t.Errorf("verdict = %s, want FAIL_ERRORS", v)
	}
}

func TestWindowFailuresPassesWhenEveryWindowIsFine(t *testing.T) {
	windows := []metrics.Window{
		window(0, 10, 0),
		window(1, 12, 0.001),
		window(2, 11, 0),
	}

	got := WindowFailures(windowConfig(), windows)
	if len(got) != 0 {
		t.Errorf("got %d failures, want none: %+v", len(got), got)
	}
	if v := VerdictForFailures(got); v != VerdictPass {
		t.Errorf("verdict = %s, want PASS", v)
	}
}

// TestWindowFailuresSkipsEmptyWindows: a window with no traffic measured
// nothing, so it must not be reported as a breach (which it would be, since an
// empty window has no percentiles and a zero rate).
func TestWindowFailuresSkipsEmptyWindows(t *testing.T) {
	windows := []metrics.Window{
		window(0, 10, 0),
		{Index: 1}, // no requests
		window(2, 10, 0),
	}

	if got := WindowFailures(windowConfig(), windows); len(got) != 0 {
		t.Errorf("got %d failures, want none: %+v", len(got), got)
	}
}

// TestWindowFailuresReportsEveryWindow keeps the report honest: if the same
// threshold is breached in three windows, all three are listed.
func TestWindowFailuresReportsEveryWindow(t *testing.T) {
	windows := []metrics.Window{
		window(0, 900, 0),
		window(1, 10, 0),
		window(2, 800, 0),
	}

	got := WindowFailures(windowConfig(), windows)
	if len(got) != 2 {
		t.Fatalf("got %d failures, want 2: %+v", len(got), got)
	}
	if got[0].Window != 0 || got[1].Window != 2 {
		t.Errorf("windows = %d, %d; want 0 and 2", got[0].Window, got[1].Window)
	}
}

// TestWindowFailuresWithoutThresholds: a config with no SLOs cannot breach one.
func TestWindowFailuresWithoutThresholds(t *testing.T) {
	cfg := &config.Config{Target: config.Target{BaseURL: "http://localhost:1"}}
	cfg.ApplyDefaults()

	if got := WindowFailures(cfg, []metrics.Window{window(0, 5000, 0.9)}); len(got) != 0 {
		t.Errorf("got %d failures, want none: %+v", len(got), got)
	}
}
