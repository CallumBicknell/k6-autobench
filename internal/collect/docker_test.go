package collect

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDocker is a Docker daemon's stats endpoint, served over a real listener so
// the collector's own client is exercised.
type fakeDocker struct {
	status   int
	body     string
	requests []string
}

func newFakeDocker(t *testing.T, status int, body string) (*fakeDocker, *httptest.Server) {
	t.Helper()
	f := &fakeDocker{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprint(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// dockerOn routes a collector's unix-socket dialling at a test server, so the
// real client path is used and only the socket name is stood in for.
func dockerOn(d *Docker, srv *httptest.Server) *Docker {
	addr := srv.Listener.Addr().String()
	d.HTTP = dialingTo(addr)
	return d
}

// dialingTo is a client that reaches addr whatever host the URL names, which is
// how the collector's own socket client works.
func dialingTo(addr string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", addr)
		},
	}}
}

// dockerStatsJSON renders a daemon stats response with the counters the
// documented CPU formula needs.
func dockerStatsJSON(cpuTotal, precpuTotal, system, presystem uint64, onlineCPUs int, memUsage, memInactive uint64) string {
	return fmt.Sprintf(`{
	  "cpu_stats": {"cpu_usage": {"total_usage": %d}, "system_cpu_usage": %d, "online_cpus": %d},
	  "precpu_stats": {"cpu_usage": {"total_usage": %d}, "system_cpu_usage": %d},
	  "memory_stats": {"usage": %d, "limit": 1073741824, "stats": {"inactive_file": %d}}
	}`, cpuTotal, system, onlineCPUs, precpuTotal, presystem, memUsage, memInactive)
}

func measureDocker(t *testing.T, d *Docker, fn func()) Report {
	t.Helper()
	stop := d.Start(context.Background())
	fn()
	return stop()
}

// TestDockerRequestsTheDocumentedStatsEndpoint pins the request contract:
// stream=false is what makes it one call and makes the daemon include the
// previous counters the CPU percentage needs.
func TestDockerRequestsTheDocumentedStatsEndpoint(t *testing.T) {
	f, srv := newFakeDocker(t, 200, dockerStatsJSON(200, 100, 1000, 500, 4, 1<<20, 0))
	d := dockerOn(&Docker{Source: "api", Container: "my-api", Interval: time.Hour}, srv)

	measureDocker(t, d, func() {})

	if len(f.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.requests))
	}
	want := "/containers/my-api/stats?stream=false"
	if f.requests[0] != want {
		t.Errorf("request = %q, want %q", f.requests[0], want)
	}
}

// TestDockerComputesCPUSFromTheDocumentedFormula checks the percentage against
// a hand-computed figure: cpu_delta 100 of system_delta 500 across 4 CPUs is
// 80%.
func TestDockerComputesCPUSFromTheDocumentedFormula(t *testing.T) {
	_, srv := newFakeDocker(t, 200, dockerStatsJSON(200, 100, 1000, 500, 4, 1<<20, 0))
	d := dockerOn(&Docker{Source: "api", Container: "my-api", Interval: time.Hour}, srv)

	report := measureDocker(t, d, func() {})
	s := seriesNamed(t, report, "cpu")
	if len(s.Samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(s.Samples))
	}
	if got := s.Samples[0].Value; got != 80 {
		t.Errorf("cpu = %v%%, want 80%%", got)
	}
	if s.Unit != "percent" {
		t.Errorf("unit = %q, want percent", s.Unit)
	}
}

// TestDockerFallsBackToThePerCPUArray covers an older daemon that omits
// online_cpus, which the documented formula says to read from percpu_usage.
func TestDockerFallsBackToThePerCPUArray(t *testing.T) {
	body := `{
	  "cpu_stats": {"cpu_usage": {"total_usage": 200, "percpu_usage": [1,2]},
	                "system_cpu_usage": 1000},
	  "precpu_stats": {"cpu_usage": {"total_usage": 100}, "system_cpu_usage": 500},
	  "memory_stats": {"usage": 1024, "limit": 0, "stats": {}}
	}`
	_, srv := newFakeDocker(t, 200, body)
	d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

	s := seriesNamed(t, measureDocker(t, d, func() {}), "cpu")
	if got := s.Samples[0].Value; got != 40 {
		t.Errorf("cpu = %v%%, want 40%% from two CPUs", got)
	}
}

// TestDockerReportsNoCPUIntervalYet covers the first stats call on a fresh
// daemon: there is no previous sample, so no percentage can be computed.
func TestDockerReportsNoCPUIntervalYet(t *testing.T) {
	_, srv := newFakeDocker(t, 200, dockerStatsJSON(100, 0, 0, 0, 4, 1<<20, 0))
	d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

	report := measureDocker(t, d, func() {})
	if len(report.Series) != 1 || report.Series[0].Name != "memory" {
		t.Errorf("series = %v, want memory alone: the CPU interval is missing", seriesNames(report))
	}
	if len(report.Failures) != 1 {
		t.Fatalf("failures = %v, want the missing CPU interval reported", report.Failures)
	}
	if !strings.Contains(report.Failures[0].Error, "two samples") {
		t.Errorf("failure = %q, want it to explain why", report.Failures[0].Error)
	}
}

// TestDockerReportsMemoryAsTheCLIDoes: the kernel counts reclaimable page cache
// as usage, and the container does not really hold it.
func TestDockerReportsMemoryAsTheCLIDoes(t *testing.T) {
	_, srv := newFakeDocker(t, 200, dockerStatsJSON(200, 100, 1000, 500, 4, 100<<20, 40<<20))
	d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

	s := seriesNamed(t, measureDocker(t, d, func() {}), "memory")
	if got, want := s.Samples[0].Value, float64(60<<20); got != want {
		t.Errorf("memory = %v bytes, want %v (usage minus inactive_file)", got, want)
	}
	if s.Unit != "bytes" {
		t.Errorf("unit = %q, want bytes", s.Unit)
	}
}

// TestDockerMemoryHandlesCgroupsV1 covers the older field name for the same
// reclaimable cache.
func TestDockerMemoryHandlesCgroupsV1(t *testing.T) {
	body := `{
	  "cpu_stats": {"cpu_usage": {"total_usage": 200}, "system_cpu_usage": 1000, "online_cpus": 1},
	  "precpu_stats": {"cpu_usage": {"total_usage": 100}, "system_cpu_usage": 500},
	  "memory_stats": {"usage": 1000000, "limit": 0, "stats": {"cache": 400000}}
	}`
	_, srv := newFakeDocker(t, 200, body)
	d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

	s := seriesNamed(t, measureDocker(t, d, func() {}), "memory")
	if got := s.Samples[0].Value; got != 600000 {
		t.Errorf("memory = %v, want 600000 (usage minus cache)", got)
	}
}

// TestDockerMemoryNeverGoesNegative keeps a daemon whose reclaimable cache
// exceeds its usage from producing a negative reading.
func TestDockerMemoryNeverGoesNegative(t *testing.T) {
	body := `{
	  "cpu_stats": {"cpu_usage": {"total_usage": 200}, "system_cpu_usage": 1000, "online_cpus": 1},
	  "precpu_stats": {"cpu_usage": {"total_usage": 100}, "system_cpu_usage": 500},
	  "memory_stats": {"usage": 1000, "limit": 0, "stats": {"inactive_file": 9999}}
	}`
	_, srv := newFakeDocker(t, 200, body)
	d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

	s := seriesNamed(t, measureDocker(t, d, func() {}), "memory")
	if got := s.Samples[0].Value; got != 1000 {
		t.Errorf("memory = %v, want the usage reported rather than a negative", got)
	}
}

// TestDockerSurvivesANegativeCounter is the bug a live run found: the API
// declares the counters unsigned, so a negative one failed the whole decode and
// took the unrelated memory reading down with it.
func TestDockerSurvivesANegativeCounter(t *testing.T) {
	body := `{
	  "cpu_stats": {"cpu_usage": {"total_usage": 200}, "system_cpu_usage": 1000, "online_cpus": 1},
	  "precpu_stats": {"cpu_usage": {"total_usage": -48}, "system_cpu_usage": -500},
	  "memory_stats": {"usage": 104857600, "limit": 0, "stats": {"inactive_file": 0}}
	}`
	_, srv := newFakeDocker(t, 200, body)
	d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

	report := measureDocker(t, d, func() {})

	// The reading the daemon did send survives, whatever the CPU section says.
	mem := seriesNamed(t, report, "memory")
	if got := mem.Samples[0].Value; got != 104857600 {
		t.Errorf("memory = %v, want the reading the daemon did send", got)
	}
	for _, f := range report.Failures {
		if strings.Contains(f.Error, "decode response") {
			t.Errorf("a negative counter should not fail the decode: %v", f)
		}
	}
}

// TestDockerRejectsACPUDeltaThatWentBackwards covers the counters moving the
// wrong way: there is no interval to measure over, so it is reported rather than
// computed into a percentage that is not there.
func TestDockerRejectsACPUDeltaThatWentBackwards(t *testing.T) {
	body := `{
	  "cpu_stats": {"cpu_usage": {"total_usage": 100}, "system_cpu_usage": 2000, "online_cpus": 1},
	  "precpu_stats": {"cpu_usage": {"total_usage": 200}, "system_cpu_usage": 1000},
	  "memory_stats": {"usage": 104857600, "limit": 0, "stats": {"inactive_file": 0}}
	}`
	_, srv := newFakeDocker(t, 200, body)
	d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

	report := measureDocker(t, d, func() {})
	if len(report.Series) != 1 || report.Series[0].Name != "memory" {
		t.Fatalf("series = %v, want memory alone", seriesNames(report))
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Error, "two samples") {
		t.Errorf("failures = %v, want the impossible interval reported", report.Failures)
	}
}

// TestDockerReportsFailures covers a daemon that does not know the container,
// one that fails, and one that answers with something else entirely.
func TestDockerReportsFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"no such container", 404, `{"message":"No such container: nope"}`, "404"},
		{"server error", 500, `{"message":"boom"}`, "500"},
		{"not json", 200, "<html>hello</html>", "decode response"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFakeDocker(t, tc.status, tc.body)
			d := dockerOn(&Docker{Source: "api", Container: "c", Interval: time.Hour}, srv)

			report := measureDocker(t, d, func() {})
			if len(report.Series) != 0 {
				t.Errorf("series = %v, want none", seriesNames(report))
			}
			if len(report.Failures) != 1 {
				t.Fatalf("failures = %v, want exactly one", report.Failures)
			}
			f := report.Failures[0]
			if f.Source != "api" || !strings.Contains(f.Error, tc.wantErr) {
				t.Errorf("failure = %+v, want it to mention %q", f, tc.wantErr)
			}
		})
	}
}

// TestDockerReportsAnUnreachableDaemon: a load test must carry on with a note
// rather than fail because the daemon is not running.
func TestDockerReportsAnUnreachableDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.Listener.Addr().String()
	srv.Close() // nothing is listening now

	d := &Docker{Source: "api", Container: "c", Interval: time.Hour, HTTP: dialingTo(addr)}

	report := measureDocker(t, d, func() {})
	if len(report.Series) != 0 {
		t.Errorf("series = %v, want none", seriesNames(report))
	}
	if len(report.Failures) != 1 {
		t.Errorf("failures = %v, want the unreachable daemon reported", report.Failures)
	}
}

// TestDockerEndpointResolution follows the docker CLI's own convention, so an
// environment already set up for docker works unchanged.
//
// The tcp:// cases are the point. This test used to assert that a tcp:// host was
// "passed through" to the unix dialer, on the reasoning that the failure would
// then name the host. It does not: dialing "tcp://1.2.3.4:2375" as a path reports
// "no such file or directory", which names a file that was never meant to exist
// and points at nothing the reader can change.
func TestDockerEndpointResolution(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		socket      string
		wantNetwork string
		wantAddress string
		wantErr     string
	}{
		{
			name: "no environment at all", wantNetwork: "unix",
			wantAddress: DefaultDockerSocket,
		},
		{
			name: "an explicit socket wins over the environment",
			env:  "unix:///env.sock", socket: "/custom.sock",
			wantNetwork: "unix", wantAddress: "/custom.sock",
		},
		{
			name: "a unix DOCKER_HOST", env: "unix:///env.sock",
			wantNetwork: "unix", wantAddress: "/env.sock",
		},
		{
			name: "a bare path is a socket", env: "/var/run/docker.sock",
			wantNetwork: "unix", wantAddress: "/var/run/docker.sock",
		},
		{
			name: "a tcp DOCKER_HOST", env: "tcp://1.2.3.4:2375",
			wantNetwork: "tcp", wantAddress: "1.2.3.4:2375",
		},
		{
			name: "an http DOCKER_HOST", env: "http://1.2.3.4:2375",
			wantNetwork: "tcp", wantAddress: "1.2.3.4:2375",
		},
		{
			name: "a scheme autobench cannot speak", env: "ssh://host",
			wantErr: "unsupported DOCKER_HOST scheme",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.env)

			got, err := (&Docker{Socket: tc.socket}).endpoint()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("endpoint = %+v, want an error", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("endpoint: %v", err)
			}
			if got.network != tc.wantNetwork || got.address != tc.wantAddress {
				t.Errorf("endpoint = %s %s, want %s %s",
					got.network, got.address, tc.wantNetwork, tc.wantAddress)
			}
		})
	}
}

// TestDockerUnsupportedSchemeSurfacesAsACollectorProblem: a configuration error
// reaches the report through the same path as a connection failure, rather than
// panicking or failing the run.
func TestDockerUnsupportedSchemeSurfacesAsACollectorProblem(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://host")

	d := &Docker{Source: "api", Container: "whatever", Interval: time.Millisecond}
	stop := d.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	rep := stop()

	if len(rep.Failures) == 0 {
		t.Fatal("no failure reported, want the scheme error surfaced")
	}
	if !strings.Contains(rep.Failures[0].Error, "unsupported DOCKER_HOST scheme") {
		t.Errorf("failure = %v, want the scheme error", rep.Failures[0])
	}
}

// TestDockerNameIsTheSource keeps the report rows labelled by the configured
// name rather than the container ID.
func TestDockerNameIsTheSource(t *testing.T) {
	d := &Docker{Source: "api", Container: "8f3c2b"}
	if d.Name() != "api" {
		t.Errorf("Name() = %q, want api", d.Name())
	}
}
