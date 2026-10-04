package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	"github.com/CallumBicknell/k6-autobench/internal/results"
)

// windowedConfig is a config with windowed analysis enabled, matching what a
// user would write to catch a collapse mid-run.
func windowedConfig() *config.Config {
	cfg := testConfig()
	cfg.Analysis.Window = config.Duration{Duration: 10 * time.Second}
	cfg.Thresholds.Latency.P95 = &config.LatencyThreshold{
		Max: config.Duration{Duration: 50 * time.Millisecond},
	}
	cfg.ApplyDefaults()
	return cfg
}

// gapWindow is a window the parser emitted for a stretch with no traffic: it
// carries its time range but nothing else.
func gapWindow(index int) metrics.Window {
	size := 10 * time.Second
	return metrics.Window{
		Index:   index,
		StartMs: float64(index) * float64(size) / float64(time.Millisecond),
		EndMs:   float64(index+1) * float64(size) / float64(time.Millisecond),
	}
}

func win(index int, requests, failed int64, p95, max float64) metrics.Window {
	size := 10 * time.Second
	return metrics.Window{
		Index:      index,
		StartMs:    float64(index) * float64(size) / float64(time.Millisecond),
		EndMs:      float64(index+1) * float64(size) / float64(time.Millisecond),
		Requests:   requests,
		Failed:     failed,
		ErrorRate:  float64(failed) / float64(requests),
		HasLatency: true,
		AvgMs:      p95 / 2,
		MaxMs:      max,
		Percentiles: []metrics.Percentile{
			{P: 50, Val: p95 / 2},
			{P: 95, Val: p95},
			{P: 99, Val: max},
		},
	}
}

// flatWindows is a run whose latency held steady.
func flatWindows() []metrics.Window {
	return []metrics.Window{
		win(0, 500, 0, 1.2, 1.8),
		win(1, 501, 0, 1.3, 2.0),
		win(2, 499, 0, 1.2, 1.9),
	}
}

// collapsingWindows is the case windowing exists to catch: fine for two windows,
// then a cliff.
func collapsingWindows() []metrics.Window {
	return []metrics.Window{
		win(0, 500, 0, 40, 60),
		win(1, 500, 0, 45, 70),
		win(2, 500, 0, 1200, 1800),
	}
}

func renderWindowsTerminal(t *testing.T, ws []metrics.Window) string {
	t.Helper()
	r := testResult()
	r.Windows = ws
	var buf bytes.Buffer
	Render(&buf, windowedConfig(), r, "0.4.0", false)
	return buf.String()
}

func TestRenderWindowsSection(t *testing.T) {
	got := renderWindowsTerminal(t, collapsingWindows())

	for _, want := range []string{
		"LATENCY OVER TIME (10s windows)",
		"0s-10s",
		"10s-20s",
		"20s-30s",
		"REQUESTS",
		"1200.0ms",
		"worst",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Worst window") || !strings.Contains(got, "20s-30s") {
		t.Errorf("worst window not summarised:\n%s", got)
	}
	if !strings.Contains(got, "x the median") {
		t.Errorf("expected the worst window compared to the median:\n%s", got)
	}
}

// TestRenderWindowsAbsentWhenDisabled: windowed analysis is opt-in, so a run
// without it must not grow an empty section.
func TestRenderWindowsAbsentWhenDisabled(t *testing.T) {
	if got := renderWindowsTerminal(t, nil); strings.Contains(got, "LATENCY OVER TIME") {
		t.Errorf("report has a window section with no windows:\n%s", got)
	}
}

func TestRenderWindowsFlatRun(t *testing.T) {
	got := renderWindowsTerminal(t, flatWindows())
	if !strings.Contains(got, "Worst window") {
		t.Errorf("expected a worst-window line:\n%s", got)
	}
}

// TestRenderWindowsSkipsEmptyWindowsInWorst: a window with no traffic measured
// nothing, so it must not be chosen as the worst.
func TestRenderWindowsSkipsEmptyWindowsInWorst(t *testing.T) {
	ws := append(flatWindows(), gapWindow(3))
	got := renderWindowsTerminal(t, ws)

	if !strings.Contains(got, "30s-40s") {
		t.Errorf("the empty window should still be listed:\n%s", got)
	}
	if strings.Contains(got, "Worst window  30s-40s") {
		t.Errorf("an empty window was reported as the worst:\n%s", got)
	}
}

func TestFeaturedPercentile(t *testing.T) {
	if p, ok := featuredPercentile([]float64{50, 75, 90, 95, 99}); !ok || p != 95 {
		t.Errorf("featuredPercentile = %v, %v; want 95, true", p, ok)
	}
	if p, ok := featuredPercentile([]float64{50, 99.9}); !ok || p != 99.9 {
		t.Errorf("featuredPercentile = %v, %v; want the highest collected 99.9", p, ok)
	}
	if _, ok := featuredPercentile(nil); ok {
		t.Error("featuredPercentile(nil) reported a percentile")
	}
}

func TestRenderHTMLWindows(t *testing.T) {
	r := testResult()
	r.Windows = collapsingWindows()

	var buf bytes.Buffer
	if err := RenderHTML(&buf, windowedConfig(), r, "0.4.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"Latency over time",
		"<svg",
		`aria-label="latency over time"`,
		"Worst window",
		"20s-30s",
		"worst",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	assertSelfContained(t, got)
}

func TestRenderHTMLWindowsAbsentWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), testResult(), "0.4.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if strings.Contains(buf.String(), "Latency over time") {
		t.Error("report has a window section with no windows")
	}
}

func TestRenderWindowChart(t *testing.T) {
	var b strings.Builder
	renderWindowChart(&b, windowChart{
		Windows: collapsingWindows(),
		P:       95,
		Size:    10 * time.Second,
		LimitMs: 50,
	})
	got := b.String()

	for _, want := range []string{"<svg", "<polyline", `class="limit"`, "p95 limit 50.0ms", "20s"} {
		if !strings.Contains(got, want) {
			t.Errorf("chart does not contain %q\n%s", want, got)
		}
	}
	if n := strings.Count(got, "<circle"); n != 3 {
		t.Errorf("chart has %d points, want 3", n)
	}
}

// TestRenderWindowChartWithoutPercentiles: a chart needs something to plot.
func TestRenderWindowChartWithoutPercentiles(t *testing.T) {
	var b strings.Builder
	renderWindowChart(&b, windowChart{
		Windows: []metrics.Window{{Index: 0, Requests: 10}},
		P:       95,
		Size:    time.Second,
	})
	if b.String() != "" {
		t.Errorf("chart = %q, want empty", b.String())
	}
}

// TestWindowPolicyDowngradesVerdict: with require_all_windows_pass the verdict
// reflects the window breach even though the run average is fine.
func TestWindowPolicyDowngradesVerdict(t *testing.T) {
	cfg := windowedConfig()
	cfg.Analysis.RequireAllWindowsPass = true

	failures := results.WindowFailures(cfg, collapsingWindows())
	if len(failures) == 0 {
		t.Fatal("expected window failures")
	}
	if v := results.ApplyWindowPolicy(cfg, results.VerdictPass, failures); v != results.VerdictFailLatency {
		t.Errorf("verdict = %s, want FAIL_LATENCY", v)
	}
}
