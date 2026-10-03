package report

import (
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/search"
)

// htmlPage is the view model shared by both HTML reports.
//
// Every field is a pre-formatted string: the report renders numbers the same way
// the terminal does (comma-grouped, unit-suffixed), and the template stays free
// of formatting logic.
type htmlPage struct {
	Version  string
	Kind     string
	Title    string
	Target   string
	Workload string
	RanAt    time.Time
	Elapsed  string

	Verdict string
	Passed  bool

	// Headline is the capacity figure, shown large when present.
	Headline string
	Subline  string

	Facts     []htmlFact
	Chart     template.HTML
	Traffic   []htmlFact
	Latency   []htmlPercentile
	Probes    []htmlProbe
	Endpoints []htmlEndpoint

	// Windowed view: latency over time (PLAN-OUTLINE.md §31).
	WindowChart template.HTML
	Windows     []htmlWindow
	WindowNote  string

	Failures  []results.Failure
	Generator *results.GeneratorStatus
	Notes     []string
}

type htmlFact struct {
	Label string
	Value string
}

type htmlPercentile struct {
	Label string
	Value string
}

type htmlProbe struct {
	Index    int
	Phase    string
	Rate     string
	P95      string
	P99      string
	Achieved string
	Verdict  string
	Passed   bool
	Note     string
	Failures []results.Failure
}

type htmlEndpoint struct {
	Name     string
	Requests string
	Rate     string
	Errors   string
	Avg      string
	P95      string
	P99      string
	Max      string
}

// htmlWindow is one row of the latency-over-time table.
type htmlWindow struct {
	Label    string
	Requests string
	Errors   string
	P95      string
	P99      string
	Max      string
	Worst    bool
	Sampled  bool
}

// windowSection renders the windowed view for a report: the chart, the table and
// the worst-window callout.
func windowSection(ws []metrics.Window, cfg *config.Config) (template.HTML, []htmlWindow, string) {
	if len(ws) == 0 {
		return "", nil, ""
	}

	var rows []htmlWindow
	worst := worstWindow(ws)
	for _, w := range ws {
		errors := commafy(w.Failed)
		rows = append(rows, htmlWindow{
			Label:    windowLabel(w),
			Requests: commafy(w.Requests),
			Errors:   errors,
			P95:      windowCell(w, 95),
			P99:      windowCell(w, 99),
			Max:      latencyOrDash(w),
			Worst:    w.Index == worst,
			Sampled:  w.Sampled,
		})
	}

	p, ok := featuredPercentile(cfg.Percentiles)
	if !ok {
		return "", rows, worstWindowSummary(ws, worst)
	}
	chart := windowChart{Windows: ws, P: p, Size: cfg.Analysis.Window.Duration}
	if t := cfg.Thresholds.Latency.P95; t != nil && p == 95 {
		chart.LimitMs = float64(t.Max.Duration) / float64(time.Millisecond)
	}

	var b strings.Builder
	renderWindowChart(&b, chart)
	return template.HTML(b.String()), rows, worstWindowSummary(ws, worst) //nolint:gosec // built by us; dynamic text is escaped inside
}

// RenderHTML writes a self-contained HTML report for a fixed-rate run.
func RenderHTML(w io.Writer, cfg *config.Config, r *results.Result, version string) error {
	page := htmlPage{
		Version:  version,
		Kind:     "fixed-rate run",
		Title:    title(cfg),
		Target:   r.Target,
		Workload: workloadSummary(cfg),
		RanAt:    r.EndedAt,
		Elapsed:  ms(r.ElapsedMs),
		Verdict:  string(r.Verdict),
		Passed:   r.Verdict.OK(),
		Facts: []htmlFact{
			{Label: "Rate", Value: fmt.Sprintf("%s iterations/%s", commafy(int64(r.Rate)), r.TimeUnit)},
			{Label: "Duration", Value: r.Duration.String()},
			{Label: "VUs", Value: fmt.Sprintf("%s pre-allocated / %s max",
				commafy(int64(r.VUs.PreAllocated)), commafy(int64(r.VUs.Max)))},
		},
		Traffic:   trafficFacts(r.Metrics),
		Latency:   latencyFacts(r.Metrics),
		Endpoints: endpointRows(r.Metrics),
		Failures:  r.Failures,
		Generator: r.Generator,
	}
	page.WindowChart, page.Windows, page.WindowNote = windowSection(r.Windows, cfg)
	return renderHTML(w, page)
}

func title(cfg *config.Config) string {
	if cfg.Name == "" {
		return "autobench report"
	}
	return cfg.Name
}

func trafficFacts(m metrics.Metrics) []htmlFact {
	return []htmlFact{
		{Label: "Requests", Value: fmt.Sprintf("%s (%s/s)", commafy(m.Requests), commafyFloat(m.RequestRate, 1))},
		{Label: "Iterations", Value: fmt.Sprintf("%s (%s/s)", commafy(m.Iterations), commafyFloat(m.IterationRate, 1))},
		{Label: "Errors", Value: fmt.Sprintf("%s (%s)", commafy(m.FailedRequests), pct(m.ErrorRate))},
		{Label: "Dropped iterations", Value: commafy(m.DroppedIterations)},
	}
}

func latencyFacts(m metrics.Metrics) []htmlPercentile {
	if !m.HasLatency {
		return nil
	}
	out := []htmlPercentile{{Label: "avg", Value: ms(m.AvgMs)}, {Label: "min", Value: ms(m.MinMs)}}
	for _, p := range m.Percentiles {
		out = append(out, htmlPercentile{Label: formatP(p.P), Value: ms(p.Val)})
	}
	return append(out, htmlPercentile{Label: "max", Value: ms(m.MaxMs)})
}

func percentileCell(m metrics.Metrics, p float64) string {
	if v, ok := m.At(p); ok {
		return ms(v)
	}
	return "—"
}

func endpointRows(m metrics.Metrics) []htmlEndpoint {
	out := make([]htmlEndpoint, 0, len(m.Endpoints))
	for _, e := range m.Endpoints {
		pcts := metrics.Metrics{Percentiles: e.Percentiles}
		out = append(out, htmlEndpoint{
			Name:     e.Name,
			Requests: commafy(e.Requests),
			Rate:     commafyFloat(e.RequestRate, 1),
			Errors:   fmt.Sprintf("%s (%s)", commafy(e.Failed), pct(e.ErrorRate)),
			Avg:      ms(e.AvgMs),
			P95:      percentileCell(pcts, 95),
			P99:      percentileCell(pcts, 99),
			Max:      ms(e.MaxMs),
		})
	}
	return out
}

// RenderCapacityHTML writes a self-contained HTML report for a capacity search,
// including the rate/latency chart and the per-probe trace.
func RenderCapacityHTML(w io.Writer, cfg *config.Config, out *search.Outcome, version string, elapsed time.Duration, ranAt time.Time) error {
	page := htmlPage{
		Version:  version,
		Kind:     "capacity search",
		Title:    title(cfg),
		Target:   cfg.Target.BaseURL,
		Workload: workloadSummary(cfg),
		RanAt:    ranAt,
		Elapsed:  ms(float64(elapsed) / float64(time.Millisecond)),
		Verdict:  string(out.Verdict()),
		Passed:   out.Pass(),
		Facts: []htmlFact{
			{Label: "Search", Value: searchSummary(cfg)},
			{Label: "Confirmation", Value: fmt.Sprintf("%d pass / %d fail",
				cfg.Search.ConfirmationRuns, cfg.Search.FailureConfirmation)},
		},
		Probes:    probeRows(out.Probes),
		Notes:     out.Notes,
		Endpoints: endpointRows(decidingMetrics(out)),
	}
	if cfg.Search.Warmup.Duration > 0 {
		page.Facts = append(page.Facts, htmlFact{
			Label: "Warmup", Value: cfg.Search.Warmup.String() + " per probe",
		})
	}
	if out.Validation != nil {
		page.Facts = append(page.Facts, htmlFact{
			Label: "Validated",
			Value: fmt.Sprintf("%s iterations/s for %s (margin %g)",
				commafy(int64(out.ValidationRate)),
				out.Validation.Result.Duration.String(),
				out.SafetyMargin),
		})
		page.Failures = out.Validation.Result.Failures
		page.Generator = out.Validation.Result.Generator
	}

	switch {
	case out.PassingRate > 0 && !out.LowerBoundOnly:
		page.Headline = commafy(int64(out.Capacity))
		page.Subline = "iterations/s sustained"
	case out.PassingRate > 0:
		page.Headline = "≥ " + commafy(int64(out.Capacity))
		page.Subline = "iterations/s, no failure boundary found"
	default:
		page.Headline = "—"
		page.Subline = "no sustainable rate found"
	}

	page.Chart = template.HTML(renderChart(out, cfg)) //nolint:gosec // built by us; dynamic text is escaped inside
	page.WindowChart, page.Windows, page.WindowNote = windowSection(decidingWindows(out), cfg)
	return renderHTML(w, page)
}

// renderChart builds the rate/latency SVG for a search outcome, or "" when no
// probe reported a p95 to plot.
func renderChart(out *search.Outcome, cfg *config.Config) string {
	chart := latencyChart{MarkerText: "capacity"}
	for _, p := range out.Probes {
		if p.Phase == search.PhaseValidation {
			continue
		}
		v, ok := p.Result.Metrics.At(95)
		if !ok {
			continue
		}
		chart.Points = append(chart.Points, chartPoint{Rate: p.Rate, Ms: v, Pass: p.Pass})
	}
	if len(chart.Points) == 0 {
		return ""
	}
	if t := cfg.Thresholds.Latency.P95; t != nil {
		chart.LimitMs = float64(t.Max.Duration) / float64(time.Millisecond)
		chart.LimitText = "p95 limit " + duration(chart.LimitMs)
	}
	if out.BracketFound {
		chart.MarkerAt = out.Capacity
	}

	var b strings.Builder
	renderLatencyChart(&b, chart)
	return b.String()
}

// decidingMetrics picks the measurement whose endpoints the report describes:
// the validation run when there is one, otherwise the probe at the capacity.
func decidingMetrics(out *search.Outcome) metrics.Metrics {
	if out.Validation != nil {
		return out.Validation.Result.Metrics
	}
	for i := len(out.Probes) - 1; i >= 0; i-- {
		if out.Probes[i].Pass {
			return out.Probes[i].Result.Metrics
		}
	}
	return metrics.Metrics{}
}

// decidingWindows is decidingMetrics for the windowed view: the validation run's
// windows when there is one, otherwise the passing probe's.
func decidingWindows(out *search.Outcome) []metrics.Window {
	if out.Validation != nil {
		return out.Validation.Result.Windows
	}
	for i := len(out.Probes) - 1; i >= 0; i-- {
		if out.Probes[i].Pass {
			return out.Probes[i].Result.Windows
		}
	}
	return nil
}

func probeRows(probes []search.Probe) []htmlProbe {
	out := make([]htmlProbe, 0, len(probes))
	for _, p := range probes {
		out = append(out, htmlProbe{
			Index:    p.Index,
			Phase:    string(p.Phase),
			Rate:     commafy(int64(p.Rate)),
			P95:      percentileCell(p.Result.Metrics, 95),
			P99:      percentileCell(p.Result.Metrics, 99),
			Achieved: commafyFloat(p.Result.Metrics.IterationRate, 0),
			Verdict:  string(p.Result.Verdict),
			Passed:   p.Result.Verdict.OK(),
			Note:     p.Note,
			Failures: p.Result.Failures,
		})
	}
	return out
}

// renderHTML executes the report template.
func renderHTML(w io.Writer, page htmlPage) error {
	if err := htmlTemplate.Execute(w, page); err != nil {
		return fmt.Errorf("render html report: %w", err)
	}
	return nil
}
