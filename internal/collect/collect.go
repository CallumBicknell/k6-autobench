// Package collect samples a system's own metrics while autobench drives load
// against it, so a report can say what the system was doing when capacity ran
// out rather than only how slow it got (PLAN-OUTLINE.md §33).
//
// The k6 summary is the view from the client: latency, throughput, error rate.
// A collector adds the view from the server: CPU, memory, connection pools,
// queue depth. Correlating the two is the difference between "p99 tripled at
// 14,500/s" and "p99 tripled at 14,500/s while PostgreSQL hit 498 of 500
// connections".
//
// Collector failures are deliberately advisory. A target's monitoring being
// unreachable says nothing about the target's capacity, so a collector can
// never fail a measurement: it reports what it could not read and the run
// carries on.
package collect

import (
	"context"
	"math"
)

// Sample is one reading of one series at one moment.
type Sample struct {
	// OffsetMs is when the reading was taken, measured from the start of the
	// measurement. It is deliberately relative: the report lines collector
	// readings up against the per-window latency table, which is relative too,
	// so a spike can be attributed to the moment it happened.
	OffsetMs float64 `json:"offset_ms"`
	Value    float64 `json:"value"`
}

// Series is one named metric's readings across a single measurement.
type Series struct {
	// Source is the collector that produced the series, e.g. "api".
	Source string `json:"source"`
	// Name identifies the metric within the source, e.g. "cpu".
	Name string `json:"name"`
	// Unit is a free-text label from the configuration. "bytes" is rendered in
	// binary units and "percent" with a percent sign; anything else is appended
	// to the number as written.
	Unit string `json:"unit,omitempty"`
	// Query is the expression that produced the series, kept so a report can be
	// reproduced by hand against the same Prometheus.
	Query   string   `json:"query,omitempty"`
	Samples []Sample `json:"samples"`
}

// Label is how a report names the series: the source first, so a row reads as
// "postgres cpu" rather than a bare "cpu".
func (s Series) Label() string { return s.Source + " " + s.Name }

// Stats summarises a series over a measurement.
type Stats struct {
	Min   float64
	Max   float64
	Avg   float64
	Last  float64
	Count int
	// MaxAtMs is when the maximum was read, relative to the measurement start.
	MaxAtMs float64
}

// Summary computes the min, max, average and last reading of a series.
//
// Non-finite readings (NaN before a counter's first sample, +Inf from a
// division by zero) cannot be averaged or plotted, so they are skipped; the
// second return value reports whether any finite reading was left.
func (s Series) Summary() (Stats, bool) {
	var st Stats
	for _, sm := range s.Samples {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		if st.Count == 0 || sm.Value < st.Min {
			st.Min = sm.Value
		}
		if st.Count == 0 || sm.Value > st.Max {
			st.Max = sm.Value
			st.MaxAtMs = sm.OffsetMs
		}
		st.Avg += sm.Value
		st.Last = sm.Value
		st.Count++
	}
	if st.Count == 0 {
		return Stats{}, false
	}
	st.Avg /= float64(st.Count)
	return st, true
}

// Failure records a collector that could not deliver readings. It is a note in
// the report, never a run failure.
type Failure struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

// Report is everything the collectors observed during one measurement.
type Report struct {
	Series   []Series  `json:"series,omitempty"`
	Failures []Failure `json:"failures,omitempty"`
}

// IsZero reports whether the report holds nothing at all, so a result from a
// run with no collectors configured stays clean in JSON.
func (r Report) IsZero() bool { return len(r.Series) == 0 && len(r.Failures) == 0 }

// Merge folds another report into r.
func (r *Report) Merge(other Report) {
	r.Series = append(r.Series, other.Series...)
	r.Failures = append(r.Failures, other.Failures...)
}

// Collector observes a system's own metrics while a measurement runs.
//
// Start begins observing in the background and returns immediately; the
// returned function stops observing and reports what was seen. Start must not
// block, because a slow or unreachable monitoring endpoint must never delay the
// measurement itself.
type Collector interface {
	// Name identifies the collector in the report, e.g. "api".
	Name() string
	// Start begins observing and returns the function that stops it. Sample
	// offsets are relative to the moment Start was called.
	Start(ctx context.Context) (stop func() Report)
}
