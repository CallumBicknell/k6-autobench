package results

import (
	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
)

// WindowFailures evaluates the configured SLOs against every window of a run and
// returns the breaches, each tagged with the window it came from.
//
// This is what makes validation strict (PLAN-OUTLINE.md §32): a ten-second spike
// cannot hide behind five minutes of averages. Only the thresholds are checked,
// because a window's achieved rate dips simply because requests were still in
// flight when it closed, which says nothing about the load generator.
func WindowFailures(cfg *config.Config, windows []metrics.Window) []Failure {
	var out []Failure
	for _, w := range windows {
		if w.Requests == 0 {
			// No traffic in this window, so nothing was measured and nothing
			// can have breached.
			continue
		}
		for _, f := range EvaluateThresholds(cfg, windowMetrics(w)) {
			f.Window = w.Index
			out = append(out, f)
		}
	}
	return out
}

// windowMetrics presents a window in the shape the threshold evaluation expects,
// so both paths judge latency and errors the same way.
func windowMetrics(w metrics.Window) metrics.Metrics {
	return metrics.Metrics{
		HasLatency:     w.HasLatency,
		Percentiles:    w.Percentiles,
		AvgMs:          w.AvgMs,
		MaxMs:          w.MaxMs,
		Requests:       w.Requests,
		FailedRequests: w.Failed,
		ErrorRate:      w.ErrorRate,
	}
}

// ApplyWindowPolicy folds window-level breaches into a verdict when the config
// requires every window to pass.
//
// A generator-saturated probe is left alone: it was discarded rather than
// measured, so a window breach inside it says nothing about the target
// (PLAN-OUTLINE.md §17).
func ApplyWindowPolicy(cfg *config.Config, verdict Verdict, windowFailures []Failure) Verdict {
	if !cfg.Analysis.RequireAllWindowsPass || len(windowFailures) == 0 {
		return verdict
	}
	if verdict == VerdictFailGenerator {
		return verdict
	}
	return VerdictForFailures(windowFailures)
}
