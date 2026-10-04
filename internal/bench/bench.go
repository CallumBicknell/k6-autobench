// Package bench turns a configuration into the measurements the capacity search
// drives. It owns the concerns the search package deliberately knows nothing
// about: sizing the k6 VU pool for each probe rate, warming the system under
// test before measuring it, and retrying a rate whose probe the load generator
// could not sustain (PLAN-OUTLINE.md §17–§18).
package bench

import (
	"context"
	"fmt"
	"time"

	"github.com/callum/k6-autobench/internal/collect"
	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/runner"
	"github.com/callum/k6-autobench/internal/search"
)

// K6Runner executes a single measurement. *runner.Runner satisfies it; tests
// substitute a fake so the sizing, warmup and retry logic can be exercised
// without spawning k6.
type K6Runner interface {
	Run(ctx context.Context, spec runner.Spec) (*runner.Outcome, error)
}

// Measurer runs fixed-rate measurements against a target and carries the state
// the search needs between them: the latency it last observed (which drives VU
// sizing) and the largest VU pool it has needed so far.
type Measurer struct {
	// Config is the base configuration. Per-probe copies come from ForRate.
	Config *config.Config
	// Runner executes k6.
	Runner K6Runner
	// Warmup, when > 0, is a discarded run before each measurement so the system
	// under test is warm (caches, connection pools, JIT) before it is measured.
	Warmup time.Duration
	// MaxVUGrowth bounds how many times a saturated probe may double the VU pool
	// across the whole search. 0 disables growth.
	MaxVUGrowth int
	// Window, when > 0, enables per-window aggregates for every measurement.
	Window time.Duration
	// Timeout, when > 0, bounds how long each k6 process may run. Zero means the
	// measurement's own duration plus a grace period.
	Timeout time.Duration
	// InsecureTLS and CAFile configure how the target's certificate is treated.
	InsecureTLS bool
	CAFile      string
	// RequestTimeout, when > 0, bounds one HTTP request. Zero means k6's own
	// default of 60s.
	RequestTimeout time.Duration
	// Collectors, when set, observe the system under test's own metrics during
	// every measurement (PLAN-OUTLINE.md §33).
	Collectors []collect.Collector
	// OnProgress receives human-readable notices such as warmup and VU growth.
	OnProgress func(string)

	observedLatencyMs float64
	// floor is the largest pool used so far. The pool is monotone: shrinking it
	// would only re-discover a saturation the search already paid for.
	floor       results.VUPlan
	growthLeft  int
	initialised bool
}

// ProbeFunc returns the function the search controller drives. Warmup and
// generator-saturation retries happen inside it, so the search sees exactly one
// evaluated measurement per rate.
func (m *Measurer) ProbeFunc() search.ProbeFunc {
	return func(ctx context.Context, rate int, duration time.Duration) (*results.Result, error) {
		m.init()
		if m.Warmup > 0 {
			m.notify(fmt.Sprintf("warming up %s at %d/s", m.Warmup, rate))
			// Best effort: a failed warmup must not fail the probe, it only
			// means the system starts the measurement cold.
			if _, err := m.measure(ctx, rate, m.Warmup); err != nil && ctx.Err() != nil {
				return nil, err
			}
		}
		return m.probe(ctx, rate, duration)
	}
}

// Measure takes a single measurement at a rate for a duration, with no warmup
// and no generator retry. The fixed-rate runner path uses it directly.
func (m *Measurer) Measure(ctx context.Context, rate int, duration time.Duration) (*results.Result, error) {
	m.init()
	return m.measure(ctx, rate, duration)
}

// probe measures a rate, doubling the VU pool and retrying while the load
// generator (rather than the system under test) is the bottleneck.
func (m *Measurer) probe(ctx context.Context, rate int, duration time.Duration) (*results.Result, error) {
	for {
		res, err := m.measure(ctx, rate, duration)
		if err != nil {
			return nil, err
		}
		if res.Generator == nil || m.growthLeft <= 0 {
			return res, nil
		}

		next, ok := m.grow()
		if !ok {
			// The pool is pinned, so the generator cannot be grown; surface the
			// saturation and let the search abort with advice.
			return res, nil
		}

		m.floor = next
		m.growthLeft--
		m.notify(fmt.Sprintf(
			"generator saturated at %d/s (%s); growing the VU pool to %d/%d and retrying",
			rate, res.Generator.Reason, next.PreAllocated, next.Max))
	}
}

// measure runs one k6 invocation and evaluates it. It is the only place that
// touches k6.
func (m *Measurer) measure(ctx context.Context, rate int, duration time.Duration) (*results.Result, error) {
	if duration <= 0 {
		return nil, fmt.Errorf("measurement duration must be greater than 0")
	}
	plan := m.planFor(rate)

	spec := runner.Spec{
		BaseURL:         m.Config.Target.BaseURL,
		Workload:        m.Config.Workload,
		Rate:            rate,
		TimeUnit:        "1s",
		Duration:        duration,
		PreAllocatedVUs: plan.PreAllocated,
		MaxVUs:          plan.Max,
		Percentiles:     m.Config.Percentiles,
		Window:          m.Window,
		Timeout:         m.Timeout,
		InsecureTLS:     m.InsecureTLS,
		CAFile:          m.CAFile,
		RequestTimeout:  m.RequestTimeout,
		Collectors:      m.Collectors,
	}

	started := time.Now()
	outcome, err := m.Runner.Run(ctx, spec)
	if err != nil {
		return nil, err
	}
	ended := time.Now()

	cfg := m.Config.ForRate(rate, m.observedLatencyMs)
	// The per-probe copy carries the probe's own duration, so the rates are
	// measured over the scenario rather than over k6's wall time.
	cfg.Run.Duration = config.Duration{Duration: duration}

	mm := metrics.Extract(outcome.Summary, cfg.Percentiles)
	mm.RateOverScenario(duration)
	verdict, failures, gen := results.Evaluate(cfg, mm)
	windowFailures := results.WindowFailures(cfg, outcome.Windows)
	verdict = results.ApplyWindowPolicy(cfg, verdict, windowFailures)
	m.observe(mm)
	m.raiseFloor(plan)

	// Raw is intentionally omitted: a search takes dozens of measurements and
	// the full k6 metric set per probe would dwarf everything else in the JSON
	// report. The normalised metrics are what the report and the search use.
	return &results.Result{
		Name:           cfg.Name,
		Target:         cfg.Target.BaseURL,
		Rate:           rate,
		TimeUnit:       "1s",
		Duration:       config.Duration{Duration: duration},
		VUs:            plan,
		StartedAt:      started,
		EndedAt:        ended,
		ElapsedMs:      float64(ended.Sub(started)) / float64(time.Millisecond),
		Metrics:        mm,
		Verdict:        verdict,
		Failures:       failures,
		Generator:      gen,
		Windows:        outcome.Windows,
		WindowFailures: windowFailures,
		System:         outcome.System,
	}, nil
}
