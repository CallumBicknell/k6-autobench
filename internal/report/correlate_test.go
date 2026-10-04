package report

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/collect"
	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
)

// windowSize is the width of every window in these fixtures.
const windowSize = 10 * time.Second

// windowsWithLatency builds one window per latency reading, so a series can be
// paired against it one reading at a time.
func windowsWithLatency(latencies []float64) []metrics.Window {
	ws := make([]metrics.Window, 0, len(latencies))
	for i, lat := range latencies {
		ws = append(ws, metrics.Window{
			Index:      i,
			StartMs:    float64(i) * float64(windowSize) / float64(time.Millisecond),
			EndMs:      float64(i+1) * float64(windowSize) / float64(time.Millisecond),
			Requests:   100,
			HasLatency: true,
			AvgMs:      lat,
			MaxMs:      lat * 2,
			Percentiles: []metrics.Percentile{
				{P: 50, Val: lat / 2},
				{P: 95, Val: lat},
			},
		})
	}
	return ws
}

// readings places one sample per window, at its midpoint, so the pairing is
// unambiguous.
func readings(values ...float64) []collect.Sample {
	out := make([]collect.Sample, 0, len(values))
	for i, v := range values {
		out = append(out, collect.Sample{
			OffsetMs: (float64(i) + 0.5) * float64(windowSize) / float64(time.Millisecond),
			Value:    v,
		})
	}
	return out
}

// risingLatency is the shape of a system sliding into trouble.
var risingLatency = []float64{1, 2, 3, 5, 8, 13}

func correlatedConfig() *config.Config {
	cfg := collectedConfig()
	cfg.Analysis.Window = config.Duration{Duration: windowSize}
	cfg.ApplyDefaults()
	return cfg
}

func TestRanks(t *testing.T) {
	got := ranks([]float64{30, 10, 20})
	want := []float64{3, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranks = %v, want %v", got, want)
		}
	}

	// Ties share the mean of the ranks they span, so a pool pinned at its
	// ceiling does not get an arbitrary ordering that manufactures a trend.
	got = ranks([]float64{10, 20, 20, 20, 30})
	want = []float64{1, 3, 3, 3, 5}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ranks with ties = %v, want %v", got, want)
			break
		}
	}
}

func TestPearson(t *testing.T) {
	tests := []struct {
		name    string
		xs, ys  []float64
		want    float64
		wantOK  bool
		epsilon float64
	}{
		{"perfect positive", []float64{1, 2, 3}, []float64{4, 5, 6}, 1, true, 1e-9},
		{"perfect negative", []float64{1, 2, 3}, []float64{6, 5, 4}, -1, true, 1e-9},
		{"a constant has no correlation", []float64{1, 1, 1}, []float64{1, 2, 3}, 0, false, 0},
		{"a constant target has none either", []float64{1, 2, 3}, []float64{5, 5, 5}, 0, false, 0},
		{"one point says nothing", []float64{1}, []float64{2}, 0, false, 0},
		{"lengths must agree", []float64{1, 2}, []float64{1, 2, 3}, 0, false, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pearson(tc.xs, tc.ys)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && math.Abs(got-tc.want) > tc.epsilon {
				t.Errorf("r = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpearmanSeesMonotonicNotLinear is the reason for ranking rather than
// correlating the raw numbers: a relationship can be perfect in order while the
// magnitudes are nothing like each other.
func TestSpearmanSeesMonotonicNotLinear(t *testing.T) {
	tests := []struct {
		name        string
		resource    []float64
		latency     []float64
		maxRawPearr float64
	}{
		{
			// Resource climbs exponentially while latency climbs linearly: the
			// same ordering, wildly different magnitudes.
			name:        "exponential against linear",
			resource:    []float64{10, 100, 1000, 10_000, 100_000, 1_000_000},
			latency:     []float64{1, 2, 3, 4, 5, 6},
			maxRawPearr: 0.95,
		},
		{
			// One bad window drags the raw correlation down; the ordering is
			// untouched, so the rank correlation still sees the relationship.
			name:        "one outlier window",
			resource:    []float64{1, 2, 3, 4, 5, 6},
			latency:     []float64{1, 2, 3, 4, 5, 100},
			maxRawPearr: 0.75,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := spearman(tc.resource, tc.latency)
			if !ok {
				t.Fatal("spearman reported no relationship")
			}
			if math.Abs(r-1) > 1e-9 {
				t.Errorf("rank correlation = %v, want 1 for a monotonic relationship", r)
			}

			raw, ok := pearson(tc.resource, tc.latency)
			if !ok {
				t.Fatal("pearson reported no relationship")
			}
			if raw >= tc.maxRawPearr {
				t.Errorf("Pearson on the raw values was %v, want under %v: "+
					"the fixture is not exercising the difference", raw, tc.maxRawPearr)
			}
		})
	}
}

func TestSpearmanRejectsAConstant(t *testing.T) {
	if _, ok := spearman([]float64{5, 5, 5, 5, 5, 5}, risingLatency); ok {
		t.Error("a series that never moved cannot correlate with anything")
	}
}

// TestPermutationPIsDeterministic keeps a report reproducible: the same
// measurement must not change its verdict between runs.
func TestPermutationPIsDeterministic(t *testing.T) {
	resources := []float64{10, 20, 30, 50, 80, 130}
	first := permutationP(resources, risingLatency, 1)
	for i := 0; i < 5; i++ {
		if got := permutationP(resources, risingLatency, 1); got != first {
			t.Fatalf("p changed between calls: %v then %v", first, got)
		}
	}
	if first <= 0 {
		t.Errorf("p = %v; the observed pairing counts itself, so it can never be exactly zero", first)
	}
}

// TestPermutationPSeparatesSignalFromNoise is the property the whole section
// rests on: a real relationship must score far below a coincidental one.
func TestPermutationPSeparatesSignalFromNoise(t *testing.T) {
	strong := permutationP([]float64{10, 20, 30, 50, 80, 130}, risingLatency, 1)
	// A shuffle of the latency readings against themselves: no relationship by
	// construction.
	noise := permutationP([]float64{50, 10, 90, 20, 70, 30}, risingLatency, 0.086)

	if strong >= 0.01 {
		t.Errorf("a perfect relationship scored p = %v, want well under 0.01", strong)
	}
	if noise <= strong*10 {
		t.Errorf("noise scored p = %v against signal %v; the test is not separating them", noise, strong)
	}
}

// TestPermutationPHandlesTies covers a pool pinned at its ceiling: half the
// readings are identical, which the textbook tables for rank correlation do not
// handle.
func TestPermutationPHandlesTies(t *testing.T) {
	resources := []float64{100, 200, 499, 499, 499, 499}
	got := permutationP(resources, risingLatency, 0.83)
	if got < 0 || got > 1 {
		t.Errorf("p = %v, want a probability", got)
	}
}

// TestSamplesInBucketsByWindow checks the join between the two timelines, which
// only works because collector samples carry an offset rather than a timestamp.
func TestSamplesInBucketsByWindow(t *testing.T) {
	ws := windowsWithLatency(risingLatency)
	s := collect.Series{Source: "api", Name: "cpu", Samples: []collect.Sample{
		{OffsetMs: 0, Value: 1},              // window 0
		{OffsetMs: 5000, Value: 2},           // window 0
		{OffsetMs: 10000, Value: 3},          // window 1 (start is inclusive)
		{OffsetMs: 19999, Value: 4},          // window 1
		{OffsetMs: 20000, Value: 5},          // window 2 (end is exclusive)
		{OffsetMs: 65000, Value: 99},         // past the last window
		{OffsetMs: 15000, Value: math.NaN()}, // skipped
		{OffsetMs: 16000, Value: math.Inf(1)},
	}}

	tests := []struct {
		window int
		want   []float64
	}{
		{0, []float64{1, 2}},
		{1, []float64{3, 4}},
		{2, []float64{5}},
	}
	for _, tc := range tests {
		got := samplesIn(ws[tc.window], s)
		if len(got) != len(tc.want) {
			t.Fatalf("window %d: samples = %v, want %v", tc.window, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("window %d: samples = %v, want %v", tc.window, got, tc.want)
				break
			}
		}
	}
}

// TestPairWithLatencyDropsGaps: a window with no traffic, no latency reading or
// no resource reading is not a data point, and padding it would invent one.
func TestPairWithLatencyDropsGaps(t *testing.T) {
	ws := windowsWithLatency(risingLatency)
	ws[1].Requests = 0       // no traffic
	ws[2].Percentiles = nil  // no latency reading
	ws[3].HasLatency = false // ditto

	// Readings in windows 0, 1, 4 and 5, but window 1 has no traffic, so only
	// windows 0, 4 and 5 hold both a latency and a resource reading.
	s := collect.Series{Source: "api", Name: "cpu", Samples: []collect.Sample{
		{OffsetMs: 5000, Value: 10},
		{OffsetMs: 15000, Value: 20},
		{OffsetMs: 45000, Value: 40},
		{OffsetMs: 55000, Value: 50},
	}}

	resources, latencies := pairWithLatency(ws, s, 95)
	if len(resources) != 3 || len(latencies) != 3 {
		t.Fatalf("pairs = %d/%d, want 3/3: windows 0, 4 and 5", len(resources), len(latencies))
	}
	if resources[0] != 10 || resources[1] != 40 || resources[2] != 50 {
		t.Errorf("resources = %v, want [10 40 50]", resources)
	}
	wantLatency := []float64{risingLatency[0], risingLatency[4], risingLatency[5]}
	for i := range wantLatency {
		if latencies[i] != wantLatency[i] {
			t.Errorf("latencies = %v, want %v", latencies, wantLatency)
			break
		}
	}
}

// TestPairWithLatencyAveragesWithinAWindow: several readings in one window
// become one number, so a series sampled faster than the window does not get
// extra weight.
func TestPairWithLatencyAveragesWithinAWindow(t *testing.T) {
	ws := windowsWithLatency(risingLatency[:1])
	s := collect.Series{Samples: []collect.Sample{
		{OffsetMs: 1000, Value: 10},
		{OffsetMs: 2000, Value: 20},
		{OffsetMs: 3000, Value: 60},
	}}

	resources, _ := pairWithLatency(ws, s, 95)
	if len(resources) != 1 || resources[0] != 30 {
		t.Errorf("resources = %v, want [30]", resources)
	}
}

// correlateSeries is one named series with one reading per window.
func correlateSeries(source, name string, values ...float64) collect.Series {
	return collect.Series{Source: source, Name: name, Samples: readings(values...)}
}

// TestCorrelateRowsFindsTheSeriesThatMoved is the §33 payoff: the resource that
// tracked latency is named, and the ones that did not are left out.
func TestCorrelateRowsFindsTheSeriesThatMoved(t *testing.T) {
	ws := windowsWithLatency(risingLatency)
	sys := collect.Report{Series: []collect.Series{
		// Tracks latency step for step.
		correlateSeries("postgres", "cpu", 10, 20, 30, 50, 80, 130),
		// Nothing to do with it.
		correlateSeries("api", "goroutines", 50, 10, 90, 20, 70, 30),
		// Pinned at its ceiling throughout.
		correlateSeries("postgres", "connections", 499, 499, 499, 499, 499, 499),
	}}

	rows := correlateRows(ws, sys, correlatedConfig())
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want exactly the series that moved", rows)
	}
	if rows[0].Series != "postgres cpu" {
		t.Errorf("series = %q, want %q", rows[0].Series, "postgres cpu")
	}
	if rows[0].R < 0.99 {
		t.Errorf("r = %v, want a near-perfect positive correlation", rows[0].R)
	}
	if rows[0].Windows != 6 {
		t.Errorf("windows = %d, want 6", rows[0].Windows)
	}
}

// TestCorrelateRowsReportsAFallingSeries: something that drops as latency climbs
// -- a cache hit rate, say -- is as worth seeing as one that rises.
func TestCorrelateRowsReportsAFallingSeries(t *testing.T) {
	ws := windowsWithLatency(risingLatency)
	sys := collect.Report{Series: []collect.Series{
		correlateSeries("postgres", "cache_hit", 99, 96, 90, 70, 40, 12),
	}}

	rows := correlateRows(ws, sys, correlatedConfig())
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want the falling series", rows)
	}
	if rows[0].R > -0.99 {
		t.Errorf("r = %v, want a near-perfect negative correlation", rows[0].R)
	}
}

// TestCorrelateRowsRanksByStrength keeps the strongest co-movement first, in
// either direction.
func TestCorrelateRowsRanksByStrength(t *testing.T) {
	ws := windowsWithLatency(risingLatency)
	sys := collect.Report{Series: []collect.Series{
		// Monotonic, but with one step out of order.
		correlateSeries("api", "weaker", 10, 20, 30, 50, 130, 80),
		// Perfectly monotonic.
		correlateSeries("postgres", "stronger", 10, 20, 30, 50, 80, 130),
	}}

	rows := correlateRows(ws, sys, correlatedConfig())
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want both", rows)
	}
	if rows[0].Series != "postgres stronger" {
		t.Errorf("first row = %q, want the stronger correlation", rows[0].Series)
	}
}

// TestCorrelateRowsRefusesTooFewWindows is the property that matters most: a
// handful of windows cannot support the claim, so the section stays empty rather
// than naming a resource on the strength of four points.
func TestCorrelateRowsRefusesTooFewWindows(t *testing.T) {
	ws := windowsWithLatency(risingLatency[:minCorrelationWindows-1])
	sys := collect.Report{Series: []collect.Series{
		correlateSeries("postgres", "cpu", 10, 20, 30, 50),
	}}

	if rows := correlateRows(ws, sys, correlatedConfig()); len(rows) != 0 {
		t.Errorf("rows = %v, want none: four windows cannot support the claim", rows)
	}
}

// TestCorrelateRowsRefusesASeriesWithGaps: a series whose readings do not line
// up with the windows has nothing to compare, however strongly it moves where it
// is present.
func TestCorrelateRowsRefusesASeriesWithGaps(t *testing.T) {
	ws := windowsWithLatency(risingLatency)
	// Only two windows hold a reading.
	sys := collect.Report{Series: []collect.Series{{
		Source: "api", Name: "cpu",
		Samples: []collect.Sample{{OffsetMs: 5000, Value: 1}, {OffsetMs: 55000, Value: 99}},
	}}}

	if rows := correlateRows(ws, sys, correlatedConfig()); len(rows) != 0 {
		t.Errorf("rows = %v, want none: two pairings is not a comparison", rows)
	}
}

// TestCorrelateRowsRefusesNoise is the guard against confidently naming an
// innocent resource. Several unrelated series are compared against the same
// windows; with the bar raised for how many were compared, none should survive.
func TestCorrelateRowsRefusesNoise(t *testing.T) {
	const (
		seriesCount = 40
		windowCount = 12
	)

	// A fixed generator, so a failure here is reproducible rather than a flake.
	rng := rand.New(rand.NewSource(20260804))

	latencies := make([]float64, windowCount)
	for i := range latencies {
		latencies[i] = 1 + rng.Float64()*50
	}
	ws := windowsWithLatency(latencies)

	var series []collect.Series
	for s := 0; s < seriesCount; s++ {
		values := make([]float64, windowCount)
		for i := range values {
			values[i] = rng.Float64() * 100
		}
		series = append(series, correlateSeries("noise", fmt.Sprintf("metric_%02d", s), values...))
	}

	rows := correlateRows(ws, collect.Report{Series: series}, correlatedConfig())
	if len(rows) != 0 {
		t.Errorf("%d of %d unrelated series were reported as correlated, want none: %v",
			len(rows), seriesCount, rows)
	}
}

// TestCorrelateRowsRaisesTheBarForMoreSeries documents the correction: the same
// perfect relationship passes when few series are compared and fails when many
// are, because comparing many is many chances of a coincidence.
func TestCorrelateRowsRaisesTheBarForMoreSeries(t *testing.T) {
	ws := windowsWithLatency(risingLatency)
	tracking := correlateSeries("postgres", "cpu", 10, 20, 30, 50, 80, 130)

	if rows := correlateRows(ws, collect.Report{Series: []collect.Series{tracking}}, correlatedConfig()); len(rows) != 1 {
		t.Fatalf("rows = %v, want the relationship found when it is one of few", rows)
	}

	// The same relationship, now buried among enough others that six windows no
	// longer carry the evidence for it.
	many := []collect.Series{tracking}
	for i := 0; i < 40; i++ {
		many = append(many, correlateSeries("api", fmt.Sprintf("metric_%02d", i), 1, 2, 3, 4, 5, 6))
	}
	for _, row := range correlateRows(ws, collect.Report{Series: many}, correlatedConfig()) {
		if row.Series == "postgres cpu" {
			t.Errorf("with 41 series and six windows the bar should not be cleared by r = %v", row.R)
		}
	}
}

// TestCorrelateRowsNeedsAWindowedView keeps the section out of a run that has
// nothing to compare against.
func TestCorrelateRowsNeedsAWindowedView(t *testing.T) {
	sys := collect.Report{Series: []collect.Series{
		correlateSeries("postgres", "cpu", 10, 20, 30, 50, 80, 130),
	}}
	if rows := correlateRows(nil, sys, correlatedConfig()); len(rows) != 0 {
		t.Errorf("rows = %v, want none without windows", rows)
	}
	if rows := correlateRows(windowsWithLatency(risingLatency), collect.Report{}, correlatedConfig()); len(rows) != 0 {
		t.Errorf("rows = %v, want none without series", rows)
	}
}

// correlatedResult is a run in which one resource tracked latency and the other
// did not, which is the shape the section exists for.
func correlatedResult() *results.Result {
	r := testResult()
	r.Windows = windowsWithLatency(risingLatency)
	r.System = collect.Report{Series: []collect.Series{
		correlateSeries("postgres", "cpu", 10, 20, 30, 50, 80, 130),
		correlateSeries("api", "goroutines", 50, 10, 90, 20, 70, 30),
	}}
	return r
}

func renderCorrelatedTerminal(t *testing.T, r *results.Result) string {
	t.Helper()
	var buf bytes.Buffer
	Render(&buf, correlatedConfig(), r, "0.8.0", false)
	return buf.String()
}

func renderCorrelatedHTML(t *testing.T, r *results.Result) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderHTML(&buf, correlatedConfig(), r, "0.8.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	return buf.String()
}

// correlatedSection returns just the LIKELY CORRELATED block of a terminal
// report. The SYSTEM table above it lists every series, so an assertion about
// what the correlation section says has to be scoped to it or it will pass on
// the wrong table.
func correlatedSection(out string) string {
	start := strings.Index(out, "LIKELY CORRELATED")
	if start < 0 {
		return ""
	}
	rest := out[start:]
	if end := strings.Index(rest, "───"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// correlatedHTMLSection is correlatedSection for the HTML report.
func correlatedHTMLSection(out string) string {
	const marker = "<h2>Likely correlated</h2>"
	start := strings.Index(out, marker)
	if start < 0 {
		return ""
	}
	// Search past the marker itself, or the closing search finds it at once.
	rest := out[start+len(marker):]
	if end := strings.Index(rest, "<h2>"); end >= 0 {
		rest = rest[:end]
	}
	return marker + rest
}

// TestRenderCorrelatedSection is the §33 payoff in the terminal: the resource
// that moved with latency is named, and the caveat is printed with it.
func TestRenderCorrelatedSection(t *testing.T) {
	section := correlatedSection(renderCorrelatedTerminal(t, correlatedResult()))

	for _, want := range []string{
		"LIKELY CORRELATED",
		"SERIES", "r", "WINDOWS",
		"postgres cpu",
		"+1.00",
		"Correlation is not cause",
		"permutation test",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("correlation section does not contain %q\n%s", want, section)
		}
	}
	// The series that did not move must not be named.
	if strings.Contains(section, "api goroutines") {
		t.Errorf("an uncorrelated series was reported:\n%s", section)
	}
}

// TestRenderCorrelatedAbsentWhenNothingQualifies keeps the section out rather
// than printing a table of coincidences.
func TestRenderCorrelatedAbsentWhenNothingQualifies(t *testing.T) {
	r := testResult()
	r.Windows = windowsWithLatency(risingLatency)
	r.System = collect.Report{Series: []collect.Series{
		correlateSeries("api", "goroutines", 50, 10, 90, 20, 70, 30),
	}}

	if got := renderCorrelatedTerminal(t, r); strings.Contains(got, "LIKELY CORRELATED") {
		t.Errorf("report claims a correlation that is not there:\n%s", got)
	}
}

// TestRenderCorrelatedAbsentWithoutWindows: the comparison needs a windowed
// view, so a run without one must not grow the section.
func TestRenderCorrelatedAbsentWithoutWindows(t *testing.T) {
	r := correlatedResult()
	r.Windows = nil

	if got := renderCorrelatedTerminal(t, r); strings.Contains(got, "LIKELY CORRELATED") {
		t.Errorf("report has a correlation section with no windows:\n%s", got)
	}
}

// TestRenderCorrelatedHTML covers the same section in the self-contained report.
func TestRenderCorrelatedHTML(t *testing.T) {
	section := correlatedHTMLSection(renderCorrelatedHTML(t, correlatedResult()))

	for _, want := range []string{
		"<h2>Likely correlated</h2>",
		"<th class=\"num\">r</th>",
		"postgres cpu",
		"1.00",
		"Correlation is not cause",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("correlation section does not contain %q\n%s", want, section)
		}
	}
	// The direction matters as much as the strength, so a sign has to survive
	// the template. html/template escapes a leading "+" as &#43;, which is what
	// a browser shows as "+"; either form is correct.
	if !strings.Contains(section, "+1.00") && !strings.Contains(section, "&#43;1.00") {
		t.Errorf("the correlation should show its direction:\n%s", section)
	}
	if strings.Contains(section, "api goroutines") {
		t.Errorf("html report named an uncorrelated series:\n%s", section)
	}
}

// TestRenderCorrelatedHTMLAbsentWhenNothingQualifies keeps the section out of a
// report that has nothing to say.
func TestRenderCorrelatedHTMLAbsentWhenNothingQualifies(t *testing.T) {
	r := testResult()
	r.Windows = windowsWithLatency(risingLatency)
	r.System = collect.Report{Series: []collect.Series{
		correlateSeries("api", "goroutines", 50, 10, 90, 20, 70, 30),
	}}

	if got := renderCorrelatedHTML(t, r); strings.Contains(got, "<h2>Likely correlated</h2>") {
		t.Error("html report claims a correlation that is not there")
	}
}

// TestCapacityReportsCarryTheCorrelation covers both capacity renderers: they
// must agree, or a reader of one has to guess what the other would have said.
func TestCapacityReportsCarryTheCorrelation(t *testing.T) {
	out := searchOutcome()
	out.Validation.Result.Windows = windowsWithLatency(risingLatency)
	out.Validation.Result.System = correlatedResult().System

	t.Run("terminal", func(t *testing.T) {
		var buf bytes.Buffer
		RenderCapacity(&buf, correlatedConfig(), out, "0.8.0", time.Second, false)
		if got := buf.String(); !strings.Contains(got, "LIKELY CORRELATED") ||
			!strings.Contains(got, "postgres cpu") {
			t.Errorf("capacity report is missing the correlation:\n%s", got)
		}
	})

	t.Run("html", func(t *testing.T) {
		var buf bytes.Buffer
		if err := RenderCapacityHTML(&buf, correlatedConfig(), out, "0.8.0",
			time.Second, time.Unix(0, 0)); err != nil {
			t.Fatalf("RenderCapacityHTML: %v", err)
		}
		if got := buf.String(); !strings.Contains(got, "<h2>Likely correlated</h2>") {
			t.Errorf("capacity html report is missing the correlation:\n%s", got)
		}
	})
}
