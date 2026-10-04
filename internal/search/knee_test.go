package search

import (
	"testing"

	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
)

// kneeOutcome builds an outcome from (rate, p99) pairs, all passing.
func kneeOutcome(pairs ...[2]float64) *Outcome {
	var out Outcome
	for _, p := range pairs {
		out.Probes = append(out.Probes, Probe{
			Rate: int(p[0]), Pass: true,
			Result: results.Result{Metrics: metrics.Metrics{
				HasLatency:  true,
				Percentiles: []metrics.Percentile{{P: 99, Val: p[1]}},
			}},
		})
	}
	return &out
}

// TestKneeFindsWhereTheCurveLeavesTheFloor uses the curve from
// PLAN-OUTLINE.md §23: flat, then a knee, then a cliff.
func TestKneeFindsWhereTheCurveLeavesTheFloor(t *testing.T) {
	out := kneeOutcome(
		[2]float64{2000, 20}, [2]float64{4000, 23}, [2]float64{6000, 25},
		[2]float64{8000, 31}, [2]float64{10000, 40}, [2]float64{12000, 91},
		[2]float64{14000, 420}, [2]float64{15000, 1800},
	)

	rate, ok := out.Knee(99, 2)
	if !ok {
		t.Fatal("Knee found nothing, want a knee")
	}
	// 40ms at 10k is exactly 2x the 20ms floor, so 10k is still within. The next
	// rate is 91ms, which is not, and the cliff beyond it is why the outline
	// cares: by 12k the system is already 4.5x slower than at rest.
	if rate != 10000 {
		t.Errorf("knee = %d/s, want 10000/s", rate)
	}

	// A looser policy sees the knee later, which is the point of configuring it.
	if rate, _ := out.Knee(99, 5); rate != 12000 {
		t.Errorf("knee at 5x = %d/s, want 12000/s", rate)
	}
}

// TestKneeIsMeasuredAgainstTheLowestRateNotTheLowestReading: a dip partway up the
// curve must not move the line.
//
// The floor is 50ms at 1000/s, so the limit is 100ms. If the code took the lowest
// reading anywhere (10ms at 2000/s) instead, the limit would be 20ms and the
// first probe would already breach it -- so this distinguishes the two.
func TestKneeIsMeasuredAgainstTheLowestRateNotTheLowestReading(t *testing.T) {
	out := kneeOutcome(
		[2]float64{1000, 50},  // the floor, and not the lowest reading on the curve
		[2]float64{2000, 10},  // a dip
		[2]float64{3000, 150}, // past 2x the floor
	)

	rate, ok := out.Knee(99, 2)
	if !ok {
		t.Fatal("Knee found nothing, want a knee")
	}
	if rate != 2000 {
		t.Errorf("knee = %d/s, want 2000/s: the baseline is the lowest rate's reading", rate)
	}
}

// TestKneeOrdersByRate: probes are recorded in the order they were taken, which
// stops being rate order once the bisection begins.
func TestKneeOrdersByRate(t *testing.T) {
	out := kneeOutcome(
		[2]float64{8000, 20},
		[2]float64{2000, 20},   // taken first, lowest rate
		[2]float64{16000, 900}, // taken in the bisection, above the multiple
		[2]float64{4000, 21},
	)

	rate, ok := out.Knee(99, 2)
	if !ok {
		t.Fatal("Knee found nothing, want a knee")
	}
	if rate != 8000 {
		t.Errorf("knee = %d/s, want 8000/s: the probes have to be read in rate order", rate)
	}
}

// TestKneeSaysNothingWhenThereIsNothingToSay keeps an absence from being dressed
// up as a finding.
func TestKneeSaysNothingWhenThereIsNothingToSay(t *testing.T) {
	tests := []struct {
		name       string
		out        *Outcome
		percentile float64
		multiple   float64
	}{
		{"no probes", kneeOutcome(), 99, 2},
		{"one probe", kneeOutcome([2]float64{1000, 10}), 99, 2},
		{"a flat curve", kneeOutcome(
			[2]float64{1000, 10}, [2]float64{2000, 10}, [2]float64{3000, 11}), 99, 2},
		{"never past the multiple", kneeOutcome(
			[2]float64{1000, 10}, [2]float64{2000, 15}, [2]float64{3000, 19}), 99, 2},
		{"the percentile was not collected", kneeOutcome(
			[2]float64{1000, 10}, [2]float64{2000, 900}), 50, 2},
		{"a zero baseline", kneeOutcome(
			[2]float64{1000, 0}, [2]float64{2000, 900}), 99, 2},
		{"no multiple", kneeOutcome(
			[2]float64{1000, 10}, [2]float64{2000, 900}), 99, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rate, ok := tc.out.Knee(tc.percentile, tc.multiple)
			if ok {
				t.Errorf("Knee = %d, true; want false: %s", rate, tc.name)
			}
		})
	}
}

// TestKneeSkipsFailingProbes: a rate that breached the SLOs is not part of the
// passing curve.
func TestKneeSkipsFailingProbes(t *testing.T) {
	out := kneeOutcome(
		[2]float64{1000, 10}, [2]float64{2000, 12}, [2]float64{4000, 900},
	)
	out.Probes[2].Pass = false // 4000/s failed

	rate, ok := out.Knee(99, 2)
	if ok {
		t.Errorf("knee = %d, true; want false: the only probe past the multiple failed", rate)
	}
}

// TestKneeBaseline reports what the knee was measured against, so the arithmetic
// in the report can be checked.
func TestKneeBaseline(t *testing.T) {
	out := kneeOutcome([2]float64{5000, 20}, [2]float64{1000, 4}, [2]float64{9000, 900})

	rate, ms, ok := out.KneeBaseline(99)
	if !ok {
		t.Fatal("KneeBaseline found nothing")
	}
	if rate != 1000 || ms != 4 {
		t.Errorf("baseline = %d/s at %vms, want 1000/s at 4ms", rate, ms)
	}
}
