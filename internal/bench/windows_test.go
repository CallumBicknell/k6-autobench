package bench

import (
	"context"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	"github.com/CallumBicknell/k6-autobench/internal/results"
	"github.com/CallumBicknell/k6-autobench/internal/runner"
)

// windowedTestConfig enables windowed analysis with a p95 SLO the run average
// meets but a single window can breach.
func windowedTestConfig() *config.Config {
	cfg := testConfig()
	cfg.Analysis.Window = config.Duration{Duration: 10 * time.Second}
	cfg.Thresholds.Latency.P95 = &config.LatencyThreshold{
		Max: config.Duration{Duration: 500 * time.Millisecond},
	}
	cfg.ApplyDefaults()
	return cfg
}

// spikyWindows is a run whose average is fine but whose last window collapses.
func spikyWindows() []metrics.Window {
	return []metrics.Window{
		{Index: 0, Requests: 100, HasLatency: true, Percentiles: []metrics.Percentile{{P: 95, Val: 10}}},
		{Index: 1, Requests: 100, HasLatency: true, Percentiles: []metrics.Percentile{{P: 95, Val: 900}}},
	}
}

func TestMeasurePassesThroughWindows(t *testing.T) {
	cfg := windowedTestConfig()
	f := &fakeRunner{windows: spikyWindows()}
	m := &Measurer{Config: cfg, Runner: f, Window: cfg.Analysis.Window.Duration}

	res, err := m.Measure(context.Background(), 100, time.Second)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if f.calls[0].Window != 10*time.Second {
		t.Errorf("spec window = %v, want 10s (k6 must stream samples)", f.calls[0].Window)
	}
	if len(res.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(res.Windows))
	}
	if len(res.WindowFailures) != 1 {
		t.Fatalf("window failures = %+v, want 1", res.WindowFailures)
	}
	if res.WindowFailures[0].Window != 1 {
		t.Errorf("failure window = %d, want 1", res.WindowFailures[0].Window)
	}
	// The policy is off, so the run's own (passing) average decides.
	if res.Verdict != results.VerdictPass {
		t.Errorf("verdict = %s, want PASS with the window policy off", res.Verdict)
	}
}

// TestMeasureWindowPolicyFailsTheProbe: with require_all_windows_pass a single
// bad window fails the whole measurement (PLAN-OUTLINE.md §32).
func TestMeasureWindowPolicyFailsTheProbe(t *testing.T) {
	cfg := windowedTestConfig()
	cfg.Analysis.RequireAllWindowsPass = true

	m := &Measurer{Config: cfg, Runner: &fakeRunner{windows: spikyWindows()}}
	res, err := m.Measure(context.Background(), 100, time.Second)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if res.Verdict != results.VerdictFailLatency {
		t.Errorf("verdict = %s, want FAIL_LATENCY", res.Verdict)
	}
}

// TestMeasureWindowPolicyLeavesGeneratorFailuresAlone: a discarded probe says
// nothing about the target, so a window breach inside it must not relabel it.
func TestMeasureWindowPolicyLeavesGeneratorFailuresAlone(t *testing.T) {
	cfg := windowedTestConfig()
	cfg.Analysis.RequireAllWindowsPass = true

	f := &fakeRunner{
		respond: func(spec runner.Spec, _ int) *metrics.Summary { return saturatedSummary(spec) },
		windows: spikyWindows(),
	}
	res, err := (&Measurer{Config: cfg, Runner: f}).Measure(context.Background(), 1000, time.Second)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if res.Verdict != results.VerdictFailGenerator {
		t.Errorf("verdict = %s, want FAIL_GENERATOR", res.Verdict)
	}
}

// TestMeasureWithoutWindowRequestsNoSamples: windowed analysis is opt-in.
func TestMeasureWithoutWindowRequestsNoSamples(t *testing.T) {
	f := &fakeRunner{}
	if _, err := (&Measurer{Config: testConfig(), Runner: f}).Measure(context.Background(), 100, time.Second); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if f.calls[0].Window != 0 {
		t.Errorf("spec window = %v, want 0 (no sampling)", f.calls[0].Window)
	}
}
