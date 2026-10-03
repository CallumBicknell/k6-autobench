package report

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// Chart geometry, in SVG user units. Fixed rather than responsive: the SVG
// scales to its container via the viewBox, so the numbers stay predictable and
// the output stays deterministic.
const (
	chartWidth  = 760
	chartHeight = 300
	chartPadL   = 68
	chartPadR   = 28
	chartPadT   = 24
	chartPadB   = 52
)

// chartPoint is one probe plotted on the rate/latency chart.
type chartPoint struct {
	Rate int
	Ms   float64
	Pass bool
}

// latencyChart plots achieved rate against observed latency. It is the shape
// that makes a capacity boundary legible: latency flat, then a knee.
type latencyChart struct {
	Points []chartPoint
	// LimitMs draws a horizontal dashed SLO line when > 0.
	LimitMs   float64
	LimitText string
	// MarkerAt draws a vertical dashed line at the discovered capacity.
	MarkerAt   int
	MarkerText string
}

// renderLatencyChart writes the chart as inline SVG.
//
// No JavaScript and no external assets: the report stays a single file that
// works offline and can be archived as a CI artefact.
func renderLatencyChart(b *strings.Builder, c latencyChart) {
	plotW := float64(chartWidth - chartPadL - chartPadR)
	plotH := float64(chartHeight - chartPadT - chartPadB)

	maxRate := 0.0
	maxMs := 0.0
	for _, p := range c.Points {
		maxRate = math.Max(maxRate, float64(p.Rate))
		maxMs = math.Max(maxMs, p.Ms)
	}
	xMax := niceCeil(maxRate)
	yMax := niceCeil(math.Max(maxMs, c.LimitMs))

	x := func(rate float64) float64 {
		if xMax <= 0 {
			return chartPadL
		}
		return chartPadL + rate/xMax*plotW
	}
	y := func(ms float64) float64 {
		if yMax <= 0 {
			return chartPadT + plotH
		}
		return chartPadT + plotH - ms/yMax*plotH
	}

	fmt.Fprintf(b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="rate versus latency">`,
		chartWidth, chartHeight)

	// Plot area.
	fmt.Fprintf(b, `<rect x="%d" y="%d" width="%.0f" height="%.0f" class="plot"/>`,
		chartPadL, chartPadT, plotW, plotH)

	// Horizontal grid and y labels.
	const yTicks = 4
	for i := 0; i <= yTicks; i++ {
		v := yMax * float64(i) / yTicks
		py := y(v)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`,
			chartPadL, py, chartPadL+plotW, py)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" class="axis y">%s</text>`,
			chartPadL-10, py+4, html.EscapeString(axisLabel(v)))
	}

	// Vertical grid and x labels.
	const xTicks = 4
	for i := 0; i <= xTicks; i++ {
		v := xMax * float64(i) / xTicks
		px := x(v)
		fmt.Fprintf(b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%.0f" class="grid"/>`,
			px, chartPadT, px, chartPadT+plotH)
		fmt.Fprintf(b, `<text x="%.1f" y="%d" class="axis x">%s</text>`,
			px, chartPadT+int(plotH)+20, html.EscapeString(commafy(int64(math.Round(v)))))
	}
	fmt.Fprintf(b, `<text x="%.1f" y="%d" class="axis title">iterations/s</text>`,
		chartPadL+plotW/2-30, chartHeight-10)

	// The SLO line, so the knee can be read against the budget it breaches.
	if c.LimitMs > 0 && c.LimitMs <= yMax {
		ly := y(c.LimitMs)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%.1f" y2="%.1f" class="limit"/>`,
			chartPadL, ly, chartPadL+plotW, ly)
		if c.LimitText != "" {
			fmt.Fprintf(b, `<text x="%d" y="%.1f" class="axis limit">%s</text>`,
				chartPadL+6, ly-6, html.EscapeString(c.LimitText))
		}
	}

	// The discovered capacity.
	if c.MarkerAt > 0 && float64(c.MarkerAt) <= xMax {
		mx := x(float64(c.MarkerAt))
		fmt.Fprintf(b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%.0f" class="marker"/>`,
			mx, chartPadT, mx, chartPadT+plotH)
		if c.MarkerText != "" {
			fmt.Fprintf(b, `<text x="%.1f" y="%d" class="axis marker">%s</text>`,
				mx+6, chartPadT+14, html.EscapeString(c.MarkerText))
		}
	}

	// The line, then the points on top of it.
	var line strings.Builder
	for i, p := range c.Points {
		if i > 0 {
			line.WriteByte(' ')
		}
		fmt.Fprintf(&line, "%.1f,%.1f", x(float64(p.Rate)), y(p.Ms))
	}
	if len(c.Points) > 1 {
		fmt.Fprintf(b, `<polyline class="series" points="%s"/>`, line.String())
	}
	for _, p := range c.Points {
		class := "pass"
		if !p.Pass {
			class = "fail"
		}
		fmt.Fprintf(b, `<circle class="%s" cx="%.1f" cy="%.1f" r="4"><title>%s</title></circle>`,
			class, x(float64(p.Rate)), y(p.Ms),
			html.EscapeString(fmt.Sprintf("%s/s → %s", commafy(int64(p.Rate)), ms(p.Ms))))
	}

	b.WriteString(`</svg>`)
}

// niceCeil rounds v up to a 1/2/2.5/5/10 x 10^n step so axis labels are readable
// numbers rather than the exact data maximum.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	base := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*base {
			return m * base
		}
	}
	return 10 * base
}

// axisLabel renders a latency tick, spelling the origin as a plain "0" rather
// than "0.0us".
func axisLabel(v float64) string {
	if v == 0 {
		return "0"
	}
	return duration(v)
}
