package search

import "sort"

// Knee returns the highest passing rate whose latency was still within multiple
// times the latency seen at the lowest passing rate.
//
// The multiple is a policy rather than a constant, because "still fine" is a
// judgement, and a tool that hides one is a tool nobody can argue with
// (PLAN-OUTLINE.md §24). The baseline is the lowest passing rate's reading rather
// than the lowest reading anywhere on the curve, so a dip partway up cannot move
// the line.
//
// ok is false when there is nothing to say: fewer than two passing probes with a
// reading, no latency at the baseline, or a curve that never rose past the
// multiple. The last is the ordinary case for a healthy system, and calling the
// highest measured rate a "knee" would dress up an absence as a finding.
func (o *Outcome) Knee(percentile, multiple float64) (int, bool) {
	if multiple <= 0 || percentile <= 0 {
		return 0, false
	}

	// A point is one passing probe's reading, kept only if it has the percentile
	// the policy asks about.
	type point struct {
		rate int
		ms   float64
	}
	var points []point
	for _, p := range o.Probes {
		if !p.Pass {
			continue
		}
		if v, has := p.Result.Metrics.At(percentile); has {
			points = append(points, point{rate: p.Rate, ms: v})
		}
	}
	if len(points) < 2 {
		return 0, false
	}

	// The probes are in the order they were taken, which stops being rate order
	// once the bisection begins.
	sort.Slice(points, func(i, j int) bool { return points[i].rate < points[j].rate })

	base := points[0].ms
	if base <= 0 {
		return 0, false
	}
	limit := base * multiple

	knee := 0
	for _, pt := range points {
		if pt.ms > limit {
			break
		}
		knee = pt.rate
	}
	// Never rose: that is where the measurement stopped, not a knee.
	if knee == 0 || knee == points[len(points)-1].rate {
		return 0, false
	}
	return knee, true
}

// KneeBaseline returns the rate and latency the knee is measured against: the
// lowest passing rate with a reading. It is what makes the knee's arithmetic
// checkable rather than a number out of nowhere.
func (o *Outcome) KneeBaseline(percentile float64) (rate int, ms float64, ok bool) {
	best := 0
	for _, p := range o.Probes {
		if !p.Pass {
			continue
		}
		v, has := p.Result.Metrics.At(percentile)
		if !has {
			continue
		}
		if best == 0 || p.Rate < best {
			best, rate, ms, ok = p.Rate, p.Rate, v, true
		}
	}
	return rate, ms, ok
}
