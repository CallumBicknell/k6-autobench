package collect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePoller stands in for a real source: it returns whatever the test set and
// records how often it was asked.
type fakePoller struct {
	mu    sync.Mutex
	calls int
	// respond builds the nth poll's result. It takes the context because a real
	// poller must honour it, and tests need to exercise that. A nil respond
	// returns one reading per call, so the loop can be exercised on its own.
	respond func(ctx context.Context, call int) ([]reading, []pollFailure)
}

func (f *fakePoller) poll(ctx context.Context) ([]reading, []pollFailure) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	if f.respond == nil {
		return []reading{read(0, "cpu", float64(call))}, nil
	}
	return f.respond(ctx, call)
}

func (f *fakePoller) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// constant returns a poller that always reports the same reading.
func constant(name string, value float64) *fakePoller {
	return &fakePoller{respond: func(context.Context, int) ([]reading, []pollFailure) {
		return []reading{read(0, name, value)}, nil
	}}
}

// read is one reading with only the fields a test cares about.
func read(order int, name string, value float64) reading {
	return reading{order: order, key: name, name: name, value: value}
}

// sampleReport runs a collector over a function and returns what it saw.
func sampleReport(p poller, interval time.Duration, fn func()) Report {
	stop := sample(context.Background(), "api", interval, p)
	fn()
	return stop()
}

// TestSampleReturnsImmediately is the property the whole design rests on: a
// collector must never delay the measurement it is observing.
func TestSampleReturnsImmediately(t *testing.T) {
	release := make(chan struct{})
	p := &fakePoller{respond: func(ctx context.Context, _ int) ([]reading, []pollFailure) {
		// A wedged source, but one that honours its context, as a real poller
		// must: the loop relies on that to bound how long stopping waits.
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}}
	defer close(release)

	start := time.Now()
	stop := sample(context.Background(), "api", time.Hour, p)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("sample blocked for %v", elapsed)
	}

	done := make(chan Report, 1)
	go func() { done <- stop() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stopping blocked past its grace behind the wedged poll")
	}
}

// TestSampleReadsImmediatelyThenOnTheInterval covers a measurement shorter than
// the interval, which must still produce a reading.
func TestSampleReadsImmediatelyThenOnTheInterval(t *testing.T) {
	p := constant("cpu", 1)
	if report := sampleReport(p, time.Hour, func() {}); len(report.Series) != 1 {
		t.Fatalf("series = %d, want one reading taken immediately", len(report.Series))
	}
	if got := p.count(); got != 1 {
		t.Errorf("polls = %d, want 1 when the measurement is shorter than the interval", got)
	}

	p = constant("cpu", 1)
	sampleReport(p, 5*time.Millisecond, func() { time.Sleep(60 * time.Millisecond) })
	if got := p.count(); got < 3 {
		t.Errorf("polls = %d, want several over 60ms at a 5ms interval", got)
	}
}

// TestSampleDefaultsTheInterval keeps a collector with no configured interval
// from spinning on a zero-duration ticker.
func TestSampleDefaultsTheInterval(t *testing.T) {
	p := constant("cpu", 1)
	if report := sampleReport(p, 0, func() {}); len(report.Series) != 1 {
		t.Fatalf("series = %d, want one reading", len(report.Series))
	}
	if got := p.count(); got != 1 {
		t.Errorf("polls = %d, want 1: a zero interval must fall back to the default, not busy-loop", got)
	}
}

// TestSampleWaitsForTheLastReading: a poll already in flight is the last reading
// of the measurement, so stopping waits for it rather than discarding it.
func TestSampleWaitsForTheLastReading(t *testing.T) {
	started := make(chan struct{})
	p := &fakePoller{respond: func(ctx context.Context, call int) ([]reading, []pollFailure) {
		if call == 1 {
			close(started)
			time.Sleep(80 * time.Millisecond)
		}
		return []reading{read(0, "cpu", float64(call))}, nil
	}}

	stop := sample(context.Background(), "api", time.Hour, p)
	<-started
	// Stop while the first poll is still running.
	report := stop()

	if s := seriesNamed(t, report, "cpu"); len(s.Samples) != 1 {
		t.Fatalf("samples = %d, want the in-flight reading kept", len(s.Samples))
	}
}

// TestSampleRecordsFailuresAlongsideReadings: one broken query must not cost the
// readings from the queries that worked.
func TestSampleRecordsFailuresAlongsideReadings(t *testing.T) {
	p := &fakePoller{respond: func(_ context.Context, _ int) ([]reading, []pollFailure) {
		return []reading{read(0, "cpu", 0.5)}, []pollFailure{{key: "memory", err: errors.New("boom")}}
	}}

	report := sampleReport(p, time.Hour, func() {})
	if len(report.Series) != 1 || report.Series[0].Name != "cpu" {
		t.Errorf("series = %v, want the reading that worked", seriesNames(report))
	}
	if len(report.Failures) != 1 {
		t.Fatalf("failures = %v, want the one that did not", report.Failures)
	}
	if report.Failures[0].Source != "api" || !strings.Contains(report.Failures[0].Error, "memory") {
		t.Errorf("failure = %+v, want it attributed to the source and the series", report.Failures[0])
	}
}

// TestSampleDeduplicatesFailures keeps a persistently broken endpoint from
// adding one line per poll.
func TestSampleDeduplicatesFailures(t *testing.T) {
	p := &fakePoller{respond: func(_ context.Context, _ int) ([]reading, []pollFailure) {
		return nil, []pollFailure{{key: "cpu", err: errors.New("boom")}}
	}}

	report := sampleReport(p, 2*time.Millisecond, func() { time.Sleep(60 * time.Millisecond) })
	if len(report.Failures) != 1 {
		t.Errorf("failures = %d (%v), want 1 however many polls failed",
			len(report.Failures), report.Failures)
	}
}

// TestSampleStopsWhenTheContextIsCancelled keeps a collector from outliving the
// measurement it was started for.
func TestSampleStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := constant("cpu", 1)

	stop := sample(ctx, "api", 2*time.Millisecond, p)
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)

	before := p.count()
	time.Sleep(40 * time.Millisecond)
	if after := p.count(); after != before {
		t.Errorf("polls went from %d to %d after cancellation; the loop should have stopped", before, after)
	}
	stop()
}

// TestAccumulatorKeepsConfiguredOrder checks the report follows the poller's
// declared order rather than the order readings arrived in.
func TestAccumulatorKeepsConfiguredOrder(t *testing.T) {
	acc := newAccumulator("api", time.Now())
	acc.add(read(2, "third", 3), 0)
	acc.add(read(0, "first", 1), 0)
	acc.add(read(1, "second", 2), 0)

	want := []string{"first", "second", "third"}
	if got := seriesNames(acc.report()); !equalStrings(got, want) {
		t.Errorf("series = %v, want %v", got, want)
	}
}

// TestAccumulatorExtendsASeries keeps repeated polls in one row rather than
// starting a new one each time.
func TestAccumulatorExtendsASeries(t *testing.T) {
	acc := newAccumulator("api", time.Now())
	acc.add(read(0, "cpu", 1), 0)
	acc.add(read(0, "cpu", 2), time.Second)
	acc.add(read(0, "cpu", 3), 2*time.Second)

	report := acc.report()
	if len(report.Series) != 1 {
		t.Fatalf("series = %d, want one row extended across polls", len(report.Series))
	}
	s := report.Series[0]
	if len(s.Samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(s.Samples))
	}
	if s.Samples[1].OffsetMs != 1000 {
		t.Errorf("offset = %vms, want 1000ms", s.Samples[1].OffsetMs)
	}
	if s.Source != "api" {
		t.Errorf("source = %q, want api", s.Source)
	}
}

// TestAccumulatorKeepsTheFirstSightingMetadata: a later poll that omits the unit
// or origin must not blank what the first one recorded.
func TestAccumulatorKeepsTheFirstSightingMetadata(t *testing.T) {
	acc := newAccumulator("postgres", time.Now())
	acc.add(reading{order: 0, key: "cpu", name: "cpu", unit: "percent", origin: "up", value: 1}, 0)
	acc.add(reading{order: 0, key: "cpu", name: "cpu", value: 2}, time.Second)

	s := acc.report().Series[0]
	if s.Unit != "percent" || s.Origin != "up" {
		t.Errorf("series = %+v, want the first sighting's unit and origin kept", s)
	}
}

// TestAccumulatorSharesAnOffsetPerPoll: every series read in one round carries
// the same timestamp, so a resource peak can be read against the latency window
// it landed in.
func TestAccumulatorSharesAnOffsetPerPoll(t *testing.T) {
	acc := newAccumulator("api", time.Now())
	p := &fakePoller{respond: func(_ context.Context, _ int) ([]reading, []pollFailure) {
		return []reading{read(0, "a", 1), read(1, "b", 2)}, nil
	}}
	pollOnce(context.Background(), p, acc)

	report := acc.report()
	if len(report.Series) != 2 {
		t.Fatalf("series = %d, want 2", len(report.Series))
	}
	if report.Series[0].Samples[0].OffsetMs != report.Series[1].Samples[0].OffsetMs {
		t.Errorf("offsets differ (%v vs %v); readings taken together must share one",
			report.Series[0].Samples[0].OffsetMs, report.Series[1].Samples[0].OffsetMs)
	}
}
