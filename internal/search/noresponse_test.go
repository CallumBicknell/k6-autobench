package search

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	"github.com/CallumBicknell/k6-autobench/internal/results"
)

// unansweredResult is a probe result from a target that never answered: every
// request failed at the transport layer, and Extract has already dropped the
// zero-duration latency samples k6 would otherwise have recorded.
func unansweredResult(rate int) *results.Result {
	return &results.Result{
		Rate:     rate,
		TimeUnit: "1s",
		Duration: config.Duration{Duration: time.Second},
		Verdict:  results.VerdictFailNoResponse,
		Metrics: metrics.Metrics{
			Requests:       int64(rate),
			FailedRequests: int64(rate),
			ErrorRate:      1,
			Iterations:     int64(rate),
			IterationRate:  float64(rate),
			Unanswered:     true,
			StatusClasses: []metrics.StatusClass{
				{Class: metrics.StatusClassOther, Count: int64(rate), Share: 1},
			},
		},
	}
}

// TestSearchReportsNoResponseWhenNothingAnswered: a search pointed at a service
// that never answers must not report a capacity. It used to grow to max_probes and
// claim one, because with no thresholds configured every probe "passed".
func TestSearchReportsNoResponseWhenNothingAnswered(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxProbes = 5

	// Every probe fails, and no response ever carried a status.
	c := New(cfg, func(_ context.Context, rate int, _ time.Duration) (*results.Result, error) {
		return unansweredResult(rate), nil
	})

	out, err := c.Run(context.Background())

	if !errors.Is(err, ErrNoResponses) {
		t.Errorf("err = %v, want it to wrap %v", err, ErrNoResponses)
	}
	if errors.Is(err, ErrNoPassingRate) {
		t.Error("a dead target must not be reported as a start rate that is too high: " +
			"lowering it would only make the benchmark quieter")
	}
	if out == nil {
		t.Fatal("outcome = nil, want the probes that were taken")
	}
	if !out.Unanswered() {
		t.Error("Unanswered() = false, want true")
	}
	if out.Verdict() != results.VerdictFailNoResponse {
		t.Errorf("verdict = %q, want %q", out.Verdict(), results.VerdictFailNoResponse)
	}
	if out.Capacity != 0 {
		t.Errorf("capacity = %d, want none", out.Capacity)
	}
	// The search must stop at the first probe rather than growing to the ceiling.
	if len(out.Probes) != 1 {
		t.Errorf("%d probes taken, want 1: there is nothing to grow toward", len(out.Probes))
	}
}

// TestSearchKeepsNoPassingRateWhenTheTargetAnswered: the two diagnoses are kept
// apart, because they call for opposite responses.
func TestSearchKeepsNoPassingRateWhenTheTargetAnswered(t *testing.T) {
	cfg := baseConfig()

	// The target answers, but too slowly for the SLO even at the start rate.
	c := New(cfg, func(_ context.Context, rate int, _ time.Duration) (*results.Result, error) {
		r := unansweredResult(rate)
		r.Metrics.Unanswered = false
		r.Metrics.StatusClasses = []metrics.StatusClass{{Class: "2xx", Count: 10, Share: 1}}
		r.Metrics.HasLatency = true
		r.Metrics.Percentiles = []metrics.Percentile{{P: 99, Val: 900}} // SLO is 500ms
		r.Verdict = results.VerdictFailLatency
		return r, nil
	})

	out, err := c.Run(context.Background())

	if !errors.Is(err, ErrNoPassingRate) {
		t.Errorf("err = %v, want it to wrap %v", err, ErrNoPassingRate)
	}
	if errors.Is(err, ErrNoResponses) {
		t.Error("a slow target must not be reported as one that never answered")
	}
	if out.Unanswered() {
		t.Error("Unanswered() = true, want false when the target answered")
	}
}

// TestOutcomeUnansweredNeedsEveryProbe: one answered probe means something was
// measured, and the ordinary logic is the right judge of it.
func TestOutcomeUnansweredNeedsEveryProbe(t *testing.T) {
	answered := unansweredResult(100)
	answered.Metrics.Unanswered = false

	tests := []struct {
		name   string
		probes []Probe
		want   bool
	}{
		{"no probes", nil, false},
		{"all unanswered", []Probe{
			{Result: *unansweredResult(100)}, {Result: *unansweredResult(200)}}, true},
		{"one answered", []Probe{
			{Result: *unansweredResult(100)}, {Result: *answered}}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := &Outcome{Probes: tc.probes}
			if got := out.Unanswered(); got != tc.want {
				t.Errorf("Unanswered() = %v, want %v", got, tc.want)
			}
		})
	}
}
