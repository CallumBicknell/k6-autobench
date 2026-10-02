package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads and decodes a YAML config file, rejecting unknown keys, then
// applies defaults and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// ApplyDefaults fills in unset fields with sensible values. It is safe to call
// on a zero Config (used when the CLI is driven purely by flags).
func (c *Config) ApplyDefaults() {
	if c.Workload.Type == "" {
		c.Workload.Type = "http"
	}
	if len(c.Workload.Requests) == 0 {
		c.Workload.Requests = []Request{{Name: "root", Method: "GET", Path: "/", Weight: 1}}
	}
	for i := range c.Workload.Requests {
		r := &c.Workload.Requests[i]
		if r.Method == "" {
			r.Method = "GET"
		}
		r.Method = strings.ToUpper(r.Method)
		if r.Path == "" {
			r.Path = "/"
		}
		if !strings.HasPrefix(r.Path, "/") {
			r.Path = "/" + r.Path
		}
		if r.Name == "" {
			r.Name = r.Method + " " + r.Path
		}
		if r.Weight <= 0 {
			r.Weight = 1
		}
	}

	if c.Run.TimeUnit == "" {
		c.Run.TimeUnit = "1s"
	}

	c.Percentiles = unionPercentiles(c.Percentiles, latencyPercentiles(c.Thresholds.Latency))
	if len(c.Percentiles) == 0 {
		c.Percentiles = DefaultPercentiles()
	}
	sort.Float64s(c.Percentiles)

	if c.LoadGenerator.PreAllocatedVUs <= 0 {
		c.LoadGenerator.PreAllocatedVUs = autoPreAllocatedVUs(c.Run.Rate)
	}
	if c.LoadGenerator.MaxVUs <= 0 {
		max := c.LoadGenerator.PreAllocatedVUs * 4
		if max < 100 {
			max = 100
		}
		c.LoadGenerator.MaxVUs = max
	}
}

// Validate reports configuration problems that should stop a run before k6 is
// ever spawned.
func (c *Config) Validate() error {
	if c.Target.BaseURL == "" {
		return errors.New("target.base_url is required")
	}
	u, err := url.Parse(c.Target.BaseURL)
	if err != nil {
		return fmt.Errorf("target.base_url %q is not a valid URL: %w", c.Target.BaseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("target.base_url %q must include a scheme and host", c.Target.BaseURL)
	}

	if c.Workload.Type != "http" {
		return fmt.Errorf("unsupported workload.type %q (only \"http\" is supported)", c.Workload.Type)
	}

	if c.Run.Rate <= 0 {
		return errors.New("run.rate must be greater than 0")
	}
	if c.Run.Duration.Duration <= 0 {
		return errors.New("run.duration must be greater than 0")
	}
	if c.LoadGenerator.MaxVUs < c.LoadGenerator.PreAllocatedVUs {
		return fmt.Errorf("load_generator.max_vus (%d) must be >= pre_allocated_vus (%d)",
			c.LoadGenerator.MaxVUs, c.LoadGenerator.PreAllocatedVUs)
	}
	return nil
}

// TargetRatePerSecond normalises run.rate to iterations per second, honouring
// a non-default time_unit (e.g. rate 600 per "1m" == 10/s).
func (c *Config) TargetRatePerSecond() float64 {
	secs := timeUnitSeconds(c.Run.TimeUnit)
	if secs <= 0 {
		return float64(c.Run.Rate)
	}
	return float64(c.Run.Rate) / secs
}

// autoPreAllocatedVUs is a rough starting estimate that assumes ~100ms per
// iteration. V0.4 replaces this with adaptive estimation; the controller still
// detects saturation and warns.
func autoPreAllocatedVUs(rate int) int {
	vus := rate / 10
	if vus < 10 {
		vus = 10
	}
	return vus
}

func latencyPercentiles(l LatencyThresholds) []float64 {
	var out []float64
	add := func(p float64, t *LatencyThreshold) {
		if t != nil {
			out = append(out, p)
		}
	}
	add(50, l.P50)
	add(75, l.P75)
	add(90, l.P90)
	add(95, l.P95)
	add(99, l.P99)
	add(99.9, l.P999)
	return out
}

// timeUnitSeconds converts a k6 timeUnit string such as "1s" or "1m" into
// seconds, falling back to 1s for unparseable input.
func timeUnitSeconds(unit string) float64 {
	d, err := time.ParseDuration(unit)
	if err != nil || d <= 0 {
		return 1
	}
	return d.Seconds()
}

func unionPercentiles(a, b []float64) []float64 {
	seen := make(map[float64]bool, len(a)+len(b))
	out := make([]float64, 0, len(a)+len(b))
	for _, p := range append(append([]float64{}, a...), b...) {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
