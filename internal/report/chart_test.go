package report

import (
	"strings"
	"testing"
)

func TestNiceCeil(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{in: 0, want: 1},
		{in: -5, want: 1},
		{in: 0.7, want: 1},
		{in: 1, want: 1},
		{in: 1.3, want: 2},
		{in: 2, want: 2},
		{in: 3.1, want: 5},
		{in: 5, want: 5},
		{in: 5.1, want: 10},
		{in: 9, want: 10},
		{in: 12, want: 20},
		{in: 23, want: 25},
		{in: 61, want: 100},
		{in: 487, want: 500},
		{in: 900, want: 1000},
		{in: 1400, want: 2000},
	}
	for _, tc := range tests {
		if got := niceCeil(tc.in); got != tc.want {
			t.Errorf("niceCeil(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAxisLabel(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{in: 0, want: "0"},
		{in: 0.5, want: "500.0us"},
		{in: 1, want: "1.0ms"},
		{in: 250, want: "250.0ms"},
		{in: 2000, want: "2.00s"},
	}
	for _, tc := range tests {
		if got := axisLabel(tc.in); got != tc.want {
			t.Errorf("axisLabel(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderLatencyChart(t *testing.T) {
	var b strings.Builder
	renderLatencyChart(&b, latencyChart{
		Points: []chartPoint{
			{Rate: 100, Ms: 1.2, Pass: true},
			{Rate: 400, Ms: 1.4, Pass: true},
			{Rate: 800, Ms: 210, Pass: false},
		},
		LimitMs:    50,
		LimitText:  "p95 limit 50.0ms",
		MarkerAt:   400,
		MarkerText: "capacity",
	})
	got := b.String()

	for _, want := range []string{
		"<svg",
		"viewBox=",
		"<polyline",
		"circle class=\"pass\"",
		"circle class=\"fail\"",
		"class=\"limit\"",
		"class=\"marker\"",
		"p95 limit 50.0ms",
		"capacity",
		"iterations/s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("chart does not contain %q\n%s", want, got)
		}
	}
	// Two passing points and one failing point.
	if n := strings.Count(got, "circle class="); n != 3 {
		t.Errorf("chart has %d points, want 3", n)
	}
	// The polyline connects all three points, so it has two coordinate pairs.
	if n := strings.Count(got, ","); n < 4 {
		t.Errorf("polyline has too few coordinates:\n%s", got)
	}
	if strings.Contains(got, "<script") {
		t.Error("chart contains a script tag")
	}
}

// TestRenderLatencyChartSinglePoint: one probe has no line to draw.
func TestRenderLatencyChartSinglePoint(t *testing.T) {
	var b strings.Builder
	renderLatencyChart(&b, latencyChart{Points: []chartPoint{{Rate: 100, Ms: 1, Pass: true}}})
	got := b.String()

	if strings.Contains(got, "<polyline") {
		t.Errorf("a single point should not be drawn as a line:\n%s", got)
	}
	if !strings.Contains(got, "circle class=\"pass\"") {
		t.Errorf("the single point is missing:\n%s", got)
	}
}

func TestRenderLatencyChartEscapesLabels(t *testing.T) {
	var b strings.Builder
	renderLatencyChart(&b, latencyChart{
		Points:     []chartPoint{{Rate: 10, Ms: 1, Pass: true}},
		MarkerAt:   10,
		MarkerText: `<script>alert(1)</script>`,
	})
	if strings.Contains(b.String(), "<script>") {
		t.Errorf("chart did not escape its label:\n%s", b.String())
	}
}
