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

// TestExecutor_ContinuouslyMutatingFormatterHitsFinalHeadRereviewBound proves
// the production Push path cannot loop forever when a misconfigured formatter
// changes the reviewed head on every attempt. Nothing reaches the remote, and
// the stable refusal code distinguishes the bound from an ordinary test error.
func TestExecutor_ContinuouslyMutatingFormatterHitsFinalHeadRereviewBound(t *testing.T) {
	upstream, dir, submitted, sctx, executorPaths := setupFinalHeadExecutor(t, config.Commands{
		Format: "printf 'formatter pass\\n' >> feature.txt",
	})
	review := &finalHeadStep{name: types.StepReview, run: approveFinalHead}
	push := &finalHeadPush{}
	exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, []pipeline.Step{review, push}, nil)

	err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
	if err == nil || !strings.Contains(err.Error(), "final_head_rereview_limit_exceeded") {
		t.Fatalf("error = %v, want stable rereview limit code", err)
	}
	if review.count() > 5 || push.count() > 5 {
		t.Fatalf("mutating formatter exceeded the small rereview bound: review=%d push=%d", review.count(), push.count())
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != submitted {
		t.Fatalf("remote changed from submitted %s to %s", submitted, remote)
	}
}

// TestExecutor_DocumentationOnlyPostReviewCommitContinuesFromTestWithoutRereview
// proves the correction reaches the branch without re-entering Review: a
// post-review commit confined to the documentation-and-records class carries
// the review approval forward to the corrected head, restarts at Test so the
// project's own command and lint run against it, and lets Push publish exactly
// that head - the 8-round documentation-edit/review loop cannot return.
func TestExecutor_DocumentationOnlyPostReviewCommitContinuesFromTestWithoutRereview(t *testing.T) {
	upstream, dir, submitted, sctx, executorPaths := setupFinalHeadExecutor(t, config.Commands{})
	review := &finalHeadStep{name: types.StepReview, run: approveFinalHead}
	test := &finalHeadStep{name: types.StepTest}
	document := &finalHeadStep{name: types.StepDocument, run: mutateFinalHeadDocumentationOnce}
	lint := &finalHeadStep{name: types.StepLint}
	push := &finalHeadPush{}
	exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, []pipeline.Step{review, test, document, lint, push}, nil)

	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if review.count() != 1 {
		t.Fatalf("review calls = %d, want exactly one: a documentation-only correction must not re-enter Review", review.count())
	}
	// Test runs twice - once before the correction, once against it - and Lint
	// exactly once, because the restart happens before Lint had run at all.
	// That is the whole efficiency claim: the corrected head still receives the
	// project's own command and its lint gate, and nothing else is repeated.
	if test.count() != 2 || lint.count() != 1 {
		t.Fatalf("test/lint calls = %d/%d, want 2/1: the corrected head receives the project's own command and lint, with no avoidable repeat", test.count(), lint.count())
	}
	if push.count() != 1 {
		t.Fatalf("push calls = %d, want exactly one remote-mutation attempt", push.count())
	}
	finalHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if finalHead == submitted {
		t.Fatal("the documentation correction never reached the branch")
	}
	assertFinalHeadBinding(t, sctx, upstream, finalHead)
	assertRestartTrigger(t, sctx, types.StepTest, "documentation_head_recheck")
}

// TestExecutor_PostReviewCommitTouchingCodeAlongsideDocsStillRestartsReview
// proves the path class is all-or-nothing: one source file in the same
// post-review commit sends the whole advance back through Review, so the rule
// can never be used to smuggle unreviewed code past it behind a .md file.
func TestExecutor_PostReviewCommitTouchingCodeAlongsideDocsStillRestartsReview(t *testing.T) {
	upstream, dir, _, sctx, executorPaths := setupFinalHeadExecutor(t, config.Commands{})
	review := &finalHeadStep{name: types.StepReview, run: approveFinalHead}
	test := &finalHeadStep{name: types.StepTest}
	document := &finalHeadStep{name: types.StepDocument, run: mutateFinalHeadDocumentationAndCodeOnce}
	lint := &finalHeadStep{name: types.StepLint}
	push := &finalHeadPush{}
	exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, []pipeline.Step{review, test, document, lint, push}, nil)

	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if review.count() != 2 {
		t.Fatalf("review calls = %d, want exactly one restart for a code-touching post-review commit", review.count())
	}
	if push.count() != 1 {
		t.Fatalf("push calls = %d, want exactly one remote-mutation attempt", push.count())
	}
	assertFinalHeadBinding(t, sctx, upstream, gitCmd(t, dir, "rev-parse", "HEAD"))
	assertFinalHeadRereviewTrigger(t, sctx)
}

// TestExecutor_DocumentationHeadRecheckLoopIsBounded proves the cheaper
// documentation path cannot loop forever either: a step that rewrites
// documentation on every pass hits a stable refusal code and nothing reaches
// the remote.
func TestExecutor_DocumentationHeadRecheckLoopIsBounded(t *testing.T) {
	upstream, dir, submitted, sctx, executorPaths := setupFinalHeadExecutor(t, config.Commands{})
	review := &finalHeadStep{name: types.StepReview, run: approveFinalHead}
	test := &finalHeadStep{name: types.StepTest}
	document := &finalHeadStep{name: types.StepDocument, run: mutateFinalHeadDocumentationEveryTime}
	push := &finalHeadPush{}
	exec := pipeline.NewExecutor(sctx.DB, executorPaths, sctx.Config, sctx.Agent, []pipeline.Step{review, test, document, push}, nil)

	err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
	if err == nil || !strings.Contains(err.Error(), "documentation_head_recheck_limit_exceeded") {
		t.Fatalf("error = %v, want stable documentation recheck limit code", err)
	}
	if review.count() != 1 {
		t.Fatalf("review calls = %d, want the documentation path never to spend the Review budget", review.count())
	}
	if test.count() > 5 || document.count() > 5 {
		t.Fatalf("documentation recheck did not converge within a small bound: test=%d document=%d", test.count(), document.count())
	}
	if push.count() != 0 {
		t.Fatalf("push ran %d times in a non-converging documentation loop", push.count())
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != submitted {
		t.Fatalf("remote changed from submitted %s to %s", submitted, remote)
	}
}

func mutateFinalHeadDocumentationOnce(call int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if call > 1 {
		return &pipeline.StepOutcome{}, nil
	}
	return commitFinalHeadFiles(call, sctx, map[string]string{"docs/reference.md": "corrected reference\n"})
}

func mutateFinalHeadDocumentationEveryTime(call int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return commitFinalHeadFiles(call, sctx, map[string]string{fmt.Sprintf("docs/reference-%02d.md", call): "corrected reference\n"})
}

func mutateFinalHeadDocumentationAndCodeOnce(call int, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if call > 1 {
		return &pipeline.StepOutcome{}, nil
	}
	return commitFinalHeadFiles(call, sctx, map[string]string{
		"docs/reference.md": "corrected reference\n",
		"feature.txt":       "feature code rewritten by a pipeline step\n",
	})
}

// commitFinalHeadFiles writes and commits an exact file set as a pipeline step
// would, then records the new head the way every pipeline commit path does.
func commitFinalHeadFiles(_ int, sctx *pipeline.StepContext, files map[string]string) (*pipeline.StepOutcome, error) {
	for name, content := range files {
		full := filepath.Join(sctx.WorkDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "commit", "-m", "pipeline correction"); err != nil {
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

// assertRestartTrigger proves the restart is durably identified on the step
// that was restarted, which is what `axi status` reads to answer "why is this
// running again".
func assertRestartTrigger(t *testing.T, sctx *pipeline.StepContext, stepName types.StepName, want string) {
	t.Helper()
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != stepName {
			continue
		}
		rounds, err := sctx.DB.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rounds) < 2 || rounds[len(rounds)-1].Trigger != want {
			t.Fatalf("%s rounds do not durably identify %s: %#v", stepName, want, rounds)
		}
		return
	}
	t.Fatalf("%s step not found", stepName)
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
