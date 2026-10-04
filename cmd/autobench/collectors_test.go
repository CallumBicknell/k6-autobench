package main

import (
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/collect"
	"github.com/callum/k6-autobench/internal/config"
)

// collectorConfig builds a config with one Prometheus source and two queries.
func collectorConfig() *config.Config {
	cfg := &config.Config{
		Collectors: config.Collectors{
			Prometheus: []config.PrometheusSource{{
				Name: "api",
				URL:  "http://localhost:9090",
				Queries: []config.PrometheusQuery{
					{Name: "cpu", Query: "rate(process_cpu_seconds_total[1m])", Unit: "cores"},
					{Name: "memory", Query: "process_resident_memory_bytes", Unit: "bytes"},
				},
			}},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

// TestBuildCollectorsWithoutConfiguration returns nothing, which is the signal
// the runner uses to skip the bookkeeping entirely.
func TestBuildCollectorsWithoutConfiguration(t *testing.T) {
	cfg := &config.Config{}
	cfg.ApplyDefaults()

	if got := buildCollectors(cfg); got != nil {
		t.Errorf("buildCollectors = %v, want nil", got)
	}
}

// TestBuildCollectorsTranslatesTheConfiguration checks the config model is
// turned into the collector the runner drives, query order included.
func TestBuildCollectorsTranslatesTheConfiguration(t *testing.T) {
	cfg := collectorConfig()

	built := buildCollectors(cfg)
	if len(built) != 1 {
		t.Fatalf("collectors = %d, want 1", len(built))
	}
	if built[0].Name() != "api" {
		t.Errorf("Name() = %q, want api", built[0].Name())
	}

	p, ok := built[0].(*collect.Prometheus)
	if !ok {
		t.Fatalf("collector is %T, want *collect.Prometheus", built[0])
	}
	if p.URL != "http://localhost:9090" {
		t.Errorf("URL = %q", p.URL)
	}
	if got, want := p.Interval, config.DefaultCollectInterval; got != want {
		t.Errorf("interval = %v, want %v", got, want)
	}
	if len(p.Queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(p.Queries))
	}
	if p.Queries[0].Name != "cpu" || p.Queries[0].Expr != "rate(process_cpu_seconds_total[1m])" {
		t.Errorf("first query = %+v", p.Queries[0])
	}
	if p.Queries[1].Name != "memory" || p.Queries[1].Unit != "bytes" {
		t.Errorf("second query = %+v", p.Queries[1])
	}
}

// TestBuildCollectorsDefaultsTheInterval covers a config built without
// ApplyDefaults, so the collector still samples at a sane rate.
func TestBuildCollectorsDefaultsTheInterval(t *testing.T) {
	cfg := &config.Config{
		Collectors: config.Collectors{
			Prometheus: []config.PrometheusSource{{
				Name:    "api",
				URL:     "http://localhost:9090",
				Queries: []config.PrometheusQuery{{Name: "cpu", Query: "up"}},
			}},
		},
	}

	built := buildCollectors(cfg)
	p, ok := built[0].(*collect.Prometheus)
	if !ok {
		t.Fatalf("collector is %T", built[0])
	}
	if got, want := p.Interval, config.DefaultCollectInterval; got != want {
		t.Errorf("interval = %v, want the default %v", got, want)
	}
}

// TestCollectorNotice describes what a run will sample, so a long search says up
// front which sources it is watching.
func TestCollectorNotice(t *testing.T) {
	if got := collectorNotice(&config.Config{}); got != "" {
		t.Errorf("notice = %q, want empty with no collectors", got)
	}

	got := collectorNotice(collectorConfig())
	want := "api (2 queries every 5s)"
	if got != want {
		t.Errorf("notice = %q, want %q", got, want)
	}
}

// TestFmtCollectorsListsEverySource checks several sources are named.
func TestFmtCollectorsListsEverySource(t *testing.T) {
	got := fmtCollectors([]string{"api", "postgres"}, 5, 10*time.Second)
	want := "api, postgres (5 queries every 10s)"
	if got != want {
		t.Errorf("fmtCollectors = %q, want %q", got, want)
	}
}
