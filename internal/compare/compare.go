package compare

import (
	"fmt"
	"math"
	"sort"

	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
)

// VerdictRegression is reported when a comparison found a change beyond the
// configured tolerance. It uses the same vocabulary as a run's verdict so exit
// codes and reports stay uniform.
const VerdictRegression results.Verdict = "FAIL_REGRESSION"

// Default tolerances. They are deliberately loose: a load test is a noisy
// measurement, and a gate that fires on run-to-run variation is a gate people
// turn off. Narrow them once you know your system's noise floor.
const (
	// DefaultLatencyTolerance is how much worse a latency percentile may get,
	// as a fraction. 0.10 means a 10% rise fails.
	DefaultLatencyTolerance = 0.10
	// DefaultCapacityTolerance is how much capacity may drop, as a fraction.
	DefaultCapacityTolerance = 0.05
	// DefaultErrorTolerance is how much the error rate may rise, in absolute
	// terms. 0.001 is a tenth of a percentage point.
	DefaultErrorTolerance = 0.001
)

// Config is how much change counts as a regression.
type Config struct {
	// Latency is how much worse a latency percentile may get, as a fraction.
	Latency float64 `json:"latency"`
	// Capacity is how much capacity may drop, as a fraction.
	Capacity float64 `json:"capacity"`
	// Errors is how much the error rate may rise, in absolute terms.
	Errors float64 `json:"errors"`
}

// DefaultConfig is the tolerance set a comparison uses when nothing overrides
// it.
func DefaultConfig() Config {
	return Config{
		Latency:  DefaultLatencyTolerance,
		Capacity: DefaultCapacityTolerance,
		Errors:   DefaultErrorTolerance,
	}
}

// Change is one metric, before and after.
type Change struct {
	// Metric names it, e.g. "p99" or "capacity".
	Metric string `json:"metric"`
	// Unit is "ms", "ratio" or "iterations/s".
	Unit string `json:"unit"`
	// Before and After are the two readings.
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	// Percent is the signed change as a fraction, e.g. 0.568 for +56.8%.
	Percent float64 `json:"percent"`
	// PercentMeaningful is false when Before is zero, where a percentage change
	// is undefined rather than infinite.
	PercentMeaningful bool `json:"percent_meaningful"`
	// Worse is true when the change is in the direction that matters: latency up,
	// capacity down, errors up.
	Worse bool `json:"worse"`
	// Regression is Worse and beyond the configured tolerance.
	Regression bool `json:"regression"`
}

// EndpointChange is one request's latency, before and after.
type EndpointChange struct {
	Name string `json:"name"`
	// Before and After are the request's percentile readings.
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	// Percent is the signed change, meaningless when Before is zero.
	Percent           float64 `json:"percent"`
	PercentMeaningful bool    `json:"percent_meaningful"`
	Worse             bool    `json:"worse"`
	Regression        bool    `json:"regression"`
	// Added and Removed mark a request only one of the runs exercised, which is
	// a change in the workload rather than a regression in it.
	Added   bool `json:"added,omitempty"`
	Removed bool `json:"removed,omitempty"`
}

// Report is what comparing two runs produced.
type Report struct {
	Before *Snapshot `json:"before"`
	After  *Snapshot `json:"after"`
	Config Config    `json:"config"`

	// Latency compares every percentile both runs collected.
	Latency []Change `json:"latency"`
	// Capacity compares the two searches' findings, and is nil when the runs are
	// not both searches.
	Capacity *Change `json:"capacity,omitempty"`
	// Errors compares the error rates.
	Errors Change `json:"errors"`
	// Endpoints compares each request both runs exercised, worst change first.
	Endpoints []EndpointChange `json:"endpoints,omitempty"`

	// Verdict is VerdictRegression when anything regressed, otherwise the
	// candidate run's own verdict: a comparison should not turn a passing run
	// into a failing one, only notice when it got worse.
	Verdict results.Verdict `json:"verdict"`
	// Regressions lists the metrics that breached their tolerance.
	Regressions []string `json:"regressions"`
	// Notes are things a reader needs to know to read the numbers honestly:
	// mismatched targets, mismatched rates, a run that saturated its generator.
	Notes []string `json:"notes,omitempty"`
}

// Regressed reports whether the comparison found a regression.
func (r *Report) Regressed() bool { return r.Verdict == VerdictRegression }

// Compare reports what changed between two recorded runs.
//
// The direction matters and is not the same for every metric: a latency rise is
// bad, a capacity drop is bad, an error rise is bad. Reporting a signed
// percentage without that would invite reading "+56%" as good news.
func Compare(before, after *Snapshot, cfg Config) *Report {
	rep := &Report{Before: before, After: after, Config: cfg, Verdict: after.Verdict}

	rep.Latency = comparePercentiles(before, after, cfg)
	rep.Capacity = compareCapacity(before, after, cfg)
	rep.Errors = compareErrorRate(before, after, cfg)
	rep.Endpoints = compareEndpoints(before, after, cfg)
	rep.Notes = comparisonNotes(before, after)

	// Collected in the order the report displays them, so the list reads as a
	// summary of the sections below it rather than a different ordering of them.
	if rep.Capacity != nil && rep.Capacity.Regression {
		rep.Regressions = append(rep.Regressions, rep.Capacity.Metric)
	}
	for _, c := range rep.Latency {
		if c.Regression {
			rep.Regressions = append(rep.Regressions, c.Metric)
		}
	}
	if rep.Errors.Regression {
		rep.Regressions = append(rep.Regressions, rep.Errors.Metric)
	}
	for _, e := range rep.Endpoints {
		if e.Regression {
			rep.Regressions = append(rep.Regressions, e.Name)
		}
	}

	if len(rep.Regressions) > 0 {
		rep.Verdict = VerdictRegression
	}
	return rep
}

// comparePercentiles pairs the percentiles both runs collected, lowest first.
//
// Only the intersection is compared: a percentile one run collected and the
// other did not has nothing to compare against, and inventing a zero would
// report a spectacular improvement.
func comparePercentiles(before, after *Snapshot, cfg Config) []Change {
	var out []Change
	for _, p := range before.Metrics.Percentiles {
		a, ok := metrics.PercentileAt(after.Metrics.Percentiles, p.P)
		if !ok {
			continue
		}
		out = append(out, change(pctLabel(p.P), "ms", p.Val, a, cfg.Latency, true, basisPercent))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

// pctLabel renders a percentile label the way the rest of the reports do.
func pctLabel(p float64) string { return "p" + trimFloat(p) }

// trimFloat renders a float without trailing zeros.
func trimFloat(f float64) string { return fmt.Sprintf("%g", f) }

// compareCapacity pairs the two searches' findings. It is nil when either run is
// a fixed-rate measurement: one measurement is not a capacity, and comparing the
// two would be a category error.
func compareCapacity(before, after *Snapshot, cfg Config) *Change {
	if !before.IsCapacity() || !after.IsCapacity() {
		return nil
	}
	if before.Capacity == 0 || after.Capacity == 0 {
		// One search found no passing rate. There is nothing to compare, and a
		// zero would read as a total collapse.
		return nil
	}
	c := change("capacity", "iterations/s", float64(before.Capacity), float64(after.Capacity),
		cfg.Capacity, false, basisPercent)
	return &c
}

// compareErrorRate compares the two runs' error rates.
func compareErrorRate(before, after *Snapshot, cfg Config) Change {
	// A rise in absolute terms, not a percentage of a percentage: going from
	// 0.001 to 0.002 is a tenth of a point, not a doubling that sounds alarming
	// and is not.
	return change("error rate", "ratio", before.Metrics.ErrorRate, after.Metrics.ErrorRate,
		cfg.Errors, true, basisAbsolute)
}

// compareEndpoints pairs each request both runs exercised, worst change first.
func compareEndpoints(before, after *Snapshot, cfg Config) []EndpointChange {
	byName := map[string]metrics.Endpoint{}
	for _, e := range before.Metrics.Endpoints {
		byName[e.Name] = e
	}

	var out []EndpointChange
	for _, a := range after.Metrics.Endpoints {
		b, ok := byName[a.Name]
		if !ok {
			out = append(out, EndpointChange{Name: a.Name, Added: true})
			continue
		}
		delete(byName, a.Name)

		bp, bOK := metrics.PercentileAt(b.Percentiles, comparedPercentile)
		ap, aOK := metrics.PercentileAt(a.Percentiles, comparedPercentile)
		if !bOK || !aOK {
			continue
		}
		c := change(a.Name, "ms", bp, ap, cfg.Latency, true, basisPercent)
		out = append(out, EndpointChange{
			Name:              a.Name,
			Before:            bp,
			After:             ap,
			Percent:           c.Percent,
			PercentMeaningful: c.PercentMeaningful,
			Worse:             c.Worse,
			Regression:        c.Regression,
		})
	}
	for name := range byName {
		out = append(out, EndpointChange{Name: name, Removed: true})
	}

	// Worst change first, so the request to look at leads.
	sort.SliceStable(out, func(i, j int) bool {
		return endpointSeverity(out[i]) > endpointSeverity(out[j])
	})
	return out
}

// comparedPercentile is the percentile per-request changes are judged on. It is
// the latency SLO most systems are written against, and it is in the default
// percentile set, so both runs usually have it.
const comparedPercentile = 95

// endpointSeverity orders endpoint changes: a regression leads, then the biggest
// movement, then additions and removals.
func endpointSeverity(e EndpointChange) float64 {
	switch {
	case e.Regression:
		return 1000 + math.Abs(e.Percent)
	case e.Added || e.Removed:
		return -1
	default:
		return math.Abs(e.Percent)
	}
}

// basis is how a change is judged against its tolerance, which is not the same
// question as how it is displayed.
type basis int

const (
	// basisPercent judges the relative change. It is right for a metric on an
	// arbitrary scale, where what matters is the proportion it moved.
	basisPercent basis = iota
	// basisAbsolute judges the raw movement. It is right for a metric that is
	// already a proportion of something: an error rate going from 0.1% to 0.2%
	// has doubled, but the doubling is not the alarming part, and a relative
	// tolerance would treat a trivial rate as a crisis.
	basisAbsolute
)

// change builds one metric's comparison.
//
// higherIsWorse picks the direction -- latency and errors are worse when they
// rise, capacity when it falls -- and b picks what the tolerance is measured
// against.
func change(metric, unit string, before, after, tolerance float64, higherIsWorse bool, b basis) Change {
	c := Change{Metric: metric, Unit: unit, Before: before, After: after}
	if before != 0 {
		c.Percent = (after - before) / before
		c.PercentMeaningful = b == basisPercent
	}

	c.Worse = worse(after, before, higherIsWorse)
	if !c.Worse {
		return c
	}

	// The breach is measured on the size of the movement.
	switch {
	case b == basisAbsolute:
		// The tolerance is already in the metric's own units, so the raw
		// movement is what it applies to.
		c.Regression = after-before > tolerance
	case c.PercentMeaningful:
		c.Regression = math.Abs(c.Percent) > tolerance
	default:
		// A relative tolerance against a zero baseline: there is no scale to be
		// relative to, so the tolerance is read against the raw movement
		// instead. That is dimensionally loose -- milliseconds judged against a
		// ratio -- and deliberately so: it is conservative in effect, treating
		// any movement off zero as a regression, which is the safer way to be
		// wrong about a metric that has just appeared where there was none.
		c.Regression = after-before > tolerance
	}
	return c
}

// worse reports whether moving from before to after is a deterioration.
func worse(after, before float64, higherIsWorse bool) bool {
	if higherIsWorse {
		return after > before
	}
	return after < before
}

// comparisonNotes records what a reader needs to know to read the numbers
// honestly. A comparison of two different things is worse than no comparison,
// because it looks like an answer.
func comparisonNotes(before, after *Snapshot) []string {
	var notes []string

	if before.Target != after.Target {
		notes = append(notes, fmt.Sprintf(
			"the two runs measured different targets (%s and %s), so the comparison is not like for like",
			before.Target, after.Target))
	}
	if before.Kind != after.Kind {
		notes = append(notes, fmt.Sprintf(
			"one run is a %s and the other a %s; latency is comparable, capacity is not",
			before.Kind, after.Kind))
	}
	// The rate warning belongs to fixed runs. Two searches decided their
	// capacity at their own rate by construction, so warning that those differ
	// would fire on every capacity change and mean nothing.
	if !before.IsCapacity() && !after.IsCapacity() && before.Rate != after.Rate {
		notes = append(notes, fmt.Sprintf(
			"the runs used different rates (%d/s and %d/s); latency at one load says nothing about latency at another",
			before.Rate, after.Rate))
	}
	for _, s := range []*Snapshot{before, after} {
		if s.GeneratorSaturated {
			notes = append(notes, fmt.Sprintf(
				"%s saturated the load generator, so its numbers describe the generator rather than the target",
				s.Path))
		}
	}
	return notes
}
