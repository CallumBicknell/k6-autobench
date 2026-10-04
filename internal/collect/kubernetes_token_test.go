package collect

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKubernetesInClusterDefaults: inside a pod, the token, the URL and the CA all
// come from the mount, so a collector configured with nothing but a name works.
func TestKubernetesInClusterDefaults(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("mounted-token"), 0o644); err != nil {
		t.Fatalf("write token: %v", err)
	}

	k := &Kubernetes{Source: "api", TokenPath: tokenPath}

	if !k.inCluster() {
		t.Fatal("a mounted token directory should read as in-cluster")
	}
	if got := k.urlOrDefault(); got != DefaultInClusterURL {
		t.Errorf("url = %q, want %q", got, DefaultInClusterURL)
	}
	// The CA follows the token's directory, because that is where the kubelet
	// puts it.
	if got := k.caOrDefault(); got != filepath.Join(dir, "ca.crt") {
		t.Errorf("ca = %q, want the ca.crt beside the token", got)
	}
}

// TestKubernetesOutOfClusterHasNoDefaults: outside a cluster there is nothing to
// guess at, and inventing an endpoint would fail with a confusing error rather
// than saying the collector is not configured.
func TestKubernetesOutOfClusterHasNoDefaults(t *testing.T) {
	k := &Kubernetes{
		Source:    "api",
		TokenPath: filepath.Join(t.TempDir(), "nope", "token"),
	}

	if k.inCluster() {
		t.Error("a missing token directory should not read as in-cluster")
	}
	if got := k.urlOrDefault(); got != "" {
		t.Errorf("url = %q, want none", got)
	}
	if got := k.caOrDefault(); got != "" {
		t.Errorf("ca = %q, want none", got)
	}
}

// TestKubernetesExplicitSettingsWin: the in-cluster defaults fill a gap, they do
// not override what someone wrote down.
func TestKubernetesExplicitSettingsWin(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("mounted-token"), 0o644); err != nil {
		t.Fatalf("write token: %v", err)
	}

	k := &Kubernetes{
		Source:    "api",
		TokenPath: tokenPath,
		URL:       "https://api.example:6443",
		CAFile:    "/etc/ca.pem",
	}

	if got := k.urlOrDefault(); got != "https://api.example:6443" {
		t.Errorf("url = %q, want the configured one", got)
	}
	if got := k.caOrDefault(); got != "/etc/ca.pem" {
		t.Errorf("ca = %q, want the configured one", got)
	}
}

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
