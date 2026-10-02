package bench

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/runner"
	"github.com/callum/k6-autobench/internal/search"
)

// fakeRunner returns canned k6 summaries and records the specs it was given.
type fakeRunner struct {
	calls []runner.Spec
	// respond builds the summary for a spec; a nil summary means "k6 failed".
	respond func(spec runner.Spec, call int) *metrics.Summary
	err     error
}

func (f *fakeRunner) Run(_ context.Context, spec runner.Spec) (*metrics.Summary, error) {
	call := len(f.calls)
	f.calls = append(f.calls, spec)
	if f.err != nil {
		return nil, f.err
	}
	if f.respond == nil {
		return passSummary(spec), nil
	}
	return f.respond(spec, call), nil
}

// passSummary is a k6 summary for a run that met the target rate and breached
// nothing.
func passSummary(spec runner.Spec) *metrics.Summary {
	rate := float64(spec.Rate)
	return &metrics.Summary{
		Metrics: map[string]metrics.Metric{
			"iterations":      {Type: "counter", Values: map[string]float64{"count": rate * spec.Duration.Seconds(), "rate": rate}},
			"http_reqs":       {Type: "counter", Values: map[string]float64{"count": rate * spec.Duration.Seconds(), "rate": rate}},
			"http_req_failed": {Type: "rate", Values: map[string]float64{"rate": 0, "passes": 0, "fails": rate * spec.Duration.Seconds()}},
			"http_req_duration": {Type: "trend", Values: map[string]float64{
				"avg": 100, "min": 1, "max": 400, "p(50)": 90, "p(95)": 200, "p(99)": 300,
			}},
			"vus":     {Type: "gauge", Values: map[string]float64{"value": float64(spec.PreAllocatedVUs)}},
			"vus_max": {Type: "gauge", Values: map[string]float64{"value": float64(spec.MaxVUs)}},
		},
	}
}

// saturatedSummary is a k6 summary for a run the generator could not sustain:
// it achieved half the target and dropped iterations.
func saturatedSummary(spec runner.Spec) *metrics.Summary {
	s := passSummary(spec)
	rate := float64(spec.Rate) / 2
	s.Metrics["iterations"] = metrics.Metric{Type: "counter",
		Values: map[string]float64{"count": rate * spec.Duration.Seconds(), "rate": rate}}
	s.Metrics["dropped_iterations"] = metrics.Metric{Type: "counter",
		Values: map[string]float64{"count": 42, "rate": 0}}
	s.Metrics["vus"] = metrics.Metric{Type: "gauge", Values: map[string]float64{"value": float64(spec.MaxVUs)}}
	return s
}

func testConfig() *config.Config {
	cfg := &config.Config{
		Name:   "test",
		Target: config.Target{BaseURL: "http://localhost:1"},
		Run:    config.Run{Rate: 100, Duration: config.Duration{Duration: time.Minute}},
	}
	cfg.ApplyDefaults()
	return cfg
}

// pinnedConfig pins the VU pool, which the search must then respect.
func pinnedConfig(pre, max int) *config.Config {
	cfg := &config.Config{
		Target:        config.Target{BaseURL: "http://localhost:1"},
		LoadGenerator: config.LoadGenerator{PreAllocatedVUs: pre, MaxVUs: max},
	}
	cfg.ApplyDefaults()
	return cfg
}

// capacityResponder passes up to capacity and degrades above it: the mean
// latency doubles and the p99 breaches a 500ms SLO, so the search has a real
// boundary to find.
func capacityResponder(capacity int) func(runner.Spec, int) *metrics.Summary {
	return func(spec runner.Spec, _ int) *metrics.Summary {
		s := passSummary(spec)
		if spec.Rate > capacity {
			s.Metrics["http_req_duration"] = metrics.Metric{Type: "trend", Values: map[string]float64{
				"avg": 200, "min": 1, "max": 1200, "p(50)": 180, "p(95)": 600, "p(99)": 900,
			}}
		}
		return s
	}
}

// TestMeasureSizesVUsForRate: with no observed latency the pool comes from the
// 100ms assumption, so concurrency = rate x 0.1s.
func TestMeasureSizesVUsForRate(t *testing.T) {
	f := &fakeRunner{}
	m := &Measurer{Config: testConfig(), Runner: f}

	if _, err := m.Measure(context.Background(), 2000, time.Second); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	spec := f.calls[0]
	if spec.Rate != 2000 {
		t.Errorf("rate = %d, want 2000", spec.Rate)
	}
	if spec.PreAllocatedVUs != 200 || spec.MaxVUs != 800 {
		t.Errorf("VUs = %d/%d, want 200/800", spec.PreAllocatedVUs, spec.MaxVUs)
	}
	if spec.TimeUnit != "1s" {
		t.Errorf("time unit = %q, want 1s", spec.TimeUnit)
	}
}

// TestMeasureUsesObservedLatency: a slow system gets a bigger pool next time,
// which is what stops the generator masquerading as the bottleneck.
func TestMeasureUsesObservedLatency(t *testing.T) {
	f := &fakeRunner{respond: func(spec runner.Spec, _ int) *metrics.Summary {
		s := passSummary(spec)
		s.Metrics["http_req_duration"] = metrics.Metric{Type: "trend", Values: map[string]float64{
			"avg": 200, "min": 1, "max": 900, "p(50)": 190, "p(99)": 800,
		}}
		return s
	}}
	m := &Measurer{Config: testConfig(), Runner: f}

	for i := 0; i < 2; i++ {
		if _, err := m.Measure(context.Background(), 1000, time.Second); err != nil {
			t.Fatalf("Measure %d: %v", i, err)
		}
	}
	if got := f.calls[0].PreAllocatedVUs; got != 100 {
		t.Errorf("first pool = %d, want 100 (100ms assumption)", got)
	}
	if got := f.calls[1].PreAllocatedVUs; got != 200 {
		t.Errorf("second pool = %d, want 200 (observed 200ms)", got)
	}
}

// TestMeasureRespectsPinnedVUs: an explicit pool is never resized.
func TestMeasureRespectsPinnedVUs(t *testing.T) {
	f := &fakeRunner{}
	m := &Measurer{Config: pinnedConfig(50, 100), Runner: f}

	if _, err := m.Measure(context.Background(), 10000, time.Second); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got := f.calls[0]; got.PreAllocatedVUs != 50 || got.MaxVUs != 100 {
		t.Errorf("VUs = %d/%d, want the pinned 50/100", got.PreAllocatedVUs, got.MaxVUs)
	}
}

// TestProbeWarmsUpFirst: the warmup run is a separate, discarded measurement.
func TestProbeWarmsUpFirst(t *testing.T) {
	f := &fakeRunner{}
	m := &Measurer{Config: testConfig(), Runner: f, Warmup: 10 * time.Second}

	if _, err := m.ProbeFunc()(context.Background(), 500, 30*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("%d k6 runs, want 2 (warmup + measurement)", len(f.calls))
	}
	if f.calls[0].Duration != 10*time.Second {
		t.Errorf("warmup duration = %v, want 10s", f.calls[0].Duration)
	}
	if f.calls[1].Duration != 30*time.Second {
		t.Errorf("measurement duration = %v, want 30s", f.calls[1].Duration)
	}
}

// TestProbeGrowsVUsOnSaturation: a probe the generator could not sustain is
// retried with a bigger pool rather than read as a capacity limit.
func TestProbeGrowsVUsOnSaturation(t *testing.T) {
	f := &fakeRunner{respond: func(spec runner.Spec, call int) *metrics.Summary {
		if call == 0 {
			return saturatedSummary(spec)
		}
		return passSummary(spec)
	}}
	var notices []string
	m := &Measurer{
		Config:      testConfig(),
		Runner:      f,
		MaxVUGrowth: 2,
		OnProgress:  func(s string) { notices = append(notices, s) },
	}

	res, err := m.ProbeFunc()(context.Background(), 1000, time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Verdict != results.VerdictPass {
		t.Errorf("verdict = %s, want PASS after the retry", res.Verdict)
	}
	if len(f.calls) != 2 {
		t.Fatalf("%d k6 runs, want 2 (saturated + retry)", len(f.calls))
	}
	if f.calls[0].PreAllocatedVUs != 100 {
		t.Errorf("first pool = %d, want 100", f.calls[0].PreAllocatedVUs)
	}
	if f.calls[1].PreAllocatedVUs != 200 {
		t.Errorf("retry pool = %d, want the doubled 200", f.calls[1].PreAllocatedVUs)
	}
	if len(notices) == 0 {
		t.Error("expected a progress notice about growing the pool")
	}
}

// TestProbeKeepsGrownPoolForLaterRates: the pool is monotone, so a later probe
// never drops below the largest pool the search has already needed.
func TestProbeKeepsGrownPoolForLaterRates(t *testing.T) {
	f := &fakeRunner{respond: func(spec runner.Spec, call int) *metrics.Summary {
		if call == 0 {
			return saturatedSummary(spec)
		}
		return passSummary(spec)
	}}
	m := &Measurer{Config: testConfig(), Runner: f, MaxVUGrowth: 3}
	probe := m.ProbeFunc()

	if _, err := probe(context.Background(), 1000, time.Second); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if _, err := probe(context.Background(), 2000, time.Second); err != nil {
		t.Fatalf("second probe: %v", err)
	}

	// The saturation at 1000/s forced the pool from 100 up to 200; the probe at
	// 2000/s auto-sizes to 200, so it stays at the floor rather than shrinking.
	if got := f.calls[2].PreAllocatedVUs; got != 200 {
		t.Errorf("later probe pool = %d, want the 200 floor", got)
	}

	// The invariant: the pool never shrinks across the search.
	for i := 1; i < len(f.calls); i++ {
		if f.calls[i].PreAllocatedVUs < f.calls[i-1].PreAllocatedVUs {
			t.Errorf("pool shrank from %d to %d at call %d",
				f.calls[i-1].PreAllocatedVUs, f.calls[i].PreAllocatedVUs, i)
		}
		if f.calls[i].MaxVUs < f.calls[i-1].MaxVUs {
			t.Errorf("max pool shrank from %d to %d at call %d",
				f.calls[i-1].MaxVUs, f.calls[i].MaxVUs, i)
		}
	}
}

// TestProbeGivesUpWithoutGrowthBudget: with growth disabled the saturation is
// surfaced so the search can abort with advice.
func TestProbeGivesUpWithoutGrowthBudget(t *testing.T) {
	f := &fakeRunner{respond: func(spec runner.Spec, _ int) *metrics.Summary {
		return saturatedSummary(spec)
	}}
	m := &Measurer{Config: testConfig(), Runner: f, MaxVUGrowth: 0}

	res, err := m.ProbeFunc()(context.Background(), 1000, time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Verdict != results.VerdictFailGenerator {
		t.Errorf("verdict = %s, want FAIL_GENERATOR", res.Verdict)
	}
	if len(f.calls) != 1 {
		t.Errorf("%d k6 runs, want 1 (no retry possible)", len(f.calls))
	}
}

// TestProbeGivesUpWhenVUsArePinned: a pinned pool cannot be grown, so retrying
// would only spin.
func TestProbeGivesUpWhenVUsArePinned(t *testing.T) {
	f := &fakeRunner{respond: func(spec runner.Spec, _ int) *metrics.Summary {
		return saturatedSummary(spec)
	}}
	m := &Measurer{Config: pinnedConfig(50, 100), Runner: f, MaxVUGrowth: 3}

	res, err := m.ProbeFunc()(context.Background(), 1000, time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Verdict != results.VerdictFailGenerator {
		t.Errorf("verdict = %s, want FAIL_GENERATOR", res.Verdict)
	}
	if len(f.calls) != 1 {
		t.Errorf("%d k6 runs, want 1 (the pinned pool cannot grow)", len(f.calls))
	}
}

// TestMeasureRejectsBadDuration guards the runner contract.
func TestMeasureRejectsBadDuration(t *testing.T) {
	m := &Measurer{Config: testConfig(), Runner: &fakeRunner{}}
	if _, err := m.Measure(context.Background(), 100, 0); err == nil {
		t.Error("Measure: got nil error for a zero duration")
	}
}

// TestMeasurePropagatesRunnerError keeps k6 failures distinguishable from SLO
// failures.
func TestMeasurePropagatesRunnerError(t *testing.T) {
	boom := errors.New("k6 exploded")
	m := &Measurer{Config: testConfig(), Runner: &fakeRunner{err: boom}}
	if _, err := m.Measure(context.Background(), 100, time.Second); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

// TestMeasureOmitsRawMetrics: a search takes dozens of measurements, so the
// full k6 metric set is dropped from each one.
func TestMeasureOmitsRawMetrics(t *testing.T) {
	m := &Measurer{Config: testConfig(), Runner: &fakeRunner{}}
	res, err := m.Measure(context.Background(), 100, time.Second)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if res.Raw != nil {
		t.Errorf("Raw = %d entries, want nil", len(res.Raw))
	}
	if res.Metrics.IterationRate != 100 {
		t.Errorf("iteration rate = %v, want 100", res.Metrics.IterationRate)
	}
}

// TestSearchThroughMeasurer is the end-to-end wiring check: the real search
// controller driving the real measurer against a fake k6, with the VU pool
// sized per probe rate.
func TestSearchThroughMeasurer(t *testing.T) {
	cfg := testConfig()
	cfg.Thresholds.Latency.P99 = &config.LatencyThreshold{
		Max: config.Duration{Duration: 500 * time.Millisecond},
	}
	cfg.ApplyDefaults()

	f := &fakeRunner{respond: capacityResponder(2000)}
	m := &Measurer{Config: cfg, Runner: f}

	ctrl := search.New(search.Config{
		StartRate:           500,
		IncreaseFactor:      2,
		Resolution:          50,
		ConfirmationRuns:    1,
		FailureConfirmation: 1,
		ProbeDuration:       time.Second,
	}, m.ProbeFunc())

	out, err := ctrl.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.BracketFound {
		t.Fatal("BracketFound = false, want a bracketed capacity")
	}
	if out.PassingRate > 2000 || out.FailingRate <= 2000 {
		t.Errorf("bracket [%d, %d] does not contain the capacity 2000",
			out.PassingRate, out.FailingRate)
	}
	if width := out.FailingRate - out.PassingRate; width > 50 {
		t.Errorf("bracket width = %d, want <= resolution 50", width)
	}
	if !out.Pass() {
		t.Errorf("verdict = %s, want PASS", out.Verdict())
	}

	// The pool must track the probe rate: the last (highest) probe needs more
	// VUs than the first.
	first, last := f.calls[0], f.calls[len(f.calls)-1]
	if last.PreAllocatedVUs <= first.PreAllocatedVUs {
		t.Errorf("VU pool did not grow with the rate: %d then %d",
			first.PreAllocatedVUs, last.PreAllocatedVUs)
	}
}
