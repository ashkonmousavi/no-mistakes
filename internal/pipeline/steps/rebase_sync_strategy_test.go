package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestRebaseStep_MergeStrategy_NonConflictingSyncProducesMergeCommit proves
// that with sync_strategy: merge, a non-conflicting divergence from the
// default branch is integrated with an ordinary merge commit rather than a
// rebase: the pre-sync head stays an ancestor of the new head, and so does
// the target's tip.
func TestRebaseStep_MergeStrategy_NonConflictingSyncProducesMergeCommit(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Advance main with a non-conflicting change, push.
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("main update\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main non-conflicting update")
	gitCmd(t, dir, "push", "origin", "main")
	originMain := gitCmd(t, dir, "rev-parse", "origin/main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.SyncStrategy = config.SyncStrategyMerge

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected clean merge, got needs-approval: %s", outcome.Findings)
	}

	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	parents := gitCmd(t, dir, "rev-list", "--parents", "-n", "1", "HEAD")
	if fields := strings.Fields(parents); len(fields) != 3 {
		t.Fatalf("expected HEAD to be a two-parent merge commit, git rev-list --parents: %q", parents)
	}
	if mb := gitCmd(t, dir, "merge-base", headSHA, newHead); mb != headSHA {
		t.Fatalf("pre-merge feature head %s is not an ancestor of the new head %s (merge-base %s)", headSHA, newHead, mb)
	}
	if mb := gitCmd(t, dir, "merge-base", originMain, newHead); mb != originMain {
		t.Fatalf("origin/main tip %s is not an ancestor of the new head %s (merge-base %s)", originMain, newHead, mb)
	}

	status := gitStatusPorcelain(t, dir)
	if status != "" {
		t.Fatalf("expected clean worktree, got: %s", status)
	}
}

// TestRebaseStep_MergeStrategy_ConflictFixUsesMergeNotRebase proves that with
// sync_strategy: merge, a conflicting sync in fix mode tells the agent to
// resolve a git merge (never a rebase) and, once the agent stages the
// resolution and commits, leaves the pre-fix head reachable as an ancestor of
// the new head.
func TestRebaseStep_MergeStrategy_ConflictFixUsesMergeNotRebase(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base content\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("main change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main conflict")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	// Agent simulates resolving a merge conflict: resolve file, git add, git commit --no-edit.
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("resolved content\n"), 0o644)
			cmd := exec.Command("git", "add", "shared.txt")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git add: %s: %w", out, err)
			}
			cmd = exec.Command("git", "commit", "--no-edit")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
				"GIT_EDITOR=true",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git commit: %s: %w", out, err)
			}
			return &agent.Result{
				Output: json.RawMessage(`{"summary":"resolve merge conflict in shared.txt"}`),
			}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.SyncStrategy = config.SyncStrategyMerge
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","file":"shared.txt","description":"merge conflict rebasing onto origin/feature"}]}`

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after successful fix")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "Resolve git merge conflicts") {
		t.Fatalf("expected merge-strategy prompt, got: %s", prompt)
	}
	if strings.Contains(prompt, "git rebase --continue") || strings.Contains(prompt, "Resolve git rebase conflicts") {
		t.Fatalf("expected prompt to never mention rebase under merge strategy, got: %s", prompt)
	}
	if !strings.Contains(prompt, "Do not run git rebase, git reset --hard") {
		t.Fatalf("expected prompt to explicitly forbid rebase/reset --hard/force-push, got: %s", prompt)
	}

	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if mb := gitCmd(t, dir, "merge-base", headSHA, newHead); mb != headSHA {
		t.Fatalf("pre-fix feature head %s is not an ancestor of the new head %s (merge-base %s)", headSHA, newHead, mb)
	}
	parents := gitCmd(t, dir, "rev-list", "--parents", "-n", "1", "HEAD")
	if fields := strings.Fields(parents); len(fields) != 3 {
		t.Fatalf("expected HEAD to be a two-parent merge commit, git rev-list --parents: %q", parents)
	}
}
