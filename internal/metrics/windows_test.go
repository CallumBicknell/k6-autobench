package metrics

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// point renders one k6 JSON-lines sample record.
func point(metric string, at time.Time, value float64) string {
	return `{"metric":"` + metric + `","type":"Point","data":{"time":"` +
		at.UTC().Format(time.RFC3339Nano) + `","value":` +
		strconv.FormatFloat(value, 'f', -1, 64) +
		`,"tags":{"name":"a"}}}`
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// sampleStream builds a k6 JSON-lines stream: the durations in window 0, then
// window 1, nothing in window 2 (a deliberate gap in traffic), then window 3.
func sampleStream(origin time.Time, durations map[int][]float64) string {
	var b strings.Builder
	// A Metric metadata record, as k6 emits once per metric.
	b.WriteString(`{"type":"Metric","data":{"name":"http_reqs"},"metric":"http_reqs"}` + "\n")

	at := func(sec int) time.Time { return origin.Add(time.Duration(sec) * time.Second) }
	add := func(metric string, t time.Time, v float64) {
		b.WriteString(point(metric, t, v) + "\n")
	}

	// Window 0: 0-10s, one failure on the first request.
	for i, d := range durations[0] {
		add("http_reqs", at(i), 1)
		add("http_req_failed", at(i), boolValue(i == 0))
		add("http_req_duration", at(i), d)
	}
	// Window 1: 10-20s, no failures.
	for i, d := range durations[1] {
		add("http_reqs", at(11+i), 1)
		add("http_req_failed", at(11+i), 0)
		add("http_req_duration", at(11+i), d)
	}
	// Window 3: 30-40s.
	for i, d := range durations[3] {
		add("http_reqs", at(31+i), 1)
		add("http_req_failed", at(31+i), 0)
		add("http_req_duration", at(31+i), d)
	}
	return b.String()
}

func origin() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

func TestReadWindowsBucketsByTime(t *testing.T) {
	stream := sampleStream(origin(), map[int][]float64{
		0: {1, 2, 3, 4, 5},
		1: {10, 20},
		3: {100},
	})

	got, err := ReadWindows(strings.NewReader(stream), WindowConfig{
		Size:        10 * time.Second,
		Percentiles: []float64{50, 95},
	})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}

	// Windows 0, 1, 2 and 3: the gap at 2 is kept so it is visible.
	if len(got) != 4 {
		t.Fatalf("got %d windows, want 4: %+v", len(got), got)
	}
	for i, w := range got {
		if w.Index != i {
			t.Errorf("window %d has Index %d", i, w.Index)
		}
		if want := float64(i) * 10000; w.StartMs != want {
			t.Errorf("window %d StartMs = %v, want %v", i, w.StartMs, want)
		}
		if want := float64(i+1) * 10000; w.EndMs != want {
			t.Errorf("window %d EndMs = %v, want %v", i, w.EndMs, want)
		}
	}

	w0 := got[0]
	if w0.Requests != 5 || w0.Failed != 1 {
		t.Errorf("window 0 requests/failed = %d/%d, want 5/1", w0.Requests, w0.Failed)
	}
	if w0.ErrorRate != 0.2 {
		t.Errorf("window 0 error rate = %v, want 0.2", w0.ErrorRate)
	}
	if !w0.HasLatency || w0.AvgMs != 3 || w0.MaxMs != 5 {
		t.Errorf("window 0 latency = %+v, want avg 3 max 5", w0)
	}
	// Sorted [1,2,3,4,5]: p50 = 3, p95 interpolates between 4 and 5.
	if p := windowPercentile(t, w0, 50); p != 3 {
		t.Errorf("window 0 p50 = %v, want 3", p)
	}
	if p := windowPercentile(t, w0, 95); p != 4.8 {
		t.Errorf("window 0 p95 = %v, want 4.8", p)
	}

	// The empty window must be reported as empty, not omitted or merged.
	w2 := got[2]
	if w2.Requests != 0 || w2.HasLatency || len(w2.Percentiles) != 0 {
		t.Errorf("window 2 = %+v, want an empty window", w2)
	}

	if got[3].Requests != 1 {
		t.Errorf("window 3 requests = %d, want 1", got[3].Requests)
	}
}

func windowPercentile(t *testing.T, w Window, p float64) float64 {
	t.Helper()
	for _, pc := range w.Percentiles {
		if pc.P == p {
			return pc.Val
		}
	}
	t.Fatalf("window %d has no p%v: %+v", w.Index, p, w.Percentiles)
	return 0
}

func TestReadWindowsRejectsZeroSize(t *testing.T) {
	if _, err := ReadWindows(strings.NewReader(""), WindowConfig{}); err == nil {
		t.Error("got nil error for a zero window size")
	}
}

func TestReadWindowsEmptyStream(t *testing.T) {
	got, err := ReadWindows(strings.NewReader(""), WindowConfig{Size: time.Second})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d windows from an empty stream", len(got))
	}
}

// TestReadWindowsIgnoresOtherMetrics: a run emits one line per metric per
// iteration; only three of them are interesting.
func TestReadWindowsIgnoresOtherMetrics(t *testing.T) {
	var b strings.Builder
	b.WriteString(point("vus", origin(), 5) + "\n")
	b.WriteString(point("iteration_duration", origin(), 12) + "\n")
	b.WriteString(point("data_sent", origin(), 128) + "\n")

	got, err := ReadWindows(strings.NewReader(b.String()), WindowConfig{Size: time.Second})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d windows, want none: %+v", len(got), got)
	}
}

func TestReadWindowsRejectsMalformedLine(t *testing.T) {
	stream := `{"metric":"http_reqs","type":"Point","data":{` + "\n"
	if _, err := ReadWindows(strings.NewReader(stream), WindowConfig{Size: time.Second}); err == nil {
		t.Error("got nil error for a malformed line")
	}
}

// TestReadWindowsCapsRetainedValues: a very high rate over a very wide window
// must not be able to exhaust memory, and the report has to say the percentiles
// came from a prefix.
func TestReadWindowsCapsRetainedValues(t *testing.T) {
	stream := sampleStream(origin(), map[int][]float64{0: {1, 2, 3, 4, 5}})

	got, err := ReadWindows(strings.NewReader(stream), WindowConfig{
		Size:        10 * time.Second,
		Percentiles: []float64{50},
		MaxValues:   2,
	})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d windows, want 1", len(got))
	}
	w := got[0]
	if !w.Sampled {
		t.Error("Sampled = false, want true for a capped window")
	}
	// The first two durations are 1 and 2.
	if w.AvgMs != 1.5 || w.MaxMs != 2 {
		t.Errorf("capped latency = avg %v max %v, want 1.5/2", w.AvgMs, w.MaxMs)
	}
	// The request count is still the whole window's.
	if w.Requests != 5 {
		t.Errorf("requests = %d, want the full 5", w.Requests)
	}
}

// TestReadWindowsAttributesOutOfOrderSamples: a backwards timestamp would be a
// k6 bug, but the sample should be folded into the window in progress rather
// than dropped or panicking on a negative index.
func TestReadWindowsAttributesOutOfOrderSamples(t *testing.T) {
	o := origin()
	var b strings.Builder
	// The origin is set by the first sample, so it must be the earliest.
	b.WriteString(point("http_req_duration", o, 1) + "\n")
	b.WriteString(point("http_req_duration", o.Add(15*time.Second), 10) + "\n")
	b.WriteString(point("http_req_duration", o.Add(5*time.Second), 20) + "\n")

	got, err := ReadWindows(strings.NewReader(b.String()), WindowConfig{
		Size: 10 * time.Second, Percentiles: []float64{50},
	})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2: %+v", len(got), got)
	}
	if p := windowPercentile(t, got[0], 50); p != 1 {
		t.Errorf("window 0 p50 = %v, want 1", p)
	}
	// Window 1 holds both the 15s sample and the late 5s one.
	if n := len(got[1].Percentiles); n != 1 {
		t.Fatalf("window 1 has %d percentiles, want 1", n)
	}
	if got[1].AvgMs != 15 {
		t.Errorf("window 1 avg = %v, want 15 (10 and the folded-in 20)", got[1].AvgMs)
	}
	if got[1].MaxMs != 20 {
		t.Errorf("window 1 max = %v, want 20", got[1].MaxMs)
	}
}

func TestPercentileOf(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5}

	tests := []struct {
		p    float64
		want float64
	}{
		{p: 0, want: 1},
		{p: 50, want: 3},
		{p: 95, want: 4.8},
		{p: 100, want: 5},
	}
	for _, tc := range tests {
		if got := PercentileOf(sorted, tc.p); got != tc.want {
			t.Errorf("PercentileOf(p%v) = %v, want %v", tc.p, got, tc.want)
		}
	}

	if got := PercentileOf(nil, 95); got != 0 {
		t.Errorf("PercentileOf(nil) = %v, want 0", got)
	}
	if got := PercentileOf([]float64{7}, 95); got != 7 {
		t.Errorf("PercentileOf(single) = %v, want 7", got)
	}
}

// TestReadWindowsRealK6Capture parses a capture from a real k6 run, so the
// assumptions about k6's --out json format (record shape, nanosecond timestamps,
// millisecond duration values, 0/1 rate samples, and the other metrics mixed in)
// are pinned to reality rather than to a fixture written to match the parser.
func TestReadWindowsRealK6Capture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "k6-samples.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	windows, err := ReadWindows(f, WindowConfig{
		Size:        time.Second,
		Percentiles: []float64{50, 95},
	})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	if len(windows) == 0 {
		t.Fatal("no windows parsed from a real capture")
	}

	var requests, failed int64
	var maxMs float64
	for _, w := range windows {
		requests += w.Requests
		failed += w.Failed
		maxMs = math.Max(maxMs, w.MaxMs)
		if w.HasLatency && len(w.Percentiles) != 2 {
			t.Errorf("window %d has %d percentiles, want 2", w.Index, len(w.Percentiles))
		}
	}

	// The capture is the first 20 completed requests of a 20/s run against a
	// local server, all of which returned 200.
	if requests != 20 {
		t.Errorf("requests = %d, want 20", requests)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	// Values are milliseconds, so a local request is a small number, not seconds.
	if maxMs <= 0 || maxMs > 1000 {
		t.Errorf("max latency = %vms, want a plausible millisecond value", maxMs)
	}
}
