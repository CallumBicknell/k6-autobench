package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// procFixture writes a fake /proc tree and returns its path, so the parser is
// tested against the kernel's real formats rather than the real kernel.
func procFixture(t *testing.T, stat, meminfo, loadavg string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"stat":    stat,
		"meminfo": meminfo,
		"loadavg": loadavg,
	} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// A real /proc/stat first line: user nice system idle iowait irq softirq steal.
const statSample = `cpu  100 0 50 800 50 0 0 0 0 0
cpu0 50 0 25 400 25 0 0 0 0 0
cpu1 50 0 25 400 25 0 0 0 0 0
intr 12345
ctxt 67890
`

const meminfoSample = `MemTotal:       16384000 kB
MemFree:         1000000 kB
MemAvailable:    4096000 kB
Buffers:          500000 kB
Cached:          3000000 kB
SwapTotal:             0 kB
`

const loadavgSample = `1.25 0.90 0.75 2/1234 56789
`

func measureSystem(t *testing.T, s *System, fn func()) Report {
	t.Helper()
	stop := s.Start(context.Background())
	fn()
	return stop()
}

// TestSystemReadsTheKernelsCounters covers the series and the files they come
// from, so a reading can be reproduced by hand.
func TestSystemReadsTheKernelsCounters(t *testing.T) {
	dir := procFixture(t, statSample, meminfoSample, loadavgSample)
	s := &System{Source: "generator", Proc: dir, Interval: time.Hour}

	report := measureSystem(t, s, func() {})

	// A fixed fixture cannot produce a CPU percentage: the counters have to
	// move between two readings, and this one never changes. Memory and load
	// need no interval.
	if names := seriesNames(report); !equalStrings(names, []string{"memory", "load"}) {
		t.Fatalf("series = %v, want memory and load", names)
	}

	mem := seriesNamed(t, report, "memory")
	// MemTotal 16384000 kB minus MemAvailable 4096000 kB, in bytes.
	if got, want := mem.Samples[0].Value, float64((16384000-4096000)*1024); got != want {
		t.Errorf("memory = %v bytes, want %v", got, want)
	}
	if mem.Unit != "bytes" {
		t.Errorf("memory unit = %q, want bytes", mem.Unit)
	}
	if !strings.HasSuffix(mem.Origin, "meminfo") {
		t.Errorf("memory origin = %q, want the file it came from", mem.Origin)
	}

	load := seriesNamed(t, report, "load")
	if load.Samples[0].Value != 1.25 {
		t.Errorf("load = %v, want 1.25", load.Samples[0].Value)
	}
	if load.Unit != "" {
		t.Errorf("load unit = %q, want none: a load average is a bare number", load.Unit)
	}
}

// TestSystemCPUReportsNoIntervalOnAFixedFixture documents why a fixed fixture
// cannot produce a CPU percentage.
func TestSystemCPUReportsNoIntervalOnAFixedFixture(t *testing.T) {
	dir := procFixture(t, statSample, meminfoSample, loadavgSample)
	s := &System{Source: "generator", Proc: dir, Interval: time.Hour}

	report := measureSystem(t, s, func() {})
	if len(report.Failures) != 1 {
		t.Fatalf("failures = %v, want the missing CPU interval reported", report.Failures)
	}
	if !strings.Contains(report.Failures[0].Error, "did not move") {
		t.Errorf("failure = %q, want it to explain why", report.Failures[0].Error)
	}
}

// TestSystemCPUMeasuresTheInterval drives the counters forward between readings,
// so a percentage can be computed and checked against a hand-computed figure.
func TestSystemCPUMeasuresTheInterval(t *testing.T) {
	dir := procFixture(t, statSample, meminfoSample, loadavgSample)

	// 1000 units later: 200 of them busy, so 20% used.
	next := `cpu  300 0 50 1600 50 0 0 0 0 0
cpu0 150 0 25 800 25 0 0 0 0 0
cpu1 150 0 25 800 25 0 0 0 0 0
`
	sp := &systemPoller{proc: dir}
	first, err := sp.readCPU()
	if err != nil {
		t.Fatalf("readCPU: %v", err)
	}
	sp.prev = &first

	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(next), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}

	r, err := sp.cpu(context.Background())
	if err != nil {
		t.Fatalf("cpu: %v", err)
	}
	if r.value != 20 {
		t.Errorf("cpu = %v%%, want 20%%", r.value)
	}
	if r.unit != "percent" {
		t.Errorf("unit = %q, want percent", r.unit)
	}
}

// TestSystemCPUHandlesCounterWeirdness keeps a counter that went backwards from
// producing a negative or impossible reading.
func TestSystemCPUHandlesCounterWeirdness(t *testing.T) {
	prev := cpuCounters{total: 10000, idle: 9000}

	// The host restarted mid-measurement, so the counters reset and there is no
	// interval to measure over.
	if _, ok := (cpuCounters{total: 100, idle: 50}).usedFraction(prev); ok {
		t.Error("counters that went backwards have no interval to measure over")
	}

	// Idle went backwards while total went forwards: inconsistent, but clamping
	// reports a believable figure rather than more than 100% used.
	if got, ok := (cpuCounters{total: 11000, idle: 99999}).usedFraction(prev); !ok || got != 0 {
		t.Errorf("used = %v%% (ok=%v), want 0%%", got*100, ok)
	}

	// Counters that did not move have nothing to measure over either.
	if _, ok := prev.usedFraction(prev); ok {
		t.Error("counters that did not move have no interval to measure over")
	}
}

// TestSystemReportsAMissingProc covers a platform without the kernel's counters,
// which must say so once rather than fail three files on every tick.
func TestSystemReportsAMissingProc(t *testing.T) {
	s := &System{Source: "generator", Proc: filepath.Join(t.TempDir(), "nope"), Interval: time.Hour}

	report := measureSystem(t, s, func() {})
	if len(report.Series) != 0 {
		t.Errorf("series = %v, want none", seriesNames(report))
	}
	if len(report.Failures) != 1 {
		t.Fatalf("failures = %v, want one clear note", report.Failures)
	}
	f := report.Failures[0]
	if f.Source != "generator" || !strings.Contains(f.Error, "Linux kernel") {
		t.Errorf("failure = %+v, want it to say why this platform cannot be read", f)
	}
}

// TestSystemReportsAProcThatIsNotADirectory covers a path that exists but is not
// the kernel's tree.
func TestSystemReportsAProcThatIsNotADirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := &System{Source: "generator", Proc: path, Interval: time.Hour}

	report := measureSystem(t, s, func() {})
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Error, "not a directory") {
		t.Errorf("failures = %v, want it to say the path is not a directory", report.Failures)
	}
}

// TestSystemReportsAnUnreadableCounter: one missing counter must not cost the
// others.
func TestSystemReportsAnUnreadableCounter(t *testing.T) {
	dir := procFixture(t, statSample, "", loadavgSample) // no meminfo
	s := &System{Source: "generator", Proc: dir, Interval: time.Hour}

	report := measureSystem(t, s, func() {})
	if len(report.Series) != 1 || report.Series[0].Name != "load" {
		t.Errorf("series = %v, want load alone", seriesNames(report))
	}
	if len(report.Failures) != 2 {
		t.Fatalf("failures = %v, want cpu and memory both reported", report.Failures)
	}
}

// TestSystemNameIsTheSource keeps report rows labelled by the configured name.
func TestSystemNameIsTheSource(t *testing.T) {
	if got := (&System{Source: "generator"}).Name(); got != "generator" {
		t.Errorf("Name() = %q, want generator", got)
	}
}
