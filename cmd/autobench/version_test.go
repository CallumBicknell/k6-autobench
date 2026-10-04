package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// statusRE finds the version the README claims this build is.
var statusRE = regexp.MustCompile(`Status: V(\d+\.\d+)`)

// TestVersionIsSemver checks the constant is shaped like a version, so the
// comparison below cannot be satisfied by accident.
func TestVersionIsSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		t.Errorf("version %q is not MAJOR.MINOR.PATCH", version)
	}
}

// TestVersionMatchesTheDocumentedStatus guards a drift that has now happened
// twice: the version constant is bumped by hand, a release ships without it, and
// nothing notices until someone runs `autobench version` and gets a surprise.
//
// The README's status line is the published statement of what this build is, so
// it is what the constant is checked against. It is not a substitute for bumping
// the constant -- it is what makes forgetting to bump it fail the build.
func TestVersionMatchesTheDocumentedStatus(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	m := statusRE.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("README has no %q line to check the version against", "Status: V<major>.<minor>")
	}
	documented := string(m[1])

	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		t.Fatalf("version %q is not MAJOR.MINOR.PATCH", version)
	}
	if got := parts[0] + "." + parts[1]; got != documented {
		t.Errorf("cmd/autobench says %q but README documents V%s: bump whichever is stale",
			version, documented)
	}
}
