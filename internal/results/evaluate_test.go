package results

import (
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
)

// baseConfig returns a valid config running 100 iterations/s for 10s with an
// error-rate SLO of 1% and a p99 latency SLO of 500ms.
func baseConfig() *config.Config {
	cfg := &config.Config{
		Target: config.Target{BaseURL: "http://localhost:3000"},
		Run: config.Run{
			Rate:     100,
			Duration: config.Duration{Duration: 10 * time.Second},
			TimeUnit: "1s",
		},
		Thresholds: config.Thresholds{
			ErrorRate: &config.ErrorRateThreshold{Max: 0.01},
			Latency: config.LatencyThresholds{
				P99: &config.LatencyThreshold{Max: config.Duration{Duration: 500 * time.Millisecond}},
			},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func TestEvaluatePass(t *testing.T) {
	m := metrics.Metrics{
		HasLatency:    true,
		IterationRate: 100,
		VUs:           5,
		MaxVUs:        50,
		Percentiles:   []metrics.Percentile{{P: 99, Val: 120}},
	}

	verdict, failures, gen := Evaluate(baseConfig(), m)

	if verdict != VerdictPass {
		t.Errorf("verdict = %s, want PASS", verdict)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none", failures)
	}
	if gen != nil {
		t.Errorf("generator = %+v, want nil", gen)
	}
	if !verdict.OK() {
		t.Error("Verdict.OK() = false for PASS")
	}
}

func TestEvaluateErrorRateBreach(t *testing.T) {
	m := metrics.Metrics{IterationRate: 100, ErrorRate: 0.02, FailedRequests: 2}

	verdict, failures, gen := Evaluate(baseConfig(), m)

	if verdict != VerdictFailErrors {
		t.Errorf("verdict = %s, want FAIL_ERRORS", verdict)
	}
	if gen != nil {
		t.Errorf("generator = %+v, want nil", gen)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %+v, want exactly 1", failures)
	}
	f := failures[0]
	if f.Metric != "error_rate" || f.Unit != "ratio" || f.Limit != 0.01 || f.Observed != 0.02 {
		t.Errorf("unexpected failure: %+v", f)
	}
}

func TestEvaluateLatencyBreach(t *testing.T) {
	m := metrics.Metrics{
		HasLatency:    true,
		IterationRate: 100,
		Percentiles:   []metrics.Percentile{{P: 99, Val: 900}},
	}

	verdict, failures, _ := Evaluate(baseConfig(), m)

	if verdict != VerdictFailLatency {
		t.Errorf("verdict = %s, want FAIL_LATENCY", verdict)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %+v, want exactly 1", failures)
	}
	f := failures[0]
	if f.Metric != "latency.p99" || f.Unit != "ms" || f.Limit != 500 || f.Observed != 900 {
		t.Errorf("unexpected failure: %+v", f)
	}
}

func TestEvaluateErrorRateOutranksLatency(t *testing.T) {
	m := metrics.Metrics{
		HasLatency:    true,
		IterationRate: 100,
		ErrorRate:     0.5,
		Percentiles:   []metrics.Percentile{{P: 99, Val: 900}},
	}

	verdict, failures, _ := Evaluate(baseConfig(), m)

	if verdict != VerdictFailErrors {
		t.Errorf("verdict = %s, want FAIL_ERRORS", verdict)
	}
	if len(failures) != 2 {
		t.Errorf("failures = %+v, want both breaches recorded", failures)
	}
}

// TestEvaluateGeneratorOutranksThresholds covers the core V0.1 invariant: a
// saturated generator invalidates the probe, so its verdict wins even when
// thresholds were also breached.
func TestEvaluateGeneratorOutranksThresholds(t *testing.T) {
	m := metrics.Metrics{
		HasLatency:        true,
		IterationRate:     100,
		ErrorRate:         0.5,
		DroppedIterations: 12,
		VUs:               50,
		MaxVUs:            50,
		Percentiles:       []metrics.Percentile{{P: 99, Val: 900}},
	}

	verdict, failures, gen := Evaluate(baseConfig(), m)

	if verdict != VerdictFailGenerator {
		t.Errorf("verdict = %s, want FAIL_GENERATOR", verdict)
	}
	if len(failures) != 2 {
		t.Errorf("failures = %+v, want threshold breaches still reported", failures)
	}
	if gen == nil {
		t.Fatal("generator status = nil, want saturation")
	}
	if !gen.Saturated || gen.Dropped != 12 || gen.TargetRate != 100 || gen.AchievedRate != 100 {
		t.Errorf("unexpected generator status: %+v", gen)
	}
	if gen.Reason != "maxVUs reached" {
		t.Errorf("reason = %q, want %q", gen.Reason, "maxVUs reached")
	}
}

func TestEvaluateGeneratorAchievedRateBelowTarget(t *testing.T) {
	m := metrics.Metrics{IterationRate: 90, VUs: 10, MaxVUs: 50} // 90% of target 100

	verdict, _, gen := Evaluate(baseConfig(), m)

	if verdict != VerdictFailGenerator {
		t.Errorf("verdict = %s, want FAIL_GENERATOR", verdict)
	}
	if gen == nil {
		t.Fatal("generator status = nil, want saturation")
	}
	if gen.Reason != "achieved iteration rate below target" {
		t.Errorf("reason = %q", gen.Reason)
	}
}

// TestEvaluateGeneratorTolerance: 98% of target is inside tolerance, 97.9% is not.
func TestEvaluateGeneratorTolerance(t *testing.T) {
	for _, tc := range []struct {
		rate      float64
		saturated bool
	}{
		{100, false},
		{98, false},
		{97.9, true},
	} {
		_, _, gen := Evaluate(baseConfig(), metrics.Metrics{IterationRate: tc.rate})
		if got := gen != nil; got != tc.saturated {
			t.Errorf("rate %v: saturated = %v, want %v", tc.rate, got, tc.saturated)
		}
	}
}

func TestEvaluateNoThresholdsConfigured(t *testing.T) {
	cfg := baseConfig()
	cfg.Thresholds = config.Thresholds{}

	verdict, failures, _ := Evaluate(cfg, metrics.Metrics{
		IterationRate: 100,
		ErrorRate:     0.9,
		Percentiles:   []metrics.Percentile{{P: 99, Val: 5000}},
	})

	if verdict != VerdictPass {
		t.Errorf("verdict = %s, want PASS when nothing is configured", verdict)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none", failures)
	}
}

// TestEvaluateMissingPercentileIsSkipped: a threshold on a percentile k6 did
// not collect must not be reported as a breach.
func TestEvaluateMissingPercentileIsSkipped(t *testing.T) {
	verdict, failures, _ := Evaluate(baseConfig(), metrics.Metrics{IterationRate: 100})

	if verdict != VerdictPass {
		t.Errorf("verdict = %s, want PASS", verdict)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none", failures)
	}
}

func TestEvaluateTargetRatePerSecondHonoursTimeUnit(t *testing.T) {
	cfg := baseConfig()
	cfg.Run.Rate = 600
	cfg.Run.TimeUnit = "1m" // 600/min == 10/s

	if got := cfg.TargetRatePerSecond(); got != 10 {
		t.Fatalf("TargetRatePerSecond() = %v, want 10", got)
	}

	verdict, _, gen := Evaluate(cfg, metrics.Metrics{IterationRate: 10, VUs: 1, MaxVUs: 50})
	if verdict != VerdictPass || gen != nil {
		t.Errorf("verdict = %s, generator = %+v; want PASS and nil", verdict, gen)
	}

	verdict, _, gen = Evaluate(cfg, metrics.Metrics{IterationRate: 4, VUs: 50, MaxVUs: 50})
	if verdict != VerdictFailGenerator || gen == nil {
		t.Errorf("verdict = %s, generator = %+v; want FAIL_GENERATOR", verdict, gen)
	}
}
