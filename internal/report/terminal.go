// Package report renders an autobench result to the terminal.
package report

import (
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/CallumBicknell/k6-autobench/internal/collect"
	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	"github.com/CallumBicknell/k6-autobench/internal/results"
)

const boxWidth = 62

// ANSI colour codes.
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cGreen  = "\033[32m"
	cRed    = "\033[31m"
	cYellow = "\033[33m"
)

type printer struct {
	w     io.Writer
	color bool
}

func (p *printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return code + s + cReset
}

func (p *printer) line(s string) { fmt.Fprintln(p.w, s) }

// kv prints an aligned "label   value" row.
func (p *printer) kv(label, value string) {
	fmt.Fprintf(p.w, "  %-16s%s\n", label, value)
}

// Render writes the full fixed-rate run report. version is the CLI version
// string, so the header cannot drift from the binary that produced it.
func Render(w io.Writer, cfg *config.Config, r *results.Result, version string, color bool) {
	p := &printer{w: w, color: color}

	p.header("AUTOBENCH v"+version, "fixed-rate run")

	p.line("")
	p.kv("Target", r.Target)
	p.kv("Workload", workloadSummary(cfg))
	p.kv("Rate", rateLine(r.Rate, r.TimeUnit, cfg.Workload.RequestsPerIteration()))
	p.kv("Duration", r.Duration.String())
	p.kv("VUs", fmt.Sprintf("%s pre-allocated / %s max",
		commafy(int64(r.VUs.PreAllocated)), commafy(int64(r.VUs.Max))))
	p.line("")

	p.separator()

	p.traffic(r)
	p.line("")
	p.latency(r)
	p.iterationLatency(r, cfg)
	p.endpoints(r.Metrics)
	p.windows(r.Windows, cfg)
	p.endpointDrift(r.Windows, r.Metrics, cfg)
	p.system(r.System, cfg)
	p.correlated(r.Windows, r.System, cfg)

	p.separator()

	p.verdict(r)

	p.line("")
	p.line(p.paint(cDim, fmt.Sprintf("  ran %s · %s", ms(r.ElapsedMs), r.EndedAt.Format("2006-01-02 15:04:05"))))
}

func (p *printer) header(title, subtitle string) {
	p.line(p.boxTop())
	p.line(p.boxLine("  " + p.paint(cBold, title) + "  " + p.paint(cDim, "— "+subtitle)))
	p.line(p.boxBottom())
}

func (p *printer) separator() {
	p.line(p.paint(cDim, "  "+strings.Repeat("─", boxWidth-4)))
}

func (p *printer) traffic(r *results.Result) {
	m := r.Metrics
	p.kv("Requests", fmt.Sprintf("%s  (%s/s)", commafy(m.Requests), commafyFloat(m.RequestRate, 1)))
	p.kv("Iterations", fmt.Sprintf("%s  (%s/s)", commafy(m.Iterations), commafyFloat(m.IterationRate, 1)))
	errors := commafy(m.FailedRequests)
	if m.ErrorRate > 0 {
		errors = p.paint(cRed, errors)
	}
	p.kv("Errors", fmt.Sprintf("%s  (%s)", errors, pct(m.ErrorRate)))
	p.statusBreakdown(m)
}

func (p *printer) latency(r *results.Result) {
	m := r.Metrics
	if m.Unanswered {
		p.kv("Latency", p.paint(cYellow, "no responses were received"))
		return
	}
	if !m.HasLatency {
		p.kv("Latency", p.paint(cYellow, "no http_req_duration samples"))
		return
	}
	p.line("  Latency")
	p.line(fmt.Sprintf("    %-10s%s", "min", ms(m.MinMs)))
	p.line(fmt.Sprintf("    %-10s%s", "avg", ms(m.AvgMs)))
	for _, pc := range m.Percentiles {
		p.line(fmt.Sprintf("    %-10s%s", formatP(pc.P), ms(pc.Val)))
	}
	p.line(fmt.Sprintf("    %-10s%s", "max", ms(m.MaxMs)))
}

// iterationLatency prints the end-to-end iteration time. It is only shown for a
// journey: for a single-request iteration it is just the request latency plus
// overhead, which would be noise.
func (p *printer) iterationLatency(r *results.Result, cfg *config.Config) {
	m := r.Metrics
	if !cfg.Workload.IsJourney() || !m.HasIterationLatency {
		return
	}
	p.line("")
	p.line(fmt.Sprintf("  %s %s", "Journey latency", p.paint(cDim, "(end to end, all steps)")))
	p.line(fmt.Sprintf("    %-10s%s", "avg", ms(m.IterationAvgMs)))
	for _, pc := range m.IterationPercentiles {
		p.line(fmt.Sprintf("    %-10s%s", formatP(pc.P), ms(pc.Val)))
	}
	p.line(fmt.Sprintf("    %-10s%s", "max", ms(m.IterationMaxMs)))
}

func (p *printer) verdict(r *results.Result) {
	label := string(r.Verdict)
	code := cGreen
	if !r.Verdict.OK() {
		code = cRed
	}
	p.line("  RESULT: " + p.paint(cBold+code, label))
	p.line("")

	for _, f := range r.Failures {
		p.line("  " + p.paint(cRed, failureLine(f)))
	}
	if hint := statusHint(r.Metrics.StatusClasses); hint != "" {
		p.line("  " + p.paint(cYellow, "⚠ "+hint))
	}
	if g := r.Generator; g != nil {
		p.line("  " + p.paint(cYellow, "⚠ GENERATOR SATURATION"))
		p.line(fmt.Sprintf("    requested  %s/s", commafyFloat(g.TargetRate, 1)))
		p.line(fmt.Sprintf("    achieved   %s/s", commafyFloat(g.AchievedRate, 1)))
		if g.Reason != "" {
			p.line("    reason     " + g.Reason)
		}
		p.line("    " + p.paint(cDim, "probe discarded — increase load_generator.max_vus"))
	}
}

func failureLine(f results.Failure) string {
	switch f.Unit {
	case "ratio":
		return fmt.Sprintf("%-14s %s  >  %s   (limit)", f.Metric, pct(f.Observed), pct(f.Limit))
	default:
		return fmt.Sprintf("%-14s %s  >  %s   (limit)", f.Metric, ms(f.Observed), duration(f.Limit))
	}
}

// system prints what the collectors saw of the system under test. It is the half
// of the story k6 cannot see: k6 knows how slow the response was, not whether
// the database was out of connections when it arrived.
//
// Like the drift section it is diagnostic, never a gate. It appears only when a
// collector is configured, and it says so plainly when a collector could not
// read anything rather than quietly showing less.
func (p *printer) system(r collect.Report, cfg *config.Config) {
	if r.IsZero() {
		return
	}

	p.line("")
	p.line("  " + p.paint(cBold, "SYSTEM") + " " + p.paint(cDim, "("+systemNote(cfg)+")"))

	rows := systemRows(r)
	if len(rows) > 0 {
		p.line("")
		p.line("  " + p.paint(cBold, fmt.Sprintf("%-24s%11s%11s%11s%10s",
			"SERIES", "MIN", "MAX", "AVG", "PEAK AT")))
		for _, row := range rows {
			if !row.Summarised {
				p.line(fmt.Sprintf("  %-24s%s", truncate(row.Series, 24),
					p.paint(cDim, "no finite readings")))
				continue
			}
			p.line(fmt.Sprintf("  %-24s%11s%11s%11s%10s",
				truncate(row.Series, 24), row.Min, row.Max, row.Avg, row.PeakAt))
		}
	}

	p.collectorIssues(r.Failures)
}

// correlated lists the resources that moved with latency across the windows:
// the closest the data can honestly get to naming a bottleneck (PLAN-OUTLINE.md
// §33).
//
// The section is hard to appear in, on purpose. A row needs enough windows to
// compare, and it has to survive a permutation test; if nothing does, there is
// no section rather than a table of coincidences. The caveat is printed with the
// table rather than left to the reader to remember.
func (p *printer) correlated(ws []metrics.Window, sys collect.Report, cfg *config.Config) {
	rows := correlateRows(ws, sys, cfg)
	if len(rows) == 0 {
		return
	}

	pct, _ := featuredPercentile(cfg.Percentiles)
	p.line("")
	p.line("  " + p.paint(cBold, fmt.Sprintf("LIKELY CORRELATED (resource vs %s, strongest first)", formatP(pct))))
	p.line("")
	p.line("  " + p.paint(cBold, fmt.Sprintf("%-24s%9s%10s", "SERIES", "r", "WINDOWS")))
	for _, row := range rows {
		r := fmt.Sprintf("%+.2f", row.R)
		if math.Abs(row.R) >= 0.7 {
			r = p.paint(cYellow, r)
		}
		p.line(fmt.Sprintf("  %-24s%9s%10d", truncate(row.Series, 24), r, row.Windows))
	}
	p.line("")
	p.line("  " + p.paint(cDim, "Rank correlation of each series against latency across the windows,"))
	p.line("  " + p.paint(cDim, "within one measurement so a constant rate cannot manufacture it."))
	p.line("  " + p.paint(cDim, "Every row survives a permutation test, with the bar raised for the number"))
	p.line("  " + p.paint(cDim, "of series compared. Correlation is not cause."))
}

// collectorIssues lists what a collector could not read. It is a warning, not a
// failure: the target's monitoring being unreachable says nothing about the
// target's capacity, so the run's verdict stands regardless.
func (p *printer) collectorIssues(failures []collect.Failure) {
	if len(failures) == 0 {
		return
	}
	p.line("")
	p.line("  " + p.paint(cYellow, "⚠ COLLECTOR PROBLEMS — readings missing, the run is unaffected"))
	for _, f := range failures {
		p.line("    " + f.Source + ": " + f.Error)
	}
}

// endpoints prints the per-request breakdown. It is the section that answers
// "which endpoint is actually the problem?", so it is skipped when the summary
// carries no per-name submetrics.
func (p *printer) endpoints(m metrics.Metrics) {
	if len(m.Endpoints) == 0 {
		return
	}
	p.line("")
	p.line("  " + p.paint(cBold, fmt.Sprintf("%-18s%9s%9s%10s%10s%10s",
		"REQUEST", "REQS", "ERRORS", "avg", "p95", "p99")))
	for _, e := range m.Endpoints {
		errors := commafy(e.Failed)
		if e.Failed > 0 {
			errors = p.paint(cRed, errors)
		}
		p.line(fmt.Sprintf("  %-18s%9s%9s%10s%10s%10s",
			truncate(e.Name, 18),
			commafy(e.Requests),
			errors,
			endpointAvg(e),
			endpointPercentile(e, 95),
			endpointPercentile(e, 99),
		))
	}
}

// endpointAvg renders an endpoint's mean latency, or "-" when it has none.
func endpointAvg(e metrics.Endpoint) string { return endpointLatency(e, e.AvgMs) }

// endpointLatency renders one endpoint latency, or "-" when the endpoint has
// none. A measurement that received no responses has no latency to show, and a
// zero would read as instant.
func endpointLatency(e metrics.Endpoint, v float64) string {
	if !e.HasLatency {
		return "-"
	}
	return ms(v)
}

// endpointPercentile renders one endpoint percentile, or "-" when the config
// does not collect it.
func endpointPercentile(e metrics.Endpoint, p float64) string {
	for _, pc := range e.Percentiles {
		if pc.P == p {
			return ms(pc.Val)
		}
	}
	return "-"
}

// rateLine renders an iteration rate and, when an iteration is more than one
// HTTP call, the request rate it implies. The two differ by a factor of the
// journey length, which is exactly what PLAN-OUTLINE.md §11 warns about.
func rateLine(rate int, timeUnit string, perIteration int) string {
	out := fmt.Sprintf("%s iterations/%s", commafy(int64(rate)), timeUnit)
	if perIteration > 1 {
		out += fmt.Sprintf("  (= %s requests/%s)", commafy(int64(rate*perIteration)), timeUnit)
	}
	return out
}

// workloadSummary describes the workload shape.
func workloadSummary(cfg *config.Config) string {
	name := cfg.Name
	if name == "" {
		name = "unnamed"
	}

	if cfg.Workload.IsJourney() {
		n := len(cfg.Workload.Journey)
		return fmt.Sprintf("%s (%d-step journey)", name, n)
	}

	n := len(cfg.Workload.Requests)
	noun := "request"
	if n != 1 {
		noun = "requests"
	}
	return fmt.Sprintf("%s (%d weighted %s)", name, n, noun)
}

func (p *printer) boxTop() string {
	return "╭" + strings.Repeat("─", boxWidth-2) + "╮"
}

func (p *printer) boxBottom() string {
	return "╰" + strings.Repeat("─", boxWidth-2) + "╯"
}

func (p *printer) boxLine(content string) string {
	pad := boxWidth - 2 - visibleLen(content)
	if pad < 0 {
		pad = 0
	}
	return "│" + content + strings.Repeat(" ", pad) + "│"
}
