package steps

import (
	"context"
	"errors"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixProgressReviewCommitsBeforeNextFindingStarts(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls <= 2 {
			if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != string(rune('0'+calls-1)) {
				t.Fatalf("finding %d started before preceding commit: %s", calls, got)
			}
			if !strings.Contains(opts.Prompt, "Authorized repair finding") {
				t.Fatal("repair scope not explicit")
			}
			if err := os.WriteFile(filepath.Join(dir, string(rune('A'+calls-1))+".txt"), []byte("applied"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: []byte(`{"summary":"repair one cause"}`)}, nil
		}
		return nil, errors.New("cut C")
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","description":"cause A","action":"auto-fix"},{"id":"B","severity":"error","description":"cause B","action":"auto-fix"},{"id":"C","severity":"error","description":"cause C","action":"auto-fix"}],"summary":"three"}`
	_, err := (&ReviewStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "cut C") {
		t.Fatalf("stop lost: %v", err)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "2" {
		t.Fatalf("completed causes lost: %s commits", got)
	}
	p, err := sctx.DB.FixProgress(sctx.Run.ID)
	if err != nil || p == nil || p.Applied != 2 || p.Total != 3 {
		t.Fatalf("progress=%+v %v", p, err)
	}
}

func TestFixSizingUnmeasuredOversizedCauseDoesNotLaunch(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return nil, errors.New("oversized fixer launched")
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","action":"auto-fix","description":"` + strings.Repeat("oversized cause ", 400) + `"}]}`
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || calls != 0 || !strings.Contains(outcome.Findings, "fix-estimate-exceeds-deadline") {
		t.Fatalf("oversized unmeasured launch: calls=%d outcome=%+v error=%v", calls, outcome, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("refusal changed head: %s", got)
	}
}

func bindStepResult(t *testing.T, sctx *pipeline.StepContext, step types.StepName) {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, step)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
}

func TestFixProgressCILocalCheckpointDoesNotPublishMidBatch(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 2 {
			if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
				t.Fatalf("first CI cause uncommitted: %s", got)
			}
			if got := gitCmd(t, dir, "ls-remote", "origin", "refs/heads/feature"); !strings.Contains(got, head) {
				t.Fatalf("intermediate CI repair published: %s", got)
			}
			return nil, errors.New("cut second check")
		}
		if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("fixed"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: []byte(`{"summary":"repair first check","code_change_needed":true}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepCI)
	sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","description":"test A","category":"ci-check","check":"A","check_id":"1","action":"auto-fix"},{"id":"B","severity":"error","description":"test B","category":"ci-check","check":"B","check_id":"2","action":"auto-fix"}]}`
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "set-url", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	targets, err := parseCIFixTargets(sctx.PreviousFindings)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&CIStep{}).autoFixCI(sctx, &mockReviewHost{}, &scm.PR{Number: "1"}, targets)
	if err == nil || !strings.Contains(err.Error(), "cut second check") {
		t.Fatalf("CI stop: %v", err)
	}
	p, err := sctx.DB.FixProgress(sctx.Run.ID)
	if err != nil || p.Applied != 1 || p.Total != 2 {
		t.Fatalf("CI progress: %+v %v", p, err)
	}
}

func TestFixProgressTestUnionsNewTestsBeforeTheirCommits(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls <= 2 {
			name := string(rune('A'+calls-1)) + "_test.go"
			if err := os.WriteFile(filepath.Join(dir, name), []byte("package main\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: []byte(`{"summary":"add regression test"}`)}, nil
		}
		if !strings.Contains(opts.Prompt, "A_test.go") || !strings.Contains(opts.Prompt, "B_test.go") {
			t.Fatalf("evidence lost earlier regression paths: %s", opts.Prompt)
		}
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"validated","tested":["fixture"],"testing_summary":"fixture","scenarios":[{"name":"fixture validation","result":"untested","live":false,"evidence":"","reason":"fixture"}],"verdict":"inconclusive","artifacts":[]}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","description":"cause A","action":"auto-fix"},{"id":"B","severity":"error","description":"cause B","action":"auto-fix"}]}`
	_, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("repair/evidence calls=%d", calls)
	}
}

// Applied is a completed edit turn, not a guarantee that another overlapping
// finding needs an extra commit or that validation passed.
func TestFixProgressReviewNoopCauseDoesNotInventACommit(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(dir, "overlap.txt"), []byte("fixed both siblings"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if calls == 3 {
			return nil, errors.New("cut focused verification")
		}
		return &agent.Result{Output: []byte(`{"summary":"cause already repaired at shared seam"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"sibling A"},{"id":"B","description":"sibling B"}]}`
	_, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{Prompt: "repair selected causes"})
	if err == nil || !strings.Contains(err.Error(), "cut focused verification") {
		t.Fatalf("verification stop = %v", err)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("overlapping noop invented commit: %s", got)
	}
	p, err := sctx.DB.FixProgress(sctx.Run.ID)
	if err != nil || p.Applied != 2 || !p.ValidationPending {
		t.Fatalf("unverified progress = %+v, %v", p, err)
	}
}

func TestFixProgressReviewInvalidSummaryStopsBeforeNextUnit(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("unfinished"), 0644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: []byte(`{"summary":""}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"first"},{"id":"B","description":"next"}]}`
	_, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{Prompt: "repair selected causes"})
	if err == nil || !strings.Contains(err.Error(), "empty repair-unit summary") {
		t.Fatalf("invalid summary accepted: %v", err)
	}
	p, err := sctx.DB.FixProgress(sctx.Run.ID)
	if err != nil || calls != 1 || p.Applied != 0 || gitCmd(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("invalid unit advanced: calls %d progress %+v err %v", calls, p, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "partial.txt")); err != nil || string(got) != "unfinished" {
		t.Fatalf("invalid unit bytes lost: %q %v", got, err)
	}
}

func TestFixProgressAuthorityPendingUnitCannotPublish(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"unfinished"}]}`
	if err := sctx.BeginFixUnit(types.StepCI, types.Finding{ID: "A", Description: "unfinished"}, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := publishRunHead(sctx, head, head, nil); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("pending repair published: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("publication changed head: %s", got)
	}
}

func TestFixProgressCIEmptySummaryUsesExistingCommitFallback(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if err := os.WriteFile(filepath.Join(opts.CWD, "empty-result.txt"), []byte("useful repair"), 0644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepCI)
	sctx.Config.CI.RevalidateRepairs = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","category":"ci-check","check":"tests","description":"failing tests"}]}`
	targets, err := parseCIFixTargets(sctx.PreviousFindings)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := (&CIStep{}).autoFixCI(sctx, &mockReviewHost{}, &scm.PR{Number: "1"}, targets)
	if err != nil || !repair.Revalidate {
		t.Fatalf("empty successful result lost existing revalidation: %+v %v", repair, err)
	}
	if calls != 1 || gitCmd(t, dir, "rev-list", "--count", head+"..HEAD") != "1" {
		t.Fatal("empty summary lost useful repair or replayed it")
	}
	if got := gitCmd(t, dir, "log", "-1", "--format=%s"); !strings.Contains(got, "repair failing checks") {
		t.Fatalf("configured fallback missing: %q", got)
	}
	units, err := sctx.DB.GetFixCheckpoints(sctx.Run.ID, "ci", "")
	if err != nil || len(units) != 1 || units[0].State != "applied" || units[0].Summary != "" {
		t.Fatalf("no-summary receipt fabricated a summary: %+v %v", units, err)
	}
}
