package metrics

import (
	"sort"
	"strings"
	"time"
)

// Percentile is a single latency percentile reading, in milliseconds.
type Percentile struct {
	P   float64 `json:"p"`
	Val float64 `json:"ms"`
}

// Endpoint is the per-request-name view of a run: what one configured workload
// request actually did, as opposed to the run as a whole.
//
// k6 only includes a tagged submetric in its summary when a threshold selects
// it, which is why the workload script declares always-satisfied thresholds for
// every request name.
type Endpoint struct {
	Name        string       `json:"name"`
	Requests    int64        `json:"requests"`
	RequestRate float64      `json:"request_rate"`
	Failed      int64        `json:"failed"`
	ErrorRate   float64      `json:"error_rate"`
	HasLatency  bool         `json:"has_latency"`
	AvgMs       float64      `json:"latency_avg_ms"`
	MaxMs       float64      `json:"latency_max_ms"`
	Percentiles []Percentile `json:"latency_percentiles_ms,omitempty"`
}

// Metrics is the normalised, controller-facing view of a k6 summary.
type Metrics struct {
	HasLatency        bool         `json:"has_latency"`
	Percentiles       []Percentile `json:"latency_percentiles_ms"`
	AvgMs             float64      `json:"latency_avg_ms"`
	MinMs             float64      `json:"latency_min_ms"`
	MaxMs             float64      `json:"latency_max_ms"`
	Requests          int64        `json:"requests"`
	RequestRate       float64      `json:"request_rate"`
	Iterations        int64        `json:"iterations"`
	IterationRate     float64      `json:"iteration_rate"`
	FailedRequests    int64        `json:"failed_requests"`
	ErrorRate         float64      `json:"error_rate"`
	DroppedIterations int64        `json:"dropped_iterations"`
	VUs               int64        `json:"vus"`
	MaxVUs            int64        `json:"max_vus"`
	TestDurationMs    float64      `json:"test_duration_ms"`
	// DeclaredDurationMs is the scenario's configured duration, set when the
	// rates were recomputed over it rather than over k6's wall time.
	DeclaredDurationMs float64 `json:"declared_duration_ms,omitempty"`
	// Endpoints is empty when the summary carries no per-name submetrics (for
	// example a summary produced by a workload script that does not tag names).
	Endpoints []Endpoint `json:"endpoints,omitempty"`

	// Iteration latency is the end-to-end time for one iteration. For a
	// multi-step journey that is what a user actually waits for, where
	// http_req_duration only covers one step of it.
	HasIterationLatency  bool         `json:"has_iteration_latency"`
	IterationAvgMs       float64      `json:"iteration_avg_ms"`
	IterationMaxMs       float64      `json:"iteration_max_ms"`
	IterationPercentiles []Percentile `json:"iteration_percentiles_ms,omitempty"`
}

// At returns the collected value for percentile p, if present.
func (m Metrics) At(p float64) (float64, bool) {
	for _, pc := range m.Percentiles {
		if pc.P == p {
			return pc.Val, true
		}
	}
	return 0, false
}

// RateOverScenario recomputes the iteration and request rates over the
// scenario's declared duration.
//
// k6's own rate divides the count by the whole wall time, including its own
// startup and teardown, during which it generates no load. On a short run that
// overhead is a visible fraction of the total: a 5s run at 100 iterations/s
// reports 98/s, which reads as a saturated generator when the scenario in fact
// hit its target exactly. Dividing by the declared duration answers the question
// that matters -- did the scenario deliver what it was asked for?
//
// A non-positive duration leaves k6's own rates alone.
func (m *Metrics) RateOverScenario(d time.Duration) {
	secs := d.Seconds()
	if secs <= 0 {
		return
	}
	m.DeclaredDurationMs = secs * 1000
	m.IterationRate = float64(m.Iterations) / secs
	m.RequestRate = float64(m.Requests) / secs
}

// Extract pulls the metrics autobench reports on out of a raw k6 summary.
// percentiles selects which trend percentiles to surface (in ms).
func Extract(s *Summary, percentiles []float64) Metrics {
	var m Metrics

	if v, ok := s.Trend("http_req_duration"); ok {
		m.HasLatency = true
		m.AvgMs = v["avg"]
		m.MinMs = v["min"]
		m.MaxMs = v["max"]
		for _, p := range percentiles {
			if val, ok := v[PercentileKey(p)]; ok {
				m.Percentiles = append(m.Percentiles, Percentile{P: p, Val: val})
			}
		}
	}
	if count, rate, ok := s.Counter("http_reqs"); ok {
		m.Requests = int64(count)
		m.RequestRate = rate
	}
	if count, rate, ok := s.Counter("iterations"); ok {
		m.Iterations = int64(count)
		m.IterationRate = rate
	}
	if rate, trues, ok := s.Rate("http_req_failed"); ok {
		m.ErrorRate = rate
		m.FailedRequests = int64(trues)
	}
	if count, _, ok := s.Counter("dropped_iterations"); ok {
		m.DroppedIterations = int64(count)
	}
	if value, ok := s.Gauge("vus"); ok {
		m.VUs = int64(value)
	}
	if value, ok := s.Gauge("vus_max"); ok {
		m.MaxVUs = int64(value)
	}
	m.TestDurationMs = s.State.TestRunDurationMs
	m.Endpoints = extractEndpoints(s, percentiles)
	m.HasIterationLatency, m.IterationAvgMs, m.IterationMaxMs, m.IterationPercentiles =
		extractIterationLatency(s, percentiles)

	return m
}

// extractIterationLatency pulls the end-to-end iteration timing out of the
// summary. k6 records it in milliseconds like http_req_duration.
func extractIterationLatency(s *Summary, percentiles []float64) (bool, float64, float64, []Percentile) {
	v, ok := s.Trend("iteration_duration")
	if !ok {
		return false, 0, 0, nil
	}
	var out []Percentile
	for _, p := range percentiles {
		if val, ok := v[PercentileKey(p)]; ok {
			out = append(out, Percentile{P: p, Val: val})
		}
	}
	return true, v["avg"], v["max"], out
}

// Submetric name prefixes. k6 renders a tagged submetric as "metric{tag:value}",
// so an exact prefix match is enough to pick one out without a full parser (and
// without tripping over http_req_duration{expected_response:true}).
const (
	reqsSubmetric     = "http_reqs{name:"
	failedSubmetric   = "http_req_failed{name:"
	durationSubmetric = "http_req_duration{name:"
)

// extractEndpoints builds the per-request-name breakdown from the tagged
// submetrics in the summary. Results are ordered busiest-first, then by name, so
// the report is stable and leads with the endpoint that carries the most load.
func extractEndpoints(s *Summary, percentiles []float64) []Endpoint {
	byName := map[string]*Endpoint{}

	get := func(name string) *Endpoint {
		if e, ok := byName[name]; ok {
			return e
		}
		e := &Endpoint{Name: name}
		byName[name] = e
		return e
	}

	for key, metric := range s.Metrics {
		if name, ok := submetricName(key, reqsSubmetric); ok {
			e := get(name)
			e.Requests = int64(metric.Values["count"])
			e.RequestRate = metric.Values["rate"]
			continue
		}
		if name, ok := submetricName(key, failedSubmetric); ok {
			e := get(name)
			// As for the run-level metric, "passes" counts true samples, and
			// every sample of http_req_failed is true when a request failed.
			e.Failed = int64(metric.Values["passes"])
			e.ErrorRate = metric.Values["rate"]
			continue
		}
		if name, ok := submetricName(key, durationSubmetric); ok {
			e := get(name)
			e.HasLatency = true
			e.AvgMs = metric.Values["avg"]
			e.MaxMs = metric.Values["max"]
			for _, p := range percentiles {
				if v, ok := metric.Values[PercentileKey(p)]; ok {
					e.Percentiles = append(e.Percentiles, Percentile{P: p, Val: v})
				}
			}
		}
	}

	if len(byName) == 0 {
		return nil
	}

	out := make([]Endpoint, 0, len(byName))
	for _, e := range byName {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// submetricName returns the tag value of key when it is metric{name:VALUE}.
func submetricName(key, prefix string) (string, bool) {
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "}") {
		return "", false
	}
	return key[len(prefix) : len(key)-1], true
}
