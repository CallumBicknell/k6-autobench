package metrics

// Percentile is a single latency percentile reading, in milliseconds.
type Percentile struct {
	P   float64 `json:"p"`
	Val float64 `json:"ms"`
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

	return m
}
