package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestDurationJSONRoundTrip is the regression test for a bug that only showed up
// when something tried to read autobench's own JSON back: Duration marshalled to
// a string but could not unmarshal from one, so `--json` was write-only.
func TestDurationJSONRoundTrip(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{`"30s"`, 30 * time.Second},
		{`"1m30s"`, 90 * time.Second},
		{`"500ms"`, 500 * time.Millisecond},
		{`5`, 5 * time.Second},
		{`0.5`, 500 * time.Millisecond},
	}

	for _, tc := range tests {
		var d Duration
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Errorf("unmarshal %s: %v", tc.in, err)
			continue
		}
		if d.Duration != tc.want {
			t.Errorf("unmarshal %s = %v, want %v", tc.in, d.Duration, tc.want)
		}

		// And the value it writes can be read again.
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var back Duration
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Errorf("round trip of %v: %v", d.Duration, err)
			continue
		}
		if back.Duration != d.Duration {
			t.Errorf("round trip = %v, want %v", back.Duration, d.Duration)
		}
	}
}

func TestDurationJSONRejectsGarbage(t *testing.T) {
	for _, in := range []string{`"soon"`, `""`, `{}`, `[]`, `true`} {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("unmarshal %s should have failed, got %v", in, d.Duration)
		}
	}
}

// TestDurationUnmarshal covers the YAML form, which accepts the same shapes.
func TestDurationUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{`"30s"`, 30 * time.Second},
		{`"2m"`, 2 * time.Minute},
		{`"1m30s"`, 90 * time.Second},
		{`"500ms"`, 500 * time.Millisecond},
		{`"1us"`, time.Microsecond},
		{`5`, 5 * time.Second}, // bare number == seconds
		{`1.5`, 1500 * time.Millisecond},
	} {
		var d Duration
		if err := yaml.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Errorf("Unmarshal(%s): %v", tc.in, err)
			continue
		}
		if d.Duration != tc.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tc.in, d.Duration, tc.want)
		}
	}
}

func TestDurationUnmarshalRejectsGarbage(t *testing.T) {
	for _, in := range []string{`"soon"`, `"3 fortnights"`, `[]`} {
		var d Duration
		if err := yaml.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want error", in)
		}
	}
}

func TestApplyDefaultsFillsWorkload(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()

	if cfg.Workload.Type != "http" {
		t.Errorf("workload.type = %q, want http", cfg.Workload.Type)
	}
	if len(cfg.Workload.Requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(cfg.Workload.Requests))
	}
	r := cfg.Workload.Requests[0]
	if r.Method != "GET" || r.Path != "/" || r.Weight != 1 {
		t.Errorf("default request = %+v", r)
	}
	if cfg.Run.TimeUnit != "1s" {
		t.Errorf("run.time_unit = %q, want 1s", cfg.Run.TimeUnit)
	}
}

func TestApplyDefaultsNormalisesRequests(t *testing.T) {
	cfg := &Config{
		Workload: Workload{Requests: []Request{
			{Method: "post", Path: "posts", Weight: 0},
		}},
	}
	cfg.ApplyDefaults()

	r := cfg.Workload.Requests[0]
	if r.Method != "POST" {
		t.Errorf("method = %q, want POST", r.Method)
	}
	if r.Path != "/posts" {
		t.Errorf("path = %q, want /posts", r.Path)
	}
	if r.Weight != 1 {
		t.Errorf("weight = %d, want 1", r.Weight)
	}
	if r.Name != "POST /posts" {
		t.Errorf("name = %q, want %q", r.Name, "POST /posts")
	}
}

// TestApplyDefaultsPercentilesIncludeThresholds: a percentile named only in a
// threshold must still be collected, or the threshold could never be evaluated.
func TestApplyDefaultsPercentilesIncludeThresholds(t *testing.T) {
	cfg := &Config{
		Thresholds: Thresholds{Latency: LatencyThresholds{
			P90: &LatencyThreshold{Max: Duration{Duration: time.Second}},
		}},
		Percentiles: []float64{99},
	}
	cfg.ApplyDefaults()

	want := map[float64]bool{90: true, 99: true}
	if len(cfg.Percentiles) != len(want) {
		t.Fatalf("percentiles = %v, want exactly %v", cfg.Percentiles, want)
	}
	for _, p := range cfg.Percentiles {
		if !want[p] {
			t.Errorf("unexpected percentile %v", p)
		}
	}
	for i := 1; i < len(cfg.Percentiles); i++ {
		if cfg.Percentiles[i-1] >= cfg.Percentiles[i] {
			t.Errorf("percentiles not sorted: %v", cfg.Percentiles)
		}
	}
}

func TestApplyDefaultsSizesVUPool(t *testing.T) {
	cfg := &Config{Run: Run{Rate: 500}}
	cfg.ApplyDefaults()

	if cfg.LoadGenerator.PreAllocatedVUs != 50 {
		t.Errorf("pre_allocated_vus = %d, want 50", cfg.LoadGenerator.PreAllocatedVUs)
	}
	if cfg.LoadGenerator.MaxVUs != 200 {
		t.Errorf("max_vus = %d, want 200", cfg.LoadGenerator.MaxVUs)
	}

	small := &Config{Run: Run{Rate: 10}}
	small.ApplyDefaults()
	if small.LoadGenerator.PreAllocatedVUs != 10 {
		t.Errorf("pre_allocated_vus = %d, want the 10 floor", small.LoadGenerator.PreAllocatedVUs)
	}
	if small.LoadGenerator.MaxVUs != 100 {
		t.Errorf("max_vus = %d, want the 100 floor", small.LoadGenerator.MaxVUs)
	}
}

func TestValidate(t *testing.T) {
	valid := func() *Config {
		cfg := &Config{
			Target: Target{BaseURL: "http://localhost:3000"},
			Run:    Run{Rate: 100, Duration: Duration{Duration: 10 * time.Second}},
		}
		cfg.ApplyDefaults()
		return cfg
	}

	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"missing base_url", func(c *Config) { c.Target.BaseURL = "" }, "base_url is required"},
		{"no scheme", func(c *Config) { c.Target.BaseURL = "localhost:3000" }, "scheme and host"},
		{"bad workload type", func(c *Config) { c.Workload.Type = "grpc" }, "unsupported workload.type"},
		{"zero rate", func(c *Config) { c.Run.Rate = 0 }, "run.rate"},
		{"zero duration", func(c *Config) { c.Run.Duration = Duration{} }, "run.duration"},
		{"max_vus below pre_allocated", func(c *Config) {
			c.LoadGenerator = LoadGenerator{PreAllocatedVUs: 10, MaxVUs: 5}
		}, "must be >= pre_allocated_vus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestTargetRatePerSecond(t *testing.T) {
	for _, tc := range []struct {
		rate int
		unit string
		want float64
	}{
		{100, "1s", 100},
		{600, "1m", 10},
		{3000, "1m", 50},
		{100, "", 100}, // empty falls back to 1s
		{100, "nonsense", 100},
	} {
		cfg := &Config{Run: Run{Rate: tc.rate, TimeUnit: tc.unit}}
		if got := cfg.TargetRatePerSecond(); got != tc.want {
			t.Errorf("rate %d/%q = %v, want %v", tc.rate, tc.unit, got, tc.want)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "autobench.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadValidConfig(t *testing.T) {
	path := writeConfig(t, `
name: demo
target:
  base_url: http://localhost:8080
workload:
  requests:
    - name: feed
      method: get
      path: /feed
      weight: 3
run:
  rate: 200
  duration: 15s
thresholds:
  error_rate:
    max: 0.005
  latency:
    p95:
      max: 250ms
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "demo" || cfg.Target.BaseURL != "http://localhost:8080" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if cfg.Run.Duration.Duration != 15*time.Second {
		t.Errorf("duration = %v, want 15s", cfg.Run.Duration.Duration)
	}
	if cfg.Workload.Requests[0].Method != "GET" {
		t.Errorf("method = %q, want GET", cfg.Workload.Requests[0].Method)
	}
	if cfg.Thresholds.ErrorRate == nil || cfg.Thresholds.ErrorRate.Max != 0.005 {
		t.Errorf("error_rate threshold = %+v", cfg.Thresholds.ErrorRate)
	}
	if cfg.LoadGenerator.PreAllocatedVUs != 20 {
		t.Errorf("pre_allocated_vus = %d, want 20 (auto-sized from rate 200)",
			cfg.LoadGenerator.PreAllocatedVUs)
	}
	// No `percentiles:` key and only a p95 threshold, so p95 is the sole
	// collected percentile: explicit/threshold percentiles win over defaults,
	// and the defaults only apply when nothing at all is requested.
	if len(cfg.Percentiles) != 1 || cfg.Percentiles[0] != 95 {
		t.Errorf("percentiles = %v, want exactly [95]", cfg.Percentiles)
	}
}

func TestLoadDefaultsPercentilesWhenNoneRequested(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:8080
run:
  rate: 10
  duration: 5s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := DefaultPercentiles()
	if len(cfg.Percentiles) != len(want) {
		t.Fatalf("percentiles = %v, want %v", cfg.Percentiles, want)
	}
	for i := range want {
		if cfg.Percentiles[i] != want[i] {
			t.Fatalf("percentiles = %v, want %v", cfg.Percentiles, want)
		}
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:8080
run:
  rate: 10
  duration: 5s
  bogus_option: true
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil, want unknown-field error")
	}
	if !strings.Contains(err.Error(), "bogus_option") {
		t.Errorf("error = %v, want it to name the unknown field", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load() = nil for a missing file, want error")
	}
}

func TestApplyDefaultsSearchDefaults(t *testing.T) {
	cfg := &Config{Run: Run{Rate: 1000}}
	cfg.ApplyDefaults()

	s := cfg.Search
	if s.Strategy != "adaptive" {
		t.Errorf("strategy = %q, want adaptive", s.Strategy)
	}
	if s.StartRate != 1000 {
		t.Errorf("start_rate = %d, want run.rate 1000", s.StartRate)
	}
	if s.IncreaseFactor != DefaultIncreaseFactor {
		t.Errorf("increase_factor = %g, want %g", s.IncreaseFactor, DefaultIncreaseFactor)
	}
	if s.Resolution != DefaultResolution {
		t.Errorf("resolution = %d, want %d", s.Resolution, DefaultResolution)
	}
	if s.ProbeDuration.Duration != DefaultProbeDuration {
		t.Errorf("probe_duration = %v, want %v", s.ProbeDuration, DefaultProbeDuration)
	}
	if s.ConfirmationRuns != 1 || s.FailureConfirmation != 1 {
		t.Errorf("confirmation = %d/%d, want 1/1", s.ConfirmationRuns, s.FailureConfirmation)
	}
	if s.MaxProbes != DefaultMaxProbes {
		t.Errorf("max_probes = %d, want %d", s.MaxProbes, DefaultMaxProbes)
	}
	// Validation is opt-in: a search without one still reports a verdict from
	// its own findings.
	if cfg.Validation.Duration.Duration != 0 {
		t.Errorf("validation.duration = %v, want 0 (disabled)", cfg.Validation.Duration)
	}
}

// TestApplyDefaultsKeepsPinnedVUsAcrossCalls guards a real bug: the second
// ApplyDefaults (which buildConfig performs after applying flag overrides) saw
// the auto-sized pool it had just filled in and mistook it for a user choice,
// which disabled per-probe sizing for the whole search.
func TestApplyDefaultsKeepsPinnedVUsAcrossCalls(t *testing.T) {
	cfg := &Config{Run: Run{Rate: 500}}
	cfg.ApplyDefaults()
	cfg.ApplyDefaults()

	if got := cfg.ForRate(5000, 0).LoadGenerator.PreAllocatedVUs; got != 500 {
		t.Errorf("pre_allocated_vus = %d, want 500 (auto-sized for the probe rate)", got)
	}

	pinned := &Config{Run: Run{Rate: 500}, LoadGenerator: LoadGenerator{PreAllocatedVUs: 7, MaxVUs: 9}}
	pinned.ApplyDefaults()
	pinned.ApplyDefaults()

	if got := pinned.ForRate(5000, 0).LoadGenerator; got.PreAllocatedVUs != 7 || got.MaxVUs != 9 {
		t.Errorf("pool = %d/%d, want the pinned 7/9", got.PreAllocatedVUs, got.MaxVUs)
	}
}

func TestForRateSizesPoolFromLatency(t *testing.T) {
	cfg := &Config{Run: Run{Rate: 500}}
	cfg.ApplyDefaults()

	// No observed latency yet: fall back to the 100ms assumption.
	if got := cfg.ForRate(2000, 0).LoadGenerator; got.PreAllocatedVUs != 200 || got.MaxVUs != 800 {
		t.Errorf("pool = %d/%d, want 200/800", got.PreAllocatedVUs, got.MaxVUs)
	}
	// A slow system needs more concurrency for the same rate.
	if got := cfg.ForRate(2000, 300).LoadGenerator; got.PreAllocatedVUs != 600 || got.MaxVUs != 2400 {
		t.Errorf("pool = %d/%d, want 600/2400", got.PreAllocatedVUs, got.MaxVUs)
	}
	// The original config must be untouched.
	if cfg.LoadGenerator.PreAllocatedVUs != 50 {
		t.Errorf("original pool changed to %d", cfg.LoadGenerator.PreAllocatedVUs)
	}
}

func TestForRatePinsRateAndTimeUnit(t *testing.T) {
	cfg := &Config{Run: Run{Rate: 500, TimeUnit: "1m"}}
	cfg.ApplyDefaults()

	probe := cfg.ForRate(300, 0)
	if probe.Run.Rate != 300 || probe.Run.TimeUnit != "1s" {
		t.Errorf("probe run = %d/%s, want 300/1s", probe.Run.Rate, probe.Run.TimeUnit)
	}
	if cfg.Run.Rate != 500 || cfg.Run.TimeUnit != "1m" {
		t.Errorf("original run changed to %d/%s", cfg.Run.Rate, cfg.Run.TimeUnit)
	}
}

func TestForRateHonoursPinnedPool(t *testing.T) {
	cfg := &Config{Run: Run{Rate: 500}, LoadGenerator: LoadGenerator{PreAllocatedVUs: 40, MaxVUs: 120}}
	cfg.ApplyDefaults()

	if got := cfg.ForRate(20000, 0).LoadGenerator; got.PreAllocatedVUs != 40 || got.MaxVUs != 120 {
		t.Errorf("pool = %d/%d, want the pinned 40/120", got.PreAllocatedVUs, got.MaxVUs)
	}
}

// TestForRateHonoursExplicitMaxOnly: a user ceiling must win over the estimate,
// since k6 requires pre-allocated <= max.
func TestForRateHonoursExplicitMaxOnly(t *testing.T) {
	cfg := &Config{Run: Run{Rate: 500}, LoadGenerator: LoadGenerator{MaxVUs: 300}}
	cfg.ApplyDefaults()

	got := cfg.ForRate(5000, 0).LoadGenerator
	if got.MaxVUs != 300 {
		t.Errorf("max_vus = %d, want the pinned 300", got.MaxVUs)
	}
	if got.PreAllocatedVUs != 300 {
		t.Errorf("pre_allocated_vus = %d, want it clamped to the ceiling 300", got.PreAllocatedVUs)
	}
}

func TestVULimits(t *testing.T) {
	unpinned := &Config{Run: Run{Rate: 100}}
	unpinned.ApplyDefaults()
	if pre, max := unpinned.VULimits(); pre != 0 || max != 0 {
		t.Errorf("limits = %d/%d, want 0/0 for an auto-sized pool", pre, max)
	}

	pinned := &Config{Run: Run{Rate: 100}, LoadGenerator: LoadGenerator{PreAllocatedVUs: 20, MaxVUs: 80}}
	pinned.ApplyDefaults()
	if pre, max := pinned.VULimits(); pre != 20 || max != 80 {
		t.Errorf("limits = %d/%d, want the pinned 20/80", pre, max)
	}

	partial := &Config{Run: Run{Rate: 100}, LoadGenerator: LoadGenerator{MaxVUs: 80}}
	partial.ApplyDefaults()
	if pre, max := partial.VULimits(); pre != 0 || max != 80 {
		t.Errorf("limits = %d/%d, want 0/80 for a pinned ceiling only", pre, max)
	}
}

func TestValidateModeCapacity(t *testing.T) {
	valid := func() *Config {
		cfg := &Config{Target: Target{BaseURL: "http://localhost:1"}}
		cfg.ApplyDefaults()
		cfg.Search.StartRate = 500
		return cfg
	}

	if err := valid().ValidateMode(ModeCapacity); err != nil {
		t.Errorf("ValidateMode(capacity) = %v, want nil", err)
	}
	// A capacity search needs no run.rate or run.duration.
	if err := valid().ValidateMode(ModeFixed); err == nil {
		t.Error("ValidateMode(fixed) = nil, want an error for a missing run.rate")
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "no start rate", mutate: func(c *Config) { c.Search.StartRate = 0; c.Run.Rate = 0 }},
		{name: "increase factor at 1", mutate: func(c *Config) { c.Search.IncreaseFactor = 1 }},
		{name: "negative resolution", mutate: func(c *Config) { c.Search.Resolution = -1 }},
		{name: "zero probe duration", mutate: func(c *Config) { c.Search.ProbeDuration.Duration = 0 }},
		{name: "negative warmup", mutate: func(c *Config) { c.Search.Warmup.Duration = -time.Second }},
		{name: "zero confirmation runs", mutate: func(c *Config) { c.Search.ConfirmationRuns = 0 }},
		{name: "negative max rate", mutate: func(c *Config) { c.Search.MaxRate = -1 }},
		{name: "negative max probes", mutate: func(c *Config) { c.Search.MaxProbes = -1 }},
		{name: "negative vu growth", mutate: func(c *Config) { c.Search.MaxVUGrowth = -1 }},
		{name: "safety margin above 1", mutate: func(c *Config) { c.Validation.SafetyMargin = 1.2 }},
		{name: "unknown strategy", mutate: func(c *Config) { c.Search.Strategy = "magic" }},
		{name: "unparseable validation rate", mutate: func(c *Config) { c.Validation.Rate = "fast" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			if err := cfg.ValidateMode(ModeCapacity); err == nil {
				t.Error("ValidateMode(capacity) = nil, want an error")
			}
		})
	}
}

func TestValidateModeUnknown(t *testing.T) {
	cfg := &Config{Target: Target{BaseURL: "http://localhost:1"}}
	cfg.ApplyDefaults()
	if err := cfg.ValidateMode(Mode("sideways")); err == nil {
		t.Error("ValidateMode(sideways) = nil, want an error")
	}
}

// TestValidateAnalysis covers the windowed-analysis settings, which apply to
// every measurement in every mode.
func TestValidateAnalysis(t *testing.T) {
	build := func(mutate func(*Config)) *Config {
		cfg := &Config{
			Target: Target{BaseURL: "http://localhost:1"},
			Run:    Run{Rate: 100, Duration: Duration{Duration: 10 * time.Second}},
		}
		cfg.ApplyDefaults()
		mutate(cfg)
		return cfg
	}

	// Off by default, and valid in both modes.
	plain := build(func(*Config) {})
	if plain.Analysis.Window.Duration != 0 {
		t.Errorf("analysis.window defaults to %v, want 0 (off)", plain.Analysis.Window)
	}
	for _, mode := range []Mode{ModeFixed, ModeCapacity} {
		if err := plain.ValidateMode(mode); err != nil {
			t.Errorf("ValidateMode(%s) = %v, want nil", mode, err)
		}
	}

	// A window alone is fine.
	windowed := build(func(c *Config) { c.Analysis.Window = Duration{Duration: 10 * time.Second} })
	if err := windowed.ValidateMode(ModeFixed); err != nil {
		t.Errorf("ValidateMode(fixed) = %v, want nil", err)
	}

	// require_all_windows_pass needs a window to apply to.
	noWindow := build(func(c *Config) { c.Analysis.RequireAllWindowsPass = true })
	if err := noWindow.ValidateMode(ModeFixed); err == nil {
		t.Error("ValidateMode = nil, want an error for require_all_windows_pass with no window")
	}

	negative := build(func(c *Config) { c.Analysis.Window = Duration{Duration: -time.Second} })
	if err := negative.ValidateMode(ModeFixed); err == nil {
		t.Error("ValidateMode = nil, want an error for a negative window")
	}
}

// TestLoadAnalysisBlock checks the YAML surface decodes and reaches validation.
func TestLoadAnalysisBlock(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:1
run:
  rate: 100
  duration: 10s
analysis:
  window: 10s
  require_all_windows_pass: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analysis.Window.Duration != 10*time.Second {
		t.Errorf("window = %v, want 10s", cfg.Analysis.Window)
	}
	if !cfg.Analysis.RequireAllWindowsPass {
		t.Error("require_all_windows_pass = false, want true")
	}
}

// TestLoadRejectsUnsupportedAnalysisKeys keeps typos failing fast.
func TestLoadRejectsUnsupportedAnalysisKeys(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:1
run:
  rate: 100
  duration: 10s
analysis:
  windows: 10s
`)
	if _, err := Load(path); err == nil {
		t.Error("Load = nil, want an error for an unknown analysis key")
	}
}

func TestValidationRateOverride(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "discovered", want: 0},
		{in: "Discovered", want: 0},
		{in: "  discovered  ", want: 0},
		{in: "5000", want: 5000},
		{in: "fast", wantErr: true},
		{in: "0", wantErr: true},
		{in: "-5", wantErr: true},
	}
	for _, tc := range tests {
		cfg := &Config{Validation: Validation{Rate: tc.in}}
		got, err := cfg.ValidationRateOverride()
		if tc.wantErr {
			if err == nil {
				t.Errorf("rate %q: got %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("rate %q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("rate %q: got %d, want %d", tc.in, got, tc.want)
		}
	}
}
