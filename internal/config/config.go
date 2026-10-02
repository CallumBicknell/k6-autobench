// Package config defines the autobench YAML configuration model and the
// defaults/validation applied before a run starts.
package config

import (
	"encoding/json"
	"fmt"
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

// DefaultPercentiles are collected when the config specifies none.
func DefaultPercentiles() []float64 { return []float64{50, 75, 90, 95, 99, 99.9} }

// Config is the root of an autobench configuration file.
type Config struct {
	Name          string        `yaml:"name"`
	Target        Target        `yaml:"target"`
	Workload      Workload      `yaml:"workload"`
	Run           Run           `yaml:"run"`
	Search        Search        `yaml:"search"`
	Thresholds    Thresholds    `yaml:"thresholds"`
	Percentiles   []float64     `yaml:"percentiles"`
	Validation    Validation    `yaml:"validation"`
	LoadGenerator LoadGenerator `yaml:"load_generator"`
}

// Target describes the system under test.
type Target struct {
	BaseURL string `yaml:"base_url"`
}

// Workload describes the HTTP requests an iteration may issue.
type Workload struct {
	Type     string    `yaml:"type"`
	Requests []Request `yaml:"requests"`
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

// Search configures the adaptive capacity search. Reserved for V0.2; it is
// decoded so that forward-looking configs parse cleanly.
type Search struct {
	Strategy         string   `yaml:"strategy"`
	StartRate        int      `yaml:"start_rate"`
	IncreaseFactor   float64  `yaml:"increase_factor"`
	Resolution       int      `yaml:"resolution"`
	ProbeDuration    Duration `yaml:"probe_duration"`
	Warmup           Duration `yaml:"warmup"`
	ConfirmationRuns int      `yaml:"confirmation_runs"`
}

// Validation configures the post-search validation run. Reserved for V0.3.
type Validation struct {
	Rate                  string   `yaml:"rate"`
	Duration              Duration `yaml:"duration"`
	SafetyMargin          float64  `yaml:"safety_margin"`
	Window                Duration `yaml:"window"`
	RequireAllWindowsPass bool     `yaml:"require_all_windows_pass"`
}
