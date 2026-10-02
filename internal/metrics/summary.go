// Package metrics models k6's end-of-test summary JSON and normalises the
// handful of values autobench cares about.
package metrics

import (
	"encoding/json"
	"strconv"
)

// Summary mirrors the object k6 passes to handleSummary().
type Summary struct {
	RootGroup json.RawMessage   `json:"root_group"`
	Metrics   map[string]Metric `json:"metrics"`
	Options   map[string]any    `json:"options"`
	State     State             `json:"state"`
}

// State carries run-level metadata k6 includes in the summary.
type State struct {
	IsStdOutTTY       bool    `json:"isStdOutTTY"`
	IsStdErrTTY       bool    `json:"isStdErrTTY"`
	TestRunDurationMs float64 `json:"testRunDurationMs"`
}

// Metric is a single k6 metric with its aggregated values.
type Metric struct {
	Type       string                     `json:"type"`
	Contains   string                     `json:"contains"`
	Values     map[string]float64         `json:"values"`
	Thresholds map[string]ThresholdResult `json:"thresholds"`
	Submetrics []Submetric                `json:"submetrics"`
}

// ThresholdResult is the pass/fail state of a k6 threshold (unused by
// autobench, which evaluates thresholds itself, but kept for fidelity).
type ThresholdResult struct {
	OK bool `json:"ok"`
}

// Submetric describes a tag-derived child metric.
type Submetric struct {
	Name   string            `json:"name"`
	Parent string            `json:"parent"`
	Suffix string            `json:"suffix"`
	Tags   map[string]string `json:"tags"`
}

// PercentileKey renders the summary values key k6 uses for a percentile, e.g.
// 99.9 -> "p(99.9)".
func PercentileKey(p float64) string {
	return "p(" + strconv.FormatFloat(p, 'f', -1, 64) + ")"
}

func (s *Summary) values(name string) (map[string]float64, bool) {
	m, ok := s.Metrics[name]
	if !ok || m.Values == nil {
		return nil, false
	}
	return m.Values, true
}

// Counter returns the count and per-second rate of a counter metric.
func (s *Summary) Counter(name string) (count, rate float64, ok bool) {
	v, ok := s.values(name)
	if !ok {
		return 0, 0, false
	}
	return v["count"], v["rate"], true
}

// Gauge returns the final value of a gauge metric.
func (s *Summary) Gauge(name string) (value float64, ok bool) {
	v, ok := s.values(name)
	if !ok {
		return 0, false
	}
	value, ok = v["value"]
	return value, ok
}

// Rate returns a rate metric's ratio and the number of samples that were true.
//
// k6 names the true-sample count "passes" and the false-sample count "fails",
// which is easy to misread: for http_req_failed every sample is true when a
// request *failed*, so "passes" is the failed-request count.
func (s *Summary) Rate(name string) (rate, trues float64, ok bool) {
	v, ok := s.values(name)
	if !ok {
		return 0, 0, false
	}
	return v["rate"], v["passes"], true
}

// Trend returns the raw stat map of a trend metric.
func (s *Summary) Trend(name string) (map[string]float64, bool) { return s.values(name) }
