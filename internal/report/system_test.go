package report

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/collect"
	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/search"
)

// collectedConfig is a config with one Prometheus source configured, matching
// what a user would write to see the server side of their system.
func collectedConfig() *config.Config {
	cfg := testConfig()
	cfg.Collectors = config.Collectors{
		Prometheus: []config.PrometheusSource{{
			Name:    "postgres",
			URL:     "http://localhost:9090",
			Queries: []config.PrometheusQuery{{Name: "cpu", Query: "up"}},
		}},
	}
	cfg.ApplyDefaults()
	return cfg
}

// series builds a collector series from offset/value pairs.
func series(source, name, unit string, samples ...collect.Sample) collect.Series {
	return collect.Series{Source: source, Name: name, Unit: unit, Samples: samples}
}

func at(offsetMs, value float64) collect.Sample {
	return collect.Sample{OffsetMs: offsetMs, Value: value}
}

// collectedSystem is the §33 shape: the API is fine while the database, which is
// what actually ran out, is not.
func collectedSystem() collect.Report {
	return collect.Report{Series: []collect.Series{
		series("api", "cpu", "cores", at(0, 0.31), at(10000, 0.42), at(20000, 0.38)),
		series("postgres", "cpu", "percent", at(0, 41), at(10000, 97), at(20000, 88)),
		series("postgres", "memory", "bytes", at(0, 214e6), at(10000, 480e6), at(20000, 350e6)),
		series("postgres", "connections", "", at(0, 120), at(10000, 498), at(20000, 410)),
	}}
}

func renderSystemTerminal(t *testing.T, sys collect.Report) string {
	t.Helper()
	r := testResult()
	r.System = sys
	var buf bytes.Buffer
	Render(&buf, collectedConfig(), r, "0.8.0", false)
	return buf.String()
}

func renderSystemHTML(t *testing.T, sys collect.Report) string {
	t.Helper()
	r := testResult()
	r.System = sys
	var buf bytes.Buffer
	if err := RenderHTML(&buf, collectedConfig(), r, "0.8.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	return buf.String()
}

// TestRenderSystemSection is the §33 payoff: the report names the resource that
// ran out, and when, next to the latency it caused.
func TestRenderSystemSection(t *testing.T) {
	got := renderSystemTerminal(t, collectedSystem())

	for _, want := range []string{
		"SYSTEM",
		"sampled every 5s",
		"SERIES", "MIN", "MAX", "AVG", "PEAK AT",
		// The unit rides on the name, so the value columns stay numbers.
		"api cpu (cores)",
		"postgres cpu",
		"postgres memory",
		"postgres connections",
		// The configured unit decides the rendering: percent, bytes, plain.
		"97.0%",
		"457.8MiB",
		"498",
		// The peak is on the measurement's timeline, matching the window labels.
		"10s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
}

// TestRenderSystemAbsentWhenNoCollector: a run without collectors must not grow
// an empty section.
func TestRenderSystemAbsentWhenNoCollector(t *testing.T) {
	r := testResult()
	var buf bytes.Buffer
	Render(&buf, testConfig(), r, "0.8.0", false)
	if strings.Contains(buf.String(), "SYSTEM") {
		t.Errorf("report has a system section with no collectors:\n%s", buf.String())
	}
}

// TestRenderSystemReportsCollectorProblems: a collector that could not read
// anything is said plainly, and the run is not blamed for it.
func TestRenderSystemReportsCollectorProblems(t *testing.T) {
	sys := collect.Report{Failures: []collect.Failure{
		{Source: "postgres", Error: `cpu: Get "http://localhost:9090/api/v1/query": connection refused`},
	}}
	got := renderSystemTerminal(t, sys)

	for _, want := range []string{
		"COLLECTOR PROBLEMS",
		"the run is unaffected",
		"postgres",
		"connection refused",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
	// The verdict is untouched: the monitoring being down says nothing about
	// the target's capacity.
	if !strings.Contains(got, "RESULT: PASS") {
		t.Errorf("a collector failure must not change the verdict:\n%s", got)
	}
}

// TestRenderSystemHTML covers the same section in the self-contained report.
func TestRenderSystemHTML(t *testing.T) {
	got := renderSystemHTML(t, collectedSystem())

	for _, want := range []string{
		"<h2>System",
		"sampled every 5s",
		"<th>Series</th>",
		"<th class=\"num\">Peak at</th>",
		"api cpu (cores)",
		"postgres cpu",
		"97.0%",
		"457.8MiB",
		"498",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("html report does not contain %q", want)
		}
	}
}

func TestRenderSystemHTMLReportsCollectorProblems(t *testing.T) {
	got := renderSystemHTML(t, collect.Report{Failures: []collect.Failure{
		{Source: "api", Error: "cpu: connection refused"},
	}})
	if !strings.Contains(got, "Collector problems") {
		t.Errorf("html report should warn about the collector:\n%s", got)
	}
	if !strings.Contains(got, "cpu: connection refused") {
		t.Error("html report should name the failing query")
	}
}

// TestRenderSystemHTMLAbsentWhenNoCollector keeps the section out of a report
// that has nothing to put in it.
func TestRenderSystemHTMLAbsentWhenNoCollector(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), testResult(), "0.8.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if strings.Contains(buf.String(), "<h2>System") {
		t.Error("html report has a system section with no collectors")
	}
}

// TestRenderSystemSeriesWithNoFiniteReadings: a series that only ever read NaN
// says so rather than reporting zeros as though it had measured something.
func TestRenderSystemSeriesWithNoFiniteReadings(t *testing.T) {
	sys := collect.Report{Series: []collect.Series{
		series("api", "cpu", "cores", at(0, math.NaN()), at(10000, math.Inf(1))),
	}}
	got := renderSystemTerminal(t, sys)

	if !strings.Contains(got, "no finite readings") {
		t.Errorf("report should say the series had no usable readings:\n%s", got)
	}
	if strings.Contains(got, "NaN") {
		t.Errorf("report should not print NaN:\n%s", got)
	}
}

// TestRenderSystemKeepsQueryOrder checks the rows follow the configured query
// order rather than an arbitrary one.
func TestRenderSystemKeepsQueryOrder(t *testing.T) {
	sys := collect.Report{Series: []collect.Series{
		series("api", "cpu", "", at(0, 1)),
		series("api", "memory", "", at(0, 2)),
		series("api", "connections", "", at(0, 3)),
	}}
	got := renderSystemTerminal(t, sys)

	cpu := strings.Index(got, "api cpu")
	memory := strings.Index(got, "api memory")
	connections := strings.Index(got, "api connections")
	if cpu < 0 || memory < 0 || connections < 0 {
		t.Fatalf("report is missing a series row:\n%s", got)
	}
	if !(cpu < memory && memory < connections) {
		t.Errorf("rows are out of configured order (cpu %d, memory %d, connections %d):\n%s",
			cpu, memory, connections, got)
	}
}

// TestSeriesValue covers the unit conventions. Prometheus values are unitless,
// so only the two that would otherwise be unreadable get special treatment.
func TestSeriesValue(t *testing.T) {
	tests := []struct {
		value float64
		unit  string
		want  string
	}{
		{0.42, "cores", "0.42"},
		{97, "percent", "97.0%"},
		{0.97, "%", "1.0%"},
		{498, "", "498"},
		{0.0035, "", "0.0035"},
		{9123456, "", "9,123,456.0"},
		{0, "", "0"},
		{-0.5, "", "-0.5"},
		{512, "bytes", "512B"},
		{214000000, "bytes", "204.1MiB"},
		{1.5e9, "bytes", "1.4GiB"},
		{-4096, "bytes", "-4.0KiB"},
		{498, "connections", "498"},
	}
	for _, tc := range tests {
		if got := seriesValue(tc.value, tc.unit); got != tc.want {
			t.Errorf("seriesValue(%v, %q) = %q, want %q", tc.value, tc.unit, got, tc.want)
		}
	}
}

// TestSeriesLabel: the unit rides on the name when the value does not already
// carry it, which is what keeps a table of mixed units aligned.
func TestSeriesLabel(t *testing.T) {
	tests := []struct {
		unit string
		want string
	}{
		{"", "api cpu"},
		{"cores", "api cpu (cores)"},
		{"connections", "api cpu (connections)"},
		// Rendered into the value, so repeating it in the name would be noise.
		{"bytes", "api cpu"},
		{"percent", "api cpu"},
		{"%", "api cpu"},
		{"  Bytes  ", "api cpu"},
	}
	for _, tc := range tests {
		s := collect.Series{Source: "api", Name: "cpu", Unit: tc.unit}
		if got := seriesLabel(s); got != tc.want {
			t.Errorf("seriesLabel(unit=%q) = %q, want %q", tc.unit, got, tc.want)
		}
	}
}

// TestPeakAt covers the timeline the peak is reported on: the same one the
// window labels use.
func TestPeakAt(t *testing.T) {
	tests := []struct {
		offsetMs float64
		want     string
	}{
		{0, "0.0ms"},
		{500, "500.0ms"},
		{10000, "10s"},
		{90000, "1m30s"},
	}
	for _, tc := range tests {
		if got := peakAt(tc.offsetMs); got != tc.want {
			t.Errorf("peakAt(%v) = %q, want %q", tc.offsetMs, got, tc.want)
		}
	}
}

// TestPeakCellLeavesAFlatSeriesUnmarked: a series that never moved has no peak
// to point at, and an offset of a few milliseconds would only suggest one.
func TestPeakCellLeavesAFlatSeriesUnmarked(t *testing.T) {
	flat := collect.Stats{Min: 499, Max: 499, Avg: 499, MaxAtMs: 6.8, Count: 5}
	if got := peakCell(flat); got != "—" {
		t.Errorf("peakCell(flat) = %q, want the unmarked dash", got)
	}

	moved := collect.Stats{Min: 120, Max: 499, Avg: 300, MaxAtMs: 9000, Count: 5}
	if got := peakCell(moved); got != "9s" {
		t.Errorf("peakCell(moved) = %q, want %q", got, "9s")
	}
}

// TestSystemRowsLeavesUnreadableSeriesUnformatted keeps systemRows honest about
// a series it cannot summarise.
func TestSystemRowsLeavesUnreadableSeriesUnformatted(t *testing.T) {
	rows := systemRows(collect.Report{Series: []collect.Series{
		series("api", "cpu", "cores", at(0, math.NaN())),
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Summarised {
		t.Error("a series with no finite readings should not be marked summarised")
	}
	if rows[0].Series != "api cpu (cores)" {
		t.Errorf("Series = %q, want %q", rows[0].Series, "api cpu (cores)")
	}
}

// TestSystemNoteNeedsAConfig keeps the note honest when there is no config to
// read an interval from.
func TestSystemNoteNeedsAConfig(t *testing.T) {
	if got := systemNote(nil); got != "" {
		t.Errorf("systemNote(nil) = %q, want empty", got)
	}
	if got := systemNote(testConfig()); got != "" {
		t.Errorf("systemNote with no collectors = %q, want empty", got)
	}
	if got := systemNote(collectedConfig()); got != "sampled every 5s" {
		t.Errorf("systemNote = %q, want %q", got, "sampled every 5s")
	}
}

// TestCapacityReportCarriesTheSystemSection checks a capacity report shows the
// deciding measurement's readings rather than nothing: the validation run when
// there is one, and a passing probe otherwise.
func TestCapacityReportCarriesTheSystemSection(t *testing.T) {
	render := func(t *testing.T, out *search.Outcome) string {
		t.Helper()
		var buf bytes.Buffer
		if err := RenderCapacityHTML(&buf, collectedConfig(), out, "0.8.0",
			time.Second, time.Unix(0, 0)); err != nil {
			t.Fatalf("RenderCapacityHTML: %v", err)
		}
		return buf.String()
	}

	t.Run("from the validation run", func(t *testing.T) {
		out := searchOutcome()
		out.Validation.Result.System = collectedSystem()
		if got := render(t, out); !strings.Contains(got, "postgres connections") {
			t.Errorf("capacity report should show the validation run's readings:\n%s", got)
		}
	})

	t.Run("from the last passing probe when there is no validation", func(t *testing.T) {
		out := searchOutcome()
		out.Validation = nil
		for i := len(out.Probes) - 1; i >= 0; i-- {
			if out.Probes[i].Pass {
				out.Probes[i].Result.System = collectedSystem()
				break
			}
		}
		if got := render(t, out); !strings.Contains(got, "postgres connections") {
			t.Errorf("capacity report should fall back to the passing probe's readings:\n%s", got)
		}
	})

	t.Run("absent when nothing was collected", func(t *testing.T) {
		out := searchOutcome()
		if got := render(t, out); strings.Contains(got, "<h2>System") {
			t.Error("capacity report has a system section with no readings")
		}
	})
}

// TestCapacityTerminalReportCarriesTheSystemSection covers the terminal report
// too: both renderers must show the same thing, or a reader has to guess which
// one to trust.
func TestCapacityTerminalReportCarriesTheSystemSection(t *testing.T) {
	out := searchOutcome()
	out.Validation.Result.System = collectedSystem()

	var buf bytes.Buffer
	RenderCapacity(&buf, collectedConfig(), out, "0.8.0", time.Second, false)
	got := buf.String()

	for _, want := range []string{"SYSTEM", "sampled every 5s", "postgres connections", "97.0%"} {
		if !strings.Contains(got, want) {
			t.Errorf("capacity report does not contain %q\n%s", want, got)
		}
	}
}

// TestCapacityTerminalReportOmitsTheSystemSection keeps the section out when
// nothing was collected.
func TestCapacityTerminalReportOmitsTheSystemSection(t *testing.T) {
	var buf bytes.Buffer
	RenderCapacity(&buf, collectedConfig(), searchOutcome(), "0.8.0", time.Second, false)
	if strings.Contains(buf.String(), "SYSTEM") {
		t.Error("capacity report has a system section with no readings")
	}
}
