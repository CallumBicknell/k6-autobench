package search

import (
	"context"
	"fmt"
	"math"

	"github.com/callum/k6-autobench/internal/results"
)

// Controller drives a capacity search.
type Controller struct {
	cfg     Config
	probe   ProbeFunc
	onProbe func(Probe)
	probes  []Probe
}

// New returns a controller that drives probe. The config is validated by Run.
func New(cfg Config, probe ProbeFunc) *Controller {
	return &Controller{cfg: cfg, probe: probe}
}

// OnProbe registers a progress hook, called after each measurement completes.
func (c *Controller) OnProbe(fn func(Probe)) { c.onProbe = fn }

// Run executes the search and returns the outcome. A non-nil outcome is
// returned alongside any error so the caller can still report the probes that
// did complete.
func (c *Controller) Run(ctx context.Context) (*Outcome, error) {
	if err := c.check(); err != nil {
		return nil, err
	}

	out := &Outcome{Resolution: c.cfg.Resolution}

	low, high, err := c.bracket(ctx, out)
	if err != nil {
		return c.finish(out, low, high), err
	}
	if low == 0 {
		out = c.finish(out, low, high)
		// A search that never reached the target is a different problem from one
		// that started too high, and the two call for opposite responses.
		if out.Unanswered() {
			return out, fmt.Errorf(
				"%w: not one probe received an HTTP response; check the target address "+
					"and that the service is listening", ErrNoResponses)
		}
		return out, fmt.Errorf(
			"%w: start_rate %d already fails; lower search.start_rate",
			ErrNoPassingRate, c.cfg.StartRate)
	}

	c.refine(ctx, out, &low, &high)

	if err := c.validate(ctx, out, low); err != nil {
		return c.finish(out, low, high), err
	}
	return c.finish(out, low, high), nil
}

// validate is phase C: re-run at (a fraction of) the discovered capacity for
// long enough to trust the number. The validation probe is the authority on the
// final verdict when it is configured.
func (c *Controller) validate(ctx context.Context, out *Outcome, capacity int) error {
	if c.cfg.ValidationDuration <= 0 || capacity <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	rate := c.cfg.ValidationRate
	if rate <= 0 {
		rate = int(math.Round(float64(capacity) * c.safetyMargin()))
	}
	if rate < 1 {
		rate = 1
	}

	p, err := c.probeOnce(ctx, PhaseValidation, rate, c.cfg.ValidationDuration)
	if err != nil {
		return err
	}
	p = c.record(p)

	out.Validation = &p
	out.ValidationRate = rate
	out.SafetyMargin = c.safetyMargin()
	return nil
}

// safetyMargin defaults to 1.0, meaning the validation run targets the
// discovered capacity exactly. PLAN-OUTLINE.md §14 suggests 0.9 for headroom.
func (c *Controller) safetyMargin() float64 {
	if c.cfg.SafetyMargin <= 0 {
		return 1
	}
	return c.cfg.SafetyMargin
}

// bracket is phase A: grow the rate geometrically until a probe fails. It
// returns the passing rate below the boundary (0 if none) and the failing rate
// above it (0 if no failure was reached).
func (c *Controller) bracket(ctx context.Context, out *Outcome) (low, high int, err error) {
	rate := c.cfg.StartRate
	for {
		if err := ctx.Err(); err != nil {
			return low, high, err
		}
		if stop, note := c.stopGrowing(rate); stop {
			out.Notes = append(out.Notes, note)
			out.LowerBoundOnly = true
			return low, 0, nil
		}

		p, err := c.measure(ctx, PhaseExponential, rate)
		if err != nil {
			return low, high, err
		}
		c.record(p)

		switch {
		case p.Result.Verdict == results.VerdictFailGenerator:
			out.GeneratorSaturated = true
			return low, high, generatorError(p)
		case p.Pass:
			low = rate
			rate = c.next(rate)
		default:
			return low, rate, nil
		}
	}
}

// refine is phase B: bisect the bracket until it is no wider than the
// configured resolution.
func (c *Controller) refine(ctx context.Context, out *Outcome, low, high *int) {
	if *high <= *low {
		return
	}
	for *high-*low > c.cfg.Resolution {
		if err := ctx.Err(); err != nil {
			return
		}
		if c.cfg.MaxProbes > 0 && len(c.probes) >= c.cfg.MaxProbes {
			out.Notes = append(out.Notes,
				fmt.Sprintf("stopped after %d probes; bracket is %d wide", len(c.probes), *high-*low))
			return
		}

		mid := *low + (*high-*low)/2
		// Guard against a bracket too narrow for integer bisection to advance.
		if mid <= *low || mid >= *high {
			return
		}

		p, err := c.measure(ctx, PhaseBinary, mid)
		if err != nil {
			return
		}
		c.record(p)

		if p.Result.Verdict == results.VerdictFailGenerator {
			out.Notes = append(out.Notes,
				"generator saturated mid-search; bracket is a lower bound only")
			out.LowerBoundOnly = true
			out.GeneratorSaturated = true
			return
		}
		if p.Pass {
			*low = mid
		} else {
			*high = mid
		}
	}
}

// finish fills in the derived fields of the outcome and runs the validation
// phase if one was configured.
func (c *Controller) finish(out *Outcome, low, high int) *Outcome {
	out.Probes = c.probes
	out.PassingRate = low
	out.FailingRate = high
	out.Capacity = low
	out.BracketFound = high > low
	return out
}

// stopGrowing reports whether the exponential phase should stop growing the
// rate, along with an explanation for the report.
func (c *Controller) stopGrowing(rate int) (bool, string) {
	switch {
	case c.cfg.MaxProbes > 0 && len(c.probes) >= c.cfg.MaxProbes:
		return true, fmt.Sprintf("stopped after %d probes without finding a failure", len(c.probes))
	case c.cfg.MaxRate > 0 && rate > c.cfg.MaxRate:
		return true, fmt.Sprintf("reached search.max_rate (%d/s) without finding a failure", c.cfg.MaxRate)
	case rate > maxSaneRate:
		return true, fmt.Sprintf("reached the sanity ceiling (%d/s) without finding a failure", maxSaneRate)
	}
	return false, ""
}

// next applies the increase factor, guaranteeing forward progress of at least
// the configured resolution.
func (c *Controller) next(rate int) int {
	next := int(math.Round(float64(rate) * c.cfg.IncreaseFactor))
	if next <= rate {
		next = rate + 1
	}
	if step := c.cfg.Resolution; step > 1 && next < rate+step {
		next = rate + step
	}
	return next
}
