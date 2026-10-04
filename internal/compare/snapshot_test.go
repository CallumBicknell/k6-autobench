package compare

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
)

// writeFile writes a result file and returns its path.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// fixedJSON renders what `autobench run --json` writes.
func fixedJSON(t *testing.T, target string, rate int, percentiles map[string]float64, errorRate float64) string {
	t.Helper()

	pcts := make([]metrics.Percentile, 0, len(percentiles))
	for p, v := range percentiles {
		var pf float64
		if _, err := fmt.Sscanf(p, "p%f", &pf); err != nil {
			t.Fatalf("bad percentile key %q", p)
		}
		pcts = append(pcts, metrics.Percentile{P: pf, Val: v})
	}

	body, err := json.Marshal(map[string]any{
		"name": "demo", "target": target, "rate": rate,
		"duration": int64(time.Second),
		"verdict":  results.VerdictPass,
		"metrics": metrics.Metrics{
			HasLatency:  true,
			AvgMs:       10,
			Percentiles: pcts,
			ErrorRate:   errorRate,
			Endpoints: []metrics.Endpoint{{
				Name:        "feed",
				Requests:    100,
				HasLatency:  true,
				AvgMs:       10,
				Percentiles: []metrics.Percentile{{P: 95, Val: 20}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// capacityJSON renders what `autobench capacity --json` writes.
func capacityJSON(t *testing.T, target string, capacity int, p95 float64, rate int) string {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"name": "demo", "target": target, "verdict": results.VerdictPass,
		"search": map[string]any{"start_rate": 100},
		"outcome": map[string]any{
			"probes": []any{}, "passing_rate": capacity, "capacity": capacity,
			"bracket_found": true,
			"validation": map[string]any{
				"index": 1, "phase": "validation", "rate": capacity, "pass": true,
				"result": map[string]any{
					"rate": capacity, "time_unit": "1s", "verdict": results.VerdictPass,
					"duration": int64(time.Second),
					"metrics": metrics.Metrics{
						HasLatency:  true,
						AvgMs:       p95 / 2,
						Percentiles: []metrics.Percentile{{P: 95, Val: p95}, {P: 99, Val: p95 * 2}},
						ErrorRate:   0,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// TestLoadFixedRun reads what a fixed-rate run writes.
func TestLoadFixedRun(t *testing.T) {
	path := writeFile(t, "run.json", fixedJSON(t, "http://a", 500, map[string]float64{"p95": 20}, 0))

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Kind != KindFixed {
		t.Errorf("kind = %q, want %q", s.Kind, KindFixed)
	}
	if s.Target != "http://a" || s.Rate != 500 {
		t.Errorf("snapshot = %+v, want the target and rate read", s)
	}
	if s.Capacity != 0 {
		t.Errorf("capacity = %d, want none for a fixed run", s.Capacity)
	}
	if len(s.Metrics.Endpoints) != 1 || s.Metrics.Endpoints[0].Name != "feed" {
		t.Errorf("endpoints = %+v, want the per-request breakdown", s.Metrics.Endpoints)
	}
}

// TestLoadCapacitySearch reads what a capacity search writes, quoting the
// deciding measurement rather than the raw probes.
func TestLoadCapacitySearch(t *testing.T) {
	path := writeFile(t, "cap.json", capacityJSON(t, "http://a", 4500, 20, 4500))

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Kind != KindCapacity {
		t.Errorf("kind = %q, want %q", s.Kind, KindCapacity)
	}
	if s.Capacity != 4500 {
		t.Errorf("capacity = %d, want 4500", s.Capacity)
	}
	if got, ok := metrics.PercentileAt(s.Metrics.Percentiles, 95); !ok || got != 20 {
		t.Errorf("p95 = %v (ok=%v), want the deciding measurement's", got, ok)
	}
}

// TestLoadRejectsSomethingElse: a file that is neither shape is refused rather
// than half-read.
func TestLoadRejectsSomethingElse(t *testing.T) {
	for _, body := range []string{`{}`, `{"hello":"world"}`, `[]`, `not json`} {
		path := writeFile(t, "other.json", body)
		if _, err := Load(path); err == nil {
			t.Errorf("Load(%s) succeeded, want a refusal", body)
		}
	}
}

// TestLoadMissingFile reports the error rather than an empty snapshot.
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("Load of a missing file should fail")
	}
}
