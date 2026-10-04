package compare

import (
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	"github.com/CallumBicknell/k6-autobench/internal/results"
)

// snap builds a snapshot with just the fields a comparison reads.
func snap(kind, target string, rate, capacity int, p95, p99, errorRate float64) *Snapshot {
	return &Snapshot{
		Path:     kind + "-" + target,
		Kind:     kind,
		Name:     "demo",
		Target:   target,
		Verdict:  results.VerdictPass,
		Rate:     rate,
		Capacity: capacity,
		Metrics: metrics.Metrics{
			HasLatency:  true,
			AvgMs:       p95 / 2,
			ErrorRate:   errorRate,
			Percentiles: []metrics.Percentile{{P: 95, Val: p95}, {P: 99, Val: p99}},
		},
	}
}

// fixedSnap is a fixed-rate run at a rate.
func fixedSnap(target string, rate int, p95, p99, errorRate float64) *Snapshot {
	return snap(KindFixed, target, rate, 0, p95, p99, errorRate)
}

// capacitySnap is a capacity search that found a capacity.
func capacitySnap(target string, capacity int, p95, p99, errorRate float64) *Snapshot {
	return snap(KindCapacity, target, capacity, capacity, p95, p99, errorRate)
}

func findChange(t *testing.T, changes []Change, metric string) Change {
	t.Helper()
	for _, c := range changes {
		if c.Metric == metric {
			return c
		}
	}
	t.Fatalf("no change for %q in %v", metric, changes)
	return Change{}
}

// TestCompareReportsLatencyRegression is the shape the outline sketches: a
// latency rise is a regression, and it says by how much.
func TestCompareReportsLatencyRegression(t *testing.T) {
	before := fixedSnap("http://a", 500, 81, 100, 0)
	after := fixedSnap("http://a", 500, 127, 100, 0)

	rep := Compare(before, after, DefaultConfig())

	p95 := findChange(t, rep.Latency, "p95")
	if p95.Before != 81 || p95.After != 127 {
		t.Errorf("p95 = %v -> %v, want 81 -> 127", p95.Before, p95.After)
	}
	if !p95.PercentMeaningful || math.Abs(p95.Percent-(127.0-81)/81) > 1e-9 {
		t.Errorf("p95 percent = %v, want +56.8%%", p95.Percent)
	}
	if !p95.Worse || !p95.Regression {
		t.Errorf("p95 = %+v, want a regression: 56.8%% is well past the 10%% tolerance", p95)
	}
	if !rep.Regressed() {
		t.Errorf("verdict = %q, want %q", rep.Verdict, VerdictRegression)
	}
}

// TestCompareToleratesNoise is what makes the gate usable: a change inside the
// tolerance is reported but does not fail the build.
func TestCompareToleratesNoise(t *testing.T) {
	before := fixedSnap("http://a", 500, 100, 100, 0)
	after := fixedSnap("http://a", 500, 105, 100, 0) // +5%, tolerance is 10%

	rep := Compare(before, after, DefaultConfig())

	p95 := findChange(t, rep.Latency, "p95")
	if !p95.Worse {
		t.Error("a 5% rise is worse even when it is within tolerance")
	}
	if p95.Regression {
		t.Errorf("p95 = %+v, want within tolerance", p95)
	}
	if rep.Regressed() {
		t.Errorf("verdict = %q, want the run's own PASS", rep.Verdict)
	}
}

// TestCompareReportsAnImprovement checks a latency drop is not a regression,
// however large.
func TestCompareReportsAnImprovement(t *testing.T) {
	before := fixedSnap("http://a", 500, 143, 100, 0)
	after := fixedSnap("http://a", 500, 81, 100, 0)

	rep := Compare(before, after, DefaultConfig())

	p95 := findChange(t, rep.Latency, "p95")
	if p95.Worse || p95.Regression {
		t.Errorf("p95 = %+v, want an improvement", p95)
	}
	if p95.Percent >= 0 {
		t.Errorf("p95 percent = %v, want a fall", p95.Percent)
	}
	if rep.Regressed() {
		t.Error("an improvement is not a regression")
	}
}

// TestCompareDirectionOfCapacity: capacity is the one metric where down is bad,
// and getting that backwards would report a collapse as good news.
func TestCompareDirectionOfCapacity(t *testing.T) {
	tests := []struct {
		name      string
		before    int
		after     int
		worse     bool
		regressed bool
	}{
		{"a drop past the tolerance", 13500, 11900, true, true}, // -11.9%, tolerance 5%
		{"a drop inside it", 13500, 13300, true, false},         // -1.5%
		{"a rise", 10200, 13500, false, false},                  // +32%
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := Compare(
				capacitySnap("http://a", tc.before, 50, 100, 0),
				capacitySnap("http://a", tc.after, 50, 100, 0),
				DefaultConfig(),
			)
			if rep.Capacity == nil {
				t.Fatal("capacity should be compared when both runs are searches")
			}
			if rep.Capacity.Worse != tc.worse {
				t.Errorf("worse = %v, want %v", rep.Capacity.Worse, tc.worse)
			}
			if rep.Capacity.Regression != tc.regressed {
				t.Errorf("regression = %v, want %v", rep.Capacity.Regression, tc.regressed)
			}
			if rep.Regressed() != tc.regressed {
				t.Errorf("verdict = %q, want regression=%v", rep.Verdict, tc.regressed)
			}
		})
	}
}

// TestCompareCapacityOnlyBetweenSearches: one measurement is not a capacity, so
// comparing a fixed run to a search on that axis would be a category error.
func TestCompareCapacityOnlyBetweenSearches(t *testing.T) {
	rep := Compare(
		fixedSnap("http://a", 500, 50, 100, 0),
		capacitySnap("http://a", 4500, 50, 100, 0),
		DefaultConfig(),
	)
	if rep.Capacity != nil {
		t.Errorf("capacity = %+v, want none when one side is a fixed run", rep.Capacity)
	}
	if len(rep.Notes) == 0 {
		t.Error("a mixed comparison should say so")
	}
}

// TestCompareCapacityFromZeroIsNotCompared: a search that found no passing rate
// has a capacity of zero, and a percentage against it would read as a total
// collapse rather than as an absence.
func TestCompareCapacityFromZeroIsNotCompared(t *testing.T) {
	rep := Compare(
		capacitySnap("http://a", 0, 50, 100, 0),
		capacitySnap("http://a", 4500, 50, 100, 0),
		DefaultConfig(),
	)
	if rep.Capacity != nil {
		t.Errorf("capacity = %+v, want none when one search found nothing", rep.Capacity)
	}
}

// TestCompareErrorRateIsAbsolute: a doubling of a tiny error rate is not a
// crisis, and a percentage would make it look like one.
func TestCompareErrorRateIsAbsolute(t *testing.T) {
	before := fixedSnap("http://a", 500, 50, 100, 0.001)
	after := fixedSnap("http://a", 500, 50, 100, 0.002)

	rep := Compare(before, after, DefaultConfig())
	if rep.Errors.PercentMeaningful {
		t.Error("an error rate change is reported in absolute terms, not a percentage")
	}
	if rep.Errors.Regression {
		t.Errorf("errors = %+v, want 0.1pp within the 0.1pp tolerance", rep.Errors)
	}

	after.Metrics.ErrorRate = 0.01
	rep = Compare(before, after, DefaultConfig())
	if !rep.Errors.Regression {
		t.Errorf("errors = %+v, want 0.9pp past the tolerance to fail", rep.Errors)
	}
}

// TestComparePercentilesOnlyComparesWhatBothHave: a percentile one run did not
// collect has nothing to compare against, and inventing a zero would report a
// spectacular improvement.
func TestComparePercentilesOnlyComparesWhatBothHave(t *testing.T) {
	before := fixedSnap("http://a", 500, 50, 100, 0)
	after := fixedSnap("http://a", 500, 50, 100, 0)
	after.Metrics.Percentiles = []metrics.Percentile{{P: 95, Val: 50}} // no p99

	rep := Compare(before, after, DefaultConfig())
	if len(rep.Latency) != 1 || rep.Latency[0].Metric != "p95" {
		t.Errorf("latency = %+v, want p95 alone", rep.Latency)
	}
}

// endpointSnap is a fixed-rate run with a per-request breakdown.
func endpointSnap(p95 map[string]float64) *Snapshot {
	s := fixedSnap("http://a", 500, 50, 100, 0)
	s.Metrics.Endpoints = nil
	for _, name := range sortedKeys(p95) {
		s.Metrics.Endpoints = append(s.Metrics.Endpoints, metrics.Endpoint{
			Name: name, HasLatency: true, AvgMs: p95[name] / 2,
			Percentiles: []metrics.Percentile{{P: 95, Val: p95[name]}},
		})
	}
	return s
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCompareEndpointsLeadsWithTheRegression keeps the request to look at first,
// which is the whole point of a per-request breakdown.
func TestCompareEndpointsLeadsWithTheRegression(t *testing.T) {
	before := endpointSnap(map[string]float64{"fast": 10, "slow": 20, "steady": 30})
	after := endpointSnap(map[string]float64{"fast": 10, "slow": 60, "steady": 31})

	rep := Compare(before, after, DefaultConfig())
	if len(rep.Endpoints) != 3 {
		t.Fatalf("endpoints = %+v, want three", rep.Endpoints)
	}
	if rep.Endpoints[0].Name != "slow" {
		t.Errorf("first endpoint = %q, want the one that regressed", rep.Endpoints[0].Name)
	}
	if !rep.Endpoints[0].Regression {
		t.Errorf("slow = %+v, want a regression: 20ms to 60ms is +200%%", rep.Endpoints[0])
	}
	for _, e := range rep.Endpoints[1:] {
		if e.Regression {
			t.Errorf("%s = %+v, want within tolerance", e.Name, e)
		}
	}
	if !rep.Regressed() {
		t.Error("an endpoint regression should fail the comparison")
	}
}

// TestCompareEndpointsMarksWorkloadChanges: a request only one run exercised is
// a change in the workload, not a regression in it, and must not fail the build.
func TestCompareEndpointsMarksWorkloadChanges(t *testing.T) {
	before := endpointSnap(map[string]float64{"kept": 10, "gone": 20})
	after := endpointSnap(map[string]float64{"kept": 10, "new": 20})

	rep := Compare(before, after, DefaultConfig())

	byName := map[string]EndpointChange{}
	for _, e := range rep.Endpoints {
		byName[e.Name] = e
	}
	if !byName["new"].Added {
		t.Errorf("new = %+v, want it marked added", byName["new"])
	}
	if !byName["gone"].Removed {
		t.Errorf("gone = %+v, want it marked removed", byName["gone"])
	}
	if byName["new"].Regression || byName["gone"].Regression {
		t.Error("a workload change is not a regression")
	}
	if rep.Regressed() {
		t.Error("adding and removing a request should not fail the comparison")
	}
}

// TestComparisonNotesWarnAboutMismatches: a comparison of two different things is
// worse than no comparison, because it looks like an answer.
func TestComparisonNotesWarnAboutMismatches(t *testing.T) {
	tests := []struct {
		name   string
		before *Snapshot
		after  *Snapshot
		want   string
	}{
		{
			name:   "different targets",
			before: fixedSnap("http://a", 500, 50, 100, 0),
			after:  fixedSnap("http://b", 500, 50, 100, 0),
			want:   "different targets",
		},
		{
			name:   "different rates",
			before: fixedSnap("http://a", 500, 50, 100, 0),
			after:  fixedSnap("http://a", 5000, 50, 100, 0),
			want:   "different rates",
		},
		{
			name:   "different kinds",
			before: fixedSnap("http://a", 500, 50, 100, 0),
			after:  capacitySnap("http://a", 500, 50, 100, 0),
			want:   "one run is a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := Compare(tc.before, tc.after, DefaultConfig())
			joined := strings.Join(rep.Notes, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("notes = %q, want them to mention %q", joined, tc.want)
			}
		})
	}
}

// TestComparisonNotesWarnAboutAGeneratorSaturatedRun keeps a comparison from
// quoting numbers that describe the generator rather than the target.
func TestComparisonNotesWarnAboutAGeneratorSaturatedRun(t *testing.T) {
	before := fixedSnap("http://a", 500, 50, 100, 0)
	after := fixedSnap("http://a", 500, 50, 100, 0)
	after.GeneratorSaturated = true

	rep := Compare(before, after, DefaultConfig())
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "saturated the load generator") {
		t.Errorf("notes = %v, want the saturated run called out", rep.Notes)
	}
}

// TestComparisonNotesAreQuietWhenNothingIsWrong: a like-for-like comparison must
// not bury the result in caveats.
func TestComparisonNotesAreQuietWhenNothingIsWrong(t *testing.T) {
	before := fixedSnap("http://a", 500, 50, 100, 0)
	after := fixedSnap("http://a", 500, 50, 100, 0)

	if notes := Compare(before, after, DefaultConfig()).Notes; len(notes) != 0 {
		t.Errorf("notes = %v, want none for a like-for-like comparison", notes)
	}
}
