package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type finalHeadStep struct {
	name  types.StepName
	calls atomic.Int32
	run   func(int, *pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (s *finalHeadStep) Name() types.StepName { return s.name }

func (s *finalHeadStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	call := int(s.calls.Add(1))
	if s.run == nil {
		return &pipeline.StepOutcome{}, nil
	}
	return s.run(call, sctx)
}

func (s *finalHeadStep) count() int { return int(s.calls.Load()) }

type finalHeadPush struct {
	calls atomic.Int32
}

func (s *finalHeadPush) Name() types.StepName { return types.StepPush }

func (s *finalHeadPush) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls.Add(1)
	return (&PushStep{}).Execute(sctx)
}

func (s *finalHeadPush) count() int { return int(s.calls.Load()) }

func TestExecutor_PostReviewMutationRestartsAtReviewBeforePush(t *testing.T) {
	for _, mutationStep := range []types.StepName{"", types.StepTest, types.StepDocument, types.StepLint} {
		name := "unchanged"
		if mutationStep != "" {
			name = string(mutationStep)
		}
		t.Run(name, func(t *testing.T) {
			upstream, dir, submitted, sctx, executorPaths := setupFinalHeadExecutor(t, config.Commands{})
			review := &finalHeadStep{name: types.StepReview, run: approveFinalHead}
			steps := []pipeline.Step{review}
			for _, stepName := range []types.StepName{types.StepTest, types.StepDocument, types.StepLint} {
				step := &finalHeadStep{name: stepName}
				if stepName == mutationStep {
					step.run = mutateFinalHeadOnce
				}
				steps = append(steps, step)
			}
			push := &finalHeadPush{}
			steps = append(steps, push)

			exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, steps, nil)
			if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil {
				t.Fatal(err)
			}
			wantReviews := 1
			if mutationStep != "" {
				wantReviews = 2
			}
			if review.count() != wantReviews {
				t.Fatalf("review calls = %d, want %d", review.count(), wantReviews)
			}
			if push.count() != 1 {
				t.Fatalf("push calls = %d, want exactly one remote-mutation attempt", push.count())
			}
			finalHead := gitCmd(t, dir, "rev-parse", "HEAD")
			if mutationStep == "" && finalHead != submitted {
				t.Fatalf("unchanged pipeline moved HEAD from %s to %s", submitted, finalHead)
			}
			assertFinalHeadBinding(t, sctx, upstream, finalHead)
		})
	}
}

func TestExecutor_PushPreparationMutationRestartsAtReviewBeforePush(t *testing.T) {
	commands := config.Commands{Format: "printf 'formatted final head\\n' > feature.txt"}
	upstream, dir, submitted, sctx, executorPaths := setupFinalHeadExecutor(t, commands)
	review := &finalHeadStep{name: types.StepReview, run: approveFinalHead}
	push := &finalHeadPush{}
	exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, []pipeline.Step{review, push}, nil)

	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if review.count() != 2 || push.count() != 2 {
		t.Fatalf("review/push calls = %d/%d, want 2/2", review.count(), push.count())
	}
	finalHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if finalHead == submitted {
		t.Fatal("formatter did not create a correction commit")
	}
	assertFinalHeadBinding(t, sctx, upstream, finalHead)
}

func TestExecutor_FailedOrInterruptedRereviewCannotAuthorizePush(t *testing.T) {
	for _, mode := range []string{"failed", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			upstream, dir, submitted, sctx, executorPaths := setupFinalHeadExecutor(t, config.Commands{})
			review := &finalHeadStep{name: types.StepReview, run: func(call int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				if call == 2 {
					if mode == "failed" {
						return nil, errors.New("rereview failed")
					}
					return &pipeline.StepOutcome{
						NeedsApproval: true,
						Findings:      `{"findings":[{"id":"final-head","severity":"error","description":"decision required","action":"ask-user"}]}`,
					}, nil
				}
				return approveFinalHead(call, sctx)
			}}
			mutation := &finalHeadStep{name: types.StepTest, run: mutateFinalHeadOnce}
			push := &finalHeadPush{}
			steps := []pipeline.Step{review, mutation, push}
			exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, steps, nil)

			if mode == "failed" {
				if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil {
					t.Fatal("expected rereview failure")
				}
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- exec.Execute(ctx, sctx.Run, sctx.Repo, dir) }()
				waitForFinalHeadReview(t, sctx, types.StepStatusAwaitingApproval)
				parked, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := pipeline.ValidateRecoveredRun(sctx.DB, parked, steps); err != nil {
					t.Fatalf("parked rereview is not recoverable: %v", err)
				}
				assertFinalHeadRereviewTrigger(t, sctx)
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("interrupted rereview did not stop")
				}
			}

			if push.count() != 0 {
				t.Fatalf("push ran %d times before rereview completed", push.count())
			}
			if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != submitted {
				t.Fatalf("remote changed from submitted %s to %s", submitted, remote)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != submitted {
				t.Fatalf("failed/interrupted rereview changed approval: %#v", run.ReviewApprovedHeadSHA)
			}
		})
	}
}

func TestExecutor_FinalHeadRereviewLoopIsBounded(t *testing.T) {
	upstream, dir, submitted, sctx, executorPaths := setupFinalHeadExecutor(t, config.Commands{})
	review := &finalHeadStep{name: types.StepReview, run: approveFinalHead}
	mutation := &finalHeadStep{name: types.StepTest, run: mutateFinalHeadEveryTime}
	push := &finalHeadPush{}
	exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, []pipeline.Step{review, mutation, push}, nil)

	err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
	if err == nil || !strings.Contains(err.Error(), "final_head_rereview_limit_exceeded") {
		t.Fatalf("error = %v, want stable rereview limit code", err)
	}
	if review.count() > 5 || mutation.count() > 5 {
		t.Fatalf("rereview did not converge within a small bound: review=%d mutation=%d", review.count(), mutation.count())
	}
	if push.count() != 0 {
		t.Fatalf("push ran %d times in a non-converging review loop", push.count())
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != submitted {
		t.Fatalf("remote changed from submitted %s to %s", submitted, remote)
	}
}

func setupFinalHeadExecutor(t *testing.T, commands config.Commands) (string, string, string, *pipeline.StepContext, *paths.Paths) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, submitted := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, commands)
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	executorPaths := paths.WithRoot(t.TempDir())
	if err := executorPaths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return upstream, dir, submitted, sctx, executorPaths
}

func approveFinalHead(_ int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{ReviewApprovedHeadSHA: head}, nil
}

func mutateFinalHeadOnce(call int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if call > 1 {
		return &pipeline.StepOutcome{}, nil
	}
	return mutateFinalHead(call, sctx)
}

func mutateFinalHeadEveryTime(call int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return mutateFinalHead(call, sctx)
}

func mutateFinalHead(call int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	filename := fmt.Sprintf("mutation-%02d.txt", call)
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, filename), []byte(filename+"\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "commit", "-m", "pipeline mutation "+filename); err != nil {
		return nil, err
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	sctx.Run.HeadSHA = head
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

func assertFinalHeadBinding(t *testing.T, sctx *pipeline.StepContext, upstream, finalHead string) {
	t.Helper()
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != finalHead {
		t.Fatalf("remote head = %s, want %s", remote, finalHead)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != finalHead {
		t.Fatalf("reviewed SHA = %#v, want %s", run.ReviewApprovedHeadSHA, finalHead)
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != finalHead || run.HeadSHA != finalHead {
		t.Fatalf("local/pushed SHA binding = head:%s pushed:%#v, want %s", run.HeadSHA, run.LastPushedSHA, finalHead)
	}
}

func waitForFinalHeadReview(t *testing.T, sctx *pipeline.StepContext, want types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
		if err == nil {
			for _, step := range steps {
				if step.StepName == types.StepReview && step.Status == want {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("review did not reach %s", want)
}

func assertFinalHeadRereviewTrigger(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != types.StepReview {
			continue
		}
		rounds, err := sctx.DB.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rounds) < 2 || rounds[len(rounds)-1].Trigger != "final_head_rereview" {
			t.Fatalf("review rounds do not durably identify rereview: %#v", rounds)
		}
		return
	}
	t.Fatal("review step not found")
}
