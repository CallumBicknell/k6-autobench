package config

import (
	"strings"
	"testing"
	"time"
)

// TestLoadCollectors covers the collectors block end to end: decoding, the
// default interval, and the order the queries are kept in.
func TestLoadCollectors(t *testing.T) {
	path := writeConfig(t, `
name: with-collectors
target:
  base_url: http://localhost:3000
run:
  rate: 100
  duration: 30s
collectors:
  prometheus:
    - name: api
      url: http://localhost:9090
      queries:
        - name: cpu
          query: rate(process_cpu_seconds_total{job="api"}[1m])
          unit: cores
        - name: memory
          query: process_resident_memory_bytes{job="api"}
          unit: bytes
    - name: postgres
      url: http://localhost:9091/
      queries:
        - name: connections
          query: pg_stat_activity_count
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.Collectors.Enabled() {
		t.Fatal("collectors should be enabled")
	}
	if got, want := cfg.Collectors.Interval.Duration, DefaultCollectInterval; got != want {
		t.Errorf("interval = %v, want the default %v", got, want)
	}
	if len(cfg.Collectors.Prometheus) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Collectors.Prometheus))
	}

	api := cfg.Collectors.Prometheus[0]
	if api.Name != "api" || api.URL != "http://localhost:9090" {
		t.Errorf("first source = %+v", api)
	}
	// Order matters: it is the order the report prints the rows in.
	wantNames := []string{"cpu", "memory"}
	if len(api.Queries) != len(wantNames) {
		t.Fatalf("queries = %d, want %d", len(api.Queries), len(wantNames))
	}
	for i, name := range wantNames {
		if api.Queries[i].Name != name {
			t.Errorf("query %d = %q, want %q", i, api.Queries[i].Name, name)
		}
	}
	if api.Queries[0].Unit != "cores" || api.Queries[1].Unit != "bytes" {
		t.Errorf("units = %q/%q, want cores/bytes", api.Queries[0].Unit, api.Queries[1].Unit)
	}
}

// TestLoadCollectorIntervalOverridesDefault checks an explicit interval is kept.
func TestLoadCollectorIntervalOverridesDefault(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:3000
run:
  rate: 100
  duration: 30s
collectors:
  interval: 15s
  prometheus:
    - name: api
      url: http://localhost:9090
      queries:
        - name: cpu
          query: up
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Collectors.Interval.Duration; got != 15*time.Second {
		t.Errorf("interval = %v, want 15s", got)
	}
}

// TestNoCollectorsKeepsTheIntervalUnset keeps a run without collectors from
// claiming a sampling interval it never used.
func TestNoCollectorsKeepsTheIntervalUnset(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()
	if cfg.Collectors.Enabled() {
		t.Error("collectors should be disabled")
	}
	if cfg.Collectors.Interval.Duration != 0 {
		t.Errorf("interval = %v, want unset", cfg.Collectors.Interval.Duration)
	}
}

// TestValidateCollectors covers the static mistakes worth catching before k6 is
// ever spawned. Whether the server answers is a run-time note, not a validation
// error.
func TestValidateCollectors(t *testing.T) {
	valid := func() *Config {
		c := &Config{
			Target: Target{BaseURL: "http://localhost:3000"},
			Run:    Run{Rate: 100, Duration: Duration{time.Second}},
			Collectors: Collectors{Prometheus: []PrometheusSource{{
				Name: "api", URL: "http://localhost:9090",
				Queries: []PrometheusQuery{{Name: "cpu", Query: "up"}},
			}}},
		}
		c.ApplyDefaults()
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "the valid case",
			mutate: func(*Config) {},
		},
		{
			name: "a source with no name",
			mutate: func(c *Config) {
				c.Collectors.Prometheus[0].Name = ""
			},
			wantErr: "name is required",
		},
		{
			name: "two sources with one name",
			mutate: func(c *Config) {
				c.Collectors.Prometheus = append(c.Collectors.Prometheus,
					PrometheusSource{Name: "api", URL: "http://localhost:9091",
						Queries: []PrometheusQuery{{Name: "cpu", Query: "up"}}})
			},
			wantErr: "duplicate collector name",
		},
		{
			name: "a source with no url",
			mutate: func(c *Config) {
				c.Collectors.Prometheus[0].URL = ""
			},
			wantErr: "has no url",
		},
		{
			name: "a url with no scheme or host",
			mutate: func(c *Config) {
				c.Collectors.Prometheus[0].URL = "localhost:9090"
			},
			wantErr: "must include a scheme and host",
		},
		{
			name: "a source with no queries",
			mutate: func(c *Config) {
				c.Collectors.Prometheus[0].Queries = nil
			},
			wantErr: "has no queries",
		},
		{
			name: "a query with no name",
			mutate: func(c *Config) {
				c.Collectors.Prometheus[0].Queries[0].Name = ""
			},
			wantErr: "query with no name",
		},
		{
			name: "two queries with one name",
			mutate: func(c *Config) {
				c.Collectors.Prometheus[0].Queries = append(c.Collectors.Prometheus[0].Queries,
					PrometheusQuery{Name: "cpu", Query: "other"})
			},
			wantErr: "two queries named",
		},
		{
			name: "a query with no expression",
			mutate: func(c *Config) {
				c.Collectors.Prometheus[0].Queries[0].Query = "   "
			},
			wantErr: "has no expression",
		},
		{
			name: "a negative interval",
			mutate: func(c *Config) {
				c.Collectors.Interval = Duration{-time.Second}
			},
			wantErr: "collectors.interval must not be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)

			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate should have failed with %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateCollectorsAppliesToEveryMode checks the collectors block is
// validated for a capacity search too, not only a fixed-rate run.
func TestValidateCollectorsAppliesToEveryMode(t *testing.T) {
	cfg := &Config{
		Target: Target{BaseURL: "http://localhost:3000"},
		Search: Search{StartRate: 100},
		Collectors: Collectors{Prometheus: []PrometheusSource{{
			Name: "api", URL: "not-a-url",
			Queries: []PrometheusQuery{{Name: "cpu", Query: "up"}},
		}}},
	}
	cfg.ApplyDefaults()
	if err := cfg.ValidateMode(ModeCapacity); err == nil {
		t.Error("a capacity search should reject a bad collector URL too")
	}
}

// TestLoadRejectsUnknownCollectorKeys checks the collectors block is covered by
// the strict decoding every other block is.
func TestLoadRejectsUnknownCollectorKeys(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:3000
run:
  rate: 100
  duration: 30s
collectors:
  prometheus:
    - name: api
      url: http://localhost:9090
      queriess:
        - name: cpu
          query: up
`)
	if _, err := Load(path); err == nil {
		t.Error("a misspelled collectors key should be rejected")
	}
}

// TestLoadEveryCollectorKind covers the whole block: four kinds of source in one
// config, decoded together.
func TestLoadEveryCollectorKind(t *testing.T) {
	path := writeConfig(t, `
target:
  base_url: http://localhost:3000
run:
  rate: 100
  duration: 30s
collectors:
  interval: 5s
  prometheus:
    - name: api
      url: http://localhost:9090
      queries:
        - name: cpu
          query: up
  docker:
    - name: worker
      container: my-worker
      socket: /var/run/docker.sock
  system:
    - name: generator
  kubernetes:
    - name: cluster-api
      url: https://127.0.0.1:6443
      namespace: bench
      selector: app=api
      insecure: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := len(cfg.Collectors.Docker); got != 1 {
		t.Fatalf("docker sources = %d, want 1", got)
	}
	d := cfg.Collectors.Docker[0]
	if d.Name != "worker" || d.Container != "my-worker" || d.Socket != "/var/run/docker.sock" {
		t.Errorf("docker source = %+v", d)
	}

	if got := len(cfg.Collectors.System); got != 1 || cfg.Collectors.System[0].Name != "generator" {
		t.Errorf("system sources = %+v, want one named generator", cfg.Collectors.System)
	}

	if got := len(cfg.Collectors.Kubernetes); got != 1 {
		t.Fatalf("kubernetes sources = %d, want 1", got)
	}
	k := cfg.Collectors.Kubernetes[0]
	if k.Name != "cluster-api" || k.URL != "https://127.0.0.1:6443" ||
		k.Namespace != "bench" || k.Selector != "app=api" || !k.Insecure {
		t.Errorf("kubernetes source = %+v", k)
	}
}

// TestValidateNewCollectors covers the static mistakes in the new kinds.
func TestValidateNewCollectors(t *testing.T) {
	valid := func() *Config {
		c := &Config{
			Target: Target{BaseURL: "http://localhost:3000"},
			Run:    Run{Rate: 100, Duration: Duration{time.Second}},
			Collectors: Collectors{
				Docker: []DockerSource{{Name: "worker", Container: "w"}},
				System: []SystemSource{{Name: "generator"}},
			},
		}
		c.ApplyDefaults()
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"the valid case", func(*Config) {}, ""},
		{
			name: "a docker source with no container",
			mutate: func(c *Config) {
				c.Collectors.Docker[0].Container = "  "
			},
			wantErr: "has no container",
		},
		{
			name: "a docker source with no name",
			mutate: func(c *Config) {
				c.Collectors.Docker[0].Name = ""
			},
			wantErr: "name is required",
		},
		{
			name: "a system source with no name",
			mutate: func(c *Config) {
				c.Collectors.System[0].Name = ""
			},
			wantErr: "name is required",
		},
		{
			// An empty url is allowed: inside a cluster the collector falls back
			// to the in-cluster service, and outside one it reports that it has
			// nowhere to read from. The config package cannot tell which, so the
			// check lives with the collector.
			name: "a kubernetes source with no url",
			mutate: func(c *Config) {
				c.Collectors.Kubernetes = []KubernetesSource{{Name: "cluster"}}
			},
			wantErr: "",
		},
		{
			name: "a kubernetes url with no scheme",
			mutate: func(c *Config) {
				c.Collectors.Kubernetes = []KubernetesSource{
					{Name: "cluster", URL: "example.com"}}
			},
			wantErr: "must include a scheme and host",
		},
		{
			name: "a kubernetes url with no scheme or host",
			mutate: func(c *Config) {
				c.Collectors.Kubernetes = []KubernetesSource{{Name: "cluster", URL: "localhost:6443"}}
			},
			wantErr: "must include a scheme and host",
		},
		{
			name: "a kubernetes url that is not a url at all",
			mutate: func(c *Config) {
				c.Collectors.Kubernetes = []KubernetesSource{{Name: "cluster", URL: "127.0.0.1:6443"}}
			},
			wantErr: "is not valid",
		},
		{
			name: "a kubernetes source setting both insecure and ca_file",
			mutate: func(c *Config) {
				c.Collectors.Kubernetes = []KubernetesSource{{
					Name: "cluster", URL: "https://127.0.0.1:6443",
					Insecure: true, CAFile: "/ca.pem",
				}}
			},
			wantErr: "pick the one you mean",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)

			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate should have failed with %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestCollectorNamesAreUniqueAcrossKinds: a Prometheus source and a Docker
// source sharing a name would produce report rows that cannot be told apart, so
// the names are claimed across every kind at once.
func TestCollectorNamesAreUniqueAcrossKinds(t *testing.T) {
	cfg := &Config{
		Target: Target{BaseURL: "http://localhost:3000"},
		Run:    Run{Rate: 100, Duration: Duration{time.Second}},
		Collectors: Collectors{
			Prometheus: []PrometheusSource{{
				Name: "api", URL: "http://localhost:9090",
				Queries: []PrometheusQuery{{Name: "cpu", Query: "up"}},
			}},
			Docker: []DockerSource{{Name: "api", Container: "my-api"}},
		},
	}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a name shared between two kinds of collector should be rejected")
	}
	if !strings.Contains(err.Error(), "duplicate collector name") {
		t.Errorf("error = %q, want it to name the clash", err)
	}
}

// TestSystemCollectorEnablesCollecting keeps the system collector from being the
// one kind that silently does nothing.
func TestSystemCollectorEnablesCollecting(t *testing.T) {
	cfg := &Config{Collectors: Collectors{System: []SystemSource{{Name: "generator"}}}}
	if !cfg.Collectors.Enabled() {
		t.Error("a configured system collector should enable collecting")
	}
	cfg.ApplyDefaults()
	if cfg.Collectors.Interval.Duration != DefaultCollectInterval {
		t.Errorf("interval = %v, want the default to be filled in", cfg.Collectors.Interval.Duration)
	}
}
