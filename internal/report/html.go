package report

import (
	"fmt"
	"html/template"
	"io"
	"math"
	"strings"
	"time"

	"github.com/callum/k6-autobench/internal/collect"
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

	// IterationLatency is the end-to-end journey time, shown only for a journey.
	IterationLatency []htmlPercentile

	// Status is the response-class split and what it implies.
	Status     []htmlFact
	StatusHint string

	// Windowed view: latency over time (PLAN-OUTLINE.md §31).
	WindowChart template.HTML
	Windows     []htmlWindow
	WindowNote  string
	// Drift attributes a window spike to the request that caused it.
	Drift []htmlDrift

	// System is the server-side view: what the collectors saw of the system
	// under test while the measurement ran (PLAN-OUTLINE.md §33).
	System       []systemRow
	SystemNote   string
	SystemIssues []string

	// Correlated ranks the series that moved with latency across the windows.
	Correlated     []htmlCorrelation
	CorrelatedNote string

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

// htmlDrift is one row of the per-request worst-window attribution.
type htmlDrift struct {
	Name   string
	RunP95 string
	Window string
	Worst  string
	Ratio  string
	// Flag marks a request whose worst window was materially worse than its
	// run-wide latency, which is the one worth looking at.
	Flag bool
}

// htmlCorrelation is one row of the resource-vs-latency ranking.
type htmlCorrelation struct {
	Series  string
	R       string
	Windows int
	// Flag marks a relationship strong enough to be worth looking at.
	Flag bool
}

// correlationSection ranks the collected series by how closely they moved with
// latency, and describes the test they had to pass to appear at all.
func correlationSection(ws []metrics.Window, sys collect.Report, cfg *config.Config) ([]htmlCorrelation, string) {
	rows := correlateRows(ws, sys, cfg)
	if len(rows) == 0 {
		return nil, ""
	}

	out := make([]htmlCorrelation, 0, len(rows))
	for _, row := range rows {
		out = append(out, htmlCorrelation{
			Series:  row.Series,
			R:       fmt.Sprintf("%+.2f", row.R),
			Windows: row.Windows,
			Flag:    math.Abs(row.R) >= 0.7,
		})
	}
	return out, correlationNote(cfg)
}

// correlationNote states the two things a reader needs to not over-read the
// table: what r measures here, and that it is not cause.
func correlationNote(cfg *config.Config) string {
	pct, ok := featuredPercentile(cfg.Percentiles)
	if !ok {
		return ""
	}
	return fmt.Sprintf(
		"Rank correlation of each series against %s across the windows, within one "+
			"measurement so a constant arrival rate cannot manufacture the relationship. "+
			"Every row survives a permutation test, with the bar raised for the number of "+
			"series compared. Correlation is not cause.",
		formatP(pct))
}

// driftSection renders the per-request window attribution, worst first.
func driftSection(ws []metrics.Window, m metrics.Metrics, cfg *config.Config) []htmlDrift {
	pct, ok := featuredPercentile(cfg.Percentiles)
	if !ok {
		return nil
	}
	rows := driftRows(ws, m, pct)
	out := make([]htmlDrift, 0, len(rows))
	for _, row := range rows {
		out = append(out, htmlDrift{
			Name:   row.Name,
			RunP95: ms(row.RunP95),
			Window: row.Window,
			Worst:  ms(row.WorstP95),
			Ratio:  fmt.Sprintf("%.1fx", row.Ratio),
			Flag:   row.Ratio >= 2,
		})
	}
	return out
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
			{Label: "Rate", Value: rateLine(r.Rate, r.TimeUnit, cfg.Workload.RequestsPerIteration())},
			{Label: "Duration", Value: r.Duration.String()},
			{Label: "VUs", Value: fmt.Sprintf("%s pre-allocated / %s max",
				commafy(int64(r.VUs.PreAllocated)), commafy(int64(r.VUs.Max)))},
		},
		Traffic:    trafficFacts(r.Metrics),
		Latency:    latencyFacts(r.Metrics),
		Endpoints:  endpointRows(r.Metrics),
		Failures:   r.Failures,
		Generator:  r.Generator,
		Status:     statusFacts(r.Metrics),
		StatusHint: statusHint(r.Metrics.StatusClasses),
	}
	if cfg.Workload.IsJourney() {
		page.IterationLatency = latencyFactsOf(r.Metrics.HasIterationLatency,
			r.Metrics.IterationAvgMs, r.Metrics.IterationMaxMs, r.Metrics.IterationPercentiles)
	}
	page.WindowChart, page.Windows, page.WindowNote = windowSection(r.Windows, cfg)
	page.Drift = driftSection(r.Windows, r.Metrics, cfg)
	page.System = systemRows(r.System)
	page.SystemNote = systemNote(cfg)
	page.SystemIssues = systemIssues(r.System)
	page.Correlated, page.CorrelatedNote = correlationSection(r.Windows, r.System, cfg)
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
	return latencyFactsOf(m.HasLatency, m.AvgMs, m.MaxMs, m.Percentiles)
}

// statusFacts renders the response-class split, or nil for a run that only ever
// saw 2xx.
func statusFacts(m metrics.Metrics) []htmlFact {
	if !hasNonSuccess(m.StatusClasses) {
		return nil
	}
	out := make([]htmlFact, 0, len(m.StatusClasses))
	for _, c := range m.StatusClasses {
		out = append(out, htmlFact{
			Label: c.Class,
			Value: fmt.Sprintf("%s (%s of responses)", commafy(c.Count), pct(c.Share)),
		})
	}
	return out
}

// latencyFactsOf renders a latency set as label/value pairs, or nil when there
// is nothing to show.
func latencyFactsOf(has bool, avg, max float64, percentiles []metrics.Percentile) []htmlPercentile {
	if !has {
		return nil
	}
	out := []htmlPercentile{{Label: "avg", Value: ms(avg)}}
	for _, p := range percentiles {
		out = append(out, htmlPercentile{Label: formatP(p.P), Value: ms(p.Val)})
	}
	return append(out, htmlPercentile{Label: "max", Value: ms(max)})
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
	page.Drift = driftSection(decidingWindows(out), decidingMetrics(out), cfg)
	page.System = systemRows(decidingSystem(out))
	page.SystemNote = systemNote(cfg)
	page.SystemIssues = systemIssues(decidingSystem(out))
	page.Correlated, page.CorrelatedNote = correlationSection(decidingWindows(out), decidingSystem(out), cfg)
	page.Status = statusFacts(decidingMetrics(out))
	page.StatusHint = statusHint(decidingMetrics(out).StatusClasses)
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

// decidingResult picks the measurement the report describes: the validation run
// when there is one, otherwise the last passing probe. It returns nil when the
// search never passed anything.
func decidingResult(out *search.Outcome) *results.Result { return out.Deciding() }

// decidingMetrics is decidingResult for the endpoint breakdown.
func decidingMetrics(out *search.Outcome) metrics.Metrics {
	if r := decidingResult(out); r != nil {
		return r.Metrics
	}
	return metrics.Metrics{}
}

// decidingWindows is decidingResult for the windowed view.
func decidingWindows(out *search.Outcome) []metrics.Window {
	if r := decidingResult(out); r != nil {
		return r.Windows
	}
	return nil
}

// decidingSystem is decidingResult for the collector readings.
func decidingSystem(out *search.Outcome) collect.Report {
	if r := decidingResult(out); r != nil {
		return r.System
	}
	return collect.Report{}
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
