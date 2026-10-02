package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

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
