package custody

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestFixProgressResumeRestoresExactRawWorkingAndIndexOnce(t *testing.T) {
	source, _ := recoveryTestRepo(t)
	write := func(dir, name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(source, "edit", "base")
	write(source, "gone", "delete me")
	gitRun(t, source, "add", "-A")
	gitRun(t, source, "commit", "-m", "source files")
	write(source, "edit", "staged")
	gitRun(t, source, "add", "edit")
	write(source, "edit", "unfinished C")
	write(source, "new.bin", string([]byte{0, 255, 10, 13}))
	if err := os.Remove(filepath.Join(source, "gone")); err != nil {
		t.Fatal(err)
	}
	p, err := PreservePartialWork(context.Background(), source, "run-1", "review", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	gitRun(t, source, "clone", "--shared", source, target)
	gitRun(t, target, "fetch", source, p.Ref+":"+p.Ref)
	gitRun(t, target, "config", "filter.restore.smudge", "git update-ref refs/no-mistakes/filter-ran HEAD")
	write(target, "author-work", "do not overwrite")
	if err = RestorePartialWork(context.Background(), target, p); err == nil {
		t.Fatal("dirty author work overwritten")
	}
	if got, e := os.ReadFile(filepath.Join(target, "author-work")); e != nil || string(got) != "do not overwrite" {
		t.Fatal("refusal lost author bytes")
	}
	if err = os.Remove(filepath.Join(target, "author-work")); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err = RestorePartialWork(context.Background(), target, p); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]string{"edit": "unfinished C", "new.bin": string([]byte{0, 255, 10, 13})} {
			got, e := os.ReadFile(filepath.Join(target, name))
			if e != nil || string(got) != want {
				t.Fatalf("restored %s = %q %v", name, got, e)
			}
		}
		if _, e := os.Stat(filepath.Join(target, "gone")); !os.IsNotExist(e) {
			t.Fatalf("deletion lost: %v", e)
		}
		staged, e := git.RunRaw(context.Background(), target, "show", ":edit")
		if e != nil || string(staged) != "staged" {
			t.Fatalf("staged bytes %q %v", staged, e)
		}
		if got := gitOutput(t, target, "rev-parse", "HEAD"); got != p.ParentHead {
			t.Fatal("rescue became ordinary head")
		}
		if got := gitOutput(t, target, "rev-parse", p.Ref); got != p.SHA {
			t.Fatal("consumption removed source rescue")
		}
	}
	if _, err = git.Run(context.Background(), target, "rev-parse", "--verify", "refs/no-mistakes/filter-ran"); err == nil {
		t.Fatal("restoration executed smudge filter")
	}
	gitRun(t, target, "config", "user.name", "test")
	gitRun(t, target, "config", "user.email", "test@example.invalid")
	gitRun(t, target, "commit", "--allow-empty", "-m", "new author head")
	if err = RestorePartialWork(context.Background(), target, p); err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("divergent parent accepted: %v", err)
	}
}
