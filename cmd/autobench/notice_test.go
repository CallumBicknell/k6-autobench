package main

import (
	"strings"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/config"
)

// windowedConfig is a config with windowed analysis on, one request per
// iteration, and a known rate and duration.
func windowedConfig() *config.Config {
	cfg := &config.Config{
		Target: config.Target{BaseURL: "http://localhost:3000"},
		Workload: config.Workload{Requests: []config.Request{
			{Name: "root", Method: "GET", Path: "/", Weight: 1},
		}},
	}
	cfg.Run.Rate = 1000
	cfg.Run.Duration = config.Duration{Duration: 10 * time.Second}
	cfg.Analysis.Window = config.Duration{Duration: 5 * time.Second}
	cfg.ApplyDefaults()
	return cfg
}

// TestWindowNoticeEstimatesTheDiskCost is the point of the notice: the cost is
// invisible until the disk fills, so it is stated before the run rather than
// discovered by a full filesystem.
func TestWindowNoticeEstimatesTheDiskCost(t *testing.T) {
	cfg := windowedConfig()

	got := windowNotice(cfg, 1000, 10*time.Second, 1)
	if !strings.Contains(got, "about 29.3MiB for this run") {
		t.Errorf("notice = %q, want an estimate of about 29.3MiB", got)
	}

	// The measured figure this is built on: 10,000 requests produced a 29 MB
	// sample file, so the estimate and the measurement have to agree.
	const measured = 29 * 1024 * 1024
	estimated := int64(1000) * 10 * sampleBytesPerRequest
	if estimated < measured || estimated > measured*2 {
		t.Errorf("estimate %d is not within a factor of two of the measured %d",
			estimated, measured)
	}
}

// TestWindowNoticeScalesWithRequestsPerIteration: a journey makes several calls
// per iteration and pays for each one.
func TestWindowNoticeScalesWithRequestsPerIteration(t *testing.T) {
	cfg := windowedConfig()
	cfg.Workload = config.Workload{Journey: []config.Request{
		{Name: "a", Method: "GET", Path: "/a"},
		{Name: "b", Method: "GET", Path: "/b"},
		{Name: "c", Method: "GET", Path: "/c"},
		{Name: "d", Method: "GET", Path: "/d"},
	}}
	cfg.ApplyDefaults()

	got := windowNotice(cfg, 1000, 10*time.Second, 1)
	if !strings.Contains(got, "about 117.2MiB") {
		t.Errorf("notice = %q, want four times the single-request estimate", got)
	}
}

// TestWindowNoticeSaysUpToForASearch: a search does not know how many probes it
// will take, so it states the ceiling rather than pretending to know.
func TestWindowNoticeSaysUpToForASearch(t *testing.T) {
	cfg := windowedConfig()

	got := windowNotice(cfg, 200, 2*time.Second, 5)
	for _, want := range []string{"about 1.2MiB per probe", "up to 5 probes"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "for this run") {
		t.Errorf("notice = %q, want the per-probe form for a search", got)
	}
}

// TestWindowNoticeStaysQuietWhenWindowingIsOff: no window, no cost, no notice.
func TestWindowNoticeStaysQuietWhenWindowingIsOff(t *testing.T) {
	cfg := windowedConfig()
	cfg.Analysis.Window = config.Duration{}

	if got := windowNotice(cfg, 1000, 10*time.Second, 1); got != "" {
		t.Errorf("notice = %q, want none without a window", got)
	}
}

// TestWindowNoticeHandlesDegenerateInputs: a notice is a courtesy, not a place to
// divide by zero or print a nonsense figure.
func TestWindowNoticeHandlesDegenerateInputs(t *testing.T) {
	cfg := windowedConfig()

	for _, tc := range []struct {
		name     string
		rate     int
		duration time.Duration
	}{
		{"no rate", 0, 10 * time.Second},
		{"no duration", 1000, 0},
		{"neither", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowNotice(cfg, tc.rate, tc.duration, 1); got != "" {
				t.Errorf("notice = %q, want none", got)
			}
		})
	}
}

// TestHumanBytes covers the formatter the notice leans on.
func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KiB"},
		{1536, "1.5KiB"},
		{1024 * 1024, "1.0MiB"},
		{29 * 1024 * 1024, "29.0MiB"},
		{3 * 1024 * 1024 * 1024, "3.0GiB"},
		{16 * 1024 * 1024 * 1024, "16.0GiB"},
	}

	for _, tc := range tests {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
