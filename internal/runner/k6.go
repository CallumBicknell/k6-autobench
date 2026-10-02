// Package runner spawns the k6 binary with an autobench-controlled environment
// and parses the JSON summary it writes back.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	k6script "github.com/callum/k6-autobench/k6"
)

// Spec is everything the controller hands to a single k6 invocation.
type Spec struct {
	BaseURL         string
	Requests        []config.Request
	Rate            int
	TimeUnit        string
	Duration        time.Duration
	PreAllocatedVUs int
	MaxVUs          int
	Percentiles     []float64
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

// Run executes one k6 run and returns the parsed summary.
func (r *Runner) Run(ctx context.Context, spec Spec) (*metrics.Summary, error) {
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

	args := []string{"run", "--quiet", "--no-color", scriptPath}
	cmd := exec.CommandContext(ctx, k6Path, args...)
	cmd.Env = env
	cmd.Stdin = nil

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

	if err := cmd.Run(); err != nil {
		var parts []string
		if out := stdout.Tail(20); out != "" {
			parts = append(parts, "k6 stdout:\n"+out)
		}
		if out := stderr.Tail(20); out != "" {
			parts = append(parts, "k6 output:\n"+out)
		}
		if len(parts) > 0 {
			return nil, fmt.Errorf("k6 run failed: %w\n%s", err, strings.Join(parts, "\n"))
		}
		return nil, fmt.Errorf("k6 run failed: %w", err)
	}

	raw, err := os.ReadFile(summaryPath)
	if err != nil {
		return nil, fmt.Errorf("k6 did not write a summary to %s: %w", summaryPath, err)
	}

	var summary metrics.Summary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("parse k6 summary: %w", err)
	}
	return &summary, nil
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
	workload := make([]workloadRequest, 0, len(spec.Requests))
	for _, req := range spec.Requests {
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

	env := os.Environ()
	env = append(env,
		"AUTOBENCH_BASE_URL="+spec.BaseURL,
		"AUTOBENCH_RATE="+strconv.Itoa(spec.Rate),
		"AUTOBENCH_TIME_UNIT="+spec.TimeUnit,
		"AUTOBENCH_DURATION="+spec.Duration.String(),
		"AUTOBENCH_PRE_ALLOCATED_VUS="+strconv.Itoa(spec.PreAllocatedVUs),
		"AUTOBENCH_MAX_VUS="+strconv.Itoa(spec.MaxVUs),
		"AUTOBENCH_PERCENTILES="+string(percentilesJSON),
		"AUTOBENCH_WORKLOAD="+string(workloadJSON),
		"AUTOBENCH_SUMMARY_PATH="+summaryPath,
	)
	return env, nil
}
