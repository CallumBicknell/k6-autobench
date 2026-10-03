package config

import (
	"testing"
	"time"
)

func TestWorkloadJourneyHelpers(t *testing.T) {
	weighted := Workload{Requests: []Request{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	if weighted.IsJourney() {
		t.Error("IsJourney = true for a weighted pool")
	}
	if got := weighted.RequestsPerIteration(); got != 1 {
		t.Errorf("RequestsPerIteration = %d, want 1 for a weighted pool", got)
	}
	if got := len(weighted.Steps()); got != 3 {
		t.Errorf("Steps = %d, want the 3-request pool", got)
	}

	journey := Workload{Journey: []Request{{Name: "login"}, {Name: "feed"}, {Name: "like"}}}
	if !journey.IsJourney() {
		t.Error("IsJourney = false for a journey")
	}
	if got := journey.RequestsPerIteration(); got != 3 {
		t.Errorf("RequestsPerIteration = %d, want 3", got)
	}
	if got := journey.Steps(); len(got) != 3 || got[0].Name != "login" || got[2].Name != "like" {
		t.Errorf("Steps = %+v, want the journey in order", got)
	}
}

// TestApplyDefaultsNormalisesJourney covers the same normalisation the weighted
// pool gets: methods upper-cased, paths leading-slashed, names defaulted.
func TestApplyDefaultsNormalisesJourney(t *testing.T) {
	cfg := &Config{
		Run: Run{Rate: 100},
		Workload: Workload{Journey: []Request{
			{Path: "login"},
			{Method: "post", Path: "/posts"},
		}},
	}
	cfg.ApplyDefaults()

	if len(cfg.Workload.Requests) != 0 {
		t.Errorf("Requests = %+v, want none for a journey-only workload", cfg.Workload.Requests)
	}
	first := cfg.Workload.Journey[0]
	if first.Method != "GET" || first.Path != "/login" || first.Name != "GET /login" {
		t.Errorf("first step = %+v, want a normalised GET /login", first)
	}
	if second := cfg.Workload.Journey[1]; second.Method != "POST" || second.Name != "POST /posts" {
		t.Errorf("second step = %+v, want POST /posts", second)
	}
}

func TestValidateWorkload(t *testing.T) {
	build := func(mutate func(*Config)) *Config {
		cfg := &Config{
			Target: Target{BaseURL: "http://localhost:1"},
			Run:    Run{Rate: 100, Duration: Duration{Duration: time.Second}},
		}
		cfg.ApplyDefaults()
		mutate(cfg)
		return cfg
	}

	// A weighted pool alone is fine, and so is a journey alone.
	if err := build(func(*Config) {}).ValidateMode(ModeFixed); err != nil {
		t.Errorf("weighted pool: %v", err)
	}
	journey := build(func(c *Config) {
		c.Workload.Requests = nil
		c.Workload.Journey = []Request{{Name: "a", Method: "GET", Path: "/a"}}
	})
	if err := journey.ValidateMode(ModeFixed); err != nil {
		t.Errorf("journey: %v", err)
	}

	// Both at once is ambiguous: what does one iteration do?
	both := build(func(c *Config) {
		c.Workload.Journey = []Request{{Name: "a", Method: "GET", Path: "/a"}}
	})
	if err := both.ValidateMode(ModeFixed); err == nil {
		t.Error("ValidateMode = nil, want an error when both requests and journey are set")
	}

	// Names tag the metrics, so duplicates would merge two endpoints into one.
	duplicate := build(func(c *Config) {
		c.Workload.Requests = []Request{{Name: "dup", Path: "/a"}, {Name: "dup", Path: "/b"}}
	})
	if err := duplicate.ValidateMode(ModeFixed); err == nil {
		t.Error("ValidateMode = nil, want an error for duplicate request names")
	}
}

// TestLoadJourneyBlock checks the YAML surface.
func TestLoadJourneyBlock(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:1
workload:
  journey:
    - name: login
      method: POST
      path: /login
      body: '{"user":"a"}'
    - name: feed
      method: GET
      path: /feed
run:
  rate: 100
  duration: 10s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Workload.IsJourney() {
		t.Fatal("IsJourney = false")
	}
	if cfg.Workload.RequestsPerIteration() != 2 {
		t.Errorf("RequestsPerIteration = %d, want 2", cfg.Workload.RequestsPerIteration())
	}
	if cfg.Workload.Journey[0].Body != `{"user":"a"}` {
		t.Errorf("step body = %q", cfg.Workload.Journey[0].Body)
	}
}

// TestAutoVUsScalesWithJourneyLength guards a real bug: the pool was sized as if
// an iteration were a single HTTP call, so a multi-step journey saturated the
// generator on its very first probe.
func TestAutoVUsScalesWithJourneyLength(t *testing.T) {
	weighted := &Config{Run: Run{Rate: 100}}
	weighted.ApplyDefaults()
	if got := weighted.LoadGenerator.PreAllocatedVUs; got != 10 {
		t.Errorf("weighted pool = %d, want 10 (100/s x 100ms)", got)
	}

	journey := &Config{
		Run: Run{Rate: 100},
		Workload: Workload{Journey: []Request{
			{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"},
		}},
	}
	journey.ApplyDefaults()
	// Four steps at ~100ms each is ~400ms per iteration.
	if got := journey.LoadGenerator.PreAllocatedVUs; got != 40 {
		t.Errorf("journey pool = %d, want 40 (100/s x 4 x 100ms)", got)
	}
}

// TestForRateScalesWithJourneyLength covers the per-probe path.
func TestForRateScalesWithJourneyLength(t *testing.T) {
	cfg := &Config{
		Run: Run{Rate: 100},
		Workload: Workload{Journey: []Request{
			{Name: "a"}, {Name: "b"}, {Name: "c"},
		}},
	}
	cfg.ApplyDefaults()

	// 200 iterations/s of a 3-step journey, nothing measured yet.
	if got := cfg.ForRate(200, 0).LoadGenerator.PreAllocatedVUs; got != 60 {
		t.Errorf("pool = %d, want 60 (200/s x 3 x 100ms)", got)
	}
	// Once the iteration latency is known it is used as-is: it already covers
	// every step.
	if got := cfg.ForRate(200, 300).LoadGenerator.PreAllocatedVUs; got != 60 {
		t.Errorf("pool = %d, want 60 (200/s x 300ms)", got)
	}
}
