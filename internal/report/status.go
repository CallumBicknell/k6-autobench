package report

import (
	"fmt"

	"github.com/callum/k6-autobench/internal/metrics"
)

// statusHint explains what a response-class mix implies.
//
// A 4xx storm and a 5xx storm look identical in an error rate but mean opposite
// things: one is the benchmark's fault, the other is the system's. Saying which
// is the difference between a number and a diagnosis (PLAN-OUTLINE.md §30).
func statusHint(classes []metrics.StatusClass) string {
	dominant := metrics.StatusClass{}
	for _, c := range classes {
		if c.Class == "2xx" {
			continue
		}
		if c.Count > dominant.Count {
			dominant = c
		}
	}
	if dominant.Count == 0 {
		return ""
	}

	share := pct(dominant.Share)
	switch dominant.Class {
	case "5xx":
		return fmt.Sprintf("%s of responses were 5xx: the system under test is failing.", share)
	case "4xx":
		return fmt.Sprintf("%s of responses were 4xx: the target is rejecting these requests — "+
			"check the workload (paths, methods, bodies) before reading the rest as capacity.", share)
	case "3xx":
		return fmt.Sprintf("%s of responses were 3xx: the target is redirecting. k6 follows "+
			"up to 10 redirects by default, so set target.max_redirects: 0 to measure the "+
			"redirect itself.", share)
	default:
		return fmt.Sprintf("%s of responses had no HTTP status: transport-level failures.", share)
	}
}

// statusBreakdown prints the response-class split, and is skipped entirely for a
// run that only ever saw 2xx: there would be nothing to say.
func (p *printer) statusBreakdown(m metrics.Metrics) {
	if !hasNonSuccess(m.StatusClasses) {
		return
	}
	for _, c := range m.StatusClasses {
		label := "    " + c.Class
		value := fmt.Sprintf("%s  (%s of responses)", commafy(c.Count), pct(c.Share))
		if c.Class != "2xx" && c.Class != "3xx" {
			value = p.paint(cRed, value)
		}
		p.line(fmt.Sprintf("%-22s%s", label, value))
	}
}

func hasNonSuccess(classes []metrics.StatusClass) bool {
	for _, c := range classes {
		if c.Class != "2xx" && c.Count > 0 {
			return true
		}
	}
	return false
}
