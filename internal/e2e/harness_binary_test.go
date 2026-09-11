//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSuppliedNoMistakesBinary proves the post-install seam accepts the exact
// no-mistakes artifact the consumer supplied, while refusing every invalid
// input instead of silently rebuilding the checkout.
func TestSuppliedNoMistakesBinary(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	valid := filepath.Join(t.TempDir(), executableName("no-mistakes"))
	cmd := exec.Command("go", "build", "-o", valid, "./cmd/no-mistakes")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build consumer no-mistakes binary: %v\n%s", err, out)
	}

	t.Run("accepts_absolute_verified_binary", func(t *testing.T) {
		t.Setenv(e2eNoMistakesBinEnv, valid)
		got, err := suppliedNoMistakesBinary()
		if err != nil {
			t.Fatalf("verify supplied binary: %v", err)
		}
		if got != valid {
			t.Fatalf("supplied binary = %q, want exact consumer path %q", got, valid)
		}
	})

	t.Run("rejects_relative_path", func(t *testing.T) {
		t.Setenv(e2eNoMistakesBinEnv, "bin/no-mistakes")
		assertSuppliedBinaryRejected(t, "must be an absolute path")
	})

	t.Run("rejects_non_executable_file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-executable")
		if err := os.WriteFile(path, []byte("not a binary\n"), 0o644); err != nil {
			t.Fatalf("write non-executable fixture: %v", err)
		}
		t.Setenv(e2eNoMistakesBinEnv, path)
		assertSuppliedBinaryRejected(t, "not executable")
	})

	t.Run("rejects_wrong_executable", func(t *testing.T) {
		wrong, err := os.Executable()
		if err != nil {
			t.Fatalf("find test executable: %v", err)
		}
		t.Setenv(e2eNoMistakesBinEnv, wrong)
		assertSuppliedBinaryRejected(t, "verify "+e2eNoMistakesBinEnv)
	})
}

func assertSuppliedBinaryRejected(t *testing.T, want string) {
	t.Helper()
	got, err := suppliedNoMistakesBinary()
	if err == nil {
		t.Fatalf("supplied binary = %q, want rejection", got)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejection = %q, want %q", err, want)
	}
}
