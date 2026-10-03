// Package search implements autobench's adaptive capacity search.
//
// The search has three phases, as described in PLAN-OUTLINE.md:
//
//	A  exponential  grow the rate until something fails, to bracket the boundary
//	B  binary       bisect the bracket down to the configured resolution
//	C  validation   re-run at (a fraction of) the discovered capacity
//
// This package deliberately knows nothing about k6 or YAML. It drives a
// caller-supplied ProbeFunc, which keeps the algorithm unit-testable without
// spawning a load generator.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/callum/k6-autobench/internal/results"
)

// Phase records where in the search a probe was taken.
type Phase string

// Search phases.
const (
	PhaseExponential Phase = "exponential"
	PhaseBinary      Phase = "binary"
	PhaseValidation  Phase = "validation"
)

// maxSaneRate bounds the exponential phase so a runaway increase factor cannot
// overflow the rate into nonsense.
const maxSaneRate = 10_000_000

// ErrNoPassingRate reports that even the starting rate failed, so the search
// has no passing rate to bracket against. The user should lower start_rate.
var ErrNoPassingRate = errors.New("no passing rate found")

// ErrGeneratorSaturated reports that the load generator could not sustain the
// requested rate. Per PLAN-OUTLINE.md §17 such a probe must be discarded, never
// read as a capacity limit, because the benchmark itself became the bottleneck.
var ErrGeneratorSaturated = errors.New("load generator saturated")

// ProbeFunc measures the system at a fixed iteration rate for the given
// duration and returns the evaluated outcome.
type ProbeFunc func(ctx context.Context, rate int, duration time.Duration) (*results.Result, error)

// Probe is one measurement plus the search metadata around it.
type Probe struct {
	Index    int            `json:"index"`
	Phase    Phase          `json:"phase"`
	Rate     int            `json:"rate"`
	Attempts int            `json:"attempts"`
	Pass     bool           `json:"pass"`
	Note     string         `json:"note,omitempty"`
	Result   results.Result `json:"result"`
}

// Config parameterises a capacity search.
type Config struct {
	// StartRate is the first rate probed.
	StartRate int `json:"start_rate"`
	// IncreaseFactor multiplies the rate after each passing probe (e.g. 1.5).
	IncreaseFactor float64 `json:"increase_factor"`
	// Resolution stops the binary phase once the bracket is this narrow.
	Resolution int `json:"resolution"`
	// ConfirmationRuns is how many passes confirm a rate as reliably passing.
	ConfirmationRuns int `json:"confirmation_runs"`
	// FailureConfirmation is how many failures confirm a rate as failing.
	FailureConfirmation int `json:"failure_confirmation"`
	// MaxRate stops the exponential phase probing above this rate (0 = no limit).
	MaxRate int `json:"max_rate,omitempty"`
	// MaxProbes caps the total number of measurements (0 = no limit).
	MaxProbes int `json:"max_probes,omitempty"`
	// ProbeDuration is how long each search measurement lasts.
	ProbeDuration time.Duration `json:"probe_duration"`
	// ValidationDuration > 0 enables a validation run after the search.
	ValidationDuration time.Duration `json:"validation_duration,omitempty"`
	// ValidationRate, when > 0, pins the validation rate instead of deriving it
	// from the discovered capacity and SafetyMargin.
	ValidationRate int `json:"validation_rate,omitempty"`
	// SafetyMargin scales the discovered capacity for the validation run.
	SafetyMargin float64 `json:"safety_margin,omitempty"`
}

// MarshalJSON renders the durations as strings ("5s" rather than 5000000000) so
// the machine-readable report stays readable.
func (c Config) MarshalJSON() ([]byte, error) {
	// The shadow type exists only to change two field types; keep it in step
	// with Config above.
	type shadow struct {
		StartRate           int     `json:"start_rate"`
		IncreaseFactor      float64 `json:"increase_factor"`
		Resolution          int     `json:"resolution"`
		ConfirmationRuns    int     `json:"confirmation_runs"`
		FailureConfirmation int     `json:"failure_confirmation"`
		MaxRate             int     `json:"max_rate,omitempty"`
		MaxProbes           int     `json:"max_probes,omitempty"`
		ProbeDuration       string  `json:"probe_duration"`
		ValidationDuration  string  `json:"validation_duration,omitempty"`
		ValidationRate      int     `json:"validation_rate,omitempty"`
		SafetyMargin        float64 `json:"safety_margin,omitempty"`
	}

	s := shadow{
		StartRate:           c.StartRate,
		IncreaseFactor:      c.IncreaseFactor,
		Resolution:          c.Resolution,
		ConfirmationRuns:    c.ConfirmationRuns,
		FailureConfirmation: c.FailureConfirmation,
		MaxRate:             c.MaxRate,
		MaxProbes:           c.MaxProbes,
		ProbeDuration:       c.ProbeDuration.String(),
		ValidationRate:      c.ValidationRate,
		SafetyMargin:        c.SafetyMargin,
	}
	if c.ValidationDuration > 0 {
		s.ValidationDuration = c.ValidationDuration.String()
	}
	return json.Marshal(s)
}

// Outcome is the complete result of a capacity search.
type Outcome struct {
	Probes         []Probe  `json:"probes"`
	PassingRate    int      `json:"passing_rate"`
	FailingRate    int      `json:"failing_rate"`
	Resolution     int      `json:"resolution"`
	Capacity       int      `json:"capacity"`
	BracketFound   bool     `json:"bracket_found"`
	LowerBoundOnly bool     `json:"lower_bound_only"`
	Validation     *Probe   `json:"validation,omitempty"`
	ValidationRate int      `json:"validation_rate,omitempty"`
	SafetyMargin   float64  `json:"safety_margin,omitempty"`
	Notes          []string `json:"notes,omitempty"`

	// GeneratorSaturated records that a probe was discarded because the load
	// generator, not the target, was the bottleneck. Capacity is then not a
	// statement about the system under test (PLAN-OUTLINE.md §17).
	GeneratorSaturated bool `json:"generator_saturated,omitempty"`
}

// Verdict names the overall outcome, in the same vocabulary as a fixed-rate run
// so reports and exit codes stay uniform.
//
// The validation run is the authority when one was configured; otherwise the
// search's own finding decides. A discarded generator probe outranks both,
// because it invalidates the measurement rather than the system.
func (o *Outcome) Verdict() results.Verdict {
	switch {
	case o.Validation != nil:
		return o.Validation.Result.Verdict
	case o.GeneratorSaturated:
		return results.VerdictFailGenerator
	case o.PassingRate > 0:
		return results.VerdictPass
	default:
		return results.VerdictFailNoCapacity
	}
}

// Pass reports whether the search produced a usable, passing capacity estimate.
func (o *Outcome) Pass() bool { return o.Verdict().OK() }
