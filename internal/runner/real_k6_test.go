package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
)

// requireK6 skips a test when there is no k6 to run. These are integration tests
// against the real generator, because the bug they exist for lived in the
// interaction between the embedded script and the environment autobench builds --
// a unit test of either half would have passed.
func requireK6(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("k6")
	if err != nil {
		t.Skip("k6 is not installed")
	}
	return path
}

// TestRealK6RunWithNoRequestTimeout is the regression test for a regression that
// shipped: the request-timeout variable was set unconditionally, and k6 reads
// "0s" as a deadline that has already passed rather than as no deadline, so every
// request in every run timed out at once.
//
// The hole in the verification was testing the new setting and never re-testing
// the default path it changed.
func TestRealK6RunWithNoRequestTimeout(t *testing.T) {
	k6 := requireK6(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := &Runner{K6Path: k6}
	spec := specFor(config.Workload{Requests: []config.Request{{Name: "root", Path: "/"}}})
	spec.BaseURL = srv.URL
	spec.Rate = 20
	spec.Duration = time.Second

	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := metrics.Extract(res.Summary, []float64{95})

	if m.Requests == 0 {
		t.Fatal("no requests were made at all")
	}
	if m.FailedRequests != 0 {
		t.Errorf("failed requests = %d of %d, want none: a run with no timeout "+
			"configured must not time every request out", m.FailedRequests, m.Requests)
	}
	if m.ErrorRate != 0 {
		t.Errorf("error rate = %v, want 0", m.ErrorRate)
	}
	if !m.HasLatency {
		t.Error("no latency recorded: the requests did not complete")
	}
}

// TestRealK6HonoursTheRequestTimeout: the setting has to do what it says, which
// is the other half of the pair.
func TestRealK6HonoursTheRequestTimeout(t *testing.T) {
	k6 := requireK6(t)

	// A server that accepts the connection and never answers.
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	r := &Runner{K6Path: k6}
	spec := specFor(config.Workload{Requests: []config.Request{{Name: "root", Path: "/"}}})
	spec.BaseURL = srv.URL
	spec.Rate = 2
	spec.Duration = time.Second
	spec.RequestTimeout = 500 * time.Millisecond

	start := time.Now()
	res, err := r.Run(context.Background(), spec)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The run is bounded by the duration plus teardown, not by the server.
	if elapsed > 30*time.Second {
		t.Errorf("run took %v, want it bounded by the configured timeout", elapsed)
	}
	m := metrics.Extract(res.Summary, []float64{95})
	if m.FailedRequests == 0 {
		t.Error("no failures recorded against a server that never answers")
	}
}
