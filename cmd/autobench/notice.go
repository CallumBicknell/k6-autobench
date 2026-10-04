package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/callum/k6-autobench/internal/config"
)

// sampleBytesPerRequest is the measured cost of windowed analysis: k6 streams a
// sample for every request to disk, at roughly this size.
//
// Measured against this project's own workload script, which tags each request
// name and each response class: 8,000 requests produced a 21 MB sample file and
// 10,000 requests a 29 MB one, so about 2.7 KiB each. It is rounded up to 3 KiB
// on purpose -- the estimate below exists to land in the right order of magnitude,
// not to be exact, and under-reporting a disk cost is the wrong way to be wrong.
const sampleBytesPerRequest = 3 << 10

// windowNotice says what windowed analysis will cost in disk, or "" when it is
// off.
//
// The cost is invisible until the disk fills. k6 has to stream every sample to
// disk to support per-window aggregates, so a run that looks modest by rate and
// duration can write gigabytes -- a ten-minute run at 10,000 iterations/s is
// around 16 GB -- and a CI runner that fills its disk fails in ways that have
// nothing to do with the benchmark.
//
// probes is how many measurements the run will take: 1 for a fixed run, and the
// search's ceiling for a capacity search, which is why that form says "up to".
func windowNotice(cfg *config.Config, rate int, duration time.Duration, probes int) string {
	window := cfg.Analysis.Window.Duration
	if window <= 0 || rate <= 0 || duration <= 0 {
		return ""
	}

	perProbe := int64(rate) * int64(duration.Seconds()) *
		int64(cfg.Workload.RequestsPerIteration())
	if perProbe <= 0 {
		return ""
	}

	const head = "windowed analysis (analysis.window %s) streams every sample to disk: "
	if probes > 1 {
		return fmt.Sprintf(head+"about %s per probe, up to %d probes",
			window, humanBytes(perProbe*sampleBytesPerRequest), probes)
	}
	return fmt.Sprintf(head+"about %s for this run",
		window, humanBytes(perProbe*sampleBytesPerRequest))
}

// humanBytes renders a byte count in binary units.
func humanBytes(v int64) string {
	const unit = 1024
	if v < unit {
		return strconv.FormatInt(v, 10) + "B"
	}

	value := float64(v)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + units[i]
}
