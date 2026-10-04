package collect

import (
	"encoding/json"
	"math"
	"testing"
)

// TestSeriesSummary covers the figures the report leads with: min, max, average
// and, crucially, when the max happened, which is what lets a reader line a
// resource spike up against a latency window.
func TestSeriesSummary(t *testing.T) {
	s := Series{
		Source: "api",
		Name:   "cpu",
		Samples: []Sample{
			{OffsetMs: 0, Value: 0.4},
			{OffsetMs: 5000, Value: 0.9},
			{OffsetMs: 10000, Value: 0.5},
		},
	}

	st, ok := s.Summary()
	if !ok {
		t.Fatal("Summary reported no finite samples")
	}
	if st.Min != 0.4 || st.Max != 0.9 {
		t.Errorf("min/max = %v/%v, want 0.4/0.9", st.Min, st.Max)
	}
	if math.Abs(st.Avg-0.6) > 1e-9 {
		t.Errorf("avg = %v, want 0.6", st.Avg)
	}
	if st.Last != 0.5 {
		t.Errorf("last = %v, want 0.5", st.Last)
	}
	if st.MaxAtMs != 5000 {
		t.Errorf("maxAt = %vms, want 5000ms", st.MaxAtMs)
	}
	if st.Count != 3 {
		t.Errorf("count = %d, want 3", st.Count)
	}
}

// TestSeriesSummarySkipsNonFinite keeps a NaN (a counter before its first
// sample) or an Inf (a division by zero) from poisoning every figure.
func TestSeriesSummarySkipsNonFinite(t *testing.T) {
	s := Series{Samples: []Sample{
		{OffsetMs: 0, Value: math.NaN()},
		{OffsetMs: 1000, Value: 2},
		{OffsetMs: 2000, Value: math.Inf(1)},
		{OffsetMs: 3000, Value: 4},
	}}

	st, ok := s.Summary()
	if !ok {
		t.Fatal("Summary reported no finite samples")
	}
	if st.Count != 2 || st.Min != 2 || st.Max != 4 || st.Avg != 3 {
		t.Errorf("stats = %+v, want the two finite samples summarised", st)
	}
}

// TestSeriesSummaryOfNothing reports failure rather than a zero reading, so the
// report can say "no data" instead of claiming everything was zero.
func TestSeriesSummaryOfNothing(t *testing.T) {
	if _, ok := (Series{}).Summary(); ok {
		t.Error("Summary of an empty series should report no data")
	}
	if _, ok := (Series{Samples: []Sample{{Value: math.NaN()}}}).Summary(); ok {
		t.Error("Summary of only non-finite samples should report no data")
	}
}

// TestReportMerge folds several collectors into one report, which is how the
// runner presents a run with more than one configured source.
func TestReportMerge(t *testing.T) {
	var r Report
	r.Merge(Report{
		Series:   []Series{{Source: "api", Name: "cpu"}},
		Failures: []Failure{{Source: "api", Error: "boom"}},
	})
	r.Merge(Report{Series: []Series{{Source: "postgres", Name: "connections"}}})

	if len(r.Series) != 2 {
		t.Fatalf("series = %v, want both sources", seriesNames(r))
	}
	if len(r.Failures) != 1 {
		t.Errorf("failures = %v, want the one from the first report", r.Failures)
	}
	if r.IsZero() {
		t.Error("IsZero = true for a report holding series")
	}
	if !(Report{}).IsZero() {
		t.Error("IsZero = false for an empty report")
	}
}

// TestReportIsOmittedWhenEmpty keeps a run with no collectors configured from
// carrying an empty object through the JSON report.
func TestReportIsOmittedWhenEmpty(t *testing.T) {
	type wrapper struct {
		System Report `json:"system,omitzero"`
	}

	b, err := json.Marshal(wrapper{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("json = %s, want {}", b)
	}

	b, err = json.Marshal(wrapper{System: Report{Series: []Series{{Name: "cpu"}}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) == "{}" {
		t.Error("a report holding a series must not be omitted")
	}
}

// TestPromPairAcceptsDocumentedAndReencodedValues pins the wire format: the
// documented API quotes sample values, but a bare number is accepted too.
func TestPromPairAcceptsDocumentedAndReencodedValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want float64
	}{
		{"documented quoted value", `[1435781451.781,"0.42"]`, 0.42},
		{"bare number", `[1435781451.781,0.42]`, 0.42},
		{"quoted integer", `[1,"3"]`, 3},
		{"quoted NaN", `[1,"NaN"]`, math.NaN()},
		{"quoted +Inf", `[1,"+Inf"]`, math.Inf(1)},
		{"quoted -Inf", `[1,"-Inf"]`, math.Inf(-1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p promPair
			if err := json.Unmarshal([]byte(tc.raw), &p); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.raw, err)
			}
			switch {
			case math.IsNaN(tc.want):
				if !math.IsNaN(p.Value) {
					t.Errorf("value = %v, want NaN", p.Value)
				}
			case p.Value != tc.want:
				t.Errorf("value = %v, want %v", p.Value, tc.want)
			}
		})
	}
}

func TestPromPairRejectsMalformedValues(t *testing.T) {
	for _, raw := range []string{
		`[]`,
		`[1]`,
		`[1,2,3]`,
		`"nope"`,
		`[1,"not a number"]`,
		`["nope","1"]`,
	} {
		var p promPair
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Errorf("unmarshal %s should have failed", raw)
		}
	}
}

// TestPrometheusRejectsAnInvalidURL makes sure a bad URL is a configuration
// mistake, not something discovered mid-run.
func TestPrometheusRejectsAnInvalidURL(t *testing.T) {
	if _, err := promEndpoint("not a url at all", "up"); err == nil {
		t.Error("promEndpoint should reject a URL with no scheme and host")
	}
	if _, err := promEndpoint("localhost:9090", "up"); err == nil {
		t.Error("promEndpoint should reject a URL with no scheme")
	}
}
