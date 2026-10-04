package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/CallumBicknell/k6-autobench/internal/compare"
)

// RenderCompare writes the terminal comparison report (PLAN-OUTLINE.md §35).
//
// It leads with the verdict, because that is the one thing a pipeline needs, and
// then shows the numbers behind it. A comparison is a gate rather than a
// measurement, so there is no chart and no per-window detail: the question is
// "did this get worse?", and everything here answers it.
func RenderCompare(w io.Writer, rep *compare.Report, version string, color bool) {
	p := &printer{w: w, color: color}

	p.header("AUTOBENCH v"+version, "regression comparison")

	p.line("")
	p.kv("Baseline", rep.Before.Path)
	p.kv("Candidate", rep.After.Path)
	p.kv("Tolerance", fmt.Sprintf("latency %s, capacity %s, errors %s",
		pctTrimmed(rep.Config.Latency), pctTrimmed(rep.Config.Capacity), pctTrimmed(rep.Config.Errors)))

	p.line("")
	p.separator()
	p.line("")

	if rep.Capacity != nil {
		p.line("  " + p.paint(cBold, "CAPACITY"))
		p.line("")
		p.changeRows([]compare.Change{*rep.Capacity}, "iterations/s")
		p.line("")
	}

	p.line("  " + p.paint(cBold, "LATENCY"))
	p.line("")
	p.changeRows(rep.Latency, "ms")
	p.line("")

	p.line("  " + p.paint(cBold, "ERRORS"))
	p.line("")
	p.changeRows([]compare.Change{rep.Errors}, "ratio")

	if len(rep.Endpoints) > 0 {
		p.line("")
		p.line("  " + p.paint(cBold, "REQUESTS"))
		p.line("")
		p.endpointChanges(rep.Endpoints)
	}

	p.line("")
	p.separator()
	p.compareVerdict(rep)

	p.line("")
	p.line(p.paint(cDim, "  a single run against a single run is a noisy measurement;"))
	p.line(p.paint(cDim, "  narrow the tolerance only once you know your noise floor"))
}

// changeRows prints one metric's before, after and change.
func (p *printer) changeRows(changes []compare.Change, unit string) {
	if len(changes) == 0 {
		p.line("  " + p.paint(cDim, "nothing comparable was recorded by both runs"))
		return
	}

	p.line("  " + p.paint(cBold, fmt.Sprintf("%-14s%12s%12s%12s   %s",
		"METRIC", "BASELINE", "CANDIDATE", "CHANGE", "")))
	for _, c := range changes {
		p.line(fmt.Sprintf("  %-14s%12s%12s%12s   %s",
			c.Metric,
			formatChangeValue(c.Before, unit),
			formatChangeValue(c.After, unit),
			formatChange(c),
			p.changeMark(c),
		))
	}
}

// endpointChanges prints the per-request comparison, worst first.
func (p *printer) endpointChanges(changes []compare.EndpointChange) {
	p.line("  " + p.paint(cBold, fmt.Sprintf("%-20s%14s%14s%12s   %s",
		"REQUEST", "BASELINE p95", "CANDIDATE p95", "CHANGE", "")))
	for _, e := range changes {
		switch {
		case e.Added:
			p.line(fmt.Sprintf("  %-20s%14s%14s%12s   %s",
				truncate(e.Name, 20), "-", "-", "-", p.paint(cDim, "added")))
		case e.Removed:
			p.line(fmt.Sprintf("  %-20s%14s%14s%12s   %s",
				truncate(e.Name, 20), "-", "-", "-", p.paint(cDim, "removed")))
		default:
			p.line(fmt.Sprintf("  %-20s%14s%14s%12s   %s",
				truncate(e.Name, 20),
				ms(e.Before), ms(e.After),
				formatSignedPercent(e.Percent, e.PercentMeaningful),
				p.endpointMark(e),
			))
		}
	}
}

// changeMark is the trailing word on a row: whether the change breaches the
// tolerance, and which way it went.
func (p *printer) changeMark(c compare.Change) string {
	switch {
	case c.Regression:
		return p.paint(cRed, "REGRESSION")
	case c.Worse:
		return p.paint(cYellow, "worse, within tolerance")
	default:
		return p.paint(cDim, "ok")
	}
}

func (p *printer) endpointMark(e compare.EndpointChange) string {
	switch {
	case e.Regression:
		return p.paint(cRed, "REGRESSION")
	case e.Worse:
		return p.paint(cYellow, "worse, within tolerance")
	default:
		return p.paint(cDim, "ok")
	}
}

// compareVerdict states the conclusion, and names what breached.
func (p *printer) compareVerdict(rep *compare.Report) {
	if rep.Regressed() {
		p.line("  RESULT: " + p.paint(cBold+cRed, string(rep.Verdict)))
		p.line("")
		p.line("  " + p.paint(cRed, fmt.Sprintf("%d metrics regressed: %s",
			len(rep.Regressions), strings.Join(rep.Regressions, ", "))))
	} else {
		p.line("  RESULT: " + p.paint(cBold+cGreen, string(rep.Verdict)))
		p.line("")
		p.line("  " + p.paint(cGreen, "nothing regressed beyond the configured tolerance"))
	}

	for _, note := range rep.Notes {
		p.line("")
		p.line("  " + p.paint(cYellow, "⚠ "+note))
	}
}

// formatChangeValue renders one side of a change using the unit it was measured
// in.
func formatChangeValue(v float64, unit string) string {
	switch unit {
	case "ms":
		return ms(v)
	case "ratio":
		return pct(v)
	case "iterations/s":
		return commafyFloat(v, 0)
	default:
		return commafyFloat(v, 2)
	}
}

// formatChange renders the change itself. An error rate moves in absolute terms,
// where a percentage would make a trivial rate look like a crisis.
func formatChange(c compare.Change) string {
	switch {
	case c.Metric == "error rate":
		return formatSigned(c.After-c.Before, "pp", 100)
	case !c.PercentMeaningful:
		return "-"
	default:
		return formatSignedPercent(c.Percent, true)
	}
}

// formatSignedPercent renders a signed ratio as a percentage.
func formatSignedPercent(ratio float64, meaningful bool) string {
	if !meaningful {
		return "-"
	}
	return fmt.Sprintf("%+.1f%%", ratio*100)
}

// formatSigned renders a signed value scaled into a unit.
func formatSigned(delta float64, unit string, scale float64) string {
	return fmt.Sprintf("%+.3f%s", delta*scale, unit)
}
