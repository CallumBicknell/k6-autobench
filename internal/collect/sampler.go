package collect

import (
	"context"
	"sort"
	"time"
)

// stopGrace bounds how long stopping waits for a poll that is already in
// flight. The reading it is about to produce is the last one of the
// measurement, so it is worth a moment; but a wedged endpoint must not hold the
// report hostage either.
const stopGrace = time.Second

// reading is one series' value in one poll.
//
// A poller fills every field, because the loop that accumulates readings knows
// nothing about where they came from: the same machinery drives a PromQL query,
// a Docker stats call and a file in /proc.
type reading struct {
	// order is the series' position in the report. A poller sets it from its own
	// configuration, so the report keeps the order the user wrote rather than
	// the order concurrent polls happened to finish in.
	order int
	// key identifies the series within its source, so a failure to read it is
	// reported once however many polls hit it.
	key string
	// name labels the series in the report, e.g. "cpu".
	name string
	// unit is a free-text label for the rendered value.
	unit string
	// origin is the expression or endpoint the reading came from, kept so a
	// report can be reproduced by hand.
	origin string
	// value is the reading itself.
	value float64
}

// pollFailure is one series a poll could not read.
type pollFailure struct {
	key string
	err error
}

// poller takes one round of readings from a system.
//
// A poller reports what it could not read alongside what it could, rather than
// failing the poll as a whole: one broken query must not cost the readings from
// the queries that worked.
//
// A poll must return once the context is cancelled. The loop relies on that to
// bound how long stopping waits for a reading already in flight; a poller that
// ignores its context can hold the report open indefinitely.
type poller interface {
	poll(ctx context.Context) (readings []reading, failures []pollFailure)
}

// sample drives a poller: it reads immediately and then on every interval, and
// accumulates what it sees into one report.
//
// It returns immediately, so a slow or unreachable endpoint can never delay the
// measurement. The returned function must be called exactly once, or the
// sampling goroutine leaks.
func sample(ctx context.Context, source string, interval time.Duration, p poller) func() Report {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ctx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	acc := newAccumulator(source, time.Now())
	done := make(chan struct{})

	go func() {
		defer close(done)

		// The first reading is taken before waiting, not left to the ticker: a
		// measurement shorter than the interval would otherwise collect nothing
		// at all.
		pollOnce(ctx, p, acc)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				pollOnce(ctx, p, acc)
			}
		}
	}()

	return func() Report {
		// Ask the loop to stop rather than cancelling outright: a poll already in
		// flight is the last reading of the measurement, and dropping it would
		// leave a measurement shorter than the interval with no data at all.
		close(stop)
		select {
		case <-done:
		case <-time.After(stopGrace):
			// The endpoint is wedged; give up on the reading and abort the
			// request so stopping cannot hang the report.
			cancel()
			<-done
		}
		cancel()
		return acc.report()
	}
}

// pollOnce takes one round of readings and records them. The offset is computed
// once per round, so every series read together shares a timestamp and a
// resource peak can be read against the latency window it landed in.
func pollOnce(ctx context.Context, p poller, acc *accumulator) {
	readings, failures := p.poll(ctx)
	offset := time.Since(acc.start)

	for _, f := range failures {
		acc.fail(f.key, f.err)
	}
	for _, r := range readings {
		acc.add(r, offset)
	}
}

// accumulator gathers one measurement's readings. Only the sampling goroutine
// writes to it, and the report is read after that goroutine has stopped, so it
// needs no lock.
type accumulator struct {
	source string
	start  time.Time

	// byName indexes series by their report name, so a later poll extends the
	// series an earlier one created rather than starting a second row.
	byName map[string]int
	series []Series
	// order is the configured position behind each series, which keeps the
	// report in the order the user wrote rather than an arbitrary one.
	order []int

	failures []Failure
	failed   map[string]bool
}

func newAccumulator(source string, start time.Time) *accumulator {
	return &accumulator{
		source: source,
		start:  start,
		byName: map[string]int{},
		failed: map[string]bool{},
	}
}

func (a *accumulator) add(r reading, offset time.Duration) {
	i, ok := a.byName[r.name]
	if !ok {
		i = len(a.series)
		a.byName[r.name] = i
		a.series = append(a.series, Series{
			Source: a.source,
			Name:   r.name,
			Unit:   r.unit,
			Origin: r.origin,
		})
		a.order = append(a.order, r.order)
	}
	a.series[i].Samples = append(a.series[i].Samples, Sample{
		OffsetMs: float64(offset) / float64(time.Millisecond),
		Value:    r.value,
	})
}

// fail records a problem once per series. A persistently unreachable endpoint
// would otherwise add one line per poll.
func (a *accumulator) fail(key string, err error) {
	if a.failed[key] {
		return
	}
	a.failed[key] = true
	a.failures = append(a.failures, Failure{
		Source: a.source,
		Error:  key + ": " + err.Error(),
	})
}

// report returns what was collected, with the series in configured order.
func (a *accumulator) report() Report {
	idx := make([]int, len(a.series))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		if a.order[idx[x]] != a.order[idx[y]] {
			return a.order[idx[x]] < a.order[idx[y]]
		}
		return a.series[idx[x]].Name < a.series[idx[y]].Name
	})

	out := Report{
		Series:   make([]Series, 0, len(a.series)),
		Failures: a.failures,
	}
	for _, i := range idx {
		out.Series = append(out.Series, a.series[i])
	}
	return out
}
