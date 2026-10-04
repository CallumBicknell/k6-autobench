package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/compare"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	"github.com/CallumBicknell/k6-autobench/internal/results"
)

// compareSnapshot builds a snapshot with the fields the renderers read.
func compareSnapshot(kind, path string, rate, capacity int, p95, errorRate float64) *compare.Snapshot {
	return &compare.Snapshot{
		Path:     path,
		Kind:     kind,
		Name:     "demo",
		Target:   "http://localhost:3000",
		Verdict:  results.VerdictPass,
		Rate:     rate,
		Capacity: capacity,
		Metrics: metrics.Metrics{
			HasLatency:  true,
			AvgMs:       p95 / 2,
			ErrorRate:   errorRate,
			Percentiles: []metrics.Percentile{{P: 95, Val: p95}, {P: 99, Val: p95 * 2}},
			Endpoints: []metrics.Endpoint{{
				Name: "feed", HasLatency: true, AvgMs: p95 / 2,
				Percentiles: []metrics.Percentile{{P: 95, Val: p95}},
			}},
		},
	}
}

// regressedComparison is a fixed-rate run that got slower.
func regressedComparison() *compare.Report {
	return compare.Compare(
		compareSnapshot(compare.KindFixed, "main.json", 500, 0, 81, 0),
		compareSnapshot(compare.KindFixed, "pr.json", 500, 0, 127, 0),
		compare.DefaultConfig(),
	)
}

// cleanComparison is a fixed-rate run that did not.
func cleanComparison() *compare.Report {
	return compare.Compare(
		compareSnapshot(compare.KindFixed, "main.json", 500, 0, 100, 0),
		compareSnapshot(compare.KindFixed, "pr.json", 500, 0, 102, 0),
		compare.DefaultConfig(),
	)
}

// capacityComparison is two searches, one of which lost capacity.
func capacityComparison() *compare.Report {
	return compare.Compare(
		compareSnapshot(compare.KindCapacity, "main.json", 13500, 13500, 50, 0),
		compareSnapshot(compare.KindCapacity, "pr.json", 11900, 11900, 50, 0),
		compare.DefaultConfig(),
	)
}

func renderCompareTerminal(t *testing.T, rep *compare.Report) string {
	t.Helper()
	var buf bytes.Buffer
	RenderCompare(&buf, rep, "1.0.0", false)
	return buf.String()
}

func renderCompareHTMLReport(t *testing.T, rep *compare.Report) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderCompareHTML(&buf, rep, "1.0.0", time.Unix(0, 0)); err != nil {
		t.Fatalf("RenderCompareHTML: %v", err)
	}
	return buf.String()
}

// TestRenderCompareRegression is the outline's own sketch: before, after, and
// the change, with the verdict up front.
func TestRenderCompareRegression(t *testing.T) {
	got := renderCompareTerminal(t, regressedComparison())

	for _, want := range []string{
		"regression comparison",
		"main.json", "pr.json",
		"LATENCY",
		"p95",
		"81.0ms", "127.0ms",
		"56.8%",
		"REGRESSION",
		"FAIL_REGRESSION",
		"noisy measurement",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
}

// TestRenderCompareClean says so plainly rather than leaving the reader to
// compare numbers themselves.
func TestRenderCompareClean(t *testing.T) {
	got := renderCompareTerminal(t, cleanComparison())

	if !strings.Contains(got, "nothing regressed") {
		t.Errorf("report should say nothing regressed:\n%s", got)
	}
	if strings.Contains(got, "REGRESSION") {
		t.Errorf("report claims a regression that is not there:\n%s", got)
	}
	if !strings.Contains(got, "RESULT: PASS") {
		t.Errorf("report should carry the run's own verdict:\n%s", got)
	}
}

// TestRenderCompareCapacity covers the axis that only two searches have.
func TestRenderCompareCapacity(t *testing.T) {
	got := renderCompareTerminal(t, capacityComparison())

	for _, want := range []string{"CAPACITY", "13,500", "11,900", "-11.9%", "REGRESSION"} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
}

// TestRenderCompareOmitsCapacityForFixedRuns: one measurement is not a capacity,
// so the section must not appear.
func TestRenderCompareOmitsCapacityForFixedRuns(t *testing.T) {
	if got := renderCompareTerminal(t, cleanComparison()); strings.Contains(got, "CAPACITY") {
		t.Errorf("report has a capacity section for fixed runs:\n%s", got)
	}
}

// TestRenderCompareNotes keeps a mismatched comparison from looking like an
// answer.
func TestRenderCompareNotes(t *testing.T) {
	rep := compare.Compare(
		compareSnapshot(compare.KindFixed, "a.json", 500, 0, 50, 0),
		compareSnapshot(compare.KindFixed, "b.json", 5000, 0, 50, 0),
		compare.DefaultConfig(),
	)
	got := renderCompareTerminal(t, rep)
	if !strings.Contains(got, "different rates") {
		t.Errorf("report should warn about the mismatched rates:\n%s", got)
	}
}

// TestRenderCompareHTML covers the self-contained comparison report.
func TestRenderCompareHTML(t *testing.T) {
	got := renderCompareHTMLReport(t, regressedComparison())

	for _, want := range []string{
		"<h1>Regression comparison</h1>",
		"<h2>Latency</h2>",
		"FAIL_REGRESSION",
		"81.0ms", "127.0ms",
		"56.8%",
		"REGRESSION",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("html report does not contain %q", want)
		}
	}
	if strings.Contains(got, `src="http`) || strings.Contains(got, `href="http`) {
		t.Error("html report should be self-contained")
	}
}

// TestRenderCompareHTMLCapacity covers the capacity block and the request table.
func TestRenderCompareHTMLCapacity(t *testing.T) {
	got := renderCompareHTMLReport(t, capacityComparison())

	for _, want := range []string{"<h2>Capacity</h2>", "13,500", "11,900", "<h2>Requests</h2>"} {
		if !strings.Contains(got, want) {
			t.Errorf("html report does not contain %q", want)
		}
	}
}

// TestRenderCompareHTMLCleanOmitsTheRegressionBanner keeps the warning out of a
// comparison that has nothing to warn about.
func TestRenderCompareHTMLCleanOmitsTheRegressionBanner(t *testing.T) {
	if got := renderCompareHTMLReport(t, cleanComparison()); strings.Contains(got, "these moved past their tolerance") {
		t.Errorf("html report claims a regression that is not there:\n%s", got)
	}
}
