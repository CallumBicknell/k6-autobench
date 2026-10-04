package collect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultProc is where the kernel's counters live on Linux.
const DefaultProc = "/proc"

// minCPUWindow is how long the system collector waits before its second reading
// of the cumulative CPU counters.
//
// A percentage needs two readings, and the first poll has only one. Waiting a
// whole sampling interval would leave the first sample with no CPU reading at
// all, so a second reading is taken this much later instead: long enough to
// measure over, short enough not to matter.
const minCPUWindow = 100 * time.Millisecond

// errNoCPUInterval is what a poll reports when the CPU counters did not move
// between two readings, so there is no interval to measure a percentage over.
var errNoCPUInterval = errors.New("the CPU counters did not move, so there is no interval to measure over")

// System samples the host autobench itself runs on.
//
// Be clear about what that is. These are the *load generator's* counters. If the
// system under test runs elsewhere they say nothing about it; if it runs on this
// machine, the two are the same host and the report cannot separate them. What
// they are unambiguously good for is the question a saturation verdict raises:
// was the generator, rather than the target, the thing that ran out?
//
// It reads the Linux kernel's own counters and says so plainly on a platform
// that has none, rather than failing three files on every tick.
type System struct {
	// Source identifies the host in the report, e.g. "generator".
	Source string
	// Proc is where the kernel's counters are read from. Empty means DefaultProc.
	Proc string
	// Interval is how often the host is sampled.
	Interval time.Duration
}

var _ Collector = (*System)(nil)

// Name implements Collector.
func (s *System) Name() string { return s.Source }

// Start begins sampling in the background and returns the function that stops
// it and reports what was seen.
func (s *System) Start(ctx context.Context) func() Report {
	proc := s.proc()
	if err := checkProc(proc); err != nil {
		// Nothing to sample, so say so once rather than starting a loop that
		// would report the same three failures on every tick.
		return func() Report {
			return Report{Failures: []Failure{{Source: s.Source, Error: err.Error()}}}
		}
	}
	return sample(ctx, s.Source, s.Interval, &systemPoller{proc: proc})
}

// checkProc reports whether the kernel's counters are readable.
func checkProc(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("reading %s: %w (this collector reads the Linux kernel's own counters)",
			dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

func (s *System) proc() string {
	if s.Proc != "" {
		return s.Proc
	}
	return DefaultProc
}

// systemPoller reads the host's own counters.
type systemPoller struct {
	proc string
	// prev is the previous CPU reading. A percentage needs two, so the first
	// poll keeps one and waits a moment for the second.
	prev *cpuCounters
}

// poll reads the host's CPU, memory and load.
//
// Each is reported on its own: a host that cannot report one of them should not
// cost the other two.
func (sp *systemPoller) poll(ctx context.Context) ([]reading, []pollFailure) {
	var readings []reading
	var failures []pollFailure

	collect := func(key string, measure func() (reading, error)) {
		r, err := measure()
		if err != nil {
			failures = append(failures, pollFailure{key: key, err: err})
			return
		}
		readings = append(readings, r)
	}

	collect("cpu", func() (reading, error) { return sp.cpu(ctx) })
	collect("memory", sp.memory)
	collect("load", sp.load)
	return readings, failures
}

// cpu measures how much of the interval since the last poll the host's CPUs
// spent doing anything but idling.
func (sp *systemPoller) cpu(ctx context.Context) (reading, error) {
	var empty reading

	if sp.prev == nil {
		first, err := sp.readCPU()
		if err != nil {
			return empty, err
		}
		sp.prev = &first
		select {
		case <-ctx.Done():
			return empty, ctx.Err()
		case <-time.After(minCPUWindow):
		}
	}

	cur, err := sp.readCPU()
	if err != nil {
		return empty, err
	}
	prev := *sp.prev
	sp.prev = &cur

	used, ok := cur.usedFraction(prev)
	if !ok {
		return empty, errNoCPUInterval
	}
	return reading{
		order: 0, key: "cpu", name: "cpu", unit: "percent",
		origin: filepath.Join(sp.proc, "stat"), value: used * 100,
	}, nil
}

// memory reports the host's used memory.
func (sp *systemPoller) memory() (reading, error) {
	used, _, err := readMemInfo(filepath.Join(sp.proc, "meminfo"))
	if err != nil {
		return reading{}, err
	}
	return reading{
		order: 1, key: "memory", name: "memory", unit: "bytes",
		origin: filepath.Join(sp.proc, "meminfo"), value: used,
	}, nil
}

// load reports the host's one-minute load average.
func (sp *systemPoller) load() (reading, error) {
	v, err := readLoadAvg(filepath.Join(sp.proc, "loadavg"))
	if err != nil {
		return reading{}, err
	}
	return reading{
		order: 2, key: "load", name: "load", origin: filepath.Join(sp.proc, "loadavg"), value: v,
	}, nil
}

func (sp *systemPoller) readCPU() (cpuCounters, error) {
	f, err := os.Open(filepath.Join(sp.proc, "stat"))
	if err != nil {
		return cpuCounters{}, err
	}
	defer f.Close()
	return parseCPUCounters(f)
}

// cpuCounters is the aggregate CPU line from /proc/stat: cumulative time in each
// state, in USER_HZ.
type cpuCounters struct {
	// total is every state summed, which is the wall time the counters span.
	total float64
	// idle is idle plus iowait: time the CPUs spent doing nothing, whether or
	// not the kernel was waiting on disk.
	idle float64
}

// parseCPUCounters reads the aggregate "cpu" line, which is the first line of
// /proc/stat and the only one that covers every core at once.
func parseCPUCounters(r io.Reader) (cpuCounters, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}

		var c cpuCounters
		for i, field := range fields[1:] {
			v, err := strconv.ParseFloat(field, 64)
			if err != nil {
				return cpuCounters{}, fmt.Errorf("cpu line field %d (%q): %w", i+1, field, err)
			}
			c.total += v
			// idle and iowait are the fourth and fifth fields after the label.
			if i == 3 || i == 4 {
				c.idle += v
			}
		}
		return c, nil
	}
	if err := scanner.Err(); err != nil {
		return cpuCounters{}, err
	}
	return cpuCounters{}, errors.New("no cpu line in /proc/stat")
}

// usedFraction is the share of the interval the CPUs were not idle.
func (c cpuCounters) usedFraction(prev cpuCounters) (float64, bool) {
	totalDelta := c.total - prev.total
	if totalDelta <= 0 {
		// The counters went backwards, which means the host restarted
		// mid-measurement. There is no interval to measure over, so say so
		// rather than compute a percentage from a reset counter.
		return 0, false
	}

	idleDelta := c.idle - prev.idle
	// Idle moving backwards while total moved forwards is inconsistent. Clamping
	// reports a believable figure instead of one above 100% or below zero.
	switch {
	case idleDelta < 0:
		idleDelta = 0
	case idleDelta > totalDelta:
		idleDelta = totalDelta
	}
	return (totalDelta - idleDelta) / totalDelta, true
}

// readMemInfo returns the host's used and total memory in bytes.
func readMemInfo(path string) (used, total float64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	values := map[string]float64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, perr := strconv.ParseFloat(fields[0], 64)
		if perr != nil {
			continue
		}
		// Only the entries that say so are in kilobytes; the rest are counts.
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			v *= 1024
		}
		values[strings.TrimSpace(key)] = v
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}

	total, ok := values["MemTotal"]
	if !ok {
		return 0, 0, errors.New("no MemTotal in /proc/meminfo")
	}
	// MemAvailable is the kernel's own estimate of what a new workload could
	// claim. It is a better answer than free plus buffers plus cache, which is
	// what the older tools had to add up by hand.
	available, ok := values["MemAvailable"]
	if !ok {
		return 0, 0, errors.New("no MemAvailable in /proc/meminfo")
	}
	if available > total {
		available = total
	}
	return total - available, total, nil
}

// readLoadAvg returns the one-minute load average.
func readLoadAvg(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, errors.New("/proc/loadavg is empty")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) == 0 {
		return 0, errors.New("/proc/loadavg has no fields")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("load average %q: %w", fields[0], err)
	}
	return v, nil
}
