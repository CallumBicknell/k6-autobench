package metrics

import (
	"sort"
	"strings"
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
	// Endpoints is empty when the summary carries no per-name submetrics (for
	// example a summary produced by a workload script that does not tag names).
	Endpoints []Endpoint `json:"endpoints,omitempty"`
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

	return m
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
