// Package config defines the autobench YAML configuration model and the
// defaults/validation applied before a run starts.
package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that decodes from human-friendly strings such as
// "30s" or "2m" and, for convenience, from bare numbers interpreted as seconds.
type Duration struct{ time.Duration }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	// yaml.v3 happily decodes a bare scalar like 5 into a string, so check the
	// resolved tag first: numeric nodes are seconds, everything else is a
	// time.ParseDuration string such as "30s".
	if value.Tag == "!!int" || value.Tag == "!!float" {
		var secs float64
		if err := value.Decode(&secs); err != nil {
			return fmt.Errorf("invalid duration %q", value.Value)
		}
		d.Duration = time.Duration(secs * float64(time.Second))
		return nil
	}

	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("invalid duration %q", value.Value)
	}
	parsed, perr := time.ParseDuration(strings.TrimSpace(s))
	if perr != nil {
		return fmt.Errorf("invalid duration %q: %w", s, perr)
	}
	d.Duration = parsed
	return nil
}

// MarshalJSON renders the duration using Go's compact form (e.g. "1m30s").
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// UnmarshalJSON reads the compact form back, and accepts a bare number as
// seconds the way the YAML decoder does.
//
// Without this the JSON report was write-only: `--json` produced a file nothing
// could read back, which only shows up when something tries to -- as `autobench
// compare` does.
func (d *Duration) UnmarshalJSON(b []byte) error {
	// A JSON number is seconds.
	var secs float64
	if err := json.Unmarshal(b, &secs); err == nil {
		d.Duration = time.Duration(secs * float64(time.Second))
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("invalid duration %s", b)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// DefaultPercentiles are collected when the config specifies none.
func DefaultPercentiles() []float64 { return []float64{50, 75, 90, 95, 99, 99.9} }

// Capacity-search defaults. They are deliberately conservative: a search with
// the defaults brackets, refines and finishes in a few minutes.
const (
	// DefaultIncreaseFactor multiplies the rate after each passing probe.
	DefaultIncreaseFactor = 1.5
	// DefaultResolution is how narrow the final bracket must be, in iterations
	// per second.
	DefaultResolution = 50
	// DefaultProbeDuration is how long each measurement lasts.
	DefaultProbeDuration = 60 * time.Second
	// DefaultMaxProbes caps the number of measurements a search may take.
	DefaultMaxProbes = 32
	// defaultIterationMs is the per-iteration latency assumed when sizing the VU
	// pool before anything has been measured.
	defaultIterationMs = 100
	// DefaultCollectInterval is how often collector queries are sampled when the
	// configuration does not say. It is deliberately longer than a typical
	// window, so a reading lands in every window without hammering the
	// monitoring server.
	DefaultCollectInterval = 5 * time.Second
)

// Config is the root of an autobench configuration file.
type Config struct {
	Name          string        `yaml:"name"`
	Target        Target        `yaml:"target"`
	Workload      Workload      `yaml:"workload"`
	Run           Run           `yaml:"run"`
	Search        Search        `yaml:"search"`
	Thresholds    Thresholds    `yaml:"thresholds"`
	Percentiles   []float64     `yaml:"percentiles"`
	Analysis      Analysis      `yaml:"analysis"`
	Validation    Validation    `yaml:"validation"`
	LoadGenerator LoadGenerator `yaml:"load_generator"`
	Collectors    Collectors    `yaml:"collectors"`

	// preAllocatedExplicit and maxVUsExplicit record whether the user pinned the
	// VU pool. When they did not, the capacity search re-sizes the pool for
	// every probe rate via ForRate.
	//
	// They are captured once, by the first ApplyDefaults, because later calls
	// fill the pool in with auto-sized values that must not be mistaken for
	// user choices.
	preAllocatedExplicit bool
	maxVUsExplicit       bool
	defaultsApplied      bool
}

// PinPreAllocatedVUs marks the pre-allocated pool as user-controlled, so ForRate
// stops re-sizing it.
func (c *Config) PinPreAllocatedVUs() { c.preAllocatedExplicit = true }

// PinMaxVUs marks the maximum pool as user-controlled, so ForRate stops
// re-sizing it.
func (c *Config) PinMaxVUs() { c.maxVUsExplicit = true }

// Target describes the system under test.
type Target struct {
	BaseURL string `yaml:"base_url"`
}

// Workload describes the HTTP work one iteration performs.
type Workload struct {
	Type string `yaml:"type"`
	// Requests is a weighted pool: each iteration performs exactly one of them,
	// chosen at random by weight. One iteration is one HTTP call.
	Requests []Request `yaml:"requests"`
	// Journey is an ordered sequence: each iteration performs every step, in
	// order. One iteration is one HTTP call per step, which is why the report
	// distinguishes iteration rate from request rate (PLAN-OUTLINE.md §11).
	//
	// At most one of Requests and Journey may be set. weight is meaningless for
	// a journey step and is ignored.
	Journey []Request `yaml:"journey"`
}

// IsJourney reports whether an iteration walks an ordered journey rather than
// drawing one request from a weighted pool.
func (w Workload) IsJourney() bool { return len(w.Journey) > 0 }

// Steps returns the requests one iteration performs, in order. For a weighted
// pool that is the pool itself; each iteration still performs only one of them.
func (w Workload) Steps() []Request {
	if w.IsJourney() {
		return w.Journey
	}
	return w.Requests
}

// RequestsPerIteration is how many HTTP calls one iteration issues: one for a
// weighted pool, one per step for a journey.
func (w Workload) RequestsPerIteration() int {
	if w.IsJourney() {
		return len(w.Journey)
	}
	return 1
}

// Request is one weighted HTTP call within the workload.
type Request struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	Weight  int               `yaml:"weight"`
	Body    string            `yaml:"body"`
	Headers map[string]string `yaml:"headers"`
}

// Run holds the fixed-rate run parameters used by `autobench run`.
type Run struct {
	Rate     int      `yaml:"rate"`
	Duration Duration `yaml:"duration"`
	TimeUnit string   `yaml:"time_unit"`
}

// LoadGenerator constrains the k6 VU pool.
type LoadGenerator struct {
	PreAllocatedVUs int `yaml:"pre_allocated_vus"`
	MaxVUs          int `yaml:"max_vus"`
}

// Thresholds define the SLOs the controller evaluates after a run.
type Thresholds struct {
	ErrorRate *ErrorRateThreshold `yaml:"error_rate"`
	Latency   LatencyThresholds   `yaml:"latency"`
}

// ErrorRateThreshold bounds the fraction of failed HTTP requests (0.01 == 1%).
type ErrorRateThreshold struct {
	Max float64 `yaml:"max"`
}

// LatencyThreshold bounds a single latency percentile.
type LatencyThreshold struct {
	Max Duration `yaml:"max"`
}

// LatencyThresholds collects the per-percentile latency bounds.
type LatencyThresholds struct {
	P50  *LatencyThreshold `yaml:"p50"`
	P75  *LatencyThreshold `yaml:"p75"`
	P90  *LatencyThreshold `yaml:"p90"`
	P95  *LatencyThreshold `yaml:"p95"`
	P99  *LatencyThreshold `yaml:"p99"`
	P999 *LatencyThreshold `yaml:"p99.9"`
}

// Search configures the adaptive capacity search driven by `autobench capacity`.
//
// See PLAN-OUTLINE.md §12–§18: the search grows the rate exponentially until a
// probe fails, bisects the bracket down to Resolution, then optionally
// validates the result.
type Search struct {
	Strategy            string   `yaml:"strategy"`
	StartRate           int      `yaml:"start_rate"`
	IncreaseFactor      float64  `yaml:"increase_factor"`
	Resolution          int      `yaml:"resolution"`
	ProbeDuration       Duration `yaml:"probe_duration"`
	Warmup              Duration `yaml:"warmup"`
	ConfirmationRuns    int      `yaml:"confirmation_runs"`
	FailureConfirmation int      `yaml:"failure_confirmation"`
	MaxRate             int      `yaml:"max_rate"`
	MaxProbes           int      `yaml:"max_probes"`
	MaxVUGrowth         int      `yaml:"max_vu_growth"`
}

// Analysis configures the windowed view of a measurement.
//
// An overall p99 can hide a collapse: a system that is fine for four minutes and
// then falls over still averages out. Windowing slices each measurement so that
// shows up (PLAN-OUTLINE.md §31–§32).
type Analysis struct {
	// Window buckets every measurement into slices of this width. It is opt-in
	// because k6 must stream every sample to disk to support it.
	Window Duration `yaml:"window"`
	// RequireAllWindowsPass makes a measurement fail when any single window
	// breaches a threshold, rather than only when the run as a whole does.
	RequireAllWindowsPass bool `yaml:"require_all_windows_pass"`
}

// Validation configures the post-search validation run that confirms the
// discovered capacity (PLAN-OUTLINE.md §13–§14).
type Validation struct {
	Rate         string   `yaml:"rate"`
	Duration     Duration `yaml:"duration"`
	SafetyMargin float64  `yaml:"safety_margin"`
}

// Collectors configures the resource collectors that observe the system under
// test while a measurement runs (PLAN-OUTLINE.md §33).
//
// k6 only ever sees the client side of a request. Collectors add the server
// side, so a report can say what the system was doing when it ran out of
// capacity instead of only how slow it got.
type Collectors struct {
	// Interval is how often every query is sampled. Defaults to
	// DefaultCollectInterval.
	Interval Duration `yaml:"interval"`
	// Prometheus lists Prometheus servers to poll for the system's own metrics.
	Prometheus []PrometheusSource `yaml:"prometheus"`
	// Docker lists containers to sample from the Docker daemon.
	Docker []DockerSource `yaml:"docker"`
	// System samples the host autobench itself runs on.
	System []SystemSource `yaml:"system"`
	// Kubernetes lists cluster selections to sample from the metrics API.
	Kubernetes []KubernetesSource `yaml:"kubernetes"`
}

// Enabled reports whether any collector is configured.
func (c Collectors) Enabled() bool {
	return len(c.Prometheus) > 0 || len(c.Docker) > 0 || len(c.System) > 0 || len(c.Kubernetes) > 0
}

// DockerSource is one container to sample from the Docker daemon.
//
// It exists for the case Prometheus cannot cover: a target with no monitoring
// installed at all. Everything Docker runs already exports CPU and memory.
type DockerSource struct {
	// Name identifies the container in the report, e.g. "api".
	Name string `yaml:"name"`
	// Container is the container's name or ID, as the daemon knows it.
	Container string `yaml:"container"`
	// Socket is the daemon's unix socket. Empty means $DOCKER_HOST (if it names
	// a unix socket) or /var/run/docker.sock, following the docker CLI.
	Socket string `yaml:"socket"`
}

// SystemSource samples the host autobench runs on.
//
// Be clear about what this is: it is the *load generator's* host. If the system
// under test runs elsewhere, these readings say nothing about it; if it runs
// here, the two are the same machine and the report cannot separate them.
type SystemSource struct {
	// Name identifies the host in the report, e.g. "generator".
	Name string `yaml:"name"`
	// Proc is where the kernel's counters are read from. Empty means /proc,
	// which is where they are on Linux. It is configurable so a test can point
	// at a fixture.
	Proc string `yaml:"proc"`
}

// KubernetesSource samples pods from the cluster's metrics API.
//
// It is for a target whose cluster has no Prometheus: the metrics API is what
// `kubectl top` reads, and it needs only API access.
type KubernetesSource struct {
	// Name identifies the workload in the report, e.g. "api".
	Name string `yaml:"name"`
	// URL is the API server's base URL, e.g. https://127.0.0.1:6443.
	URL string `yaml:"url"`
	// Token authenticates against the API server. Empty means
	// $AUTOBENCH_K8S_TOKEN, then a service account token if one is mounted.
	Token string `yaml:"token"`
	// Namespace to read pod metrics from. Empty means "default".
	Namespace string `yaml:"namespace"`
	// Selector picks the pods to watch, e.g. "app=api". Empty means every pod in
	// the namespace.
	Selector string `yaml:"selector"`
	// CAFile is a PEM bundle to verify the API server against. Empty means the
	// system roots.
	CAFile string `yaml:"ca_file"`
	// Insecure skips verification of the API server's certificate. It is
	// sometimes the only way to reach a cluster whose CA is not at hand.
	Insecure bool `yaml:"insecure"`
}

// PrometheusSource is one Prometheus server to poll for the system under test's
// own metrics.
type PrometheusSource struct {
	// Name identifies the server in the report, e.g. "api" or "postgres".
	Name string `yaml:"name"`
	// URL is the server's base URL, e.g. http://localhost:9090. A path prefix is
	// kept, so a Prometheus behind a reverse proxy works.
	URL string `yaml:"url"`
	// Queries are the expressions sampled on every interval, in report order.
	Queries []PrometheusQuery `yaml:"queries"`
}

// PrometheusQuery is one PromQL instant query sampled on the collector interval.
//
// It is a list rather than a map so the report keeps the order the queries were
// written in; a report that reorders its own rows between runs is not worth
// having.
type PrometheusQuery struct {
	// Name labels the series in the report, e.g. "cpu". It must be unique within
	// its source, because it is what the report calls the row.
	Name string `yaml:"name"`
	// Query is the PromQL expression, e.g. rate(process_cpu_seconds_total[1m]).
	Query string `yaml:"query"`
	// Unit is a free-text label for the rendered value. "bytes" is shown in
	// binary units and "percent" with a percent sign; anything else is appended
	// to the number as written.
	Unit string `yaml:"unit"`
}

// Mode selects what the CLI does with a configuration.
type Mode string

// Supported modes.
const (
	// ModeFixed runs a single measurement at run.rate for run.duration.
	ModeFixed Mode = "fixed"
	// ModeCapacity runs the adaptive capacity search.
	ModeCapacity Mode = "capacity"
)

// ValidationRateOverride interprets validation.rate. An empty value or
// "discovered" returns 0, meaning "derive the rate from the capacity the search
// found"; a bare positive integer pins the rate explicitly.
func (c *Config) ValidationRateOverride() (int, error) {
	s := strings.TrimSpace(c.Validation.Rate)
	switch strings.ToLower(s) {
	case "", "discovered":
		return 0, nil
	}
	rate, err := strconv.Atoi(s)
	if err != nil || rate <= 0 {
		return 0, fmt.Errorf("validation.rate %q must be \"discovered\" or a positive integer", c.Validation.Rate)
	}
	return rate, nil
}
