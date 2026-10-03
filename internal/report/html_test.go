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

// assertSelfContained checks the report needs nothing from the network: no
// scripts, no external stylesheets, no images. That is what makes it safe to
// archive as a CI artefact and readable offline.
func assertSelfContained(t *testing.T, html string) {
	t.Helper()
	for _, bad := range []string{"<script", " src=", "href=", "@import", "url("} {
		if strings.Contains(html, bad) {
			t.Errorf("report references external content (%q)", bad)
		}
	}
}

func TestRenderHTMLFixedRun(t *testing.T) {
	r := testResult()
	r.Metrics.Endpoints = []metrics.Endpoint{
		{Name: "feed", Requests: 250, RequestRate: 50, HasLatency: true, AvgMs: 3.1, MaxMs: 12,
			Percentiles: []metrics.Percentile{{P: 95, Val: 6.8}, {P: 99, Val: 9.2}}},
		{Name: "post", Requests: 250, RequestRate: 50, Failed: 5, ErrorRate: 0.02, HasLatency: true,
			AvgMs: 4.0, MaxMs: 20, Percentiles: []metrics.Percentile{{P: 95, Val: 8.0}}},
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), r, "0.3.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"<!DOCTYPE html>",
		"demo",
		"fixed-rate run",
		"http://localhost:3000",
		"PASS",
		"Endpoints",
		"feed",
		"post",
		"6.8ms",
		"autobench v0.3.0",
		"2.000%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	assertSelfContained(t, got)

	// A fixed run has no rate/latency chart.
	if strings.Contains(got, "<svg") {
		t.Error("a fixed-rate report should not contain a chart")
	}
}

func TestRenderHTMLCapacitySearch(t *testing.T) {
	out := searchOutcome()
	out.Validation.Result.Metrics.Endpoints = []metrics.Endpoint{
		{Name: "get-user", Requests: 2190, RequestRate: 438, HasLatency: true, AvgMs: 0.9, MaxMs: 3,
			Percentiles: []metrics.Percentile{{P: 95, Val: 1.2}, {P: 99, Val: 1.8}}},
	}

	var buf bytes.Buffer
	if err := RenderCapacityHTML(&buf, testConfig(), out, "0.3.0", 3*time.Minute, time.Unix(0, 0)); err != nil {
		t.Fatalf("RenderCapacityHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"capacity search",
		"<svg",
		"Rate vs latency",
		"4,500",
		"iterations/s sustained",
		"PASS",
		"exponential",
		"binary",
		"validation",
		"get-user",
		"Validated",
		"autobench v0.3.0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	assertSelfContained(t, got)
}

// TestRenderHTMLCapacityLowerBound covers a search that never found a failure:
// the headline must not claim a hard number.
func TestRenderHTMLCapacityLowerBound(t *testing.T) {
	out := searchOutcome()
	out.FailingRate = 0
	out.BracketFound = false
	out.LowerBoundOnly = true
	out.Validation = nil
	out.Notes = []string{"reached search.max_rate (5000/s) without finding a failure"}

	var buf bytes.Buffer
	if err := RenderCapacityHTML(&buf, testConfig(), out, "0.3.0", time.Minute, time.Unix(0, 0)); err != nil {
		t.Fatalf("RenderCapacityHTML: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, "≥ 4,500") {
		t.Errorf("expected a lower-bound headline:\n%s", got)
	}
	if !strings.Contains(got, "no failure boundary found") {
		t.Error("expected the lower bound to be explained")
	}
	if !strings.Contains(got, "max_rate") {
		t.Error("expected the search note to be rendered")
	}
}

// TestRenderHTMLEscapesUserContent: endpoint names come from config and end up
// in the document, so they must be escaped rather than trusted.
func TestRenderHTMLEscapesUserContent(t *testing.T) {
	r := testResult()
	r.Metrics.Endpoints = []metrics.Endpoint{{
		Name:       `</td><script>alert(1)</script>`,
		Requests:   1,
		HasLatency: true,
	}}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), r, "0.3.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if strings.Contains(buf.String(), "<script>") {
		t.Errorf("endpoint name was not escaped:\n%s", buf.String())
	}
}

// TestRenderHTMLGeneratorWarning covers the discarded-probe path.
func TestRenderHTMLGeneratorWarning(t *testing.T) {
	r := testResult()
	r.Verdict = results.VerdictFailGenerator
	r.Generator = &results.GeneratorStatus{
		Saturated: true, TargetRate: 8000, AchievedRate: 4000, Reason: "maxVUs reached",
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), r, "0.3.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{"FAIL_GENERATOR", "Generator saturation", "maxVUs reached"} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	// A failing verdict must not be styled as a pass.
	if strings.Contains(got, `class="verdict ok"`) {
		t.Error("a failing run was rendered with the pass styling")
	}
}

// TestRenderHTMLThresholdBreaches covers the failure list.
func TestRenderHTMLThresholdBreaches(t *testing.T) {
	r := testResult()
	r.Verdict = results.VerdictFailLatency
	r.Failures = []results.Failure{
		{Metric: "latency.p95", Limit: 50, Observed: 210, Unit: "ms"},
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), r, "0.3.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, "Threshold breaches") || !strings.Contains(got, "latency.p95") {
		t.Errorf("threshold breach missing from report:\n%s", got)
	}
}

// TestRenderHTMLOmitsEmptySections keeps a bare run from rendering empty tables.
func TestRenderHTMLOmitsEmptySections(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), testResult(), "0.3.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, absent := range []string{"<svg", "Endpoints", "<table"} {
		if strings.Contains(got, absent) {
			t.Errorf("report contains %q for a run with no endpoints", absent)
		}
	}
}

func TestRenderHTMLUnknownConfigName(t *testing.T) {
	cfg := testConfig()
	cfg.Name = ""

	var buf bytes.Buffer
	if err := RenderHTML(&buf, cfg, testResult(), "0.3.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(buf.String(), "autobench report") {
		t.Error("expected a fallback title for an unnamed config")
	}
}

// TestRenderHTMLCapacityChartIncludesLimitLine checks the SLO line is drawn when
// a p95 threshold is configured.
func TestRenderHTMLCapacityChartIncludesLimitLine(t *testing.T) {
	cfg := testConfig()
	cfg.Thresholds.Latency.P95 = &config.LatencyThreshold{
		Max: config.Duration{Duration: 250 * time.Millisecond},
	}

	var buf bytes.Buffer
	if err := RenderCapacityHTML(&buf, cfg, searchOutcome(), "0.3.0", time.Minute, time.Unix(0, 0)); err != nil {
		t.Fatalf("RenderCapacityHTML: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, `class="limit"`) || !strings.Contains(got, "p95 limit 250.0ms") {
		t.Errorf("expected the p95 SLO line on the chart:\n%s", got)
	}
}
