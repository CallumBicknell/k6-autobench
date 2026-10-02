// Package results holds the controller-facing result of a run and the logic
// that turns raw metrics into a verdict.
package results

import (
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
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
)

// OK reports whether the verdict is a pass.
func (v Verdict) OK() bool { return v == VerdictPass }

// Failure records a single threshold breach.
type Failure struct {
	Metric   string  `json:"metric"` // e.g. "latency.p99" or "error_rate"
	Limit    float64 `json:"limit"`
	Observed float64 `json:"observed"`
	Unit     string  `json:"unit"` // "ms" or "ratio"
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
}
