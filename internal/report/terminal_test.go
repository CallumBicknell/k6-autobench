package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
)

func testConfig() *config.Config {
	cfg := &config.Config{
		Name:   "demo",
		Target: config.Target{BaseURL: "http://localhost:3000"},
		Workload: config.Workload{Requests: []config.Request{
			{Name: "feed", Method: "GET", Path: "/feed", Weight: 1},
			{Name: "post", Method: "GET", Path: "/posts/1", Weight: 1},
		}},
		Run: config.Run{Rate: 100, Duration: config.Duration{Duration: 5 * time.Second}, TimeUnit: "1s"},
	}
	cfg.ApplyDefaults()
	return cfg
}

func testResult() *results.Result {
	return &results.Result{
		Name:      "demo",
		Target:    "http://localhost:3000",
		Rate:      100,
		TimeUnit:  "1s",
		Duration:  config.Duration{Duration: 5 * time.Second},
		VUs:       results.VUPlan{PreAllocated: 10, Max: 100},
		StartedAt: time.Unix(0, 0),
		EndedAt:   time.Unix(0, 0),
		ElapsedMs: 5000,
		Metrics: metrics.Metrics{
			HasLatency:    true,
			MinMs:         0.2,
			AvgMs:         0.4,
			MaxMs:         1.0,
			Percentiles:   []metrics.Percentile{{P: 50, Val: 0.4}, {P: 99, Val: 0.7}},
			Requests:      500,
			RequestRate:   100,
			Iterations:    500,
			IterationRate: 100,
		},
		Verdict: results.VerdictPass,
	}
}

func render(t *testing.T, r *results.Result, color bool) string {
	t.Helper()
	var buf bytes.Buffer
	Render(&buf, testConfig(), r, color)
	return buf.String()
}

func TestRenderPlainOutputHasNoANSI(t *testing.T) {
	out := render(t, testResult(), false)
	if strings.Contains(out, "\x1b[") {
		t.Errorf("color=false output contains ANSI escapes:\n%q", out)
	}
}

func TestRenderColorOutputHasANSI(t *testing.T) {
	if out := render(t, testResult(), true); !strings.Contains(out, "\x1b[") {
		t.Error("color=true output contains no ANSI escapes")
	}
}

func TestRenderReportsRunShapeAndVerdict(t *testing.T) {
	out := render(t, testResult(), false)

	for _, want := range []string{
		"AUTOBENCH v0.1",
		"http://localhost:3000",
		"demo (2 weighted requests)",
		"100 iterations/1s",
		"RESULT: PASS",
		"p99",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderReportsFailures(t *testing.T) {
	r := testResult()
	r.Verdict = results.VerdictFailErrors
	r.Metrics.ErrorRate = 0.05
	r.Metrics.FailedRequests = 25
	r.Failures = []results.Failure{
		{Metric: "error_rate", Limit: 0.01, Observed: 0.05, Unit: "ratio"},
	}

	out := render(t, r, false)

	if !strings.Contains(out, "RESULT: FAIL_ERRORS") {
		t.Errorf("missing failing verdict:\n%s", out)
	}
	if !strings.Contains(out, "5.000%  >  1.000%") {
		t.Errorf("missing ratio failure line:\n%s", out)
	}
}

// TestRenderSubMillisecondLimit: a 1us limit must not collapse to "0.0ms".
func TestRenderSubMillisecondLimit(t *testing.T) {
	r := testResult()
	r.Verdict = results.VerdictFailLatency
	r.Failures = []results.Failure{
		{Metric: "latency.p50", Limit: 0.001, Observed: 0.5, Unit: "ms"},
	}

	out := render(t, r, false)
	if !strings.Contains(out, "0.5ms  >  1.0us") {
		t.Errorf("sub-millisecond limit not rendered in microseconds:\n%s", out)
	}
}

func TestRenderGeneratorSaturation(t *testing.T) {
	r := testResult()
	r.Verdict = results.VerdictFailGenerator
	r.Generator = &results.GeneratorStatus{
		Saturated:    true,
		TargetRate:   4000,
		AchievedRate: 24,
		Dropped:      15,
		Reason:       "maxVUs reached",
	}

	out := render(t, r, false)
	for _, want := range []string{"GENERATOR SATURATION", "4,000.0/s", "24.0/s", "maxVUs reached"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderBoxWidth: every framed line must share one visible width, even when
// colour codes are present (they must not be counted).
func TestRenderBoxWidth(t *testing.T) {
	out := render(t, testResult(), true)

	var widths []int
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "│") || strings.HasPrefix(line, "╭") || strings.HasPrefix(line, "╰") {
			widths = append(widths, visibleLen(line))
		}
	}
	if len(widths) != 3 {
		t.Fatalf("found %d framed lines, want 3", len(widths))
	}
	for _, w := range widths {
		if w != widths[0] {
			t.Errorf("framed line widths differ: %v", widths)
		}
	}
	if widths[0] != boxWidth {
		t.Errorf("box width = %d, want %d", widths[0], boxWidth)
	}
}

func TestDurationFormatter(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.0005, "0.5us"},
		{0.001, "1.0us"},
		{0.5, "500.0us"},
		{0.999, "999.0us"},
		{1, "1.0ms"},
		{500, "500.0ms"},
		{1000, "1.00s"},
		{1500, "1.50s"},
	} {
		if got := duration(tc.in); got != tc.want {
			t.Errorf("duration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCommafy(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1200, "1,200"},
		{1234567, "1,234,567"},
		{-1200, "-1,200"},
	} {
		if got := commafy(tc.in); got != tc.want {
			t.Errorf("commafy(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
