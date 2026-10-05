package custody

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestFixProgressRescuePreservesIndexAndWorkingBytes(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("edit", []byte("base\n"))
	write("gone", []byte("gone\n"))
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "files")
	head := gitOutput(t, dir, "rev-parse", "HEAD")
	write("edit", []byte("staged\n"))
	gitRun(t, dir, "add", "edit")
	write("edit", []byte("unfinished\n"))
	write("new.bin", []byte{0, 255, 13, 10})
	if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := PreservePartialWork(context.Background(), dir, "run-1", "review", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "saved" || snapshot.ParentHead != head {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	for spec, expected := range map[string]string{snapshot.Ref + ":edit": "unfinished\n", snapshot.Ref + "^2:edit": "staged\n", snapshot.Ref + ":new.bin": string([]byte{0, 255, 13, 10})} {
		got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", spec)
		if err != nil || string(got) != expected {
			t.Fatalf("%s = %q, %v", spec, got, err)
		}
	}
	if _, err := git.Run(context.Background(), dir, "cat-file", "-e", snapshot.Ref+":gone"); err == nil {
		t.Fatal("deleted file saved as present")
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || string(after) != string(before) {
		t.Fatal("capture modified live index")
	}
	if got := gitOutput(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("capture moved HEAD")
	}
	if err := ValidatePartialWork(context.Background(), dir, snapshot); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "update-ref", snapshot.Ref, head)
	if err := ValidatePartialWork(context.Background(), dir, snapshot); err == nil {
		t.Fatal("moved ref accepted")
	}
}

func TestFixProgressRescueIgnoredWorkRetainsOriginal(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	for name, data := range map[string]string{".gitignore": "private\n", "private": "useful unfinished bytes\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := PreservePartialWork(context.Background(), dir, "run-1", "test", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "retained" || snapshot.Reason == "" || snapshot.Ref == "" {
		t.Fatalf("ignored bytes incorrectly declared saved: %+v", snapshot)
	}
}
