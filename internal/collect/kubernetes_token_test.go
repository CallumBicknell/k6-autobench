package collect

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKubernetesTokenPrecedence covers the three places a token comes from. The
// mounted file is the one that matters in a cluster, and it is a fixed path this
// test cannot write to, so the path is settable for the same reason System.Proc
// and Docker.Socket are.
func TestKubernetesTokenPrecedence(t *testing.T) {
	mounted := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(mounted, []byte("  mounted-token\n"), 0o644); err != nil {
		t.Fatalf("write token: %v", err)
	}

	t.Run("an explicit token wins", func(t *testing.T) {
		t.Setenv("AUTOBENCH_K8S_TOKEN", "env-token")
		k := &Kubernetes{Token: "explicit", TokenPath: mounted}
		if got := k.token(); got != "explicit" {
			t.Errorf("token = %q, want the explicit one", got)
		}
	})

	t.Run("the environment beats the mount", func(t *testing.T) {
		t.Setenv("AUTOBENCH_K8S_TOKEN", "env-token")
		k := &Kubernetes{TokenPath: mounted}
		if got := k.token(); got != "env-token" {
			t.Errorf("token = %q, want the environment's", got)
		}
	})

	t.Run("the mounted file is read and trimmed", func(t *testing.T) {
		t.Setenv("AUTOBENCH_K8S_TOKEN", "")
		k := &Kubernetes{TokenPath: mounted}
		if got := k.token(); got != "mounted-token" {
			t.Errorf("token = %q, want the mounted one, trimmed", got)
		}
	})

	t.Run("a missing mount is empty rather than an error", func(t *testing.T) {
		t.Setenv("AUTOBENCH_K8S_TOKEN", "")
		k := &Kubernetes{TokenPath: filepath.Join(t.TempDir(), "nope")}
		if got := k.token(); got != "" {
			t.Errorf("token = %q, want empty", got)
		}
	})

	t.Run("the default path is the kubelet's", func(t *testing.T) {
		if got := (&Kubernetes{}).tokenPath(); got != DefaultServiceAccountToken {
			t.Errorf("tokenPath = %q, want %q", got, DefaultServiceAccountToken)
		}
	})
}
