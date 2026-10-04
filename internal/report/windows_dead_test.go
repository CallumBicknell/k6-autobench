package report

import (
	"strings"
	"testing"

	"github.com/CallumBicknell/k6-autobench/internal/metrics"
)

// windowAt builds one window with a p95 and an error count.
func windowAt(index int, p95 float64, failed, requests int64) metrics.Window {
	return metrics.Window{
		Index: index, Requests: requests, Failed: failed,
		HasLatency:  true,
		MaxMs:       p95,
		Percentiles: []metrics.Percentile{{P: 95, Val: p95}},
	}
}

// deadWindow is a window where nothing answered: the latency is gone and the flag
// says why.
func deadWindow(index int, failed, requests int64) metrics.Window {
	return metrics.Window{
		Index: index, Requests: requests, Failed: failed,
		Unanswered: true,
		ErrorRate:  1,
	}
}

// TestWorstWindowSkipsOneWhereNothingAnswered is the regression test for a window
// report that pointed at a healthy window while the target was dead: a dead
// window's zero latency made it look like the *best* one.
func TestWorstWindowSkipsOneWhereNothingAnswered(t *testing.T) {
	ws := []metrics.Window{
		windowAt(0, 1.5, 0, 500),
		windowAt(1, 1.2, 0, 500),
		deadWindow(2, 500, 500),
		deadWindow(3, 500, 500),
	}

	if got := worstWindow(ws); got != 0 {
		t.Errorf("worstWindow = %d, want 0: the highest real latency", got)
	}
}

// TestWorstWindowIsNoneWhenEveryWindowIsDead: there is no latency to rank.
func TestWorstWindowIsNoneWhenEveryWindowIsDead(t *testing.T) {
	ws := []metrics.Window{deadWindow(0, 10, 10), deadWindow(1, 10, 10)}

	if got := worstWindow(ws); got != -1 {
		t.Errorf("worstWindow = %d, want -1 when nothing answered", got)
	}
}

// TestRenderWindowsSaysWhyTheLatencyIsBlank: a blank column without a reason
// reads as missing data rather than as a dead target.
func TestRenderWindowsSaysWhyTheLatencyIsBlank(t *testing.T) {
	ws := []metrics.Window{
		windowAt(0, 1.5, 0, 500),
		deadWindow(1, 500, 500),
	}

	var b strings.Builder
	p := &printer{w: &b, color: false}
	p.windows(ws, testConfig())
	got := b.String()

	for _, want := range []string{
		"1 of 2 windows received no response at all",
		"left blank rather than reported as zero",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("window block does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "0.0ms") {
		t.Errorf("a dead window was rendered as a zero latency:\n%s", got)
	}
}

// TestRenderWindowsIsQuietWhenNothingIsWrong.
func TestRenderWindowsIsQuietWhenNothingIsWrong(t *testing.T) {
	ws := []metrics.Window{windowAt(0, 1.5, 0, 500), windowAt(1, 1.2, 0, 500)}

	var b strings.Builder
	p := &printer{w: &b, color: false}
	p.windows(ws, testConfig())

	if strings.Contains(b.String(), "received no response") {
		t.Errorf("a healthy run should not mention unanswered windows:\n%s", b.String())
	}
}
