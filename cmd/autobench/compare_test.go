package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/compare"
	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/results"
)

// fixedResultJSON renders what `autobench run --json` writes, with one p95.
func fixedResultJSON(t *testing.T, p95 float64) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": "demo", "target": "http://localhost:3000", "rate": 500,
		"duration": config.Duration{Duration: time.Second}, "verdict": results.VerdictPass,
		"metrics": metrics.Metrics{
			HasLatency:  true,
			AvgMs:       p95 / 2,
			Percentiles: []metrics.Percentile{{P: 95, Val: p95}},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// newCompareFlagSet mirrors the flags compareCommand registers.
func newCompareFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Float64Var(new(float64), "tolerance-latency", 0, "")
	fs.Float64Var(new(float64), "tolerance-capacity", 0, "")
	fs.Float64Var(new(float64), "tolerance-errors", 0, "")
	fs.StringVar(new(string), "json", "", "")
	fs.StringVar(new(string), "html", "", "")
	fs.BoolVar(new(bool), "no-color", false, "")
	return fs
}

// TestCompareArgs takes the two paths in either position, the way the other
// commands take a config path.
func TestCompareArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"both after no flags", []string{"a.json", "b.json"}, []string{"a.json", "b.json"}},
		{"a flag first", []string{"--no-color", "a.json", "b.json"}, []string{"a.json", "b.json"}},
		{"a flag in the middle", []string{"a.json", "--no-color", "b.json"}, []string{"a.json", "b.json"}},
		{"a valued flag first", []string{"--tolerance-latency", "0.05", "a.json", "b.json"},
			[]string{"a.json", "b.json"}},
		{"a valued flag between", []string{"a.json", "--tolerance-latency", "0.05", "b.json"},
			[]string{"a.json", "b.json"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, code := compareArgs(newCompareFlagSet(), tc.args)
			if code != exitPass {
				t.Fatalf("code = %d, want %d", code, exitPass)
			}
			if len(got) != 2 || got[0] != tc.want[0] || got[1] != tc.want[1] {
				t.Errorf("paths = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCompareArgsRejectsTheWrongCount: a comparison needs exactly two runs, and
// guessing which one is missing would be worse than saying so.
func TestCompareArgsRejectsTheWrongCount(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"only.json"},
		{"a.json", "b.json", "c.json"},
	} {
		if _, code := compareArgs(newCompareFlagSet(), args); code != exitError {
			t.Errorf("compareArgs(%v) code = %d, want %d", args, code, exitError)
		}
	}
}

// TestCompareArgsRejectsUnknownFlags keeps a typo from being read as a path.
func TestCompareArgsRejectsUnknownFlags(t *testing.T) {
	if _, code := compareArgs(newCompareFlagSet(), []string{"--nope", "a.json", "b.json"}); code != exitError {
		t.Errorf("code = %d, want %d", code, exitError)
	}
}

// TestValidateTolerances rejects a tolerance that could not mean anything.
func TestValidateTolerances(t *testing.T) {
	if err := validateTolerances(compare.DefaultConfig()); err != nil {
		t.Errorf("the defaults should be valid: %v", err)
	}
	for _, cfg := range []compare.Config{
		{Latency: -0.1},
		{Capacity: -0.1},
		{Errors: -0.1},
	} {
		if err := validateTolerances(cfg); err == nil {
			t.Errorf("validateTolerances(%+v) succeeded, want a rejection", cfg)
		}
	}
}

// TestCompareCommandEndToEnd drives the command over two real result files and
// checks the exit code, which is what a pipeline reads.
func TestCompareCommandEndToEnd(t *testing.T) {
	dir := t.TempDir()
	baseline := filepath.Join(dir, "baseline.json")
	candidate := filepath.Join(dir, "candidate.json")

	write := func(path string, p95 float64) {
		t.Helper()
		if err := os.WriteFile(path, []byte(fixedResultJSON(t, p95)), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Run("a regression fails", func(t *testing.T) {
		write(baseline, 100)
		write(candidate, 200) // +100%, tolerance is 10%

		if code := compareCommand([]string{baseline, candidate, "--no-color"}); code != exitFail {
			t.Errorf("code = %d, want %d", code, exitFail)
		}
	})

	t.Run("within tolerance passes", func(t *testing.T) {
		write(baseline, 100)
		write(candidate, 105) // +5%

		if code := compareCommand([]string{baseline, candidate, "--no-color"}); code != exitPass {
			t.Errorf("code = %d, want %d", code, exitPass)
		}
	})

	t.Run("a missing file is an error, not a failure", func(t *testing.T) {
		code := compareCommand([]string{filepath.Join(dir, "nope.json"), candidate, "--no-color"})
		if code != exitError {
			t.Errorf("code = %d, want %d", code, exitError)
		}
	})
}

// pascalKeyRE finds a JSON object key in Go's default PascalCase, which is what
// the output would fall back to if a struct field lost its tag.
var pascalKeyRE = regexp.MustCompile(`"[A-Z][A-Za-z_]*":`)

// TestCompareJSONUsesSnakeCase guards the shape of the --json output, which is a
// public interface. A consumer reads these keys, and Go's default field names
// would be inconsistent with every other result file autobench writes.
func TestCompareJSONUsesSnakeCase(t *testing.T) {
	dir := t.TempDir()
	baseline := filepath.Join(dir, "baseline.json")
	candidate := filepath.Join(dir, "candidate.json")
	out := filepath.Join(dir, "compare.json")

	if err := os.WriteFile(baseline, []byte(fixedResultJSON(t, 100)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(candidate, []byte(fixedResultJSON(t, 200)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code := compareCommand([]string{baseline, candidate, "--no-color", "--json", out})
	if code != exitFail {
		t.Fatalf("code = %d, want %d for a 100%% regression", code, exitFail)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, key := range []string{
		"baseline", "candidate", "ran_at", "verdict", "regressed", "tolerance", "result",
	} {
		if _, ok := doc[key]; !ok {
			t.Errorf("comparison json has no %q key: %s", key, raw)
		}
	}

	// And nothing anywhere in it fell back to a Go field name.
	if bad := pascalKeyRE.FindAllString(string(raw), -1); len(bad) > 0 {
		t.Errorf("comparison json uses Go field names %v, want snake_case", bad)
	}
}
func TestCompareCommandWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	baseline := filepath.Join(dir, "baseline.json")
	candidate := filepath.Join(dir, "candidate.json")
	jsonOut := filepath.Join(dir, "compare.json")
	htmlOut := filepath.Join(dir, "compare.html")

	if err := os.WriteFile(baseline, []byte(fixedResultJSON(t, 100)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(candidate, []byte(fixedResultJSON(t, 100)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code := compareCommand([]string{
		baseline, candidate, "--no-color", "--json", jsonOut, "--html", htmlOut,
	})
	if code != exitPass {
		t.Fatalf("code = %d, want %d", code, exitPass)
	}

	raw, err := os.ReadFile(jsonOut)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	if !strings.Contains(string(raw), `"baseline"`) || !strings.Contains(string(raw), `"verdict"`) {
		t.Errorf("json = %s, want the comparison", raw)
	}

	html, err := os.ReadFile(htmlOut)
	if err != nil {
		t.Fatalf("read html: %v", err)
	}
	if !strings.Contains(string(html), "<h1>Regression comparison</h1>") {
		t.Error("html report is missing its heading")
	}
	if strings.Contains(string(html), `src="http`) || strings.Contains(string(html), `href="http`) {
		t.Error("html report should be self-contained")
	}
}
