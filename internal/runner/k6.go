// Package runner spawns the k6 binary with an autobench-controlled environment
// and parses the JSON summary it writes back.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/collect"
	"github.com/CallumBicknell/k6-autobench/internal/config"
	"github.com/CallumBicknell/k6-autobench/internal/metrics"
	k6script "github.com/CallumBicknell/k6-autobench/k6"
)

// Spec is everything the controller hands to a single k6 invocation.
type Spec struct {
	BaseURL string
	// Workload is the HTTP work one iteration performs: a weighted pool, or an
	// ordered journey. It decides both what the script does and how many HTTP
	// calls an iteration makes.
	Workload        config.Workload
	Rate            int
	TimeUnit        string
	Duration        time.Duration
	PreAllocatedVUs int
	MaxVUs          int
	Percentiles     []float64
	// Window, when > 0, makes k6 stream per-sample data so the runner can return
	// per-window aggregates alongside the summary. It costs a sample file
	// proportional to rate x duration, so it is opt-in.
	Window time.Duration
	// Collectors, when set, observe the system under test while k6 runs, so the
	// result carries the server-side view alongside k6's client-side one
	// (PLAN-OUTLINE.md §33).
	Collectors []collect.Collector
	// InsecureTLS accepts any certificate the target presents.
	InsecureTLS bool
	// CAFile is a PEM bundle to verify the target against. k6 reads it from
	// SSL_CERT_FILE, so it replaces the system roots for this run.
	CAFile string
	// ClientCertFile and ClientKeyFile are a client certificate and its key, for
	// a target that authenticates the caller. They are read here rather than by
	// k6 because k6 takes them as PEM contents, not paths.
	ClientCertFile string
	ClientKeyFile  string
	// RequestTimeout bounds one HTTP request. Zero means k6's own default.
	RequestTimeout time.Duration
	// MaxRedirects is how many redirects one request may follow. nil means k6's
	// own default of 10; zero means follow none.
	MaxRedirects *int
	// Timeout bounds how long the k6 process may run. Zero means Duration plus
	// DefaultGrace, which is generous enough never to fire on a healthy run and
	// bounded enough that a wedged k6 cannot hang a pipeline forever.
	Timeout time.Duration
}

// DefaultGrace is the slack a k6 process is given beyond its declared duration
// before it is killed.
//
// A healthy run finishes just after its scenario ends, but k6 has to start up
// and, when windowing is on, write a sample file proportional to rate x
// duration. A minute covers that; ten percent of the run covers a long soak.
const DefaultGrace = time.Minute

// waitDelay bounds how long a killed k6's output pipes may stay open before Run
// gives up waiting on them.
const waitDelay = 5 * time.Second

// timeout is how long the k6 process may take: the declared duration plus slack.
func (s Spec) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	grace := DefaultGrace
	if tenth := s.Duration / 10; tenth > grace {
		grace = tenth
	}
	return s.Duration + grace
}

// Outcome is what one k6 invocation produced.
type Outcome struct {
	Summary *metrics.Summary
	// Windows holds per-window aggregates; empty unless Spec.Window was set.
	Windows []metrics.Window
	// System holds what the configured collectors saw of the system under test
	// while this measurement ran. Empty when no collector is configured, and
	// advisory either way: a collector can never fail a measurement.
	System collect.Report
}

// Runner executes k6.
type Runner struct {
	// K6Path is the k6 binary; defaults to "k6" on PATH.
	K6Path string
	// ScriptPath overrides the embedded workload script when non-empty.
	ScriptPath string
	// Stderr receives k6's own diagnostics. Defaults to os.Stderr.
	Stderr io.Writer
}

// workloadRequest is the JSON shape handed to the k6 script.
type workloadRequest struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Weight  int               `json:"weight"`
	Body    string            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Run executes one k6 run and returns the parsed summary plus, when requested,
// per-window aggregates.
func (r *Runner) Run(ctx context.Context, spec Spec) (*Outcome, error) {
	k6Path, err := exec.LookPath(r.binary())
	if err != nil {
		return nil, fmt.Errorf("k6 binary not found (%q): %w", r.binary(), err)
	}

	tmpDir, err := os.MkdirTemp("", "autobench-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	scriptPath, err := r.script(tmpDir)
	if err != nil {
		return nil, err
	}

	summaryPath := filepath.Join(tmpDir, "summary.json")
	env, err := buildEnv(spec, summaryPath)
	if err != nil {
		return nil, err
	}

	args := []string{"run", "--quiet", "--no-color"}
	var samplesPath string
	if spec.Window > 0 {
		samplesPath = filepath.Join(tmpDir, "samples.jsonl")
		args = append(args, "--out", "json="+samplesPath)
	}
	args = append(args, scriptPath)

	// Bound the k6 process. A healthy run finishes just after its scenario ends,
	// so the deadline only fires on a wedged k6 -- and without it a wedged k6
	// hangs the caller, which in a pipeline means a hung job.
	runCtx, cancel := context.WithTimeout(ctx, spec.timeout())
	defer cancel()

	cmd := exec.CommandContext(runCtx, k6Path, args...)
	cmd.Env = env
	cmd.Stdin = nil
	// Killing k6 kills k6, but anything it spawned could still be holding the
	// output pipes open, and Wait would block on them until it exited. WaitDelay
	// bounds that, so a deadline that fires always returns.
	cmd.WaitDelay = waitDelay

	// k6 logs one warning per failed request, which floods the terminal when the
	// target is unreachable. Capture a bounded tail instead and surface it only
	// if the run fails; an explicit Stderr writer still receives the live stream.
	stdout := &tailBuffer{limit: outputCaptureLimit}
	stderr := &tailBuffer{limit: outputCaptureLimit}
	cmd.Stdout = stdout
	if r.Stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, r.Stderr)
	} else {
		cmd.Stderr = stderr
	}

	// Start observing before k6 does, so the collectors' timelines bracket the
	// measurement rather than starting partway into it.
	stopObserving := observe(ctx, spec.Collectors)

	runErr := cmd.Run()
	// Always stop, even on failure: leaving the sampling goroutines running
	// would leak them into the next measurement.
	system := stopObserving()

	if runErr != nil {
		// A deadline that fired is worth saying plainly: the run was killed,
		// which is different from k6 failing on its own.
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf(
				"k6 did not finish within %s and was killed; it was asked to run for %s. "+
					"Raise run.k6_timeout if the scenario legitimately takes longer",
				spec.timeout(), spec.Duration)
		}

		var parts []string
		if out := stdout.Tail(20); out != "" {
			parts = append(parts, "k6 stdout:\n"+out)
		}
		if out := stderr.Tail(20); out != "" {
			parts = append(parts, "k6 output:\n"+out)
		}
		if len(parts) > 0 {
			return nil, fmt.Errorf("k6 run failed: %w\n%s", runErr, strings.Join(parts, "\n"))
		}
		return nil, fmt.Errorf("k6 run failed: %w", runErr)
	}

	raw, err := os.ReadFile(summaryPath)
	if err != nil {
		return nil, fmt.Errorf("k6 did not write a summary to %s: %w", summaryPath, err)
	}

	var summary metrics.Summary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("parse k6 summary: %w", err)
	}

	out := &Outcome{Summary: &summary, System: system}
	if samplesPath != "" {
		windows, err := readSamples(samplesPath, spec)
		if err != nil {
			return nil, err
		}
		out.Windows = windows
	}
	return out, nil
}

// observe starts every configured collector and returns the function that stops
// them and folds their readings into one report.
//
// Collector failures are advisory by design. The target's monitoring being
// unreachable says nothing about the target's capacity, so a collector reports
// what it could not read and the measurement carries on: a load test that
// errors because Prometheus is down would be worse than useless.
func observe(ctx context.Context, collectors []collect.Collector) func() collect.Report {
	if len(collectors) == 0 {
		return func() collect.Report { return collect.Report{} }
	}

	stops := make([]func() collect.Report, 0, len(collectors))
	for _, c := range collectors {
		stops = append(stops, c.Start(ctx))
	}

	return func() collect.Report {
		reports := make([]collect.Report, len(stops))
		// Stop in reverse, so the observation windows nest around the
		// measurement rather than one source cutting another short.
		for i := len(stops) - 1; i >= 0; i-- {
			reports[i] = stops[i]()
		}
		// Merge in start order, so the report lists sources in the order they
		// were configured rather than the order they happened to stop.
		var out collect.Report
		for _, r := range reports {
			out.Merge(r)
		}
		return out
	}
}

// readSamples parses the JSON-lines sample stream k6 wrote. The temp directory
// is removed when Run returns, so this must happen before then.
func readSamples(path string, spec Spec) ([]metrics.Window, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open k6 samples: %w", err)
	}
	defer f.Close()

	windows, err := metrics.ReadWindows(f, metrics.WindowConfig{
		Size:        spec.Window,
		Percentiles: spec.Percentiles,
	})
	if err != nil {
		return nil, fmt.Errorf("parse k6 samples: %w", err)
	}
	return windows, nil
}

func (r *Runner) binary() string {
	if r.K6Path != "" {
		return r.K6Path
	}
	if v := os.Getenv("AUTOBENCH_K6"); v != "" {
		return v
	}
	return "k6"
}

// outputCaptureLimit bounds how many bytes of k6 stdout/stderr are retained.
const outputCaptureLimit = 64 << 10

// tailBuffer is an io.Writer that keeps only the last limit bytes written.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

// Tail returns at most maxLines trailing lines of captured output.
func (t *tailBuffer) Tail(maxLines int) string {
	s := strings.TrimRight(string(t.buf), "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func (r *Runner) script(tmpDir string) (string, error) {
	if r.ScriptPath != "" {
		if _, err := os.Stat(r.ScriptPath); err != nil {
			return "", fmt.Errorf("workload script %s: %w", r.ScriptPath, err)
		}
		return r.ScriptPath, nil
	}
	path := filepath.Join(tmpDir, "runner.js")
	if err := os.WriteFile(path, []byte(k6script.RunnerJS), 0o644); err != nil {
		return "", fmt.Errorf("write workload script: %w", err)
	}
	return path, nil
}

func buildEnv(spec Spec, summaryPath string) ([]string, error) {
	steps := spec.Workload.Steps()
	workload := make([]workloadRequest, 0, len(steps))
	for _, req := range steps {
		workload = append(workload, workloadRequest{
			Name:    req.Name,
			Method:  req.Method,
			Path:    req.Path,
			Weight:  req.Weight,
			Body:    req.Body,
			Headers: req.Headers,
		})
	}
	workloadJSON, err := json.Marshal(workload)
	if err != nil {
		return nil, fmt.Errorf("encode workload: %w", err)
	}
	percentilesJSON, err := json.Marshal(spec.Percentiles)
	if err != nil {
		return nil, fmt.Errorf("encode percentiles: %w", err)
	}

	if spec.Rate <= 0 {
		return nil, errors.New("rate must be greater than 0")
	}

	mode := "weighted"
	if spec.Workload.IsJourney() {
		mode = "journey"
	}

	env := os.Environ()
	env = append(env,
		"AUTOBENCH_BASE_URL="+spec.BaseURL,
		"AUTOBENCH_RATE="+strconv.Itoa(spec.Rate),
		"AUTOBENCH_TIME_UNIT="+spec.TimeUnit,
		"AUTOBENCH_DURATION="+spec.Duration.String(),
		"AUTOBENCH_PRE_ALLOCATED_VUS="+strconv.Itoa(spec.PreAllocatedVUs),
		"AUTOBENCH_MAX_VUS="+strconv.Itoa(spec.MaxVUs),
		"AUTOBENCH_PERCENTILES="+string(percentilesJSON),
		"AUTOBENCH_WORKLOAD_MODE="+mode,
		"AUTOBENCH_WORKLOAD="+string(workloadJSON),
		"AUTOBENCH_SUMMARY_PATH="+summaryPath,
		"AUTOBENCH_INSECURE_TLS="+strconv.FormatBool(spec.InsecureTLS),
	)
	// Only set when there is one. k6 reads "0s" as a deadline that has already
	// passed rather than as no deadline, so an always-present variable would time
	// every request out immediately -- which is exactly what happened when this
	// was set unconditionally.
	if spec.RequestTimeout > 0 {
		env = append(env, "AUTOBENCH_REQUEST_TIMEOUT="+spec.RequestTimeout.String())
	}
	// Zero is a setting here, so presence is what matters rather than truthiness.
	if spec.MaxRedirects != nil {
		env = append(env, "AUTOBENCH_MAX_REDIRECTS="+strconv.Itoa(*spec.MaxRedirects))
	}
	// k6 reads the trust store from SSL_CERT_FILE, so a private CA is passed the
	// way k6 itself expects rather than through the script.
	if ca := strings.TrimSpace(spec.CAFile); ca != "" {
		env = append(env, "SSL_CERT_FILE="+ca)
	}
	// A client certificate goes through the script: k6 takes tlsAuth as PEM
	// contents, and maps it to the target by domain, so the host goes too.
	if cert := strings.TrimSpace(spec.ClientCertFile); cert != "" {
		host, err := targetHost(spec.BaseURL)
		if err != nil {
			return nil, err
		}
		certPEM, err := os.ReadFile(cert)
		if err != nil {
			return nil, fmt.Errorf("read client certificate: %w", err)
		}
		keyPEM, err := os.ReadFile(strings.TrimSpace(spec.ClientKeyFile))
		if err != nil {
			return nil, fmt.Errorf("read client key: %w", err)
		}
		env = append(env,
			"AUTOBENCH_CLIENT_CERT="+string(certPEM),
			"AUTOBENCH_CLIENT_KEY="+string(keyPEM),
			"AUTOBENCH_TARGET_HOST="+host,
		)
	}
	return env, nil
}

// targetHost is the hostname a client certificate is mapped to. k6's tlsAuth is
// keyed by domain, and its JavaScript runtime has no URL parser to derive one
// from the base URL.
func targetHost(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse target URL %q: %w", baseURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("target URL %q has no host to map a client certificate to", baseURL)
	}
	return host, nil
}
