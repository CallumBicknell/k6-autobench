package report

import (
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
)

// featuredPercentile picks the percentile to lead with in the windowed view. It
// prefers p95, which is the most common latency SLO and part of the default
// percentile set, and falls back to the highest collected percentile.
func featuredPercentile(collected []float64) (float64, bool) {
	for _, p := range collected {
		if p == 95 {
			return 95, true
		}
	}
	if len(collected) == 0 {
		return 0, false
	}
	best := collected[0]
	for _, p := range collected {
		if p > best {
			best = p
		}
	}
	return best, true
}

// percentileValue reads one percentile out of a collected set.
func percentileValue(percentiles []metrics.Percentile, p float64) (float64, bool) {
	return metrics.PercentileAt(percentiles, p)
}

// windowPercentile reads one percentile out of a window.
func windowPercentile(w metrics.Window, p float64) (float64, bool) {
	return percentileValue(w.Percentiles, p)
}

// driftRow is one request's worst window against its own run-wide latency.
type driftRow struct {
	Name     string
	RunP95   float64
	Window   string
	WorstP95 float64
	Ratio    float64
}

// endpointDrift attributes the windowed view to individual requests: for each
// endpoint, its worst window against its own run-wide latency, worst first.
//
// The aggregate window table says latency spiked in 5s-10s. This says which
// request did it, which is the difference between noticing and diagnosing.
func (p *printer) endpointDrift(ws []metrics.Window, m metrics.Metrics, cfg *config.Config) {
	pct, ok := featuredPercentile(cfg.Percentiles)
	if !ok {
		return
	}
	rows := driftRows(ws, m, pct)
	if len(rows) == 0 {
		return
	}

	p.line("")
	p.line("  " + p.paint(cBold, fmt.Sprintf("ENDPOINT DRIFT (worst window vs the run, %s)", formatP(pct))))
	p.line("")
	p.line("  " + p.paint(cBold, fmt.Sprintf("%-20s%11s%14s%11s%10s",
		"REQUEST", "RUN p95", "WORST WINDOW", "p95", "RATIO")))
	for _, row := range rows {
		ratio := fmt.Sprintf("%.1fx", row.Ratio)
		if row.Ratio >= 2 {
			ratio = p.paint(cYellow, ratio)
		}
		p.line(fmt.Sprintf("  %-20s%11s%14s%11s%10s",
			truncate(row.Name, 20),
			ms(row.RunP95),
			row.Window,
			ms(row.WorstP95),
			ratio,
		))
	}
}

// driftRows finds, for every endpoint that has both a run-wide and a windowed
// reading, the window where its latency peaked.
func driftRows(ws []metrics.Window, m metrics.Metrics, p float64) []driftRow {
	if len(ws) < 2 || len(m.Endpoints) == 0 {
		return nil
	}

	var rows []driftRow
	for _, runEP := range m.Endpoints {
		base, ok := percentileValue(runEP.Percentiles, p)
		if !ok || base <= 0 {
			// Without a baseline there is no drift to measure.
			continue
		}

		worst, worstWin, worstVal := -1, -1.0, 0.0
		for i := range ws {
			ep := findEndpoint(ws[i].Endpoints, runEP.Name)
			if ep == nil {
				continue
			}
			v, ok := percentileValue(ep.Percentiles, p)
			if !ok {
				continue
			}
			if worst < 0 || v > worstVal {
				worst, worstWin, worstVal = i, v, v
			}
		}
		if worst < 0 {
			continue
		}

		rows = append(rows, driftRow{
			Name:     runEP.Name,
			RunP95:   base,
			Window:   windowLabel(ws[worst]),
			WorstP95: worstWin,
			Ratio:    worstVal / base,
		})
	}

	// Worst drift first: the request that moved most is the one to look at.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ratio != rows[j].Ratio {
			return rows[i].Ratio > rows[j].Ratio
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

// findEndpoint looks one request up in a window's breakdown.
func findEndpoint(eps []metrics.Endpoint, name string) *metrics.Endpoint {
	for i := range eps {
		if eps[i].Name == name {
			return &eps[i]
		}
	}
	return nil
}

// windows prints latency over time, so a collapse at the end of a run cannot
// hide behind the run's own average (PLAN-OUTLINE.md §31).
func (p *printer) windows(ws []metrics.Window, cfg *config.Config) {
	if len(ws) == 0 {
		return
	}
	p.line("")
	p.line("  " + p.paint(cBold, fmt.Sprintf("LATENCY OVER TIME (%s windows)", cfg.Analysis.Window)))
	p.line("")
	p.line("  " + p.paint(cBold, fmt.Sprintf("%-14s%10s%9s%11s%11s%11s",
		"WINDOW", "REQUESTS", "ERRORS", "p95", "p99", "max")))

	worst := worstWindow(ws)
	for _, w := range ws {
		marker := ""
		if w.Index == worst {
			marker = p.paint(cYellow, "← worst")
		}
		errors := commafy(w.Failed)
		if w.Failed > 0 {
			errors = p.paint(cRed, errors)
		}
		p.line(fmt.Sprintf("  %-14s%10s%9s%11s%11s%11s %s",
			windowLabel(w),
			commafy(w.Requests),
			errors,
			windowCell(w, 95),
			windowCell(w, 99),
			latencyOrDash(w),
			marker,
		))
	}

	if summary := worstWindowSummary(ws, worst); summary != "" {
		p.line("")
		p.line("  " + p.paint(cYellow, summary))
	}
	if anySampled(ws) {
		p.line("  " + p.paint(cDim,
			"Some windows held more samples than were retained; their percentiles come from a prefix."))
	}
}

// windowLabel renders a window's time range, e.g. "10s-20s".
func windowLabel(w metrics.Window) string {
	return fmt.Sprintf("%s-%s",
		time.Duration(w.StartMs)*time.Millisecond,
		time.Duration(w.EndMs)*time.Millisecond)
}

// windowCell renders a window percentile, or "-" when it was not collected.
func windowCell(w metrics.Window, p float64) string {
	if v, ok := windowPercentile(w, p); ok {
		return ms(v)
	}
	return "-"
}

func latencyOrDash(w metrics.Window) string {
	if !w.HasLatency {
		return "-"
	}
	return ms(w.MaxMs)
}

// worstWindow returns the index of the window with the highest featured
// percentile, or the highest max latency when no percentile was collected.
func worstWindow(ws []metrics.Window) int {
	worst, worstVal := -1, -1.0
	for _, w := range ws {
		if w.Requests == 0 {
			continue
		}
		v := w.MaxMs
		if p, ok := windowPercentile(w, 95); ok {
			v = p
		}
		if v > worstVal {
			worst, worstVal = w.Index, v
		}
	}
	return worst
}

// worstWindowSummary describes the worst window relative to the typical one. It
// is deliberately factual: "this window was 18x the median" is a statement about
// the data, not a heuristic guess at a cause.
func worstWindowSummary(ws []metrics.Window, worst int) string {
	if worst < 0 {
		return ""
	}
	var worstW *metrics.Window
	var values []float64
	for i := range ws {
		if ws[i].Index == worst {
			worstW = &ws[i]
		}
		if ws[i].Requests == 0 {
			continue
		}
		if p, ok := windowPercentile(ws[i], 95); ok {
			values = append(values, p)
		}
	}
	if worstW == nil || len(values) < 2 {
		return ""
	}

	sort.Float64s(values)
	median := metrics.PercentileOf(values, 50)
	v, ok := windowPercentile(*worstW, 95)
	if !ok {
		v = worstW.MaxMs
	}
	line := fmt.Sprintf("Worst window  %s: p95 %s", windowLabel(*worstW), ms(v))
	if median > 0 {
		line += fmt.Sprintf(" (%.1fx the median %s)", v/median, ms(median))
	}
	return line
}

func anySampled(ws []metrics.Window) bool {
	for _, w := range ws {
		if w.Sampled {
			return true
		}
	}
	return false
}

// windowChart plots the featured percentile across windows, with the same SLO
// line as the rate/latency chart so a breach is visible rather than inferred.
type windowChart struct {
	Windows []metrics.Window
	P       float64
	Size    time.Duration
	LimitMs float64
}

// renderWindowChart draws latency over time as inline SVG, matching the styling
// of the rate/latency chart.
func renderWindowChart(b *strings.Builder, c windowChart) {
	const (
		width, height = 760, 220
		padL, padR    = 68, 28
		padT, padB    = 20, 44
	)
	plotW := float64(width - padL - padR)
	plotH := float64(height - padT - padB)

	var points []chartPoint
	maxMs := 0.0
	for _, w := range c.Windows {
		v, ok := windowPercentile(w, c.P)
		if !ok {
			continue
		}
		points = append(points, chartPoint{Rate: w.Index, Ms: v})
		maxMs = math.Max(maxMs, v)
	}
	if len(points) == 0 {
		return
	}
	yMax := niceCeil(math.Max(maxMs, c.LimitMs))
	xMax := float64(points[len(points)-1].Rate)
	if xMax <= 0 {
		xMax = 1
	}

	x := func(idx float64) float64 { return padL + idx/xMax*plotW }
	y := func(v float64) float64 { return padT + plotH - v/yMax*plotH }

	fmt.Fprintf(b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="latency over time">`,
		width, height)
	fmt.Fprintf(b, `<rect x="%d" y="%d" width="%.0f" height="%.0f" class="plot"/>`,
		padL, padT, plotW, plotH)

	const yTicks = 4
	for i := 0; i <= yTicks; i++ {
		v := yMax * float64(i) / yTicks
		py := y(v)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`,
			padL, py, padL+plotW, py)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" class="axis y">%s</text>`,
			padL-10, py+4, html.EscapeString(axisLabel(v)))
	}

	if c.LimitMs > 0 && c.LimitMs <= yMax {
		ly := y(c.LimitMs)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%.1f" y2="%.1f" class="limit"/>`,
			padL, ly, padL+plotW, ly)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" class="axis limit">%s</text>`,
			padL+6, ly-6, html.EscapeString("p"+formatP(c.P)+" limit "+duration(c.LimitMs)))
	}

	// Label every other window when there are many, so the axis stays legible.
	step := 1
	if n := len(points); n > 12 {
		step = n / 12
	}
	var line strings.Builder
	for i, pt := range points {
		if i > 0 {
			line.WriteByte(' ')
		}
		fmt.Fprintf(&line, "%.1f,%.1f", x(float64(pt.Rate)), y(pt.Ms))
		if i%step == 0 {
			fmt.Fprintf(b, `<text x="%.1f" y="%d" class="axis x">%s</text>`,
				x(float64(pt.Rate)), padT+int(plotH)+18,
				html.EscapeString((time.Duration(pt.Rate) * c.Size).String()))
		}
	}
	if len(points) > 1 {
		fmt.Fprintf(b, `<polyline class="series" points="%s"/>`, line.String())
	}
	for _, pt := range points {
		fmt.Fprintf(b, `<circle class="pass" cx="%.1f" cy="%.1f" r="3"><title>%s</title></circle>`,
			x(float64(pt.Rate)), y(pt.Ms),
			html.EscapeString(fmt.Sprintf("%s-%s: %s",
				time.Duration(pt.Rate)*c.Size,
				time.Duration(pt.Rate+1)*c.Size, ms(pt.Ms))))
	}
	b.WriteString(`</svg>`)
}
