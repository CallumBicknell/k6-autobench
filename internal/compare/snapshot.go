// Package compare reads two recorded runs and says what changed between them
// (PLAN-OUTLINE.md §35).
//
// It is the gate a pipeline needs. A capacity search answers "how much can this
// system take?"; it cannot answer "did this change make it worse?", because that
// is a question about two runs. Comparing them is what turns autobench from a
// measurement tool into a regression test.
package compare

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/search"
)

// Snapshot is a recorded run normalised for comparison, whichever command wrote
// it.
//
// A fixed-rate run and a capacity search record different things, and a
// comparison needs one shape to read. The fields that only one of them has are
// left at their zero value, and Comparable says which parts can be compared.
type Snapshot struct {
	// Path is the file this came from, so a report can name its inputs.
	Path string
	// Kind is what produced it: "fixed-rate run" or "capacity search".
	Kind string
	// Name and Target identify what was measured.
	Name   string
	Target string

	// Verdict is the run's own conclusion.
	Verdict results.Verdict

	// Rate is the fixed run's iteration rate, and is zero for a search.
	Rate int
	// Duration is the fixed run's configured duration, or the deciding
	// measurement's for a search.
	Duration time.Duration

	// Capacity is the discovered capacity, and is zero for a fixed run.
	Capacity int
	// CapacityIsLowerBound records a search that never found a failure boundary,
	// so the capacity is a floor rather than a measurement.
	CapacityIsLowerBound bool

	// Metrics is the measurement's own numbers. For a search it is the deciding
	// measurement's: the validation run when there is one, otherwise the last
	// passing probe.
	Metrics metrics.Metrics

	// GeneratorSaturated records that the load generator, not the target, was
	// the bottleneck. A comparison against such a run says nothing about the
	// system under test.
	GeneratorSaturated bool
}

// Kind values.
const (
	KindFixed    = "fixed-rate run"
	KindCapacity = "capacity search"
)

// IsCapacity reports whether the snapshot came from a capacity search.
func (s *Snapshot) IsCapacity() bool { return s.Kind == KindCapacity }

// Load reads a recorded run, whichever command wrote it.
//
// It does not trust the file to say which shape it is: it looks for the fields
// each command writes, so a hand-edited or truncated file is rejected rather
// than half-read.
func Load(path string) (*Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var probe struct {
		Outcome *json.RawMessage `json:"outcome"`
		Search  *json.RawMessage `json:"search"`
		Metrics *json.RawMessage `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	switch {
	case probe.Outcome != nil || probe.Search != nil:
		return loadCapacity(path, raw)
	case probe.Metrics != nil:
		return loadFixed(path, raw)
	default:
		return nil, fmt.Errorf("%s is not an autobench result: it has neither a search outcome "+
			"nor a fixed-rate measurement (write one with `autobench run --json` or "+
			"`autobench capacity --json`)", path)
	}
}

// fixedReport is the shape `autobench run --json` writes.
type fixedReport struct {
	Name      string                   `json:"name"`
	Target    string                   `json:"target"`
	Rate      int                      `json:"rate"`
	Duration  time.Duration            `json:"duration"`
	ElapsedMs float64                  `json:"elapsed_ms"`
	Metrics   metrics.Metrics          `json:"metrics"`
	Verdict   results.Verdict          `json:"verdict"`
	Generator *results.GeneratorStatus `json:"generator,omitempty"`
}

func loadFixed(path string, raw []byte) (*Snapshot, error) {
	var rep fixedReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &Snapshot{
		Path:               path,
		Kind:               KindFixed,
		Name:               rep.Name,
		Target:             rep.Target,
		Verdict:            rep.Verdict,
		Rate:               rep.Rate,
		Duration:           rep.Duration,
		Metrics:            rep.Metrics,
		GeneratorSaturated: rep.Generator != nil && rep.Generator.Saturated,
	}, nil
}

// capacityReport is the shape `autobench capacity --json` writes.
type capacityReport struct {
	Name      string          `json:"name"`
	Target    string          `json:"target"`
	ElapsedMs float64         `json:"elapsed_ms"`
	Outcome   *search.Outcome `json:"outcome,omitempty"`
	Verdict   results.Verdict `json:"verdict"`
}

func loadCapacity(path string, raw []byte) (*Snapshot, error) {
	var rep capacityReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	s := &Snapshot{
		Path:    path,
		Kind:    KindCapacity,
		Name:    rep.Name,
		Target:  rep.Target,
		Verdict: rep.Verdict,
	}
	if rep.Outcome == nil {
		// A search that failed before producing an outcome. It is still worth
		// comparing -- the verdict may have changed -- but there is nothing to
		// quote.
		return s, nil
	}

	s.Capacity = rep.Outcome.Capacity
	s.CapacityIsLowerBound = rep.Outcome.LowerBoundOnly
	s.GeneratorSaturated = rep.Outcome.GeneratorSaturated
	if deciding := rep.Outcome.Deciding(); deciding != nil {
		s.Metrics = deciding.Metrics
		s.Duration = deciding.Duration.Duration
		s.Rate = deciding.Rate
	}
	return s, nil
}
