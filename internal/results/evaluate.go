package results

import (
	"fmt"
	"strconv"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
)

// generatorTolerance is how far below the target iteration rate the achieved
// rate may fall before the run is treated as generator-saturated.
const generatorTolerance = 0.98

type latencyCheck struct {
	p   float64
	max time.Duration
}

// Evaluate turns a normalised metric set into a verdict, the list of threshold
// breaches, and (when applicable) a generator-saturation status.
//
// Precedence: a saturated generator invalidates the probe entirely, so it
// outranks threshold failures. Otherwise error-rate breaches outrank latency
// breaches.
func Evaluate(cfg *config.Config, m metrics.Metrics) (Verdict, []Failure, *GeneratorStatus) {
	gen := detectGenerator(cfg, m)
	failures := EvaluateThresholds(cfg, m)

	var verdict Verdict
	switch {
	// No response at all outranks everything, including generator saturation. A
	// target that hangs produces both -- slow requests pile up VUs until k6 drops
	// iterations -- and the honest answer is the cause, not the symptom. Telling
	// someone to raise max_vus when their service is not answering sends them the
	// wrong way.
	case m.Unanswered:
		verdict = VerdictFailNoResponse
	case gen != nil:
		verdict = VerdictFailGenerator
	case len(failures) == 0:
		verdict = VerdictPass
	case hasUnit(failures, "ratio"):
		verdict = VerdictFailErrors
	default:
		verdict = VerdictFailLatency
	}
	return verdict, failures, gen
}

// EvaluateThresholds checks only the configured SLOs, ignoring the load
// generator.
//
// It is separate from Evaluate because a single time window cannot be judged on
// generator saturation: a window's achieved rate dips simply because requests
// were still in flight when it closed, which says nothing about the load
// generator (PLAN-OUTLINE.md §32 keeps the two concerns apart).
func EvaluateThresholds(cfg *config.Config, m metrics.Metrics) []Failure {
	var failures []Failure

	if t := cfg.Thresholds.ErrorRate; t != nil && m.ErrorRate > t.Max {
		failures = append(failures, Failure{
			Metric:   "error_rate",
			Limit:    t.Max,
			Observed: m.ErrorRate,
			Unit:     "ratio",
		})
	}

	for _, lc := range latencyChecks(cfg.Thresholds.Latency) {
		val, ok := m.At(lc.p)
		if !ok {
			continue
		}
		limitMs := float64(lc.max) / float64(time.Millisecond)
		if val > limitMs {
			failures = append(failures, Failure{
				Metric:   "latency." + formatP(lc.p),
				Limit:    limitMs,
				Observed: val,
				Unit:     "ms",
			})
		}
	}
	return failures
}

// VerdictForFailures classifies a set of threshold breaches the same way
// Evaluate does: an error-rate breach outranks a latency breach.
func VerdictForFailures(failures []Failure) Verdict {
	switch {
	case len(failures) == 0:
		return VerdictPass
	case hasUnit(failures, "ratio"):
		return VerdictFailErrors
	default:
		return VerdictFailLatency
	}
}

func detectGenerator(cfg *config.Config, m metrics.Metrics) *GeneratorStatus {
	target := cfg.TargetRatePerSecond()
	achieved := m.IterationRate

	saturated := false
	reason := ""

	switch {
	case m.DroppedIterations > 0:
		saturated = true
		reason = fmt.Sprintf("%d dropped iterations", m.DroppedIterations)
	case target > 0 && achieved < target*generatorTolerance:
		saturated = true
		reason = "achieved iteration rate below target"
	}

	if !saturated {
		return nil
	}
	if m.MaxVUs > 0 && m.VUs >= m.MaxVUs {
		reason = "maxVUs reached"
	}
	return &GeneratorStatus{
		Saturated:    true,
		TargetRate:   target,
		AchievedRate: achieved,
		Dropped:      m.DroppedIterations,
		Reason:       reason,
	}
}

func latencyChecks(l config.LatencyThresholds) []latencyCheck {
	var out []latencyCheck
	add := func(p float64, t *config.LatencyThreshold) {
		if t != nil {
			out = append(out, latencyCheck{p: p, max: t.Max.Duration})
		}
	}
	add(50, l.P50)
	add(75, l.P75)
	add(90, l.P90)
	add(95, l.P95)
	add(99, l.P99)
	add(99.9, l.P999)
	return out
}

func hasUnit(fs []Failure, unit string) bool {
	for _, f := range fs {
		if f.Unit == unit {
			return true
		}
	}
	return false
}

func formatP(p float64) string {
	return "p" + strconv.FormatFloat(p, 'f', -1, 64)
}
