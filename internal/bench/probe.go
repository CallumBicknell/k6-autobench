package bench

import (
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	"github.com/CallumBicknell/k6-autobench/internal/results"
)

// init lazily initialises the growth budget so a zero-value Measurer is usable.
func (m *Measurer) init() {
	if m.initialised {
		return
	}
	m.initialised = true
	m.growthLeft = m.MaxVUGrowth
}

// observe records the latency the system just showed, so the next probe sizes
// its VU pool from reality rather than from the 100ms-per-call guess.
//
// Little's law wants the time one iteration occupies a VU. For a single-request
// iteration that is the request latency; for a multi-step journey it is the
// end-to-end iteration time, which is several times larger and would otherwise
// leave the pool far too small.
func (m *Measurer) observe(mm metrics.Metrics) {
	if m.Config.Workload.IsJourney() && mm.HasIterationLatency && mm.IterationAvgMs > 0 {
		m.observedLatencyMs = mm.IterationAvgMs
		return
	}
	if mm.AvgMs > 0 {
		m.observedLatencyMs = mm.AvgMs
	}
}

// planFor sizes the VU pool for a probe rate, never dropping below the largest
// pool the search has already used.
func (m *Measurer) planFor(rate int) results.VUPlan {
	cfg := m.Config.ForRate(rate, m.observedLatencyMs)
	plan := results.VUPlan{
		PreAllocated: cfg.LoadGenerator.PreAllocatedVUs,
		Max:          cfg.LoadGenerator.MaxVUs,
	}
	if plan.PreAllocated < m.floor.PreAllocated {
		plan.PreAllocated = m.floor.PreAllocated
	}
	if plan.Max < m.floor.Max {
		plan.Max = m.floor.Max
	}
	return plan
}

// raiseFloor keeps the pool monotone across the search.
func (m *Measurer) raiseFloor(plan results.VUPlan) {
	if plan.PreAllocated > m.floor.PreAllocated {
		m.floor.PreAllocated = plan.PreAllocated
	}
	if plan.Max > m.floor.Max {
		m.floor.Max = plan.Max
	}
}

// grow doubles the pool that just saturated, capped by anything the user pinned.
// It reports false when the pool cannot actually get bigger than the attempt
// that failed, so the search stops retrying instead of spinning.
func (m *Measurer) grow() (results.VUPlan, bool) {
	next := results.VUPlan{
		PreAllocated: m.floor.PreAllocated * 2,
		Max:          m.floor.Max * 2,
	}
	preCap, maxCap := m.Config.VULimits()
	if preCap > 0 && next.PreAllocated > preCap {
		next.PreAllocated = preCap
	}
	if maxCap > 0 && next.Max > maxCap {
		next.Max = maxCap
	}
	if next.Max < next.PreAllocated {
		next.PreAllocated = next.Max
	}
	if next.Max <= m.floor.Max {
		return results.VUPlan{}, false
	}
	return next, true
}

func (m *Measurer) notify(msg string) {
	if m.OnProgress != nil {
		m.OnProgress(msg)
	}
}
