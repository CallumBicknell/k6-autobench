package metrics

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Window is one time slice of a measurement: the same aggregates as Metrics, but
// restricted to a slice of the run.
//
// An overall p99 can hide a collapse: a system that is fine for four minutes and
// then falls over still averages out. Per-window percentiles are how that shows
// up (PLAN-OUTLINE.md §31).
type Window struct {
	Index      int     `json:"index"`
	StartMs    float64 `json:"start_ms"`
	EndMs      float64 `json:"end_ms"`
	Requests   int64   `json:"requests"`
	Failed     int64   `json:"failed"`
	ErrorRate  float64 `json:"error_rate"`
	HasLatency bool    `json:"has_latency"`
	AvgMs      float64 `json:"latency_avg_ms"`
	MaxMs      float64 `json:"latency_max_ms"`
	// Sampled is set when the window held more samples than the retention cap,
	// so its percentiles come from a prefix of the window rather than all of it.
	Sampled bool `json:"sampled,omitempty"`
	// Unanswered records that every request in the window failed before a
	// response came back, so the window has no latency to report. Its durations
	// are k6's zeros for requests that never got an answer, and reporting those
	// as 0ms would make a dead window look like the fastest one in the run.
	Unanswered  bool         `json:"unanswered,omitempty"`
	Percentiles []Percentile `json:"latency_percentiles_ms,omitempty"`
	// Endpoints is the per-request breakdown of this window, so a spike can be
	// attributed to the request that caused it rather than only noticed.
	Endpoints []Endpoint `json:"endpoints,omitempty"`
}

// DefaultWindowValues is the per-window cap on retained latency samples. It is
// generous enough that percentiles from a capped window are indistinguishable
// from percentiles over the whole window, while bounding memory for a very high
// rate over a very wide window.
const DefaultWindowValues = 1_000_000

// WindowConfig parameterises windowed analysis of a k6 sample stream.
type WindowConfig struct {
	// Size is the width of each window.
	Size time.Duration
	// Percentiles to compute per window, in milliseconds.
	Percentiles []float64
	// MaxValues caps how many latency samples are retained per window; 0 means
	// DefaultWindowValues.
	MaxValues int
}

// ReadWindows streams a k6 JSON-lines sample file (written by `k6 run --out
// json=...`) and returns per-window aggregates.
//
// It reads one line at a time and keeps only the window in progress, so memory
// is bounded by the window width rather than the length of the run. Windows with
// no samples are still emitted, so a gap in traffic is visible rather than
// silently skipped.
func ReadWindows(r io.Reader, cfg WindowConfig) ([]Window, error) {
	if cfg.Size <= 0 {
		return nil, errors.New("window size must be greater than 0")
	}
	maxValues := cfg.MaxValues
	if maxValues <= 0 {
		maxValues = DefaultWindowValues
	}

	sc := bufio.NewScanner(r)
	// A k6 point line is a few hundred bytes; allow a generous ceiling so a
	// wider tag set cannot trip the scanner.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		windows []Window
		cur     *windowAcc
		origin  time.Time
		started bool
	)

	flush := func() {
		if cur == nil {
			return
		}
		// Fill any gap with empty windows so indices stay contiguous.
		for len(windows) < cur.index {
			i := len(windows)
			windows = append(windows, Window{
				Index:   i,
				StartMs: float64(i) * float64(cfg.Size) / float64(time.Millisecond),
				EndMs:   float64(i+1) * float64(cfg.Size) / float64(time.Millisecond),
			})
		}
		windows = append(windows, cur.finish(cfg.Percentiles, maxValues))
		cur = nil
	}

	for sc.Scan() {
		line := sc.Bytes()
		// Cheap reject before paying for JSON: a run emits one line per metric
		// per iteration, and we only want three of them.
		if !interesting(line) {
			continue
		}

		var s sample
		if err := json.Unmarshal(line, &s); err != nil {
			return nil, err
		}
		if s.Type != "Point" {
			continue
		}
		if !started {
			origin, started = s.Data.Time, true
		}

		idx := 0
		if offset := s.Data.Time.Sub(origin); offset > 0 {
			idx = int(offset / cfg.Size)
		}
		switch {
		case cur == nil:
			cur = &windowAcc{index: idx, size: cfg.Size}
		case idx > cur.index:
			flush()
			cur = &windowAcc{index: idx, size: cfg.Size}
		case idx < cur.index:
			// Out-of-order timestamps would be a k6 bug; attribute the sample
			// to the window we are already in rather than dropping it.
			idx = cur.index
		}
		cur.add(s.Metric, s.Data.Value, s.Data.Tags["name"])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()

	return windows, nil
}

// sample is the part of a k6 JSON-lines record that windowed analysis needs.
type sample struct {
	Metric string `json:"metric"`
	Type   string `json:"type"`
	Data   struct {
		Time  time.Time         `json:"time"`
		Value float64           `json:"value"`
		Tags  map[string]string `json:"tags"`
	} `json:"data"`
}

// The metric names windowed analysis reads. Everything else in the stream is
// ignored.
var (
	metricDuration = []byte(`"http_req_duration"`)
	metricRequests = []byte(`"http_reqs"`)
	metricFailed   = []byte(`"http_req_failed"`)
	// The response-class counters, so a window where nothing answered can be told
	// apart from one that was merely fast.
	metricStatus = []byte(`"` + StatusClassPrefix)
)

func interesting(line []byte) bool {
	return bytes.Contains(line, metricDuration) ||
		bytes.Contains(line, metricRequests) ||
		bytes.Contains(line, metricFailed) ||
		bytes.Contains(line, metricStatus)
}

// windowAcc accumulates one window. Durations are retained because percentiles
// cannot be computed incrementally.
type windowAcc struct {
	index     int
	size      time.Duration
	requests  int64
	failed    int64
	durations []float64
	// answered records that at least one response carried an HTTP status. A
	// window where none did received no response at all, and its durations are
	// k6's zeros for requests that never got an answer rather than measurements.
	answered bool
	// sawStatus records that the window saw any response-class sample at all. A
	// capture from a workload script that does not record classes has none, and
	// that is not the same as a window where nothing answered.
	sawStatus bool
	// endpoints holds the same aggregates per request name, so a window can be
	// attributed. A sample with no name tag counts only towards the window.
	endpoints map[string]*endpointAcc
}

// endpointAcc accumulates one request name within one window.
type endpointAcc struct {
	requests  int64
	failed    int64
	durations []float64
	answered  bool
	sawStatus bool
}

func (w *windowAcc) add(metric string, value float64, name string) {
	var ep *endpointAcc
	if name != "" {
		if w.endpoints == nil {
			w.endpoints = map[string]*endpointAcc{}
		}
		ep = w.endpoints[name]
		if ep == nil {
			ep = &endpointAcc{}
			w.endpoints[name] = ep
		}
	}

	switch metric {
	case "http_reqs":
		w.requests += int64(value)
		if ep != nil {
			ep.requests += int64(value)
		}
	case "http_req_failed":
		// A rate point is 1 when the sample was true, and every sample of
		// http_req_failed is true when a request failed.
		if value == 1 {
			w.failed++
			if ep != nil {
				ep.failed++
			}
		}
	case "http_req_duration":
		w.durations = append(w.durations, value)
		if ep != nil {
			ep.durations = append(ep.durations, value)
		}
	default:
		// A response-class counter. Only a class other than "other" means a
		// status line came back; "other" is the transport failure k6 records a
		// zero duration for.
		if !strings.HasPrefix(metric, StatusClassPrefix) {
			return
		}
		w.sawStatus = true
		if ep != nil {
			ep.sawStatus = true
		}
		if strings.TrimPrefix(metric, StatusClassPrefix) != StatusClassOther {
			w.answered = true
			if ep != nil {
				ep.answered = true
			}
		}
	}
}

func (w *windowAcc) finish(percentiles []float64, maxValues int) Window {
	out := Window{
		Index:    w.index,
		StartMs:  float64(w.index) * float64(w.size) / float64(time.Millisecond),
		EndMs:    float64(w.index+1) * float64(w.size) / float64(time.Millisecond),
		Requests: w.requests,
		Failed:   w.failed,
	}
	if w.requests > 0 {
		out.ErrorRate = float64(w.failed) / float64(w.requests)
	}

	durations, sampled := capValues(w.durations, maxValues)
	out.Sampled = sampled

	// A window where nothing answered measured no latency. k6 records a duration
	// of zero for a request that never got a response, and a zero reads as
	// instant -- so a window where the target was dead would otherwise look like
	// the fastest one in the run, and would be ranked as such.
	if w.requests > 0 && w.sawStatus && !w.answered {
		out.Unanswered = true
	} else if len(durations) > 0 {
		out.HasLatency = true
		out.AvgMs = mean(durations)
		out.MaxMs = durations[len(durations)-1]
		for _, p := range percentiles {
			out.Percentiles = append(out.Percentiles, Percentile{P: p, Val: PercentileOf(durations, p)})
		}
	}

	out.Endpoints = finishEndpoints(w.endpoints, percentiles, maxValues)
	return out
}

// finishEndpoints renders each request name's slice of the window, busiest first
// and then by name so the order is stable.
func finishEndpoints(accs map[string]*endpointAcc, percentiles []float64, maxValues int) []Endpoint {
	if len(accs) == 0 {
		return nil
	}
	out := make([]Endpoint, 0, len(accs))
	for name, acc := range accs {
		durations, sampled := capValues(acc.durations, maxValues)
		e := Endpoint{
			Name:     name,
			Requests: acc.requests,
			Failed:   acc.failed,
			Sampled:  sampled,
		}
		if acc.requests > 0 {
			e.ErrorRate = float64(acc.failed) / float64(acc.requests)
		}
		// Same rule as the window: an endpoint whose requests never got an
		// answer has no latency, and a zero would read as instant.
		if acc.requests > 0 && acc.sawStatus && !acc.answered {
			// The latency is dropped; the request and error counts remain.
		} else if len(durations) > 0 {
			e.HasLatency = true
			e.AvgMs = mean(durations)
			e.MaxMs = durations[len(durations)-1]
			for _, p := range percentiles {
				e.Percentiles = append(e.Percentiles, Percentile{P: p, Val: PercentileOf(durations, p)})
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// capValues sorts and truncates a window's samples to the retention cap,
// reporting whether anything was dropped.
func capValues(values []float64, maxValues int) ([]float64, bool) {
	if len(values) == 0 {
		return nil, false
	}
	sort.Float64s(values)
	if len(values) > maxValues {
		return values[:maxValues], true
	}
	return values, false
}

func mean(sorted []float64) float64 {
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return sum / float64(len(sorted))
}

// PercentileOf returns the p-th percentile of a sorted slice using linear
// interpolation between closest ranks, the method most latency dashboards use.
//
// Windowed percentiles are computed from raw samples, so they need not match
// k6's own summary percentile to the last decimal.
func PercentileOf(sorted []float64, p float64) float64 {
	switch {
	case len(sorted) == 0:
		return 0
	case len(sorted) == 1:
		return sorted[0]
	}
	rank := p / 100 * float64(len(sorted)-1)
	lo := math.Floor(rank)
	hi := math.Ceil(rank)
	if lo == hi {
		return sorted[int(lo)]
	}
	frac := rank - lo
	return sorted[int(lo)]*(1-frac) + sorted[int(hi)]*frac
}
