package report

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"

	"github.com/CallumBicknell/k6-autobench/internal/collect"
	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
)

// minCorrelationWindows is how many windows a series must share with a latency
// reading before it can be compared at all.
//
// Five is not a round number picked for comfort: with fewer than five pairings
// even a perfect relationship cannot reach the significance the permutation test
// below demands, so a smaller sample could only ever produce silence.
const minCorrelationWindows = 5

// correlationAlpha is the chance of a false positive this report is willing to
// live with across the whole table, not per series.
const correlationAlpha = 0.05

// correlationIterations is how many shuffles the permutation test runs. It sets
// the smallest p-value that can be reported at 1/(iterations+1).
const correlationIterations = 2000

// correlationRow is one series' co-movement with latency.
type correlationRow struct {
	Series  string
	R       float64
	Windows int
}

// candidate is a series that could be compared, with its paired readings kept
// so the significance test does not have to recompute them.
type candidate struct {
	series    collect.Series
	resources []float64
	latencies []float64
	r         float64
}

// correlateRows finds the collected series that moved with latency across the
// measurement's windows, most strongly first.
//
// This is the closest the data can honestly get to "here is the bottleneck"
// (PLAN-OUTLINE.md §33). It is deliberately hard to appear in:
//
//   - A series needs at least minCorrelationWindows windows that hold both a
//     latency reading and one of its own, because a comparison across three
//     points says nothing.
//   - It has to survive a permutation test, so a relationship that a handful of
//     windows could have produced by chance is not reported.
//   - The comparison happens *within* one measurement, where the arrival rate is
//     constant. Across rates, every resource rises with load and would correlate
//     with latency for that reason alone.
//
// Correlation is still not cause, and the section says so. What it can honestly
// offer is a shortlist: if a resource tracks latency and nothing else does, it is
// worth a look.
func correlateRows(ws []metrics.Window, sys collect.Report, cfg *config.Config) []correlationRow {
	pct, ok := featuredPercentile(cfg.Percentiles)
	if !ok || len(ws) < minCorrelationWindows || len(sys.Series) == 0 {
		return nil
	}

	var candidates []candidate
	for _, s := range sys.Series {
		resources, latencies := pairWithLatency(ws, s, pct)
		if len(resources) < minCorrelationWindows {
			continue
		}
		r, ok := spearman(resources, latencies)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{series: s, resources: resources, latencies: latencies, r: r})
	}
	if len(candidates) == 0 {
		return nil
	}

	// Comparing a dozen resources is a dozen chances of a coincidence, so the
	// bar rises with how many were compared. This is what stops "likely
	// correlated" from quietly meaning "one of these was bound to look like it".
	//
	// The cost is that a handful of windows carries little power: with six
	// pairings even a perfect relationship only just clears the bar, and a wide
	// config will show nothing. That is the honest answer for six data points,
	// and narrowing analysis.window buys more of them.
	alpha := correlationAlpha / float64(len(candidates))

	var rows []correlationRow
	for _, c := range candidates {
		if permutationP(c.resources, c.latencies, c.r) >= alpha {
			continue
		}
		rows = append(rows, correlationRow{Series: seriesLabel(c.series), R: c.r, Windows: len(c.resources)})
	}

	// Strongest co-movement first, in either direction: a resource that fell as
	// latency rose -- a cache hit rate, say -- is as worth seeing as one that
	// climbed.
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := math.Abs(rows[i].R), math.Abs(rows[j].R)
		if a != b {
			return a > b
		}
		return rows[i].Series < rows[j].Series
	})
	return rows
}

// pairWithLatency pairs each window's latency with the average of the series'
// readings inside that window.
//
// A window holding no traffic, no latency reading, or no resource reading drops
// out: a gap in either series is not a data point, and padding it would invent
// one.
func pairWithLatency(ws []metrics.Window, s collect.Series, p float64) (resources, latencies []float64) {
	for _, w := range ws {
		if w.Requests == 0 {
			continue
		}
		lat, ok := windowPercentile(w, p)
		if !ok {
			continue
		}
		values := samplesIn(w, s)
		if len(values) == 0 {
			continue
		}
		resources = append(resources, mean(values))
		latencies = append(latencies, lat)
	}
	return resources, latencies
}

// samplesIn returns the series' finite readings that fall inside a window. Both
// timelines start at the measurement, which is why collector samples carry an
// offset rather than a timestamp.
func samplesIn(w metrics.Window, s collect.Series) []float64 {
	var out []float64
	for _, sm := range s.Samples {
		if sm.OffsetMs < w.StartMs || sm.OffsetMs >= w.EndMs {
			continue
		}
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		out = append(out, sm.Value)
	}
	return out
}

func mean(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// spearman is the rank correlation of two series: the Pearson correlation of
// their ranks.
//
// Ranks rather than raw values, because a resource that climbs 20% and a latency
// that triples are still moving together, and Pearson on the raw numbers would
// see only the difference in magnitude. It also survives the outlier that a
// single bad window would otherwise contribute.
//
// It reports false when either series never moved: a constant has no rank
// ordering, and so no relationship with anything.
func spearman(xs, ys []float64) (float64, bool) {
	if len(xs) != len(ys) || len(xs) < 2 {
		return 0, false
	}
	return pearson(ranks(xs), ranks(ys))
}

// ranks replaces each value with its rank, ties sharing the mean of the ranks
// they span. Ties matter: a connection pool pinned at its ceiling produces a run
// of identical readings, and giving them arbitrary distinct ranks would
// manufacture a trend.
func ranks(vs []float64) []float64 {
	order := make([]int, len(vs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return vs[order[a]] < vs[order[b]] })

	out := make([]float64, len(vs))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && vs[order[j+1]] == vs[order[i]] {
			j++
		}
		shared := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[order[k]] = shared
		}
		i = j + 1
	}
	return out
}

// pearson is the product-moment correlation of two equally long series. It
// reports false when either is constant, because the correlation is undefined
// rather than zero.
func pearson(xs, ys []float64) (float64, bool) {
	if len(xs) != len(ys) || len(xs) < 2 {
		return 0, false
	}
	n := float64(len(xs))

	var sumX, sumY float64
	for i := range xs {
		sumX += xs[i]
		sumY += ys[i]
	}
	meanX, meanY := sumX/n, sumY/n

	var covariance, varX, varY float64
	for i := range xs {
		dx, dy := xs[i]-meanX, ys[i]-meanY
		covariance += dx * dy
		varX += dx * dx
		varY += dy * dy
	}
	if varX == 0 || varY == 0 {
		return 0, false
	}
	return covariance / math.Sqrt(varX*varY), true
}

// permutationP estimates how often a correlation at least as strong as the
// observed one turns up when the latency readings are shuffled.
//
// Shuffling rather than a formula, because a handful of windows is exactly the
// case where the usual normal approximation is at its worst, and because it
// assumes nothing about the shape of either series. It also handles ties in the
// resource readings, which the textbook tables for rank correlation do not.
//
// The seed is derived from the data, so the same measurement always reports the
// same figure and a report stays reproducible.
func permutationP(resources, latencies []float64, observed float64) float64 {
	rng := rand.New(rand.NewSource(seedFrom(resources, latencies)))
	shuffled := make([]float64, len(latencies))

	// The observed pairing counts itself, so a p-value of exactly zero -- an
	// impossible certainty -- can never be reported.
	atLeast := 1
	for i := 0; i < correlationIterations; i++ {
		copy(shuffled, latencies)
		rng.Shuffle(len(shuffled), func(a, b int) {
			shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
		})
		if r, ok := spearman(resources, shuffled); ok && math.Abs(r) >= math.Abs(observed) {
			atLeast++
		}
	}
	return float64(atLeast) / float64(correlationIterations+1)
}

// seedFrom derives a stable seed from the data itself, so the report needs no
// global random source and two runs over the same measurement agree.
func seedFrom(resources, latencies []float64) int64 {
	h := fnv.New64a()
	var buf [8]byte
	for _, series := range [][]float64{resources, latencies} {
		for _, v := range series {
			binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
			h.Write(buf[:])
		}
	}
	return int64(h.Sum64())
}
