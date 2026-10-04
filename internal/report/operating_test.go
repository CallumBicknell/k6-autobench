package report

import (
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/search"
)

// capacityConfig is a config with the capacity-search bits filled in.
func capacityConfig() *config.Config {
	cfg := testConfig()
	cfg.Search.StartRate = 2000
	cfg.Search.ProbeDuration = config.Duration{Duration: time.Second}
	return cfg
}

// policyConfig is a capacity config with an operating policy.
func policyConfig(utilization float64) *config.Config {
	cfg := capacityConfig()
	cfg.OperatingPolicy.MaxUtilizationOfCapacity = utilization
	return cfg
}

// policyOutcome is a search that found a capacity, with a passing curve whose
// latency leaves the floor at 10,000/s.
func policyOutcome() *search.Outcome {
	out := &search.Outcome{
		PassingRate:  14000,
		FailingRate:  15000,
		Capacity:     14000,
		BracketFound: true,
	}
	for _, p := range [][2]float64{
		{2000, 20}, {4000, 23}, {8000, 31}, {10000, 40}, {12000, 91}, {14000, 420},
	} {
		out.Probes = append(out.Probes, search.Probe{
			Rate: int(p[0]), Pass: true,
			Result: results.Result{Metrics: metrics.Metrics{
				HasLatency:  true,
				Percentiles: []metrics.Percentile{{P: 99, Val: p[1]}},
			}},
		})
	}
	return out
}

// TestOperatingLimitUsesThePolicy is the outline's own arithmetic: 80% of the
// measured capacity, stated as a calculation rather than a recommendation.
func TestOperatingLimitUsesThePolicy(t *testing.T) {
	op := operatingLimit(policyOutcome(), policyConfig(0.8))

	if op.PolicyLimit != 11200 {
		t.Errorf("policy limit = %d, want 11200 (80%% of 14000)", op.PolicyLimit)
	}
	// The knee is at 10,000, below the policy limit, so the knee binds.
	if op.Limit != 10000 {
		t.Errorf("limit = %d, want 10000: the knee is the lower of the two", op.Limit)
	}
	if op.Bound != "the latency knee" {
		t.Errorf("bound = %q, want the latency knee", op.Bound)
	}
}

// TestOperatingLimitLetsThePolicyBind when the knee is above it.
func TestOperatingLimitLetsThePolicyBind(t *testing.T) {
	cfg := policyConfig(0.5) // 7000/s, below the 10,000/s knee
	op := operatingLimit(policyOutcome(), cfg)

	if op.Limit != 7000 {
		t.Errorf("limit = %d, want 7000", op.Limit)
	}
	if op.Bound != "the policy limit" {
		t.Errorf("bound = %q, want the policy limit", op.Bound)
	}
}

// TestOperatingLimitWithoutAKnee still applies the policy: the knee is the half
// that needs a curve, and the policy does not.
func TestOperatingLimitWithoutAKnee(t *testing.T) {
	out := policyOutcome()
	out.Probes = nil // no curve to read a knee from

	op := operatingLimit(out, policyConfig(0.8))
	if op.HasKnee {
		t.Error("HasKnee = true, want false with no probes")
	}
	if op.Limit != 11200 {
		t.Errorf("limit = %d, want 11200 from the policy alone", op.Limit)
	}
}

// TestOperatingLimitIgnoresALowerBoundCapacity: a capacity that is only a floor
// cannot bound anything from below, and the report says so rather than staying
// silent about a block the user configured.
func TestOperatingLimitIgnoresALowerBoundCapacity(t *testing.T) {
	out := policyOutcome()
	out.LowerBoundOnly = true

	cfg := policyConfig(0.8)
	op := operatingLimit(out, cfg)
	if op.PolicyLimit != 0 {
		t.Errorf("policy limit = %d, want none from a lower bound", op.PolicyLimit)
	}
	if op.PolicySkip == "" {
		t.Error("PolicySkip is empty, want a reason")
	}

	var b strings.Builder
	p := &printer{w: &b, color: false}
	op.render(p)
	if !strings.Contains(b.String(), "not computed") {
		t.Errorf("report should say why there is no policy limit:\n%s", b.String())
	}
}

// TestOperatingLimitWithNoPolicy recommends nothing.
func TestOperatingLimitWithNoPolicy(t *testing.T) {
	cfg := capacityConfig() // no operating_policy block
	op := operatingLimit(policyOutcome(), cfg)

	if op.Limit != 0 || op.PolicyLimit != 0 || op.HasKnee {
		t.Errorf("op = %+v, want nothing when no policy is configured", op)
	}
	if facts := op.facts(); len(facts) != 0 {
		t.Errorf("facts = %+v, want none", facts)
	}
}

// TestOperatingLimitKneeOnly: the knee is useful without a utilisation figure, so
// a policy with only knee settings still produces a limit.
func TestOperatingLimitKneeOnly(t *testing.T) {
	cfg := capacityConfig()
	cfg.OperatingPolicy.KneeLatencyMultiple = 2

	op := operatingLimit(policyOutcome(), cfg)
	if !op.HasKnee || op.Knee != 10000 {
		t.Fatalf("knee = %d (ok=%v), want 10000", op.Knee, op.HasKnee)
	}
	if op.PolicyLimit != 0 {
		t.Errorf("policy limit = %d, want none", op.PolicyLimit)
	}
	if op.Limit != 10000 || op.Bound != "the latency knee" {
		t.Errorf("limit = %d bound %q, want 10000 by the knee", op.Limit, op.Bound)
	}
}

// TestOperatingLimitNilOutcome: a search that produced no outcome must not panic.
func TestOperatingLimitNilOutcome(t *testing.T) {
	if op := operatingLimit(nil, policyConfig(0.8)); op.Limit != 0 {
		t.Errorf("limit = %d, want none", op.Limit)
	}
}

// TestOperatingFacts covers the HTML rows, which are built separately from the
// terminal block.
func TestOperatingFacts(t *testing.T) {
	cfg := policyConfig(0.8)
	op := operatingLimit(policyOutcome(), cfg)
	facts := op.facts()

	if len(facts) != 3 {
		t.Fatalf("facts = %+v, want knee, policy limit and operating range", facts)
	}
	want := map[string]string{
		"Latency knee":    "10,000/s (p99 within 2× the 20.0ms seen at 2,000/s)",
		"Policy limit":    "11,200/s (80% of measured capacity)",
		"Operating range": "< 10,000/s, set by the latency knee",
	}
	for _, f := range facts {
		if want[f.Label] != f.Value {
			t.Errorf("%s = %q, want %q", f.Label, f.Value, want[f.Label])
		}
	}
}

// TestOperatingFactsWithoutAPolicy: no block, no rows.
func TestOperatingFactsWithoutAPolicy(t *testing.T) {
	if facts := operatingLimit(policyOutcome(), capacityConfig()).facts(); len(facts) != 0 {
		t.Errorf("facts = %+v, want none", facts)
	}
}

// TestRenderOperatingShowsTheArithmetic is the outline's requirement: a limit
// someone cannot check is one they have to take on faith.
func TestRenderOperatingShowsTheArithmetic(t *testing.T) {
	var b strings.Builder
	p := &printer{w: &b, color: false}
	cfg := policyConfig(0.8)
	operatingLimit(policyOutcome(), cfg).render(p)
	got := b.String()

	for _, want := range []string{
		"Latency knee", "10,000/s",
		// The percentile label already carries its "p"; asserting on it is what
		// caught a "pp99" in the first version.
		"(p99 still within 2× the 20.0ms seen at 2,000/s)",
		"Policy limit", "11,200/s", "80% of measured capacity",
		"Operating range", "< 10,000/s", "set by the latency knee",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("operating block does not contain %q:\n%s", want, got)
		}
	}
}

// TestRenderOperatingSaysWhenThereIsNoKnee keeps an absence from looking like a
// missing section.
func TestRenderOperatingSaysWhenThereIsNoKnee(t *testing.T) {
	out := policyOutcome()
	out.Probes = nil

	var b strings.Builder
	p := &printer{w: &b, color: false}
	cfg := policyConfig(0.8)
	operatingLimit(out, cfg).render(p)

	if !strings.Contains(b.String(), "none found") {
		t.Errorf("report should say no knee was found:\n%s", b.String())
	}
}
