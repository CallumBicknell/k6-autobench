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
// it and reports what was seen.
func (p *Prometheus) Start(ctx context.Context) func() Report {
	return sample(ctx, p.Source, p.interval(), &promPoller{
		client:  p.client(),
		url:     p.URL,
		timeout: p.timeout(),
		queries: p.Queries,
	})
}

// promPoller reads one Prometheus source. It is the adapter between the
// Prometheus API and the sampling loop, which knows nothing about either.
type promPoller struct {
	client  *http.Client
	url     string
	timeout time.Duration
	queries []Query
}

// poll issues every query once and collects what came back.
//
// The queries are issued concurrently, because a wide query set would otherwise
// take long enough to skew the readings against each other. The results are
// returned in configured order regardless, so the report is stable between runs.
func (pp *promPoller) poll(ctx context.Context) ([]reading, []pollFailure) {
	results := make([][]vectorEntry, len(pp.queries))
	errs := make([]error, len(pp.queries))

	var wg sync.WaitGroup
	for i, q := range pp.queries {
		wg.Add(1)
		go func(i int, q Query) {
			defer wg.Done()
			results[i], errs[i] = pp.query(ctx, q)
		}(i, q)
	}
	wg.Wait()

	var (
		readings []reading
		failures []pollFailure
	)
	for i, q := range pp.queries {
		switch {
		case errs[i] != nil:
			failures = append(failures, pollFailure{key: q.Name, err: errs[i]})
		case len(results[i]) == 0:
			// An empty vector is a success to Prometheus and almost always a
			// mistake in the expression, so it is reported rather than dropped.
			failures = append(failures, pollFailure{key: q.Name, err: errNoSeries})
		default:
			for n, e := range results[i] {
				readings = append(readings, reading{
					order:  i,
					key:    q.Name,
					name:   seriesName(q, e.labels, n, len(results[i])),
					unit:   q.Unit,
					origin: q.Expr,
					value:  e.value,
				})
			}
		}
	}
	return readings, failures
}

// vectorEntry is one series returned by a query.
type vectorEntry struct {
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
func (pp *promPoller) query(ctx context.Context, q Query) ([]vectorEntry, error) {
	endpoint, err := promEndpoint(pp.url, q.Expr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, pp.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := pp.client.Do(req)
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
	return env.entries()
}

// promEndpoint builds the instant-query URL for an expression.
func promEndpoint(base, expr string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid url %q: needs a scheme and host", base)
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

// entries decodes the result. An instant query returns a vector or a scalar;
// anything else is rejected rather than half-read.
func (e promEnvelope) entries() ([]vectorEntry, error) {
	switch e.Data.ResultType {
	case "vector":
		var v []struct {
			Metric map[string]string `json:"metric"`
			Value  promPair          `json:"value"`
		}
		if err := json.Unmarshal(e.Data.Result, &v); err != nil {
			return nil, fmt.Errorf("decode vector: %w", err)
		}
		out := make([]vectorEntry, 0, len(v))
		for _, s := range v {
			out = append(out, vectorEntry{labels: s.Metric, value: s.Value.Value})
		}
		return out, nil
	case "scalar":
		var pair promPair
		if err := json.Unmarshal(e.Data.Result, &pair); err != nil {
			return nil, fmt.Errorf("decode scalar: %w", err)
		}
		return []vectorEntry{{value: pair.Value}}, nil
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
