package report

import (
	"fmt"
	"io"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/search"
)

// RenderCapacity writes the report for a capacity search: every probe it took,
// the bracket it converged on and the validation run that confirmed it.
func RenderCapacity(w io.Writer, cfg *config.Config, out *search.Outcome, version string, elapsed time.Duration, color bool) {
	p := &printer{w: w, color: color}

	p.header("AUTOBENCH v"+version, "capacity search")

	p.line("")
	p.kv("Target", cfg.Target.BaseURL)
	p.kv("Workload", workloadSummary(cfg))
	p.kv("Search", searchSummary(cfg))
	if cfg.Search.Warmup.Duration > 0 {
		p.kv("Warmup", cfg.Search.Warmup.String()+" per probe")
	}
	p.kv("Confirmation", fmt.Sprintf("%d pass / %d fail",
		cfg.Search.ConfirmationRuns, cfg.Search.FailureConfirmation))
	p.line("")

	p.separator()
	p.probeTable(out.Probes)
	p.line("")

	p.separator()
	p.capacity(out, cfg)
	p.endpoints(decidingMetrics(out))
	p.windows(decidingWindows(out), cfg)
	p.endpointDrift(decidingWindows(out), decidingMetrics(out), cfg)
	p.system(decidingSystem(out), cfg)
	p.correlated(decidingWindows(out), decidingSystem(out), cfg)
	p.line("")

	p.separator()
	p.capacityVerdict(out)

	p.line("")
	p.line(p.paint(cDim, fmt.Sprintf("  %d probes · ran %s",
		len(out.Probes), ms(float64(elapsed)/float64(time.Millisecond)))))
}

// searchSummary renders the search parameters in one line.
func searchSummary(cfg *config.Config) string {
	s := cfg.Search
	out := fmt.Sprintf("%s: start %s/s ×%g, resolution %s/s, probe %s",
		s.Strategy, commafy(int64(s.StartRate)), s.IncreaseFactor,
		commafy(int64(s.Resolution)), s.ProbeDuration)
	if s.MaxRate > 0 {
		out += fmt.Sprintf(", ceiling %s/s", commafy(int64(s.MaxRate)))
	}
	return out
}

// probeTable renders one row per measurement: this is the rate/latency curve in
// tabular form.
func (p *printer) probeTable(probes []search.Probe) {
	p.line("  " + p.paint(cBold, fmt.Sprintf("%-4s%-12s%10s%10s%10s%10s  %s",
		"#", "PHASE", "RATE", "P95", "P99", "ACHIEVED", "VERDICT")))

	for _, pr := range probes {
		verdict := string(pr.Result.Verdict)
		code := cGreen
		if !pr.Result.Verdict.OK() {
			code = cRed
		}

		row := fmt.Sprintf("%-4d%-12s%10s%10s%10s%10s  %s",
			pr.Index,
			pr.Phase,
			commafy(int64(pr.Rate)),
			latencyCell(pr.Result.Metrics, 95),
			latencyCell(pr.Result.Metrics, 99),
			commafyFloat(pr.Result.Metrics.IterationRate, 0),
			p.paint(code, verdict),
		)
		if pr.Attempts > 1 {
			row += p.paint(cDim, fmt.Sprintf("  (%d runs)", pr.Attempts))
		}
		p.line("  " + row)

		if pr.Note != "" {
			p.line("      " + p.paint(cDim, pr.Note))
		}
		for _, f := range pr.Result.Failures {
			p.line("      " + p.paint(cRed, failureLine(f)))
		}
	}
}

// latencyCell renders a percentile, or "-" when the config does not collect it.
func latencyCell(m metrics.Metrics, p float64) string {
	v, ok := m.At(p)
	if !ok {
		return "-"
	}
	return ms(v)
}

// capacity renders the bracket the search converged on.
func (p *printer) capacity(out *search.Outcome, cfg *config.Config) {
	perIteration := cfg.Workload.RequestsPerIteration()

	if out.PassingRate > 0 {
		p.kv("Sustained", rateLine(out.PassingRate, "s", perIteration))
	}
	if out.FailingRate > 0 {
		p.kv("Breached", rateLine(out.FailingRate, "s", perIteration))
	}
	if out.BracketFound {
		p.kv("Resolution", fmt.Sprintf("%s/s (bracket width %s)",
			commafy(int64(out.Resolution)),
			commafy(int64(out.FailingRate-out.PassingRate))))
	}
	if out.Capacity > 0 {
		label := "Capacity"
		if out.LowerBoundOnly {
			label = "Capacity ≥"
		}
		p.kv(label, rateLine(out.Capacity, "s", perIteration))
	}
	if out.Validation != nil {
		p.kv("Validated", fmt.Sprintf("%s iterations/s for %s (margin %g)",
			commafy(int64(out.ValidationRate)),
			out.Validation.Result.Duration.String(),
			out.SafetyMargin))
	}
	for _, note := range out.Notes {
		p.line("  " + p.paint(cYellow, "⚠ "+note))
	}

	operatingLimit(out, cfg).render(p)
}

// capacityVerdict prints the overall verdict and why it was reached.
func (p *printer) capacityVerdict(out *search.Outcome) {
	verdict := out.Verdict()
	code := cGreen
	if !verdict.OK() {
		code = cRed
	}
	p.line("  RESULT: " + p.paint(cBold+code, string(verdict)))
	p.line("")

	// The response-class split of the measurement the verdict rests on.
	if hint := statusHint(decidingMetrics(out).StatusClasses); hint != "" {
		p.line("  " + p.paint(cYellow, "⚠ "+hint))
	}

	if v := out.Validation; v != nil {
		p.line("  Validation run:")
		p.line(fmt.Sprintf("    rate       %s/s", commafy(int64(v.Rate))))
		p.line(fmt.Sprintf("    duration   %s", v.Result.Duration))
		p.line(fmt.Sprintf("    achieved   %s/s", commafyFloat(v.Result.Metrics.IterationRate, 1)))
		if v.Result.Metrics.HasLatency {
			p.line(fmt.Sprintf("    avg        %s", ms(v.Result.Metrics.AvgMs)))
			for _, pc := range v.Result.Metrics.Percentiles {
				p.line(fmt.Sprintf("    %-10s %s", formatP(pc.P), ms(pc.Val)))
			}
		}
		p.line(fmt.Sprintf("    errors     %s", pct(v.Result.Metrics.ErrorRate)))
		for _, f := range v.Result.Failures {
			p.line("    " + p.paint(cRed, failureLine(f)))
		}
		if g := v.Result.Generator; g != nil {
			p.line("    " + p.paint(cYellow, "⚠ generator saturated: "+g.Reason))
		}
		p.line("")
	}

	switch {
	case out.Unanswered():
		p.line("  " + p.paint(cRed, "The target never answered: not one request received an HTTP response."))
		p.line("  " + p.paint(cDim, "Check target.base_url, and that the service is listening, before reading"))
		p.line("  " + p.paint(cDim, "any of the above as capacity."))
	case out.GeneratorSaturated:
		p.line("  " + p.paint(cYellow, "The load generator, not the target, was the bottleneck."))
		p.line("  " + p.paint(cDim, "Raise load_generator.max_vus and search again."))
	case out.LowerBoundOnly:
		p.line("  " + p.paint(cYellow, "No failure boundary was reached within the search limits,"))
		p.line("  " + p.paint(cDim, "so the capacity above is a lower bound."))
	}
}
