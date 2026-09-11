package pipeline

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	_ "modernc.org/sqlite"
)

func TestExecutor_TerminalizesRunWhenInitialStatusWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunRunning))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

func TestExecutor_TerminalizesRunWhenFinalCompletedWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunCompleted))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

// TestExecutor_PostReviewHeadAdvanceRoundWriteFailureStopsBeforePush proves
// the executor cannot continue toward publication after the durable round
// that invalidates the prior Review epoch fails to persist.
func TestExecutor_PostReviewHeadAdvanceRoundWriteFailureStopsBeforePush(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "pipeline@example.test"},
		{"config", "user.name", "Pipeline Test"},
	} {
		if _, err := git.Run(context.Background(), workDir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "base"}} {
		if _, err := git.Run(context.Background(), workDir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	head, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	repo.WorkingPath = workDir
	installHeadAdvanceRoundFailureTrigger(t, p.DB())

	review := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{ReviewApprovedHeadSHA: head}, nil
	}}
	test := newPassStep(types.StepTest)
	documentCalls := 0
	document := &adaptiveCallStep{name: types.StepDocument, fn: func(sctx *StepContext) (*StepOutcome, error) {
		documentCalls++
		if documentCalls > 1 {
			return &StepOutcome{}, nil
		}
		path := filepath.Join(sctx.WorkDir, "docs", "reference.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte("corrected reference\n"), 0o644); err != nil {
			return nil, err
		}
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "docs/reference.md"); err != nil {
			return nil, err
		}
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "commit", "-m", "document correction"); err != nil {
			return nil, err
		}
		// The agent committed directly, leaving the ordinary outcome unaware of
		// the head advance. bindPostReviewHead must discover and persist it.
		return &StepOutcome{FixSummary: FixSummaryNoChangesApplied}, nil
	}}
	push := newPassStep(types.StepPush)
	exec := NewExecutor(database, p, nil, nil, []Step{review, test, document, push}, nil)

	err = exec.Execute(context.Background(), run, repo, workDir)
	if err == nil || !strings.Contains(err.Error(), "persist post-review head advance") {
		t.Fatalf("Execute() error = %v, want durable head-advance round refusal", err)
	}
	if push.callCount() != 0 {
		t.Fatalf("push calls = %d, want no publication after provenance write failure", push.callCount())
	}
	persistedRun, dbErr := database.GetRun(run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if persistedRun.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", persistedRun.Status)
	}
	steps, dbErr := database.GetStepsByRun(run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	for _, step := range steps {
		if step.StepName == types.StepDocument && step.Status != types.StepStatusFailed {
			t.Fatalf("Document status = %s, want failed", step.Status)
		}
	}
}

func installHeadAdvanceRoundFailureTrigger(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_post_review_head_advance_round
		BEFORE INSERT ON step_rounds
		WHEN NEW.fix_summary = 'changes applied'
		BEGIN
			SELECT RAISE(FAIL, 'injected head-advance round write failure');
		END`)
	if err != nil {
		t.Fatal(err)
	}
}

func installRunStatusFailureTrigger(t *testing.T, path, status string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_test_run_status
		BEFORE UPDATE OF status ON runs
		WHEN NEW.status = '` + status + `'
		BEGIN
			SELECT RAISE(FAIL, 'injected status write failure');
		END`)
	if err != nil {
		t.Fatal(err)
	}
}

func assertDurableFailedRunAndEvent(t *testing.T, database *db.DB, runID string, events *eventCollector) {
	t.Helper()
	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("durable run status = %s, want failed", got.Status)
	}
	completed := events.findRunEvent(ipc.EventRunCompleted)
	if completed == nil || completed.Status == nil || *completed.Status != string(types.RunFailed) {
		t.Fatalf("terminal event = %+v, want failed run_completed", completed)
	}
}
