package search

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/results"
)

// capacityProbe models a system with a hard capacity: probes at or below it
// pass, probes above it breach the latency SLO.
func capacityProbe(capacity int, calls *[]int) ProbeFunc {
	return func(_ context.Context, rate int, _ time.Duration) (*results.Result, error) {
		*calls = append(*calls, rate)
		v := results.VerdictPass
		if rate > capacity {
			v = results.VerdictFailLatency
		}
		return &results.Result{Rate: rate, Verdict: v}, nil
	}
}

// saturatingProbe reports generator saturation above limit, which is a property
// of the load generator rather than of the system under test.
func saturatingProbe(limit int, calls *[]int) ProbeFunc {
	return func(_ context.Context, rate int, _ time.Duration) (*results.Result, error) {
		*calls = append(*calls, rate)
		if rate > limit {
			return &results.Result{
				Rate:    rate,
				Verdict: results.VerdictFailGenerator,
				Generator: &results.GeneratorStatus{
					Saturated:    true,
					TargetRate:   float64(rate),
					AchievedRate: float64(limit),
					Reason:       "maxVUs reached",
				},
			}, nil
		}
		return &results.Result{Rate: rate, Verdict: results.VerdictPass}, nil
	}
}

// scriptedProbe returns a scripted sequence of verdicts per rate, repeating the
// last scripted verdict once the sequence is exhausted and falling back to
// fallback for rates that were not scripted.
type scriptedProbe struct {
	script   map[int][]results.Verdict
	fallback results.Verdict
	seen     map[int]int
	calls    []int
}

func newScripted(fallback results.Verdict) *scriptedProbe {
	return &scriptedProbe{
		script:   map[int][]results.Verdict{},
		fallback: fallback,
		seen:     map[int]int{},
	}
}

func (s *scriptedProbe) fn(_ context.Context, rate int, _ time.Duration) (*results.Result, error) {
	s.calls = append(s.calls, rate)
	seq := s.script[rate]
	i := s.seen[rate]
	s.seen[rate]++

	v := s.fallback
	switch {
	case i < len(seq):
		v = seq[i]
	case len(seq) > 0:
		v = seq[len(seq)-1]
	}
	return &results.Result{Rate: rate, Verdict: v}, nil
}

func mustRun(t *testing.T, cfg Config, probe ProbeFunc) (*Outcome, error) {
	t.Helper()
	return New(cfg, probe).Run(context.Background())
}

func baseConfig() Config {
	return Config{
		StartRate:           5000,
		IncreaseFactor:      1.5,
		Resolution:          100,
		ConfirmationRuns:    1,
		FailureConfirmation: 1,
		ProbeDuration:       time.Second,
	}
}

// TestBracketAndRefine is the happy path from PLAN-OUTLINE.md §4-§5: grow until
// something fails, then bisect until the bracket is within the resolution.
func TestBracketAndRefine(t *testing.T) {
	var calls []int
	out, err := mustRun(t, baseConfig(), capacityProbe(10000, &calls))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !out.BracketFound {
		t.Error("BracketFound = false, want true")
	}
	if out.LowerBoundOnly {
		t.Error("LowerBoundOnly = true, want false")
	}
	if out.PassingRate > 10000 {
		t.Errorf("PassingRate = %d, want <= capacity 10000", out.PassingRate)
	}
	if out.FailingRate <= 10000 {
		t.Errorf("FailingRate = %d, want > capacity 10000", out.FailingRate)
	}
	if got := out.FailingRate - out.PassingRate; got > 100 {
		t.Errorf("bracket width = %d, want <= resolution 100", got)
	}
	if out.Capacity != out.PassingRate {
		t.Errorf("Capacity = %d, want %d", out.Capacity, out.PassingRate)
	}

	// The exponential phase must have probed 5000, 7500 and 11250 before any
	// bisection happened.
	want := []int{5000, 7500, 11250}
	for i, r := range want {
		if i >= len(calls) || calls[i] != r {
			t.Fatalf("exponential probes = %v, want prefix %v", calls, want)
		}
	}

	// Probe indices are 1-based and contiguous.
	for i, p := range out.Probes {
		if p.Index != i+1 {
			t.Errorf("probe %d has Index %d", i, p.Index)
		}
	}
	if len(out.Probes) != len(calls) {
		t.Errorf("%d probes recorded but %d measurements taken", len(out.Probes), len(calls))
	}
}

// TestResolutionBoundsTheBracket checks the binary phase stops as soon as the
// bracket is no wider than the configured resolution.
func TestResolutionBoundsTheBracket(t *testing.T) {
	for _, res := range []int{1, 25, 100, 500} {
		cfg := baseConfig()
		cfg.Resolution = res

		var calls []int
		out, err := mustRun(t, cfg, capacityProbe(10000, &calls))
		if err != nil {
			t.Fatalf("resolution %d: Run: %v", res, err)
		}
		if width := out.FailingRate - out.PassingRate; width > res {
			t.Errorf("resolution %d: bracket width = %d, want <= %d", res, width, res)
		}
		if out.PassingRate > 10000 || out.FailingRate <= 10000 {
			t.Errorf("resolution %d: bracket [%d, %d] does not contain capacity 10000",
				res, out.PassingRate, out.FailingRate)
		}
	}
}

// TestNoFailureBeforeMaxRate covers a system that never fails within the
// configured ceiling: the result is a lower bound, not a capacity.
func TestNoFailureBeforeMaxRate(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxRate = 20000

	var calls []int
	out, err := mustRun(t, cfg, capacityProbe(100000, &calls))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.LowerBoundOnly {
		t.Error("LowerBoundOnly = false, want true")
	}
	if out.BracketFound {
		t.Error("BracketFound = true, want false")
	}
	if out.FailingRate != 0 {
		t.Errorf("FailingRate = %d, want 0", out.FailingRate)
	}
	if out.Capacity != 16875 {
		t.Errorf("Capacity = %d, want 16875 (last rate below max_rate)", out.Capacity)
	}
	if len(out.Notes) == 0 {
		t.Error("expected a note explaining the missing failure boundary")
	}
}

// TestNoPassingRate covers a start_rate that is already above capacity.
func TestNoPassingRate(t *testing.T) {
	var calls []int
	out, err := mustRun(t, baseConfig(), capacityProbe(100, &calls))
	if !errors.Is(err, ErrNoPassingRate) {
		t.Fatalf("err = %v, want ErrNoPassingRate", err)
	}
	if out == nil {
		t.Fatal("outcome is nil; the completed probes must still be reported")
	}
	if len(out.Probes) != 1 {
		t.Fatalf("len(Probes) = %d, want 1", len(out.Probes))
	}
	if out.Probes[0].Rate != 5000 {
		t.Errorf("probe rate = %d, want 5000", out.Probes[0].Rate)
	}
	if out.Pass() {
		t.Error("Pass() = true, want false")
	}
}

// TestGeneratorSaturationAborts covers PLAN-OUTLINE.md §17: a saturated
// generator invalidates the probe, so the search must stop rather than record
// the rate as a capacity limit.
func TestGeneratorSaturationAborts(t *testing.T) {
	var calls []int
	out, err := mustRun(t, baseConfig(), saturatingProbe(8000, &calls))
	if !errors.Is(err, ErrGeneratorSaturated) {
		t.Fatalf("err = %v, want ErrGeneratorSaturated", err)
	}
	if out == nil {
		t.Fatal("outcome is nil")
	}
	if len(out.Probes) != 3 {
		t.Fatalf("len(Probes) = %d, want 3 (5000, 7500, 11250)", len(out.Probes))
	}
	if last := out.Probes[2]; last.Pass || last.Result.Verdict != results.VerdictFailGenerator {
		t.Errorf("last probe = %+v, want a generator failure", last)
	}
	// The saturated rate must not have been mistaken for a failing bracket edge.
	if out.FailingRate != 0 {
		t.Errorf("FailingRate = %d, want 0", out.FailingRate)
	}
	// The error must be actionable: the probe was discarded, so it should say
	// how to get past the saturation.
	if !strings.Contains(err.Error(), "max_vu_growth") {
		t.Errorf("error %q does not mention search.max_vu_growth", err)
	}
}

// TestConfirmationRunsPass checks that a rate is only accepted once it has
// passed the configured number of times in a row.
func TestConfirmationRunsPass(t *testing.T) {
	cfg := baseConfig()
	cfg.ConfirmationRuns = 2

	s := newScripted(results.VerdictFailLatency)
	s.script[5000] = []results.Verdict{results.VerdictPass, results.VerdictPass}

	out, err := mustRun(t, cfg, s.fn)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := out.Probes[0]
	if !first.Pass {
		t.Error("first probe Pass = false, want true")
	}
	if first.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", first.Attempts)
	}
	if first.Note == "" {
		t.Error("expected a note recording the confirmation runs")
	}
	if out.PassingRate != 5000 {
		t.Errorf("PassingRate = %d, want 5000", out.PassingRate)
	}
}

// TestUnconfirmedPassIsAFailure checks that a single pass is not enough when
// confirmation_runs is raised: the follow-up failure decides the rate.
func TestUnconfirmedPassIsAFailure(t *testing.T) {
	cfg := baseConfig()
	cfg.ConfirmationRuns = 2

	s := newScripted(results.VerdictPass)
	s.script[5000] = []results.Verdict{results.VerdictPass, results.VerdictFailLatency}

	out, err := mustRun(t, cfg, s.fn)
	if !errors.Is(err, ErrNoPassingRate) {
		t.Fatalf("err = %v, want ErrNoPassingRate", err)
	}
	if len(out.Probes) != 1 {
		t.Fatalf("len(Probes) = %d, want 1", len(out.Probes))
	}
	if p := out.Probes[0]; p.Pass || p.Attempts != 2 {
		t.Errorf("probe = {Pass: %v, Attempts: %d}, want {false, 2}", p.Pass, p.Attempts)
	}
}

// TestFailureConfirmation checks the mirror rule: with failure_confirmation
// raised, a single transient failure is retried and does not condemn the rate.
func TestFailureConfirmation(t *testing.T) {
	cfg := baseConfig()
	cfg.FailureConfirmation = 2

	s := newScripted(results.VerdictFailLatency)
	s.script[5000] = []results.Verdict{results.VerdictFailLatency, results.VerdictPass}

	out, err := mustRun(t, cfg, s.fn)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p := out.Probes[0]; !p.Pass || p.Attempts != 2 {
		t.Errorf("probe = {Pass: %v, Attempts: %d}, want {true, 2}", p.Pass, p.Attempts)
	}
	if out.PassingRate != 5000 {
		t.Errorf("PassingRate = %d, want 5000", out.PassingRate)
	}
}

// TestMaxProbesCap checks the hard cap on measurements.
func TestMaxProbesCap(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxProbes = 4

	var calls []int
	out, err := mustRun(t, cfg, capacityProbe(1_000_000, &calls))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Probes) != 4 {
		t.Fatalf("len(Probes) = %d, want 4", len(out.Probes))
	}
	if !out.LowerBoundOnly {
		t.Error("LowerBoundOnly = false, want true")
	}
	if out.Capacity != 16875 {
		t.Errorf("Capacity = %d, want 16875", out.Capacity)
	}
}

// TestValidationRun covers phase C: the validation probe is the authority on
// the final verdict and runs at the discovered capacity scaled by the margin.
func TestValidationRun(t *testing.T) {
	tests := []struct {
		name         string
		margin       float64
		pinnedRate   int
		wantMultiple float64
	}{
		{name: "default margin targets capacity", wantMultiple: 1},
		{name: "safety margin scales down", margin: 0.9, wantMultiple: 0.9},
		{name: "pinned rate wins", margin: 0.5, pinnedRate: 4000, wantMultiple: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.ValidationDuration = time.Minute
			cfg.SafetyMargin = tc.margin
			cfg.ValidationRate = tc.pinnedRate

			var calls []int
			out, err := mustRun(t, cfg, capacityProbe(10000, &calls))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if out.Validation == nil {
				t.Fatal("Validation is nil, want a validation probe")
			}
			if out.Validation.Phase != PhaseValidation {
				t.Errorf("phase = %q, want %q", out.Validation.Phase, PhaseValidation)
			}
			if !out.Validation.Pass {
				t.Error("validation failed, want a pass")
			}
			if !out.Pass() {
				t.Error("Pass() = false, want true")
			}

			want := tc.pinnedRate
			if want == 0 {
				want = int(math.Round(float64(out.PassingRate) * tc.wantMultiple))
			}
			if out.ValidationRate != want {
				t.Errorf("ValidationRate = %d, want %d", out.ValidationRate, want)
			}
			if out.Validation.Rate != want {
				t.Errorf("validation probe rate = %d, want %d", out.Validation.Rate, want)
			}
			// The validation probe is part of the trace, so it is recorded last.
			if last := out.Probes[len(out.Probes)-1]; last.Phase != PhaseValidation {
				t.Errorf("last probe phase = %q, want %q", last.Phase, PhaseValidation)
			}
		})
	}
}

// TestValidationOverridesVerdict checks that a failing validation run fails the
// whole search even though the search itself found a passing rate.
func TestValidationOverridesVerdict(t *testing.T) {
	cfg := baseConfig()
	cfg.ValidationDuration = time.Minute
	cfg.ValidationRate = 50000 // well above the system's capacity

	var calls []int
	out, err := mustRun(t, cfg, capacityProbe(10000, &calls))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Validation == nil || out.Validation.Pass {
		t.Fatalf("Validation = %+v, want a failing validation probe", out.Validation)
	}
	if out.Pass() {
		t.Error("Pass() = true, want false")
	}
	if out.PassingRate == 0 {
		t.Error("PassingRate = 0; the search's own finding must still be reported")
	}
}

// TestValidationDisabledByDefault checks that omitting validation_duration skips
// phase C entirely.
func TestValidationDisabledByDefault(t *testing.T) {
	var calls []int
	out, err := mustRun(t, baseConfig(), capacityProbe(10000, &calls))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Validation != nil {
		t.Errorf("Validation = %+v, want nil", out.Validation)
	}
	for _, p := range out.Probes {
		if p.Phase == PhaseValidation {
			t.Errorf("probe %d is a validation probe, want none", p.Index)
		}
	}
}

// TestProgressHook checks the hook fires once per completed measurement.
func TestProgressHook(t *testing.T) {
	var got []int
	c := New(baseConfig(), capacityProbe(10000, new([]int)))
	c.OnProbe(func(p Probe) { got = append(got, p.Rate) })

	out, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != len(out.Probes) {
		t.Fatalf("hook fired %d times, want %d", len(got), len(out.Probes))
	}
	for i := range got {
		if got[i] != out.Probes[i].Rate {
			t.Errorf("hook %d saw rate %d, want %d", i, got[i], out.Probes[i].Rate)
		}
	}
}

// TestControllerDefaults covers standalone use: anything the config layer would
// normally default is filled in by the controller too.
func TestControllerDefaults(t *testing.T) {
	cfg := Config{StartRate: 1000, ProbeDuration: time.Second}

	var calls []int
	out, err := mustRun(t, cfg, capacityProbe(2000, &calls))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// With the default resolution of 1 the binary phase pins the boundary
	// exactly, so the passing rate is the capacity itself.
	if out.PassingRate != 2000 {
		t.Errorf("PassingRate = %d, want 2000", out.PassingRate)
	}
	if out.FailingRate != 2001 {
		t.Errorf("FailingRate = %d, want 2001", out.FailingRate)
	}
	if width := out.FailingRate - out.PassingRate; width > 1 {
		t.Errorf("bracket width = %d, want <= 1 (default resolution)", width)
	}
}

// TestControllerRejectsBadConfig covers the guards on the controller itself.
func TestControllerRejectsBadConfig(t *testing.T) {
	noop := func(context.Context, int, time.Duration) (*results.Result, error) {
		return &results.Result{Verdict: results.VerdictPass}, nil
	}

	tests := []struct {
		name string
		cfg  Config
		fn   ProbeFunc
	}{
		{name: "nil probe", cfg: baseConfig()},
		{name: "zero start rate", cfg: Config{ProbeDuration: time.Second}, fn: noop},
		{name: "zero probe duration", cfg: Config{StartRate: 100}, fn: noop},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg, tc.fn).Run(context.Background()); err == nil {
				t.Error("Run: got nil error, want a config error")
			}
		})
	}
}

// TestProbeErrorPropagates checks that a probe failure (not a threshold breach)
// aborts the search and surfaces the error.
func TestProbeErrorPropagates(t *testing.T) {
	boom := errors.New("k6 exploded")
	probe := func(context.Context, int, time.Duration) (*results.Result, error) {
		return nil, boom
	}
	out, err := New(baseConfig(), probe).Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if out == nil {
		t.Fatal("outcome is nil")
	}
}

// TestContextCancellation checks the search stops promptly when cancelled.
func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := New(baseConfig(), capacityProbe(10000, new([]int))).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(out.Probes) != 0 {
		t.Errorf("len(Probes) = %d, want 0", len(out.Probes))
	}
}

// TestConfigJSONRendersDurationsAsStrings keeps the machine-readable report
// readable: a raw nanosecond count is not something a human can check.
func TestConfigJSONRendersDurationsAsStrings(t *testing.T) {
	cfg := Config{
		StartRate:           100,
		IncreaseFactor:      2,
		Resolution:          20,
		ConfirmationRuns:    1,
		FailureConfirmation: 1,
		MaxProbes:           12,
		ProbeDuration:       5 * time.Second,
		ValidationDuration:  90 * time.Second,
		SafetyMargin:        0.9,
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)

	for _, want := range []string{
		`"probe_duration":"5s"`,
		`"validation_duration":"1m30s"`,
		`"start_rate":100`,
		`"safety_margin":0.9`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON %s does not contain %s", got, want)
		}
	}
	if strings.Contains(got, "5000000000") {
		t.Errorf("JSON still exposes nanosecond durations: %s", got)
	}

	// An unset validation duration is omitted rather than rendered as "0s".
	noValidation, err := json.Marshal(Config{ProbeDuration: time.Minute})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(noValidation), "validation_duration") {
		t.Errorf("JSON %s should omit an unset validation duration", noValidation)
	}
}
