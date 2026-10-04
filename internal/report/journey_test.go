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

// journeyConfig is a 3-step journey: 100 iterations/s is 300 requests/s.
func journeyConfig() *config.Config {
	cfg := &config.Config{
		Name:   "checkout",
		Target: config.Target{BaseURL: "http://localhost:3000"},
		Workload: config.Workload{Journey: []config.Request{
			{Name: "login", Method: "POST", Path: "/login"},
			{Name: "cart", Method: "GET", Path: "/cart"},
			{Name: "order", Method: "POST", Path: "/orders"},
		}},
		Run: config.Run{Rate: 100, Duration: config.Duration{Duration: 30 * time.Second}, TimeUnit: "1s"},
	}
	cfg.ApplyDefaults()
	return cfg
}

func journeyResult() *results.Result {
	r := testResult()
	r.Rate = 100
	r.Metrics.HasIterationLatency = true
	r.Metrics.IterationAvgMs = 45
	r.Metrics.IterationMaxMs = 220
	r.Metrics.IterationPercentiles = []metrics.Percentile{{P: 95, Val: 120}, {P: 99, Val: 190}}
	r.Metrics.Endpoints = []metrics.Endpoint{
		{Name: "login", Requests: 3000, HasLatency: true, AvgMs: 10, MaxMs: 60,
			Percentiles: []metrics.Percentile{{P: 95, Val: 25}}},
		{Name: "cart", Requests: 3000, HasLatency: true, AvgMs: 12, MaxMs: 70,
			Percentiles: []metrics.Percentile{{P: 95, Val: 30}}},
		{Name: "order", Requests: 3000, HasLatency: true, AvgMs: 23, MaxMs: 90,
			Percentiles: []metrics.Percentile{{P: 95, Val: 65}}},
	}
	return r
}

func TestRateLine(t *testing.T) {
	// One request per iteration: the two rates are the same, so only one is shown.
	if got := rateLine(500, "1s", 1); got != "500 iterations/1s" {
		t.Errorf("rateLine(1 request) = %q", got)
	}
	// A 3-step journey: 100 iterations/s is 300 requests/s.
	got := rateLine(100, "1s", 3)
	if !strings.Contains(got, "100 iterations/1s") || !strings.Contains(got, "300 requests/1s") {
		t.Errorf("rateLine(3 steps) = %q, want both rates", got)
	}
	// Comma grouping on both.
	if got := rateLine(5000, "1s", 3); !strings.Contains(got, "5,000") || !strings.Contains(got, "15,000") {
		t.Errorf("rateLine = %q, want grouped rates", got)
	}
}

func TestWorkloadSummary(t *testing.T) {
	if got := workloadSummary(journeyConfig()); got != "checkout (3-step journey)" {
		t.Errorf("workloadSummary(journey) = %q", got)
	}
	if got := workloadSummary(testConfig()); !strings.Contains(got, "weighted requests") {
		t.Errorf("workloadSummary(weighted) = %q", got)
	}
}

// TestRenderJourneyShowsBothRates is the point of PLAN-OUTLINE.md §11: an
// iteration rate is not a request rate.
func TestRenderJourneyShowsBothRates(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, journeyConfig(), journeyResult(), "0.5.0", false)
	got := buf.String()

	for _, want := range []string{
		"3-step journey",
		"100 iterations/1s",
		"300 requests/1s",
		"Journey latency",
		"end to end",
		"120.0ms", // journey p95
		"login",   // per-step breakdown
		"cart",
		"order",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
}

// TestRenderWeightedHasNoJourneySection: a single-request iteration has no
// journey latency to show, and showing it would be noise.
func TestRenderWeightedHasNoJourneySection(t *testing.T) {
	r := testResult()
	r.Metrics.HasIterationLatency = true
	r.Metrics.IterationAvgMs = 1

	var buf bytes.Buffer
	Render(&buf, testConfig(), r, "0.5.0", false)
	if strings.Contains(buf.String(), "Journey latency") {
		t.Errorf("a weighted pool should not report journey latency:\n%s", buf.String())
	}
}

func TestRenderJourneyHTML(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHTML(&buf, journeyConfig(), journeyResult(), "0.5.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"3-step journey",
		"300 requests/1s",
		"Journey latency",
		"120.0ms",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	assertSelfContained(t, got)
}

func TestRenderCapacityJourneyRates(t *testing.T) {
	cfg := journeyConfig()
	cfg.Search.StartRate = 100
	cfg.ApplyDefaults()

	out := searchOutcome()
	out.PassingRate, out.Capacity, out.FailingRate = 450, 450, 500
	out.Validation = nil
	out.ValidationRate = 0

	var buf bytes.Buffer
	RenderCapacity(&buf, cfg, out, "0.5.0", time.Minute, false)
	got := buf.String()

	// 450 iterations/s of a 3-step journey is 1,350 requests/s.
	if !strings.Contains(got, "450 iterations/s") || !strings.Contains(got, "1,350 requests/s") {
		t.Errorf("capacity report does not show both rates:\n%s", got)
	}
}
