package report

import (
	"fmt"
	"math"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/search"
)

// operating is what a policy makes of a measured capacity (PLAN-OUTLINE.md §24).
//
// It carries both candidates rather than only the answer, so the report can show
// the arithmetic: a limit someone cannot check is a number they have to take on
// faith, which is what the outline warns against.
type operating struct {
	// Configured is whether the config asked for a policy at all. The block is
	// shown whenever it is set, including when it produces nothing: a configured
	// block that silently vanishes leaves the reader wondering whether it was read.
	Configured bool

	// Limit is the rate to run at, and zero when the policy asks for none.
	Limit int
	// Bound names what set the limit, for when the two disagree.
	Bound string

	// PolicyLimit is capacity × max_utilization_of_capacity, zero if unset.
	PolicyLimit int
	// Utilization is the configured fraction, for the report's arithmetic.
	Utilization float64
	// PolicySkip says why no policy limit was computed, when one was asked for.
	PolicySkip string

	// Knee is the latency knee, and HasKnee says whether one was found.
	Knee    int
	HasKnee bool
	// KneePercentile and KneeMultiple are the policy that found it.
	KneePercentile float64
	KneeMultiple   float64
	// BaseRate and BaseMs are what the knee was measured against.
	BaseRate int
	BaseMs   float64
}

// operatingLimit applies the policy to the outcome.
//
// The limit is the lower of the two candidates. Running at 80% of a capacity the
// system reached with latency already collapsed is not a recommendation worth
// making, so a knee below the policy ceiling wins.
func operatingLimit(out *search.Outcome, cfg *config.Config) operating {
	op := operating{
		Configured:  cfg.OperatingPolicy.KneePolicy(),
		Utilization: cfg.OperatingPolicy.MaxUtilizationOfCapacity,
	}

	if out == nil {
		return op
	}

	if u := cfg.OperatingPolicy.MaxUtilizationOfCapacity; u > 0 {
		switch {
		case out.Capacity <= 0:
			op.PolicySkip = "no capacity was found"
		case out.LowerBoundOnly:
			// A floor cannot bound anything from below.
			op.PolicySkip = "the capacity is only a lower bound"
		default:
			op.PolicyLimit = int(math.Round(float64(out.Capacity) * u))
		}
	}

	if cfg.OperatingPolicy.KneePolicy() {
		op.KneePercentile = cfg.OperatingPolicy.KneePercentileOr()
		op.KneeMultiple = cfg.OperatingPolicy.KneeMultipleOr()
		op.Knee, op.HasKnee = out.Knee(op.KneePercentile, op.KneeMultiple)
		if op.HasKnee {
			op.BaseRate, op.BaseMs, _ = out.KneeBaseline(op.KneePercentile)
		}
	}

	switch {
	case op.PolicyLimit > 0 && op.HasKnee && op.Knee < op.PolicyLimit:
		op.Limit, op.Bound = op.Knee, "the latency knee"
	case op.PolicyLimit > 0:
		op.Limit, op.Bound = op.PolicyLimit, "the policy limit"
	case op.HasKnee:
		op.Limit, op.Bound = op.Knee, "the latency knee"
	}
	return op
}

// facts renders the policy as report rows, or nothing when no policy is
// configured.
func (o operating) facts() []htmlFact {
	if !o.Configured {
		return nil
	}
	var out []htmlFact

	if o.HasKnee {
		out = append(out, htmlFact{
			Label: "Latency knee",
			Value: fmt.Sprintf("%s/s (%s within %g× the %s seen at %s/s)",
				commafy(int64(o.Knee)), formatP(o.KneePercentile), o.KneeMultiple,
				ms(o.BaseMs), commafy(int64(o.BaseRate))),
		})
	} else {
		out = append(out, htmlFact{
			Label: "Latency knee",
			Value: fmt.Sprintf("none found (no passing rate reached %g× the %s at the bottom)",
				o.KneeMultiple, formatP(o.KneePercentile)),
		})
	}

	switch {
	case o.PolicyLimit > 0:
		out = append(out, htmlFact{
			Label: "Policy limit",
			Value: fmt.Sprintf("%s/s (%g%% of measured capacity)",
				commafy(int64(o.PolicyLimit)), o.Utilization*100),
		})
	case o.PolicySkip != "":
		out = append(out, htmlFact{
			Label: "Policy limit", Value: "not computed: " + o.PolicySkip,
		})
	}
	if o.Limit > 0 {
		out = append(out, htmlFact{
			Label: "Operating range",
			Value: fmt.Sprintf("< %s/s, set by %s", commafy(int64(o.Limit)), o.Bound),
		})
	}
	return out
}

// render writes the policy block, or nothing when no policy is configured.
func (o operating) render(p *printer) {
	if !o.Configured {
		return
	}

	p.line("")
	if o.HasKnee {
		p.kv("Latency knee", fmt.Sprintf("%s/s  %s",
			commafy(int64(o.Knee)),
			p.paint(cDim, fmt.Sprintf("(%s still within %g× the %s seen at %s/s)",
				formatP(o.KneePercentile), o.KneeMultiple, ms(o.BaseMs),
				commafy(int64(o.BaseRate))))))
	} else {
		p.kv("Latency knee", p.paint(cDim, fmt.Sprintf(
			"none found (no passing rate reached %g× the %s at the bottom)",
			o.KneeMultiple, formatP(o.KneePercentile))))
	}
	switch {
	case o.PolicyLimit > 0:
		p.kv("Policy limit", fmt.Sprintf("%s/s  %s",
			commafy(int64(o.PolicyLimit)),
			p.paint(cDim, fmt.Sprintf("(%g%% of measured capacity)", o.Utilization*100))))
	case o.PolicySkip != "":
		p.kv("Policy limit", p.paint(cDim, "not computed: "+o.PolicySkip))
	}
	if o.Limit > 0 {
		p.kv("Operating range", p.paint(cBold, fmt.Sprintf("< %s/s", commafy(int64(o.Limit)))))
		p.line("    " + p.paint(cDim, "set by "+o.Bound))
	}
}
