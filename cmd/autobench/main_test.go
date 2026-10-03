package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
)

// runFlags mirrors the variables runCommand binds its flags to.
type runFlags struct {
	configPath string
	target     string
	rate       int
	duration   string
	vus        int
	maxVUs     int
	timeUnit   string
	k6Path     string
	scriptPath string
	jsonOut    string
	noColor    bool
}

// newRunFlagSet mirrors the flags runCommand registers, so parseFlags is
// exercised against the real names.
func newRunFlagSet(v *runFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&v.configPath, "config", "", "")
	fs.StringVar(&v.configPath, "c", "", "")
	fs.StringVar(&v.target, "target", "", "")
	fs.IntVar(&v.rate, "rate", 0, "")
	fs.StringVar(&v.duration, "duration", "", "")
	fs.IntVar(&v.vus, "vus", 0, "")
	fs.IntVar(&v.maxVUs, "max-vus", 0, "")
	fs.StringVar(&v.timeUnit, "time-unit", "", "")
	fs.StringVar(&v.k6Path, "k6", "", "")
	fs.StringVar(&v.scriptPath, "script", "", "")
	fs.StringVar(&v.jsonOut, "json", "", "")
	fs.BoolVar(&v.noColor, "no-color", false, "")
	return fs
}

// TestParseFlagsAfterConfigPath is the regression test for the bug where Go's
// flag package stopped at the positional config path and silently ignored every
// flag that followed it.
func TestParseFlagsAfterConfigPath(t *testing.T) {
	var v runFlags
	fs := newRunFlagSet(&v)

	args := []string{"examples/api.yaml", "--target", "http://127.0.0.1:18080",
		"--rate", "200", "--duration", "6s", "--no-color", "--json", "/tmp/out.json"}
	if code := parseFlags(fs, args, &v.configPath); code != exitPass {
		t.Fatalf("parseFlags = %d, want %d", code, exitPass)
	}

	if v.configPath != "examples/api.yaml" {
		t.Errorf("configPath = %q, want examples/api.yaml", v.configPath)
	}
	if v.target != "http://127.0.0.1:18080" {
		t.Errorf("target = %q, want the override to be applied", v.target)
	}
	if v.rate != 200 {
		t.Errorf("rate = %d, want 200", v.rate)
	}
	if v.duration != "6s" {
		t.Errorf("duration = %q, want 6s", v.duration)
	}
	if !v.noColor {
		t.Error("noColor = false, want true")
	}
	if v.jsonOut != "/tmp/out.json" {
		t.Errorf("jsonOut = %q, want /tmp/out.json", v.jsonOut)
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, name := range []string{"target", "rate", "duration", "no-color", "json"} {
		if !set[name] {
			t.Errorf("flag %q not reported as set", name)
		}
	}
}

func TestParseFlagsBeforeConfigPath(t *testing.T) {
	var v runFlags
	fs := newRunFlagSet(&v)

	if code := parseFlags(fs, []string{"--rate", "50", "bench.yaml"}, &v.configPath); code != exitPass {
		t.Fatalf("parseFlags = %d, want %d", code, exitPass)
	}
	if v.configPath != "bench.yaml" || v.rate != 50 {
		t.Errorf("configPath = %q, rate = %d; want bench.yaml, 50", v.configPath, v.rate)
	}
}

func TestParseFlagsConfigFlagWins(t *testing.T) {
	var v runFlags
	fs := newRunFlagSet(&v)

	if code := parseFlags(fs, []string{"-c", "explicit.yaml", "--rate", "10"}, &v.configPath); code != exitPass {
		t.Fatalf("parseFlags = %d, want %d", code, exitPass)
	}
	if v.configPath != "explicit.yaml" || v.rate != 10 {
		t.Errorf("configPath = %q, rate = %d; want explicit.yaml, 10", v.configPath, v.rate)
	}
}

func TestParseFlagsRejectsExtraPositional(t *testing.T) {
	var v runFlags
	fs := newRunFlagSet(&v)

	if code := parseFlags(fs, []string{"one.yaml", "two.yaml"}, &v.configPath); code != exitError {
		t.Errorf("parseFlags = %d, want %d for a second positional", code, exitError)
	}
}

func TestParseFlagsRejectsUnknownFlag(t *testing.T) {
	var v runFlags
	fs := newRunFlagSet(&v)

	if code := parseFlags(fs, []string{"--nope"}, &v.configPath); code != exitError {
		t.Errorf("parseFlags = %d, want %d for an unknown flag", code, exitError)
	}
}

func writeBenchConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bench.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestBuildConfigOverridesOnlySetFlags: an unset flag must leave the config
// file's value alone, while a set flag replaces it.
func TestBuildConfigOverridesOnlySetFlags(t *testing.T) {
	path := writeBenchConfig(t, `
name: from-file
target:
  base_url: http://localhost:3000
run:
  rate: 100
  duration: 30s
`)

	cfg, code := buildConfig(path, config.ModeFixed,
		map[string]bool{"rate": true, "duration": true},
		overrides{rate: 250, duration: "5s"})
	if code != exitPass {
		t.Fatalf("buildConfig code = %d, want %d", code, exitPass)
	}
	if cfg.Run.Rate != 250 {
		t.Errorf("rate = %d, want the override 250", cfg.Run.Rate)
	}
	if cfg.Run.Duration.Duration != 5*time.Second {
		t.Errorf("duration = %v, want the override 5s", cfg.Run.Duration.Duration)
	}
	if cfg.Target.BaseURL != "http://localhost:3000" {
		t.Errorf("base_url = %q, want the file value untouched", cfg.Target.BaseURL)
	}
	if cfg.Name != "from-file" {
		t.Errorf("name = %q, want from-file", cfg.Name)
	}
}

func TestBuildConfigWithoutFileUsesFlags(t *testing.T) {
	cfg, code := buildConfig("", config.ModeFixed,
		map[string]bool{"target": true, "rate": true, "duration": true},
		overrides{target: "http://localhost:9999", rate: 50, duration: "3s"})
	if code != exitPass {
		t.Fatalf("buildConfig code = %d, want %d", code, exitPass)
	}
	if cfg.Target.BaseURL != "http://localhost:9999" || cfg.Run.Rate != 50 {
		t.Errorf("unexpected config: target=%q rate=%d", cfg.Target.BaseURL, cfg.Run.Rate)
	}
	if cfg.Run.Duration.Duration != 3*time.Second {
		t.Errorf("duration = %v, want 3s", cfg.Run.Duration.Duration)
	}
	// Defaults still apply: a workload and a VU pool are synthesised.
	if len(cfg.Workload.Requests) != 1 || cfg.LoadGenerator.MaxVUs == 0 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestBuildConfigRejectsBadDuration(t *testing.T) {
	_, code := buildConfig("", config.ModeFixed,
		map[string]bool{"target": true, "rate": true, "duration": true},
		overrides{target: "http://localhost:9999", rate: 50, duration: "soon"})
	if code != exitError {
		t.Errorf("buildConfig code = %d, want %d", code, exitError)
	}
}

func TestBuildConfigMissingFile(t *testing.T) {
	_, code := buildConfig(filepath.Join(t.TempDir(), "nope.yaml"), config.ModeFixed, nil, overrides{})
	if code != exitError {
		t.Errorf("buildConfig code = %d, want %d", code, exitError)
	}
}

// TestBuildConfigValidates: a config with no target must be rejected before k6
// is ever spawned.
func TestBuildConfigValidates(t *testing.T) {
	_, code := buildConfig("", config.ModeFixed,
		map[string]bool{"rate": true, "duration": true},
		overrides{rate: 50, duration: "3s"})
	if code != exitError {
		t.Errorf("buildConfig code = %d, want %d", code, exitError)
	}
}

// TestBuildConfigCapacityFromFlagsOnly: a capacity search needs no config file
// at all, and the fixed-rate requirements (run.rate, run.duration) must not
// apply.
func TestBuildConfigCapacityFromFlagsOnly(t *testing.T) {
	cfg, code := buildConfig("", config.ModeCapacity,
		map[string]bool{"target": true, "start-rate": true, "probe-duration": true},
		overrides{target: "http://localhost:9999", startRate: 500, probeDuration: "20s"})
	if code != exitPass {
		t.Fatalf("buildConfig code = %d, want %d", code, exitPass)
	}
	if cfg.Search.StartRate != 500 {
		t.Errorf("start_rate = %d, want 500", cfg.Search.StartRate)
	}
	if cfg.Search.ProbeDuration.Duration != 20*time.Second {
		t.Errorf("probe_duration = %v, want 20s", cfg.Search.ProbeDuration.Duration)
	}
	if cfg.Run.Rate != 0 {
		t.Errorf("run.rate = %d, want 0 (unused by a capacity search)", cfg.Run.Rate)
	}
	// Search defaults fill in the rest.
	if cfg.Search.IncreaseFactor != config.DefaultIncreaseFactor {
		t.Errorf("increase_factor = %g, want the default %g",
			cfg.Search.IncreaseFactor, config.DefaultIncreaseFactor)
	}
	if cfg.Search.Resolution != config.DefaultResolution {
		t.Errorf("resolution = %d, want the default %d",
			cfg.Search.Resolution, config.DefaultResolution)
	}
}

// TestBuildConfigCapacityFlagBeatsFile: a flag must be able to supply what the
// config file omits, which is why the file is decoded before it is validated.
func TestBuildConfigCapacityFlagBeatsFile(t *testing.T) {
	path := writeBenchConfig(t, `
target:
  base_url: http://localhost:3000
`)

	if _, code := buildConfig(path, config.ModeCapacity, nil, overrides{}); code != exitError {
		t.Fatalf("buildConfig code = %d, want %d without a start rate", code, exitError)
	}

	cfg, code := buildConfig(path, config.ModeCapacity,
		map[string]bool{"start-rate": true}, overrides{startRate: 250})
	if code != exitPass {
		t.Fatalf("buildConfig code = %d, want %d with --start-rate", code, exitPass)
	}
	if cfg.Search.StartRate != 250 {
		t.Errorf("start_rate = %d, want the flag value 250", cfg.Search.StartRate)
	}
}

// TestBuildConfigCapacityUsesRunRateAsStartRate: a config written for a fixed
// run can be reused for a capacity search without repeating the rate.
func TestBuildConfigCapacityUsesRunRateAsStartRate(t *testing.T) {
	path := writeBenchConfig(t, `
target:
  base_url: http://localhost:3000
run:
  rate: 750
  duration: 30s
`)

	cfg, code := buildConfig(path, config.ModeCapacity, nil, overrides{})
	if code != exitPass {
		t.Fatalf("buildConfig code = %d, want %d", code, exitPass)
	}
	if cfg.Search.StartRate != 750 {
		t.Errorf("start_rate = %d, want run.rate 750", cfg.Search.StartRate)
	}
}

// TestBuildConfigCapacityRejectsBadSearchValues covers the mode-aware validation
// of the search block.
func TestBuildConfigCapacityRejectsBadSearchValues(t *testing.T) {
	const target = "http://localhost:9999"
	base := func(extra map[string]bool) map[string]bool {
		set := map[string]bool{"target": true, "start-rate": true}
		for k := range extra {
			set[k] = true
		}
		return set
	}

	tests := []struct {
		name string
		set  map[string]bool
		ov   overrides
	}{
		{name: "increase factor at 1", set: base(map[string]bool{"increase-factor": true}),
			ov: overrides{target: target, startRate: 100, increaseFactor: 1}},
		{name: "negative resolution", set: base(map[string]bool{"resolution": true}),
			ov: overrides{target: target, startRate: 100, resolution: -1}},
		{name: "safety margin above 1", set: base(map[string]bool{"safety-margin": true}),
			ov: overrides{target: target, startRate: 100, safetyMargin: 1.5}},
		{name: "garbage validation rate", set: base(map[string]bool{"validation-rate": true}),
			ov: overrides{target: target, startRate: 100, validationRate: "fast"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, code := buildConfig("", config.ModeCapacity, tc.set, tc.ov); code != exitError {
				t.Errorf("buildConfig code = %d, want %d", code, exitError)
			}
		})
	}
}

// TestParseFlagsCapacityFlags checks the capacity flag set parses flags that
// follow the positional config path, mirroring the fixed-rate regression test.
func TestParseFlagsCapacityFlags(t *testing.T) {
	var ov overrides
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&ov.configPath, "config", "", "")
	fs.StringVar(&ov.configPath, "c", "", "")
	fs.IntVar(&ov.startRate, "start-rate", 0, "")
	fs.StringVar(&ov.probeDuration, "probe-duration", "", "")
	fs.StringVar(&ov.validationRate, "validation-rate", "", "")

	args := []string{"bench.yaml", "--start-rate", "300", "--probe-duration", "15s",
		"--validation-rate", "discovered"}
	if code := parseFlags(fs, args, &ov.configPath); code != exitPass {
		t.Fatalf("parseFlags = %d, want %d", code, exitPass)
	}
	if ov.configPath != "bench.yaml" || ov.startRate != 300 ||
		ov.probeDuration != "15s" || ov.validationRate != "discovered" {
		t.Errorf("unexpected overrides: %+v", ov)
	}
}
