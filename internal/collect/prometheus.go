package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultInterval is how often queries are sampled when the configuration does
// not say.
const DefaultInterval = 5 * time.Second

// DefaultTimeout bounds one query round trip. It exists so a wedged Prometheus
// cannot stall the sampling loop past the end of the measurement.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes bounds a single query response, so a URL that turns out not
// to be Prometheus at all cannot stream unbounded data into a report.
const maxResponseBytes = 4 << 20

// errNoSeries is what a query that matched nothing reports. Prometheus treats an
// empty result as success, but for a resource metric it almost always means the
// expression is wrong, and silently reporting nothing would hide that.
var errNoSeries = errors.New("matched no series")

// stopGrace bounds how long stopping waits for a poll that is already in
// flight. The reading it is about to produce is the last one of the
// measurement, so it is worth a moment; but a wedged monitoring endpoint must
// not hold the report hostage either.
const stopGrace = time.Second

// Query is one PromQL instant query sampled on the collector interval.
type Query struct {
	// Name identifies the series in the report, e.g. "cpu".
	Name string
	// Expr is the PromQL expression, e.g. "rate(process_cpu_seconds_total[1m])".
	Expr string
	// Unit is a free-text label for the rendered value.
	Unit string
}

// Prometheus samples a Prometheus server's instant-query API while a
// measurement runs.
//
// It polls rather than being pushed to: the point is to observe the system
// under test from outside it, so autobench needs no instrumentation of its own
// and works with whatever the target already exports.
type Prometheus struct {
	// Source identifies this server in the report, e.g. "api" or "postgres".
	Source string
	// URL is the server's base URL, e.g. "http://localhost:9090". A path prefix
	// is preserved, so a Prometheus behind a reverse proxy works.
	URL string
	// Queries are the expressions sampled on every tick, in report order.
	Queries []Query
	// Interval is how often the queries are sampled. Defaults to DefaultInterval.
	Interval time.Duration
	// Timeout bounds one query round trip. Defaults to DefaultTimeout.
	Timeout time.Duration
	// HTTP is the client used for queries; nil means a default one.
	HTTP *http.Client
}

var _ Collector = (*Prometheus)(nil)

// Name implements Collector.
func (p *Prometheus) Name() string { return p.Source }

// Start begins sampling in the background and returns the function that stops
// it. It returns immediately: sampling must never delay the measurement.
//
// The returned function must be called exactly once, or the sampling goroutine
// leaks.
func (p *Prometheus) Start(ctx context.Context) func() Report {
	ctx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	acc := newAccumulator(p.Source, time.Now())
	done := make(chan struct{})

	go func() {
		defer close(done)
		p.loop(ctx, stop, acc)
	}()

	return func() Report {
		// Ask the loop to stop rather than cancelling outright: a poll already
		// in flight is the last reading of the measurement, and dropping it
		// would leave a measurement shorter than the interval with no data at
		// all.
		close(stop)
		select {
		case <-done:
		case <-time.After(stopGrace):
			// The endpoint is wedged; give up on the reading and abort the
			// request so stopping cannot hang the report.
			cancel()
			<-done
		}
		cancel()
		return acc.report()
	}
}

// loop samples immediately and then on every tick. The first sample is not left
// to the ticker: a measurement shorter than the interval would otherwise
// collect nothing at all.
func (p *Prometheus) loop(ctx context.Context, stop <-chan struct{}, acc *accumulator) {
	client := p.client()
	p.poll(ctx, client, acc)

	ticker := time.NewTicker(p.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			p.poll(ctx, client, acc)
		}
	}
}

// poll samples every query once. The queries are issued concurrently, because a
// wide query set would otherwise take long enough to skew the readings against
// each other, but their results are recorded in configured order so the report
// is stable between runs.
func (p *Prometheus) poll(ctx context.Context, client *http.Client, acc *accumulator) {
	results := make([][]reading, len(p.Queries))
	errs := make([]error, len(p.Queries))

	var wg sync.WaitGroup
	for i, q := range p.Queries {
		wg.Add(1)
		go func(i int, q Query) {
			defer wg.Done()
			results[i], errs[i] = p.query(ctx, client, q)
		}(i, q)
	}
	wg.Wait()

	offset := time.Since(acc.start)
	for i, q := range p.Queries {
		switch {
		case errs[i] != nil:
			acc.fail(q.Name, errs[i])
		case len(results[i]) == 0:
			acc.fail(q.Name, errNoSeries)
		default:
			for n, r := range results[i] {
				acc.add(q, i, seriesName(q, r.labels, n, len(results[i])), r.value, offset)
			}
		}
	}
}

// reading is one series returned by a query.
type reading struct {
	labels map[string]string
	value  float64
}

// seriesName distinguishes the series a query returned. A query that selects a
// single series keeps its configured name; one that fans out over several gains
// the labels that tell them apart, because collapsing them into one reading
// would be a lie.
func seriesName(q Query, labels map[string]string, idx, total int) string {
	if total <= 1 {
		return q.Name
	}
	if suffix := labelList(labels); suffix != "" {
		return q.Name + "{" + suffix + "}"
	}
	// Nothing distinguished them, so fall back to position rather than letting
	// two rows claim the same name.
	return fmt.Sprintf("%s[%d]", q.Name, idx+1)
}

// labelList renders labels in a stable order. Map iteration is random, and a
// report that renames its own rows between runs is not worth having.
func labelList(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		// __name__ names the metric the query already names, and job is implied
		// by the collector's own name.
		if k == "__name__" || k == "job" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Quote(labels[k]))
	}
	return strings.Join(parts, ",")
}

// query issues one instant query and returns the series it matched.
func (p *Prometheus) query(ctx context.Context, client *http.Client, q Query) ([]reading, error) {
	endpoint, err := p.endpoint(q.Expr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, firstLine(body))
	}

	var env promEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if env.Status != "success" {
		msg := strings.TrimSpace(env.Error)
		if msg == "" {
			msg = "no error message"
		}
		return nil, fmt.Errorf("prometheus error: %s", msg)
	}
	return env.readings()
}

// endpoint builds the instant-query URL for an expression.
func (p *Prometheus) endpoint(expr string) (string, error) {
	u, err := url.Parse(p.URL)
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", p.URL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid url %q: needs a scheme and host", p.URL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/query"
	q := url.Values{}
	q.Set("query", expr)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *Prometheus) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return DefaultInterval
}

func (p *Prometheus) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultTimeout
}

func (p *Prometheus) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: p.timeout()}
}

// promEnvelope is the documented /api/v1/query response envelope.
type promEnvelope struct {
	Status string `json:"status"`
	// Error is only set when Status is "error".
	Error string `json:"error"`
	Data  struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// readings decodes the result. An instant query returns a vector or a scalar;
// anything else is rejected rather than half-read.
func (e promEnvelope) readings() ([]reading, error) {
	switch e.Data.ResultType {
	case "vector":
		var v []struct {
			Metric map[string]string `json:"metric"`
			Value  promPair          `json:"value"`
		}
		if err := json.Unmarshal(e.Data.Result, &v); err != nil {
			return nil, fmt.Errorf("decode vector: %w", err)
		}
		out := make([]reading, 0, len(v))
		for _, s := range v {
			out = append(out, reading{labels: s.Metric, value: s.Value.Value})
		}
		return out, nil
	case "scalar":
		var pair promPair
		if err := json.Unmarshal(e.Data.Result, &pair); err != nil {
			return nil, fmt.Errorf("decode scalar: %w", err)
		}
		return []reading{{value: pair.Value}}, nil
	case "string":
		return nil, errors.New("query returned a string, not a number")
	case "":
		return nil, errors.New("response has no result type")
	default:
		return nil, fmt.Errorf(
			"unsupported result type %q (an instant query returns a vector or a scalar)", e.Data.ResultType)
	}
}

// promPair is Prometheus's [<unix_time>, "<sample_value>"] pair. Sample values
// arrive as JSON strings because JSON has no NaN or Inf; a bare number is
// accepted too, so a server or proxy that re-encodes them still works.
type promPair struct {
	Value float64
}

func (p *promPair) UnmarshalJSON(b []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("expected [timestamp, value], got %d elements", len(pair))
	}

	// The server's timestamp is validated but not used: readings are placed on
	// the measurement's own timeline (Sample.OffsetMs), which is what lines them
	// up with the per-window latency table. A Prometheus whose clock differs
	// from this host's would otherwise shift every reading.
	var ts float64
	if err := json.Unmarshal(pair[0], &ts); err != nil {
		return fmt.Errorf("timestamp: %w", err)
	}

	if err := json.Unmarshal(pair[1], &p.Value); err == nil {
		return nil
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return fmt.Errorf("value %s is neither a number nor a string", pair[1])
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("value %q: %w", s, err)
	}
	p.Value = f
	return nil
}

// firstLine keeps an error body to one readable line.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// accumulator gathers one measurement's readings. Only the sampling goroutine
// writes to it, and the report is read after that goroutine has stopped, so it
// needs no lock.
type accumulator struct {
	source string
	start  time.Time

	// byName indexes series by their report name, so a later poll extends the
	// series an earlier one created rather than starting a second row.
	byName map[string]int
	series []Series
	// order is the configured query index behind each series, which keeps the
	// report in the order the queries were written rather than an arbitrary one.
	order []int

	failures []Failure
	failed   map[string]bool
}

func newAccumulator(source string, start time.Time) *accumulator {
	return &accumulator{
		source: source,
		start:  start,
		byName: map[string]int{},
		failed: map[string]bool{},
	}
}

func (a *accumulator) add(q Query, order int, name string, value float64, offset time.Duration) {
	i, ok := a.byName[name]
	if !ok {
		i = len(a.series)
		a.byName[name] = i
		a.series = append(a.series, Series{
			Source: a.source,
			Name:   name,
			Unit:   q.Unit,
			Query:  q.Expr,
		})
		a.order = append(a.order, order)
	}
	a.series[i].Samples = append(a.series[i].Samples, Sample{
		OffsetMs: float64(offset) / float64(time.Millisecond),
		Value:    value,
	})
}

// fail records a collector problem once per query. A persistently unreachable
// endpoint would otherwise add one line per poll.
func (a *accumulator) fail(query string, err error) {
	if a.failed[query] {
		return
	}
	a.failed[query] = true
	a.failures = append(a.failures, Failure{
		Source: a.source,
		Error:  query + ": " + err.Error(),
	})
}

// report returns what was collected, with the series in configured order.
func (a *accumulator) report() Report {
	idx := make([]int, len(a.series))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		if a.order[idx[x]] != a.order[idx[y]] {
			return a.order[idx[x]] < a.order[idx[y]]
		}
		return a.series[idx[x]].Name < a.series[idx[y]].Name
	})

	out := Report{
		Series:   make([]Series, 0, len(a.series)),
		Failures: a.failures,
	}
	for _, i := range idx {
		out.Series = append(out.Series, a.series[i])
	}
	return out
}
