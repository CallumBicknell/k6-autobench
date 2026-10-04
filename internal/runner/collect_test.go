package runner

import (
	"context"
	"testing"

	"github.com/callum/k6-autobench/internal/collect"
)

// fakeCollector records its lifecycle, so the runner's bracketing of a
// measurement can be asserted without a real monitoring endpoint.
type fakeCollector struct {
	name string
	// events, when set, collects "start <name>" / "stop <name>" in order.
	events *[]string

	started bool
	stopped bool
	report  collect.Report
}

func (f *fakeCollector) Name() string { return f.name }

func (f *fakeCollector) Start(context.Context) func() collect.Report {
	f.started = true
	if f.events != nil {
		*f.events = append(*f.events, "start "+f.name)
	}
	return func() collect.Report {
		f.stopped = true
		if f.events != nil {
			*f.events = append(*f.events, "stop "+f.name)
		}
		return f.report
	}
}

// TestObserveWithNoCollectors keeps a run without collectors free of any
// bookkeeping.
func TestObserveWithNoCollectors(t *testing.T) {
	report := observe(context.Background(), nil)()
	if !report.IsZero() {
		t.Errorf("report = %+v, want empty", report)
	}
}

// TestObserveMergesCollectorReports checks every source ends up in one report,
// including the ones that could not read anything.
func TestObserveMergesCollectorReports(t *testing.T) {
	api := &fakeCollector{
		name:   "api",
		report: collect.Report{Series: []collect.Series{{Source: "api", Name: "cpu"}}},
	}
	postgres := &fakeCollector{
		name: "postgres",
		report: collect.Report{
			Series:   []collect.Series{{Source: "postgres", Name: "connections"}},
			Failures: []collect.Failure{{Source: "postgres", Error: "connection refused"}},
		},
	}

	stop := observe(context.Background(), []collect.Collector{api, postgres})
	if !api.started || !postgres.started {
		t.Fatal("every collector should be started")
	}

	report := stop()
	if !api.stopped || !postgres.stopped {
		t.Error("every collector should be stopped")
	}
	if len(report.Series) != 2 {
		t.Errorf("series = %d, want both sources", len(report.Series))
	}
	if len(report.Failures) != 1 || report.Failures[0].Source != "postgres" {
		t.Errorf("failures = %v, want the collector's own note kept", report.Failures)
	}
}

// TestObserveBracketsTheMeasurement checks the collectors are started before the
// measurement and stopped after it, and that stopping unwinds in reverse so the
// observation windows nest rather than cut each other short.
func TestObserveBracketsTheMeasurement(t *testing.T) {
	var events []string
	a := &fakeCollector{name: "first", events: &events}
	b := &fakeCollector{name: "second", events: &events}

	stop := observe(context.Background(), []collect.Collector{a, b})
	events = append(events, "measure")
	stop()
	events = append(events, "done")

	want := []string{"start first", "start second", "measure", "stop second", "stop first", "done"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}
