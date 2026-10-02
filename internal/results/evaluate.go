package results

import (
	"fmt"
	"strconv"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
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

	var failures []Failure

	if t := cfg.Thresholds.ErrorRate; t != nil && m.ErrorRate > t.Max {
		failures = append(failures, Failure{
			Metric:   "error_rate",
			Limit:    t.Max,
			Observed: m.ErrorRate,
			Unit:     "ratio",
		})
	}

	var latencyFailures []Failure
	for _, lc := range latencyChecks(cfg.Thresholds.Latency) {
		val, ok := m.At(lc.p)
		if !ok {
			continue
		}
		limitMs := float64(lc.max) / float64(time.Millisecond)
		if val > limitMs {
			latencyFailures = append(latencyFailures, Failure{
				Metric:   "latency." + formatP(lc.p),
				Limit:    limitMs,
				Observed: val,
				Unit:     "ms",
			})
		}
	}
	failures = append(failures, latencyFailures...)

	var verdict Verdict
	switch {
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
