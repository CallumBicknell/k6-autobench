package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/callum/k6-autobench/internal/metrics"
)

// classCount is one class/count pair for building a test fixture.
type classCount struct {
	class string
	count int
}

func c(class string, count int) classCount { return classCount{class, count} }

// classes builds a status breakdown with shares derived from the total.
func classes(pairs ...classCount) []metrics.StatusClass {
	total := 0
	for _, p := range pairs {
		total += p.count
	}
	out := make([]metrics.StatusClass, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, metrics.StatusClass{
			Class: p.class,
			Count: int64(p.count),
			Share: float64(p.count) / float64(total),
		})
	}
	return out
}

// TestStatusHintIsThePointOfTheBreakdown: an error rate alone cannot tell a
// workload mistake from a failing system, and the two need different actions.
func TestStatusHintIsThePointOfTheBreakdown(t *testing.T) {
	tests := []struct {
		name  string
		in    []metrics.StatusClass
		want  string
		empty bool
	}{
		{
			name:  "healthy run says nothing",
			in:    classes(c("2xx", 1000)),
			empty: true,
		},
		{
			name: "5xx blames the system",
			in:   classes(c("2xx", 250), c("5xx", 750)),
			want: "the system under test is failing",
		},
		{
			name: "4xx blames the workload",
			in:   classes(c("2xx", 250), c("4xx", 750)),
			want: "check the workload",
		},
		{
			// The old text claimed k6 does not follow redirects, which is not
			// true of any version this supports: both v0.49.0 and v2.1.0 follow
			// up to ten by default.
			name: "3xx explains redirects",
			in:   classes(c("2xx", 900), c("3xx", 100)),
			want: "follows",
		},
		{
			name: "other means transport failures",
			in:   classes(c("other", 50)),
			want: "transport-level failures",
		},
		{
			name: "the dominant class wins",
			in:   classes(c("2xx", 100), c("4xx", 100), c("5xx", 800)),
			want: "the system under test is failing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := statusHint(tc.in)
			if tc.empty {
				if got != "" {
					t.Errorf("hint = %q, want none", got)
				}
				return
			}
			// The wording wraps across lines in the source; compare on the
			// words that carry the meaning.
			flat := strings.ReplaceAll(got, "\n", " ")
			flat = strings.Join(strings.Fields(flat), " ")
			if !strings.Contains(flat, strings.Join(strings.Fields(tc.want), " ")) {
				t.Errorf("hint = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

func TestStatusBreakdownRendered(t *testing.T) {
	r := testResult()
	r.Metrics.StatusClasses = classes(c("2xx", 250), c("5xx", 750))

	var buf bytes.Buffer
	Render(&buf, testConfig(), r, "0.6.0", false)
	got := buf.String()

	for _, want := range []string{
		"2xx", "250", "25.000% of responses",
		"5xx", "750", "75.000% of responses",
		"the system under test is failing",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q\n%s", want, got)
		}
	}
}

// TestStatusBreakdownHiddenWhenHealthy: a run that only saw 2xx has nothing to
// say about response classes.
func TestStatusBreakdownHiddenWhenHealthy(t *testing.T) {
	r := testResult()
	r.Metrics.StatusClasses = classes(c("2xx", 1000))

	var buf bytes.Buffer
	Render(&buf, testConfig(), r, "0.6.0", false)
	got := buf.String()

	if strings.Contains(got, "of responses") {
		t.Errorf("a healthy run should not print a class breakdown:\n%s", got)
	}
}

func TestRenderHTMLStatusBreakdown(t *testing.T) {
	r := testResult()
	r.Metrics.StatusClasses = classes(c("2xx", 250), c("5xx", 750))

	var buf bytes.Buffer
	if err := RenderHTML(&buf, testConfig(), r, "0.6.0"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{"Responses", "5xx", "the system under test is failing"} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	assertSelfContained(t, got)
}
