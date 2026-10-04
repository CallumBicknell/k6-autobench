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
	if len(c.Workload.Requests) == 0 && len(c.Workload.Journey) == 0 {
		c.Workload.Requests = []Request{{Name: "root", Method: "GET", Path: "/", Weight: 1}}
	}
	for _, set := range [][]Request{c.Workload.Requests, c.Workload.Journey} {
		for i := range set {
			r := &set[i]
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
	}

	if c.Run.TimeUnit == "" {
		c.Run.TimeUnit = "1s"
	}

	c.Percentiles = unionPercentiles(c.Percentiles, latencyPercentiles(c.Thresholds.Latency))
	if len(c.Percentiles) == 0 {
		c.Percentiles = DefaultPercentiles()
	}
	// A policy asks for a latency knee, and a knee needs its percentile to have
	// been collected. A config that sets a latency threshold collects only the
	// percentiles that threshold names, so a p95 SLO would leave the knee's
	// default p99 uncollected and the knee silently unfindable.
	if c.OperatingPolicy.KneePolicy() {
		c.Percentiles = unionPercentiles(c.Percentiles,
			[]float64{c.OperatingPolicy.KneePercentileOr()})
	}
	sort.Float64s(c.Percentiles)

	if c.LoadGenerator.PreAllocatedVUs <= 0 {
		c.LoadGenerator.PreAllocatedVUs = c.autoVUsForRate(c.Run.Rate, 0)
	}
	if c.LoadGenerator.MaxVUs <= 0 {
		c.LoadGenerator.MaxVUs = maxVUsFor(c.LoadGenerator.PreAllocatedVUs)
	}

	c.applySearchDefaults()
	c.applyCollectorDefaults()
}

// applyCollectorDefaults fills in the sampling interval, but only when a
// collector is actually configured: a run with no collectors should not claim a
// sampling interval it never used.
func (c *Config) applyCollectorDefaults() {
	if c.Collectors.Enabled() && c.Collectors.Interval.Duration == 0 {
		c.Collectors.Interval.Duration = DefaultCollectInterval
	}
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
	if err := c.validateAnalysis(); err != nil {
		return err
	}
	if c.Run.K6Timeout.Duration < 0 {
		return errors.New("run.k6_timeout must not be negative")
	}
	if err := c.validateCollectors(); err != nil {
		return err
	}
	if err := c.validateOperatingPolicy(); err != nil {
		return err
	}
	if err := c.validateTargetTLS(); err != nil {
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
	return c.validateWorkload()
}

// validateWorkload checks the workload shape. Exactly one of the weighted pool
// and the journey may be set: allowing both would make "what does one iteration
// do?" ambiguous, and the whole point of the distinction is that the report can
// state the iteration and request rates honestly.
func (c *Config) validateWorkload() error {
	if len(c.Workload.Requests) > 0 && len(c.Workload.Journey) > 0 {
		return errors.New("workload.requests and workload.journey are mutually exclusive: " +
			"a weighted pool picks one request per iteration, a journey performs every step")
	}

	seen := map[string]bool{}
	for _, r := range c.Workload.Steps() {
		if seen[r.Name] {
			return fmt.Errorf("duplicate workload request name %q: names tag the metrics, "+
				"so they must be unique", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// validateAnalysis checks the windowed-analysis settings, which apply to every
// measurement in every mode.
func (c *Config) validateAnalysis() error {
	if c.Analysis.Window.Duration < 0 {
		return errors.New("analysis.window must not be negative")
	}
	if c.Analysis.RequireAllWindowsPass && c.Analysis.Window.Duration <= 0 {
		return errors.New("analysis.require_all_windows_pass needs analysis.window to be set")
	}
	return nil
}

// validateCollectors checks the resource-collector settings. These are static
// checks only: whether a source actually answers is discovered at run time and
// reported as a note, because a target's monitoring being unreachable must never
// fail a load test.
// validateTargetTLS rejects a TLS configuration that could not work.
//
// A CA file that is not there is worth catching before the run: it is passed to
// k6 as SSL_CERT_FILE, and a path that does not exist leaves the process with an
// empty trust store, so every request fails verification for a reason that names
// the certificate rather than the typo.
func (c *Config) validateTargetTLS() error {
	if c.Target.RequestTimeout.Duration < 0 {
		return errors.New("target.request_timeout must not be negative")
	}

	ca := strings.TrimSpace(c.Target.TLS.CAFile)
	if ca == "" {
		return nil
	}
	info, err := os.Stat(ca)
	if err != nil {
		return fmt.Errorf("target.tls.ca_file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("target.tls.ca_file %s is a directory, not a PEM bundle", ca)
	}
	return nil
}

// validateOperatingPolicy rejects a policy that could not mean anything.
func (c *Config) validateOperatingPolicy() error {
	p := c.OperatingPolicy
	switch {
	case p.MaxUtilizationOfCapacity < 0:
		return errors.New("operating_policy.max_utilization_of_capacity must not be negative")
	case p.MaxUtilizationOfCapacity > 1:
		return fmt.Errorf(
			"operating_policy.max_utilization_of_capacity is a fraction of capacity, "+
				"so it cannot exceed 1 (got %g)", p.MaxUtilizationOfCapacity)
	case p.KneePercentile < 0 || p.KneePercentile >= 100:
		return fmt.Errorf(
			"operating_policy.knee_percentile must be above 0 and below 100 (got %g)",
			p.KneePercentile)
	case p.KneeLatencyMultiple < 0:
		return errors.New("operating_policy.knee_latency_multiple must not be negative")
	}
	return nil
}

func (c *Config) validateCollectors() error {
	if c.Collectors.Interval.Duration < 0 {
		return errors.New("collectors.interval must not be negative")
	}
	if !c.Collectors.Enabled() {
		return nil
	}

	// A source name labels every series it produces, and two sources sharing one
	// would produce report rows that cannot be told apart. So the names are
	// claimed across every kind of collector at once.
	names := &sourceNames{seen: map[string]bool{}}

	for _, src := range c.Collectors.Prometheus {
		if err := names.claim("collectors.prometheus", src.Name); err != nil {
			return err
		}
		if err := validatePrometheusSource(src); err != nil {
			return err
		}
	}

	for _, src := range c.Collectors.Docker {
		if err := names.claim("collectors.docker", src.Name); err != nil {
			return err
		}
		if strings.TrimSpace(src.Container) == "" {
			return fmt.Errorf("collectors.docker %q has no container", src.Name)
		}
	}

	for _, src := range c.Collectors.System {
		if err := names.claim("collectors.system", src.Name); err != nil {
			return err
		}
	}

	for _, src := range c.Collectors.Kubernetes {
		if err := names.claim("collectors.kubernetes", src.Name); err != nil {
			return err
		}
		if err := validateKubernetesSource(src); err != nil {
			return err
		}
	}
	return nil
}

// sourceNames tracks the names collectors have claimed, so two sources cannot
// share one.
type sourceNames struct{ seen map[string]bool }

func (s *sourceNames) claim(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s[].name is required: it labels every series in the report", kind)
	}
	if s.seen[name] {
		return fmt.Errorf("duplicate collector name %q: it labels every series, "+
			"so it must be unique across every kind of collector", name)
	}
	s.seen[name] = true
	return nil
}

// validateKubernetesSource checks a cluster selection.
func validateKubernetesSource(src KubernetesSource) error {
	if strings.TrimSpace(src.URL) == "" {
		return fmt.Errorf("collectors.kubernetes %q has no url", src.Name)
	}
	u, err := url.Parse(src.URL)
	if err != nil {
		return fmt.Errorf("collectors.kubernetes %q url %q is not valid: %w", src.Name, src.URL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("collectors.kubernetes %q url %q must include a scheme and host",
			src.Name, src.URL)
	}
	if src.Insecure && src.CAFile != "" {
		return fmt.Errorf("collectors.kubernetes %q sets both insecure and ca_file; "+
			"pick the one you mean", src.Name)
	}
	return nil
}

// validatePrometheusSource checks one Prometheus server and its queries.
func validatePrometheusSource(src PrometheusSource) error {
	if err := validatePrometheusURL(src); err != nil {
		return err
	}
	if len(src.Queries) == 0 {
		return fmt.Errorf("collectors.prometheus %q has no queries", src.Name)
	}

	seen := map[string]bool{}
	for _, q := range src.Queries {
		if strings.TrimSpace(q.Name) == "" {
			return fmt.Errorf("collectors.prometheus %q has a query with no name", src.Name)
		}
		if seen[q.Name] {
			return fmt.Errorf("collectors.prometheus %q has two queries named %q", src.Name, q.Name)
		}
		seen[q.Name] = true

		if strings.TrimSpace(q.Query) == "" {
			return fmt.Errorf("collectors.prometheus %q query %q has no expression", src.Name, q.Name)
		}
	}
	return nil
}

// validatePrometheusURL checks a source's base URL, since a typo there is a
// configuration mistake rather than something to discover mid-run.
func validatePrometheusURL(src PrometheusSource) error {
	if src.URL == "" {
		return fmt.Errorf("collectors.prometheus %q has no url", src.Name)
	}
	u, err := url.Parse(src.URL)
	if err != nil {
		return fmt.Errorf("collectors.prometheus %q url %q is not valid: %w", src.Name, src.URL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("collectors.prometheus %q url %q must include a scheme and host",
			src.Name, src.URL)
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
// observedIterationLatencyMs is the per-iteration latency seen by the previous
// probe; 0 means "unknown" and falls back to assuming ~100ms per HTTP call.
// Anything the user pinned explicitly in load_generator is left alone, so an
// explicit max_vus still caps the pool (and surfaces as generator saturation).
func (c *Config) ForRate(rate int, observedIterationLatencyMs float64) *Config {
	cp := *c
	cp.Run.Rate = rate
	cp.Run.TimeUnit = "1s"

	if !c.preAllocatedExplicit {
		cp.LoadGenerator.PreAllocatedVUs = c.autoVUsForRate(rate, observedIterationLatencyMs)
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
// assumes defaultIterationMs, which reproduces the V0.1 rate/10 heuristic for a
// single-request iteration. The capacity search passes the latency it actually
// observed, so the pool tracks a slow system instead of saturating the generator
// spuriously.
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

// autoVUsForRate sizes the pool for a rate, assuming defaultIterationMs per HTTP
// call when nothing has been measured yet. A multi-step journey issues one call
// per step, so it needs proportionally more concurrency for the same iteration
// rate -- without this, a journey saturates the generator on its first probe.
func (c *Config) autoVUsForRate(rate int, iterationLatencyMs float64) int {
	if iterationLatencyMs <= 0 {
		iterationLatencyMs = defaultIterationMs * float64(c.Workload.RequestsPerIteration())
	}
	return autoPreAllocatedVUs(rate, iterationLatencyMs)
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
