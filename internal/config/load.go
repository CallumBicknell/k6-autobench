package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads and decodes a YAML config file, rejecting unknown keys, then
// applies defaults and validates the result for a fixed-rate run.
func Load(path string) (*Config, error) { return LoadMode(path, ModeFixed) }

// LoadMode is Load for a specific mode: the fixed-rate runner and the capacity
// search require different parts of the config, so validation is mode-aware.
func LoadMode(path string, mode Mode) (*Config, error) {
	cfg, err := Decode(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateMode(mode); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Decode reads a YAML config file, rejecting unknown keys, and applies
// defaults. Validation is left to the caller so flag overrides can be applied
// first.
func Decode(path string) (*Config, error) {
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
	return &cfg, nil
}

// ApplyDefaults fills in unset fields with sensible values. It is safe to call
// on a zero Config (used when the CLI is driven purely by flags).
func (c *Config) ApplyDefaults() {
	// Remember which parts of the VU pool the user pinned, so ForRate knows what
	// it may re-size for a probe rate. Only the first call counts: every call
	// after that sees the pool this function filled in, which would otherwise
	// look like a user choice.
	if !c.defaultsApplied {
		c.preAllocatedExplicit = c.LoadGenerator.PreAllocatedVUs > 0
		c.maxVUsExplicit = c.LoadGenerator.MaxVUs > 0
		c.defaultsApplied = true
	}

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
		c.LoadGenerator.PreAllocatedVUs = autoPreAllocatedVUs(c.Run.Rate, 0)
	}
	if c.LoadGenerator.MaxVUs <= 0 {
		c.LoadGenerator.MaxVUs = maxVUsFor(c.LoadGenerator.PreAllocatedVUs)
	}

	c.applySearchDefaults()
}

// applySearchDefaults fills in the capacity-search parameters. start_rate falls
// back to run.rate so a config that already knows its fixed rate can be reused
// for a capacity search unchanged.
//
// Only a zero value counts as "unset": a negative value is left alone so
// ValidateMode can reject it rather than have it silently repaired.
func (c *Config) applySearchDefaults() {
	s := &c.Search
	if s.Strategy == "" {
		s.Strategy = "adaptive"
	}
	if s.StartRate <= 0 {
		s.StartRate = c.Run.Rate
	}
	if s.IncreaseFactor == 0 {
		s.IncreaseFactor = DefaultIncreaseFactor
	}
	if s.Resolution == 0 {
		s.Resolution = DefaultResolution
	}
	if s.ProbeDuration.Duration == 0 {
		s.ProbeDuration.Duration = DefaultProbeDuration
	}
	if s.ConfirmationRuns == 0 {
		s.ConfirmationRuns = 1
	}
	if s.FailureConfirmation == 0 {
		s.FailureConfirmation = 1
	}
	if s.MaxProbes == 0 {
		s.MaxProbes = DefaultMaxProbes
	}
}

// Validate reports configuration problems that should stop a fixed-rate run
// before k6 is ever spawned.
func (c *Config) Validate() error { return c.ValidateMode(ModeFixed) }

// ValidateMode validates the parts of the config the given mode actually needs:
// a fixed-rate run requires run.rate and run.duration, whereas a capacity
// search requires the search block instead.
func (c *Config) ValidateMode(mode Mode) error {
	if err := c.validateTarget(); err != nil {
		return err
	}
	if c.LoadGenerator.MaxVUs < c.LoadGenerator.PreAllocatedVUs {
		return fmt.Errorf("load_generator.max_vus (%d) must be >= pre_allocated_vus (%d)",
			c.LoadGenerator.MaxVUs, c.LoadGenerator.PreAllocatedVUs)
	}

	switch mode {
	case ModeFixed:
		if c.Run.Rate <= 0 {
			return errors.New("run.rate must be greater than 0")
		}
		if c.Run.Duration.Duration <= 0 {
			return errors.New("run.duration must be greater than 0")
		}
	case ModeCapacity:
		return c.validateSearch()
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	return nil
}

// validateTarget checks the parts shared by every mode.
func (c *Config) validateTarget() error {
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
	return nil
}

// validateSearch checks the capacity-search parameters.
func (c *Config) validateSearch() error {
	s := c.Search
	switch {
	case s.Strategy != "" && s.Strategy != "adaptive":
		return fmt.Errorf("unsupported search.strategy %q (only \"adaptive\" is implemented)", s.Strategy)
	case s.StartRate <= 0:
		return errors.New("search.start_rate must be greater than 0 (or set run.rate)")
	case s.IncreaseFactor <= 1:
		return fmt.Errorf("search.increase_factor (%g) must be greater than 1", s.IncreaseFactor)
	case s.Resolution <= 0:
		return errors.New("search.resolution must be greater than 0")
	case s.ProbeDuration.Duration <= 0:
		return errors.New("search.probe_duration must be greater than 0")
	case s.Warmup.Duration < 0:
		return errors.New("search.warmup must not be negative")
	case s.ConfirmationRuns < 1:
		return errors.New("search.confirmation_runs must be at least 1")
	case s.FailureConfirmation < 1:
		return errors.New("search.failure_confirmation must be at least 1")
	case s.MaxRate < 0:
		return errors.New("search.max_rate must not be negative")
	case s.MaxProbes < 1:
		return errors.New("search.max_probes must be at least 1")
	case s.MaxVUGrowth < 0:
		return errors.New("search.max_vu_growth must not be negative")
	case c.Validation.SafetyMargin < 0 || c.Validation.SafetyMargin > 1:
		return fmt.Errorf("validation.safety_margin (%g) must be between 0 and 1", c.Validation.SafetyMargin)
	}
	if _, err := c.ValidationRateOverride(); err != nil {
		return err
	}
	return nil
}

// ForRate returns a copy of the config pinned to a single iteration rate, with
// the load-generator pool sized for that rate. The capacity search uses it to
// turn one config into one probe per rate.
//
// observedLatencyMs is the per-iteration latency seen by the previous probe; 0
// means "unknown" and falls back to the default assumption. Anything the user
// pinned explicitly in load_generator is left alone, so an explicit max_vus
// still caps the pool (and surfaces as generator saturation).
func (c *Config) ForRate(rate int, observedLatencyMs float64) *Config {
	cp := *c
	cp.Run.Rate = rate
	cp.Run.TimeUnit = "1s"

	if !c.preAllocatedExplicit {
		cp.LoadGenerator.PreAllocatedVUs = autoPreAllocatedVUs(rate, observedLatencyMs)
	}
	switch {
	case !c.maxVUsExplicit:
		cp.LoadGenerator.MaxVUs = maxVUsFor(cp.LoadGenerator.PreAllocatedVUs)
	case cp.LoadGenerator.MaxVUs < cp.LoadGenerator.PreAllocatedVUs:
		// Honour the user's ceiling; k6 requires pre-allocated <= max.
		cp.LoadGenerator.PreAllocatedVUs = cp.LoadGenerator.MaxVUs
	}
	return &cp
}

// VULimits returns the ceilings a VU pool may be grown to, or 0 for "unbounded"
// where the user pinned nothing. The capacity search uses it to refuse a
// generator-saturation retry that could not actually give k6 more VUs.
func (c *Config) VULimits() (preAllocated, max int) {
	if c.preAllocatedExplicit {
		preAllocated = c.LoadGenerator.PreAllocatedVUs
	}
	if c.maxVUsExplicit {
		max = c.LoadGenerator.MaxVUs
	}
	return preAllocated, max
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

// autoPreAllocatedVUs estimates a starting VU pool for a rate using Little's
// law: concurrency = rate x latency. A zero latencyMs means "unknown" and
// assumes defaultIterationMs, which reproduces the V0.1 rate/10 heuristic.
// The capacity search passes the latency it actually observed, so the pool
// tracks a slow system instead of saturating the generator spuriously.
func autoPreAllocatedVUs(rate int, latencyMs float64) int {
	if rate <= 0 {
		return 10
	}
	if latencyMs <= 0 {
		latencyMs = defaultIterationMs
	}
	vus := int(math.Ceil(float64(rate) * latencyMs / 1000))
	if vus < 10 {
		vus = 10
	}
	return vus
}

// maxVUsFor derives a VU ceiling from a pre-allocated pool, leaving k6 room to
// grow when an iteration blocks on a slow response.
func maxVUsFor(preAllocated int) int {
	max := preAllocated * 4
	if max < 100 {
		max = 100
	}
	return max
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
