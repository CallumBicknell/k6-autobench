package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/callum/k6-autobench/internal/results"
)

// measure runs the probe at a rate until the confirmation rule is satisfied.
//
// The rule is symmetric: a rate is accepted as passing once ConfirmationRuns
// passes have been seen, and as failing once FailureConfirmation failures have
// been seen. With the defaults (1 and 1) a single run decides, which is the
// V0.1 behaviour; raising them is what makes a noisy result re-run instead of
// being believed (PLAN-OUTLINE.md §15).
func (c *Controller) measure(ctx context.Context, phase Phase, rate int) (Probe, error) {
	need := c.cfg.ConfirmationRuns + c.cfg.FailureConfirmation - 1
	if need < 1 {
		need = 1
	}

	passes, fails := 0, 0
	var last *results.Result

	attempts := 0
	for {
		attempts++
		if err := ctx.Err(); err != nil {
			return Probe{}, err
		}
		res, err := c.probe(ctx, rate, c.cfg.ProbeDuration)
		if err != nil {
			return Probe{}, err
		}
		last = res
		if res.Verdict.OK() {
			passes++
		} else {
			fails++
		}
		if passes >= c.cfg.ConfirmationRuns || fails >= c.cfg.FailureConfirmation || attempts >= need {
			break
		}
	}

	p := Probe{
		Phase:    phase,
		Rate:     rate,
		Attempts: attempts,
		Pass:     last.Verdict.OK(),
		Result:   *last,
	}
	if attempts > 1 {
		p.Note = fmt.Sprintf("%d pass / %d fail over %d runs", passes, fails, attempts)
	}
	return p, nil
}

// probeOnce takes a single unconfirmed measurement, used for the validation
// run: it is already long enough that repeating it would be wasteful.
func (c *Controller) probeOnce(ctx context.Context, phase Phase, rate int, d time.Duration) (Probe, error) {
	res, err := c.probe(ctx, rate, d)
	if err != nil {
		return Probe{}, err
	}
	return Probe{
		Phase:    phase,
		Rate:     rate,
		Attempts: 1,
		Pass:     res.Verdict.OK(),
		Result:   *res,
	}, nil
}

// record indexes a probe, appends it to the trace and fires the progress hook.
func (c *Controller) record(p Probe) Probe {
	p.Index = len(c.probes) + 1
	c.probes = append(c.probes, p)
	if c.onProbe != nil {
		c.onProbe(p)
	}
	return p
}

// check validates the config, filling in defaults for anything the caller left
// unset so the controller is usable without going through the config package.
func (c *Controller) check() error {
	if c.probe == nil {
		return errors.New("search: probe function is required")
	}
	if c.cfg.StartRate <= 0 {
		return errors.New("search: start_rate must be greater than 0")
	}
	if c.cfg.ProbeDuration <= 0 {
		return errors.New("search: probe_duration must be greater than 0")
	}
	if c.cfg.IncreaseFactor <= 1 {
		c.cfg.IncreaseFactor = 1.5
	}
	if c.cfg.Resolution <= 0 {
		c.cfg.Resolution = 1
	}
	if c.cfg.ConfirmationRuns <= 0 {
		c.cfg.ConfirmationRuns = 1
	}
	if c.cfg.FailureConfirmation <= 0 {
		c.cfg.FailureConfirmation = 1
	}
	return nil
}

// generatorError explains why a saturated probe aborted the search.
func generatorError(p Probe) error {
	g := p.Result.Generator
	if g == nil {
		return fmt.Errorf("%w at %d/s; raise load_generator.max_vus", ErrGeneratorSaturated, p.Rate)
	}
	msg := fmt.Sprintf("at %d/s: requested %.0f/s, achieved %.0f/s",
		p.Rate, g.TargetRate, g.AchievedRate)
	if g.Reason != "" {
		msg += " (" + g.Reason + ")"
	}
	// The probe is discarded rather than read as a capacity limit, so say what
	// would let the search continue.
	return fmt.Errorf("%w %s; raise load_generator.max_vus, or let autobench "+
		"grow the pool itself with search.max_vu_growth", ErrGeneratorSaturated, msg)
}
