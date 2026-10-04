package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/callum/k6-autobench/internal/config"
)

// TestSpecTimeout covers the deadline a k6 process is given: the declared
// duration plus slack, or an explicit override.
func TestSpecTimeout(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want time.Duration
	}{
		{"a short run gets the grace", Spec{Duration: 30 * time.Second}, 90 * time.Second},
		{"a run at the boundary gets the grace", Spec{Duration: time.Minute}, 2 * time.Minute},
		{"a long soak gets a tenth of itself", Spec{Duration: time.Hour}, 66 * time.Minute},
		{"an explicit timeout wins", Spec{Duration: time.Hour, Timeout: time.Minute}, time.Minute},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.timeout(); got != tc.want {
				t.Errorf("timeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunKillsAWedgedK6 is the production property: a k6 that never finishes must
// not hang the caller, which in a pipeline means a hung job. The test uses a
// shell script standing in for k6, because that is the only way to hang a
// process on purpose.
func TestRunKillsAWedgedK6(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in script is a shell script")
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "k6")
	// A k6 that starts, ignores its arguments, and never exits. The exec
	// matters: it replaces the shell with sleep, so the process CommandContext
	// kills is the one that is actually hanging.
	script := "#!/bin/sh\nexec sleep 300\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake k6: %v", err)
	}

	r := &Runner{K6Path: fake}
	spec := specFor(config.Workload{Requests: []config.Request{{Name: "root", Path: "/"}}})
	spec.Duration = time.Second
	spec.Timeout = 2 * time.Second

	start := time.Now()
	_, err := r.Run(context.Background(), spec)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a wedged k6 should be reported as an error")
	}
	if !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("error = %q, want it to say the run was killed", err)
	}
	if elapsed > 15*time.Second {
		t.Errorf("Run took %v; the deadline should have fired at 2s", elapsed)
	}
}

// TestRunSaysNothingAboutADeadlineWhenItDidNotFire keeps an ordinary k6 failure
// from being reported as a timeout.
func TestRunSaysNothingAboutADeadlineWhenItDidNotFire(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in script is a shell script")
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "k6")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write fake k6: %v", err)
	}

	r := &Runner{K6Path: fake}
	spec := specFor(config.Workload{Requests: []config.Request{{Name: "root", Path: "/"}}})
	spec.Duration = time.Second
	spec.Timeout = time.Minute

	_, err := r.Run(context.Background(), spec)
	if err == nil {
		t.Fatal("a k6 that exits non-zero should be an error")
	}
	if strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("error = %q, want an ordinary failure rather than a timeout", err)
	}
}
