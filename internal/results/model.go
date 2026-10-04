// Package results holds the controller-facing result of a run and the logic
// that turns raw metrics into a verdict.
package results

import (
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/collect"
	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
)

// Verdict classifies how a run ended. It is richer than a boolean so the
// report can explain *why* something failed.
type Verdict string

// Possible verdicts.
const (
	VerdictPass          Verdict = "PASS"
	VerdictFailLatency   Verdict = "FAIL_LATENCY"
	VerdictFailErrors    Verdict = "FAIL_ERRORS"
	VerdictFailGenerator Verdict = "FAIL_GENERATOR"
	// VerdictFailNoCapacity is reported by a capacity search that could not find
	// any passing rate, because even search.start_rate breached the SLOs.
	VerdictFailNoCapacity Verdict = "FAIL_NO_CAPACITY"
	// VerdictFailNoResponse is reported when not one request received an HTTP
	// response, so the run measured nothing about the target.
	//
	// It is not a statement that the system is slow or failing: it is a statement
	// that the benchmark never reached it. A pass would claim otherwise, which is
	// how a benchmark pointed at a dead service used to go green.
	VerdictFailNoResponse Verdict = "FAIL_NO_RESPONSE"
)

// OK reports whether the verdict is a pass.
func (v Verdict) OK() bool { return v == VerdictPass }

// Failure records a single threshold breach.
type Failure struct {
	Metric   string  `json:"metric"` // e.g. "latency.p99" or "error_rate"
	Limit    float64 `json:"limit"`
	Observed float64 `json:"observed"`
	Unit     string  `json:"unit"` // "ms" or "ratio"
	// Window is the time window the breach was seen in, when the failure came
	// from windowed analysis rather than the run as a whole. -1 means the whole
	// run, and is omitted from JSON.
	Window int `json:"window,omitempty"`
}

// GeneratorStatus describes the state of the load generator relative to the
// requested rate. It is only populated when the generator is saturated, in
// which case the probe must be discarded rather than read as a capacity limit.
type GeneratorStatus struct {
	Saturated    bool    `json:"saturated"`
	TargetRate   float64 `json:"target_rate"`
	AchievedRate float64 `json:"achieved_rate"`
	Dropped      int64   `json:"dropped_iterations"`
	Reason       string  `json:"reason,omitempty"`
}

// VUPlan records the VU pool that was requested from k6.
type VUPlan struct {
	PreAllocated int `json:"pre_allocated"`
	Max          int `json:"max"`
}

// Result is the complete outcome of one fixed-rate run.
type Result struct {
	Name      string                    `json:"name"`
	Target    string                    `json:"target"`
	Rate      int                       `json:"rate"`
	TimeUnit  string                    `json:"time_unit"`
	Duration  config.Duration           `json:"duration"`
	VUs       VUPlan                    `json:"vus"`
	StartedAt time.Time                 `json:"started_at"`
	EndedAt   time.Time                 `json:"ended_at"`
	ElapsedMs float64                   `json:"elapsed_ms"`
	Metrics   metrics.Metrics           `json:"metrics"`
	Verdict   Verdict                   `json:"verdict"`
	Failures  []Failure                 `json:"failures,omitempty"`
	Generator *GeneratorStatus          `json:"generator,omitempty"`
	Raw       map[string]metrics.Metric `json:"raw,omitempty"`
	// Windows holds per-window aggregates when windowed analysis was enabled.
	Windows []metrics.Window `json:"windows,omitempty"`
	// WindowFailures holds SLO breaches found in individual windows. They only
	// affect the verdict when the validation requires every window to pass.
	WindowFailures []Failure `json:"window_failures,omitempty"`
	// System holds what the resource collectors saw of the system under test
	// while this measurement ran (PLAN-OUTLINE.md §33). It is diagnostic: it
	// never affects the verdict. Empty when no collector is configured.
	System collect.Report `json:"system,omitzero"`
}
