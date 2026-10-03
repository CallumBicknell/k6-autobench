package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
)

// envValue reads one entry out of a built environment.
func envValue(t *testing.T, env []string, key string) string {
	t.Helper()
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	t.Fatalf("environment has no %s", key)
	return ""
}

// decodedWorkload is the JSON the script parses.
type decodedWorkload struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Weight  int               `json:"weight"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

func specFor(w config.Workload) Spec {
	return Spec{
		BaseURL:         "http://localhost:1",
		Workload:        w,
		Rate:            100,
		TimeUnit:        "1s",
		Duration:        time.Second,
		PreAllocatedVUs: 10,
		MaxVUs:          100,
		Percentiles:     []float64{95},
	}
}

// TestBuildEnvWeightedMode is the contract with k6/runner.js: the mode decides
// whether an iteration picks one request or walks them all.
func TestBuildEnvWeightedMode(t *testing.T) {
	w := config.Workload{Requests: []config.Request{
		{Name: "feed", Method: "GET", Path: "/feed", Weight: 50},
		{Name: "like", Method: "POST", Path: "/like", Weight: 15, Body: "{}"},
	}}

	env, err := buildEnv(specFor(w), "/tmp/summary.json")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}

	if got := envValue(t, env, "AUTOBENCH_WORKLOAD_MODE"); got != "weighted" {
		t.Errorf("mode = %q, want weighted", got)
	}

	var got []decodedWorkload
	if err := json.Unmarshal([]byte(envValue(t, env, "AUTOBENCH_WORKLOAD")), &got); err != nil {
		t.Fatalf("decode workload: %v", err)
	}
	if len(got) != 2 || got[0].Name != "feed" || got[0].Weight != 50 {
		t.Errorf("workload = %+v, want the weighted pool with its weights", got)
	}
	if got[1].Body != "{}" {
		t.Errorf("body = %q, want {}", got[1].Body)
	}
}

// TestBuildEnvJourneyMode: a journey sends the steps in order, and the script
// performs all of them per iteration.
func TestBuildEnvJourneyMode(t *testing.T) {
	w := config.Workload{Journey: []config.Request{
		{Name: "login", Method: "POST", Path: "/login"},
		{Name: "feed", Method: "GET", Path: "/feed"},
		{Name: "like", Method: "POST", Path: "/like"},
	}}

	env, err := buildEnv(specFor(w), "/tmp/summary.json")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}

	if got := envValue(t, env, "AUTOBENCH_WORKLOAD_MODE"); got != "journey" {
		t.Errorf("mode = %q, want journey", got)
	}

	var got []decodedWorkload
	if err := json.Unmarshal([]byte(envValue(t, env, "AUTOBENCH_WORKLOAD")), &got); err != nil {
		t.Fatalf("decode workload: %v", err)
	}
	wantOrder := []string{"login", "feed", "like"}
	for i, want := range wantOrder {
		if got[i].Name != want {
			t.Errorf("step %d = %q, want %q", i, got[i].Name, want)
		}
	}
}

func TestBuildEnvRejectsZeroRate(t *testing.T) {
	spec := specFor(config.Workload{Requests: []config.Request{{Name: "a"}}})
	spec.Rate = 0
	if _, err := buildEnv(spec, "/tmp/s.json"); err == nil {
		t.Error("buildEnv = nil, want an error for a zero rate")
	}
}

// TestBuildEnvCarriesEverythingElse keeps the rest of the contract honest.
func TestBuildEnvCarriesEverythingElse(t *testing.T) {
	spec := specFor(config.Workload{Requests: []config.Request{{Name: "a", Path: "/a"}}})
	spec.Window = 5 * time.Second

	env, err := buildEnv(spec, "/tmp/summary.json")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	checks := map[string]string{
		"AUTOBENCH_BASE_URL":          "http://localhost:1",
		"AUTOBENCH_RATE":              "100",
		"AUTOBENCH_TIME_UNIT":         "1s",
		"AUTOBENCH_DURATION":          "1s",
		"AUTOBENCH_SUMMARY_PATH":      "/tmp/summary.json",
		"AUTOBENCH_PERCENTILES":       "[95]",
		"AUTOBENCH_PRE_ALLOCATED_VUS": "10",
		"AUTOBENCH_MAX_VUS":           "100",
	}
	for key, want := range checks {
		if got := envValue(t, env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
