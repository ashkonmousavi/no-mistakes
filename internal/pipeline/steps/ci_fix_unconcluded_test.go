package steps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Regression tests for a CI fix agent that stops part-way: an unfinished merge
// or a declared stop must never be committed or published, the agent's bytes
// stay where it left them, and an ordinary finished repair still publishes.
// They use only identifiers that already exist upstream so the same file runs
// against the unpatched base.

func unconcludedGit(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=a@x", "GIT_EDITOR=true")
	_ = cmd.Run()
}

// newUnconcludedFixture advances main past the reviewed feature head, on the
// same file when conflict is true, and wires a fake agent.
func newUnconcludedFixture(t *testing.T, conflict bool, act func(dir string), conclusion string) *ciRepairFixture {
	t.Helper()
	f := newCIRepairFixture(t, false, nil)
	gitCmd(t, f.dir, "checkout", "main")
	name := "main-only.txt"
	if conflict {
		name = "feature.txt"
	}
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte("main side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "advance main")
	gitCmd(t, f.dir, "checkout", "feature")
	f.sctx.Agent = &mockAgent{name: "native", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		act(opts.CWD)
		return &agent.Result{Output: json.RawMessage(conclusion)}, nil
	}}
	f.sctx.Ctx = context.Background()
	f.sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test","check_id":"github-check-run:42"}]}`
	return f
}

func (f *ciRepairFixture) repairRound(t *testing.T) *pipeline.StepOutcome {
	t.Helper()
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, CompletedAt: time.Now()}}}
	outcome, err := (&CIStep{}).repairFromFindings(f.sctx, host, &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("repair round error: %v\nlog:\n%s", err, f.log())
	}
	return outcome
}

func mergeHeadPresent(t *testing.T, dir string) bool {
	t.Helper()
	path := gitCmd(t, dir, "rev-parse", "--git-path", "MERGE_HEAD")
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	_, err := os.Stat(path)
	return err == nil
}

func leaveMerge(dir string) { unconcludedGit(dir, "merge", "--no-edit", "main") }

func TestCIRepair_UnfinishedMergeIsNeverPublished(t *testing.T) {
	// Today's agents report true with a prose refusal; the guard must not
	// depend on the conclusion at all.
	f := newUnconcludedFixture(t, true, leaveMerge, `{"summary":"admission refused; merge left unresolved","code_change_needed":true}`)
	outcome := f.repairRound(t)
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s; an unfinished merge was published", got)
	}
	if published := gitCmd(t, f.upstream, "show", "refs/heads/feature:feature.txt"); strings.Contains(published, "<<<<<<<") {
		t.Errorf("published content carries conflict markers:\n%s", published)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want the round parked for a decision", outcome)
	}
	if !mergeHeadPresent(t, f.dir) {
		t.Error("the agent's unfinished merge was not left in place")
	}
	if status := gitCmd(t, f.dir, "status", "--porcelain"); !strings.Contains(status, "AA feature.txt") {
		t.Errorf("conflicted bytes were not preserved; status:\n%s", status)
	}
}

func TestCIRepair_StoppedChangedHeadIsRetainedNotPublished(t *testing.T) {
	f := newUnconcludedFixture(t, false, leaveMerge, `{"summary":"post-merge admission refused","code_change_needed":true,"stopped":true}`)
	outcome := f.repairRound(t)
	local := f.localHead(t)
	if local == f.headSHA {
		t.Fatal("fixture error: the agent's merge commit is missing")
	}
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s; a stopped repair was published", got)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want the round parked for a decision", outcome)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA != nil && *run.LastPushedSHA == local {
		t.Error("a stopped repair was recorded as published")
	}
	if f.localHead(t) != local {
		t.Error("the agent's commit was not kept in the run worktree")
	}
}

func TestCIRepair_StoppedWithoutChangesParks(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(string) {}, `{"summary":"pre-merge admission refused","code_change_needed":true,"stopped":true}`)
	outcome := f.repairRound(t)
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s", got)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want a declared stop to park instead of resuming monitoring", outcome)
	}
}

func TestCIRepair_ResolvedMergeStillPublishes(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(dir string) {
		leaveMerge(dir)
		_ = os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("main side\nfeature\n"), 0o644)
		unconcludedGit(dir, "add", "-A")
		unconcludedGit(dir, "commit", "--no-edit")
	}, `{"summary":"merged main and resolved the conflict","code_change_needed":true}`)
	outcome := f.repairRound(t)
	local := f.localHead(t)
	if local == f.headSHA || f.remoteHead(t) != local {
		t.Errorf("remote = %s, local = %s; the finished repair was not published", f.remoteHead(t), local)
	}
	if published := gitCmd(t, f.upstream, "show", "refs/heads/feature:feature.txt"); strings.Contains(published, "<<<<<<<") {
		t.Errorf("published content carries conflict markers:\n%s", published)
	}
	if outcome != nil && outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want monitoring to resume after a published repair", outcome)
	}
}

// The shared catch-all staging boundary serves CI repair commits, the
// Review/Test/Document/Lint fix commits, and Push's leftover commit.
func TestStagePipelineChanges_RefusesUnfinishedMerge(t *testing.T) {
	f := newUnconcludedFixture(t, true, leaveMerge, `{}`)
	leaveMerge(f.dir)
	if !mergeHeadPresent(t, f.dir) {
		t.Fatal("fixture error: no unfinished merge")
	}
	if err := stagePipelineChanges(f.sctx); err == nil {
		t.Error("staging succeeded over an unfinished merge")
	}
	if unmerged := gitCmd(t, f.dir, "diff", "--name-only", "--diff-filter=U"); unmerged != "feature.txt" {
		t.Errorf("unmerged paths after refusal = %q, want the index left untouched", unmerged)
	}
	if err := commitAgentFixes(f.sctx, types.StepReview, "apply review fix", ""); err == nil {
		t.Error("a Review fix commit concluded an unfinished merge")
	}
	if got := f.localHead(t); got != f.headSHA {
		t.Errorf("HEAD moved to %s", got)
	}
	if !mergeHeadPresent(t, f.dir) {
		t.Error("the unfinished merge was not left in place")
	}
}
