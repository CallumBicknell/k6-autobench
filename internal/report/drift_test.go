package report

import (
	"bytes"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/metrics"
)

// winEP builds a window whose per-endpoint breakdown carries the given p95s.
func winEP(index int, endpoints map[string]float64) metrics.Window {
	size := 10 * time.Second
	w := metrics.Window{
		Index:      index,
		StartMs:    float64(index) * float64(size) / float64(time.Millisecond),
		EndMs:      float64(index+1) * float64(size) / float64(time.Millisecond),
		Requests:   100,
		HasLatency: true,
	}
	names := make([]string, 0, len(endpoints))
	for name := range endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w.Endpoints = append(w.Endpoints, metrics.Endpoint{
			Name:        name,
			Requests:    50,
			HasLatency:  true,
			Percentiles: []metrics.Percentile{{P: 95, Val: endpoints[name]}},
		})
	}
	return w
}

// runEndpoints is the run-wide baseline the drift is measured against.
func runEndpoints(p95 map[string]float64) []metrics.Endpoint {
	names := make([]string, 0, len(p95))
	for name := range p95 {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []metrics.Endpoint
	for _, name := range names {
		out = append(out, metrics.Endpoint{
			Name:        name,
			Requests:    500,
			HasLatency:  true,
			Percentiles: []metrics.Percentile{{P: 95, Val: p95[name]}},
		})
	}
	return out
}

// TestDriftRowsAttributesTheSpike is the point of per-endpoint windows: the
// aggregate table says a window spiked, this says which request did it.
func TestDriftRowsAttributesTheSpike(t *testing.T) {
	ws := []metrics.Window{
		winEP(0, map[string]float64{"browse": 1, "read": 1}),
		winEP(1, map[string]float64{"browse": 1, "read": 900}),
		winEP(2, map[string]float64{"browse": 1, "read": 1}),
	}
	m := metrics.Metrics{Endpoints: runEndpoints(map[string]float64{"browse": 1, "read": 1})}

	rows := driftRows(ws, m, 95)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	// Worst drift first, so the culprit leads.
	if rows[0].Name != "read" {
		t.Errorf("first row = %q, want the request that drifted", rows[0].Name)
	}
	if rows[0].Ratio != 900 {
		t.Errorf("ratio = %v, want 900", rows[0].Ratio)
	}
	if rows[0].Window != "10s-20s" {
		t.Errorf("window = %q, want the one it spiked in", rows[0].Window)
	}
	if rows[0].WorstP95 != 900 {
		t.Errorf("worst p95 = %v, want 900", rows[0].WorstP95)
	}
	if rows[1].Name != "browse" || rows[1].Ratio != 1 {
		t.Errorf("second row = %+v, want browse at 1.0x", rows[1])
	}
}

// TestDriftRowsIgnoresEndpointsWithoutABaseline: a request with no run-wide
// percentile has nothing to be measured against.
func TestDriftRowsIgnoresEndpointsWithoutABaseline(t *testing.T) {
	ws := []metrics.Window{
		winEP(0, map[string]float64{"read": 1}),
		winEP(1, map[string]float64{"read": 900}),
	}
	m := metrics.Metrics{Endpoints: []metrics.Endpoint{
		{Name: "read", HasLatency: true}, // no percentiles collected
		{Name: "gone", HasLatency: true, Percentiles: []metrics.Percentile{{P: 95, Val: 5}}},
	}}

	if rows := driftRows(ws, m, 95); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}

// TestDriftRowsNeedsTwoWindows: drift is a change over time, so a single window
// has nothing to say.
func TestDriftRowsNeedsTwoWindows(t *testing.T) {
	ws := []metrics.Window{winEP(0, map[string]float64{"read": 900})}
	m := metrics.Metrics{Endpoints: runEndpoints(map[string]float64{"read": 1})}

	if rows := driftRows(ws, m, 95); len(rows) != 0 {
		t.Errorf("rows = %+v, want none for a single window", rows)
	}
}

func TestRenderEndpointDrift(t *testing.T) {
	r := testResult()
	r.Windows = []metrics.Window{
		winEP(0, map[string]float64{"browse": 1, "read": 1}),
		winEP(1, map[string]float64{"browse": 1, "read": 900}),
		winEP(2, map[string]float64{"browse": 1, "read": 1}),
	}
	r.Metrics.Endpoints = runEndpoints(map[string]float64{"browse": 1, "read": 1})

	var buf bytes.Buffer
	Render(&buf, windowedConfig(), r, "0.7.0", false)
	got := buf.String()

	for _, want := range []string{
		"ENDPOINT DRIFT (worst window vs the run, p95)",
		"read", "900.0ms", "900.0x", "10s-20s",
		"browse", "1.0x",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
}

// TestRenderEndpointDriftAbsentWithoutWindows: drift needs a windowed view.
func TestRenderEndpointDriftAbsentWithoutWindows(t *testing.T) {
	r := testResult()
	r.Metrics.Endpoints = runEndpoints(map[string]float64{"read": 1})

	var buf bytes.Buffer
	Render(&buf, windowedConfig(), r, "0.7.0", false)
	if strings.Contains(buf.String(), "ENDPOINT DRIFT") {
		t.Errorf("drift printed with no windows:\n%s", buf.String())
	}
}

func TestRenderHTMLEndpointDrift(t *testing.T) {
	r := testResult()
	r.Windows = []metrics.Window{
		winEP(0, map[string]float64{"browse": 1, "read": 1}),
		winEP(1, map[string]float64{"browse": 1, "read": 900}),
	}
	r.Metrics.Endpoints = runEndpoints(map[string]float64{"browse": 1, "read": 1})

	var buf bytes.Buffer
	if err := RenderHTML(&buf, windowedConfig(), r, "0.7.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{"Endpoint drift", "900.0x", "10s-20s"} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	assertSelfContained(t, got)
}
