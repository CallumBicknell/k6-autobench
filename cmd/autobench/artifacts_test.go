package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/search"
)

// runResult is a fixed run's result.
func runResult() *results.Result {
	return &results.Result{
		Name:     "demo",
		Target:   "http://localhost:3000",
		Rate:     100,
		TimeUnit: "1s",
		Duration: config.Duration{Duration: 5 * time.Second},
		Verdict:  results.VerdictPass,
		Metrics: metrics.Metrics{
			HasLatency:  true,
			AvgMs:       0.5,
			Percentiles: []metrics.Percentile{{P: 95, Val: 0.8}},
			Requests:    500,
			ErrorRate:   0,
		},
	}
}

// TestRunOutDirWritesTheSameShapeAsASearch: §26 describes one stored layout, and a
// fixed run had no way to produce it -- only --json and --html.
func TestRunOutDirWritesTheSameShapeAsASearch(t *testing.T) {
	dir := t.TempDir()
	if err := writeRunArtifacts(dir, "", artifactConfig(), runResult()); err != nil {
		t.Fatalf("writeRunArtifacts: %v", err)
	}

	for _, name := range []string{"manifest.json", "summary.json", "report.html"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	// A fixed run takes one measurement, so there is nothing to put in probes/.
	if _, err := os.Stat(filepath.Join(dir, "probes")); !os.IsNotExist(err) {
		t.Errorf("probes/ exists for a fixed run (err=%v)", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse summary: %v", err)
	}
	for _, key := range []string{"name", "verdict", "requests", "failed_requests", "rate"} {
		if _, ok := got[key]; !ok {
			t.Errorf("summary has no %q: %s", key, raw)
		}
	}
}

// TestRunOutDirCopiesTheConfigWhenItHasOne.
func TestRunOutDirCopiesTheConfigWhenItHasOne(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "bench.yaml")
	if err := os.WriteFile(src, []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := writeRunArtifacts(dir, src, artifactConfig(), runResult()); err != nil {
		t.Fatalf("writeRunArtifacts: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if string(got) != "name: demo\n" {
		t.Errorf("config.yaml = %q, want the source config", got)
	}
}

// TestRunOutDirClearsAnEarlierRun: the same rule as a search's --out-dir, so a
// rerun does not leave two runs in one directory.
func TestRunOutDirClearsAnEarlierRun(t *testing.T) {
	dir := t.TempDir()

	// Something a previous search left behind, and something of the user's own.
	if err := os.MkdirAll(filepath.Join(dir, "probes"), 0o755); err != nil {
		t.Fatalf("mkdir probes: %v", err)
	}
	stale := filepath.Join(dir, "probes", "001-exponential-100.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write stale probe: %v", err)
	}
	mine := filepath.Join(dir, "NOTES.txt")
	if err := os.WriteFile(mine, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	if err := writeRunArtifacts(dir, "", artifactConfig(), runResult()); err != nil {
		t.Fatalf("writeRunArtifacts: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a previous run's probe survived (err=%v)", err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("the user's own file was removed: %v", err)
	}
}

// artifactConfig is the minimum a capacity report needs to render.
func artifactConfig() *config.Config {
	cfg := &config.Config{Target: config.Target{BaseURL: "http://localhost:3000"}}
	cfg.Search.StartRate = 100
	cfg.ApplyDefaults()
	return cfg
}

// artifactReport is a search outcome with n probes.
func artifactReport(n int) (*capacityReport, *search.Outcome) {
	out := &search.Outcome{PassingRate: 100, Capacity: 100}
	for i := 1; i <= n; i++ {
		out.Probes = append(out.Probes, search.Probe{
			Index: i, Phase: search.PhaseExponential, Rate: 100 * i, Pass: true,
		})
	}
	return &capacityReport{Name: "demo", Target: "http://localhost:3000"}, out
}

// writeProbes writes a run of n probes into dir.
func writeProbes(t *testing.T, dir string, n int) {
	t.Helper()
	rep, out := artifactReport(n)
	if err := writeArtifacts(dir, "", rep, artifactConfig(), out, time.Second); err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
}

// probeNames lists the probe files in an out-dir.
func probeNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "probes"))
	if err != nil {
		t.Fatalf("read probes: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestOutDirDoesNotKeepAnEarlierRunsProbes is the regression test for a directory
// holding two runs at once: the manifest and summary are overwritten, so nothing
// on disk says the surplus probes do not belong to this run.
func TestOutDirDoesNotKeepAnEarlierRunsProbes(t *testing.T) {
	dir := t.TempDir()

	writeProbes(t, dir, 5)
	if got := probeNames(t, dir); len(got) != 5 {
		t.Fatalf("first run wrote %d probes, want 5", len(got))
	}

	writeProbes(t, dir, 2)
	got := probeNames(t, dir)

	if len(got) != 2 {
		t.Errorf("second run left %d probes, want 2: %v", len(got), got)
	}
	for _, name := range got {
		if name != "001-exponential-100.json" && name != "002-exponential-200.json" {
			t.Errorf("unexpected probe %q left behind", name)
		}
	}
}

// TestOutDirLeavesFilesItDidNotWrite keeps a rerun from being destructive: only
// the names autobench owns are cleared.
func TestOutDirLeavesFilesItDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	writeProbes(t, dir, 3)

	mine := map[string]string{
		filepath.Join(dir, "NOTES.txt"):                 "keep me",
		filepath.Join(dir, "probes", "notes.json"):      "not a probe name",
		filepath.Join(dir, "probes", "001-custom.json"): "also not a probe name",
		filepath.Join(dir, "results"):                   "",
	}
	for path, body := range mine {
		if body == "" {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	writeProbes(t, dir, 1)

	for path, body := range mine {
		if body == "" {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("directory %s was removed: %v", path, err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("file %s was removed: %v", path, err)
			continue
		}
		if string(got) != body {
			t.Errorf("%s = %q, want %q", path, got, body)
		}
	}
}

// TestOutDirReplacesAConfigItNoLongerHas: a run with no config path used to leave
// the previous config.yaml next to a manifest that did not come from it.
func TestOutDirReplacesAConfigItNoLongerHas(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(t.TempDir(), "bench.yaml")
	if err := os.WriteFile(src, []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rep, out := artifactReport(1)
	if err := writeArtifacts(dir, src, rep, artifactConfig(), out, time.Second); err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatalf("the first run should have written config.yaml: %v", err)
	}

	// A second run with no config path must not leave the first one's.
	writeProbes(t, dir, 1)
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("config.yaml survived a run that had none (err=%v)", err)
	}
}

// TestOutDirIntoAFreshDirectory is the ordinary case.
func TestOutDirIntoAFreshDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "results")
	writeProbes(t, dir, 3)

	for _, name := range []string{"manifest.json", "summary.json", "report.html", "probes"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
}
