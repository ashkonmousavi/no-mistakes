package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestParkedShutdownPreservesInitialAndRecoveredGates(t *testing.T) {
	for _, mode := range []struct {
		name      string
		recovered bool
		status    types.StepStatus
	}{
		{"initial", false, types.StepStatusAwaitingApproval},
		{"recovered_approval", true, types.StepStatusAwaitingApproval},
		{"recovered_fix_review", true, types.StepStatusFixReview},
	} {
		for _, stop := range []struct {
			name   string
			cause  error
			status types.RunStatus
		}{
			{"shutdown", ErrDaemonShutdown, types.RunRunning},
			{"operator_abort", errors.New(types.RunCancelReasonAbortedByUser), types.RunCancelled},
			{"ordinary_cancel", context.Canceled, types.RunFailed},
		} {
			t.Run(mode.name+"/"+stop.name, func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				workDir := t.TempDir()
				initGitRepo(t, workDir)
				head, err := git.HeadSHA(context.Background(), workDir)
				if err != nil {
					t.Fatal(err)
				}
				if err = database.UpdateRunHeadSHA(run.ID, head); err != nil {
					t.Fatal(err)
				}
				run.HeadSHA = head
				findings := `{"findings":[{"id":"A","severity":"error","action":"ask-user","description":"original cause"},{"id":"fix-estimate-exceeds-deadline","severity":"warning","action":"ask-user","description":"estimated work does not fit"}]}`
				step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
					if mode.recovered {
						t.Fatal("recovery reran the finished asking turn")
					}
					return &StepOutcome{NeedsApproval: true, Findings: findings}, nil
				}}
				if mode.recovered {
					if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					sr, err := database.InsertStepResult(run.ID, types.StepTest)
					if err != nil {
						t.Fatal(err)
					}
					if err = database.StartStep(sr.ID); err != nil {
						t.Fatal(err)
					}
					if _, err = database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 10); err != nil {
						t.Fatal(err)
					}
					if err = database.ParkStepForApproval(run.ID, sr.ID, mode.status, 10, &findings); err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				parked := make(chan struct{}, 1)
				exec := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
					if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(mode.status) {
						select {
						case parked <- struct{}{}:
						default:
						}
					}
				})
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				done := make(chan error, 1)
				go func() {
					if mode.recovered {
						done <- exec.Resume(ctx, run, repo, workDir)
					} else {
						done <- exec.Execute(ctx, run, repo, workDir)
					}
				}()
				select {
				case <-parked:
				case <-time.After(5 * time.Second):
					t.Fatal("gate never parked")
				}
				before, err := database.GetStepsByRun(run.ID)
				if err != nil || len(before) != 1 || before[0].FindingsJSON == nil {
					t.Fatalf("parked gate=%+v error=%v", before, err)
				}
				parkedFindings := *before[0].FindingsJSON
				cancel(stop.cause)
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("stopped execution returned success")
					}
					if stop.cause == ErrDaemonShutdown && !errors.Is(err, ErrRunSuspended) {
						t.Errorf("shutdown was not a suspension: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("parked executor did not stop")
				}
				stored, err := database.GetRun(run.ID)
				if err != nil || stored.Status != stop.status {
					t.Fatalf("run=%+v error=%v, want %s", stored, err, stop.status)
				}
				if stop.cause == ErrDaemonShutdown {
					results, err := database.GetStepsByRun(run.ID)
					if err != nil || len(results) != 1 {
						t.Fatalf("steps=%+v error=%v", results, err)
					}
					if stored.Error != nil || stored.AwaitingAgentSince == nil || results[0].Status != mode.status || results[0].FindingsJSON == nil || *results[0].FindingsJSON != parkedFindings {
						t.Fatalf("shutdown erased parked truth: run=%+v step=%+v", stored, results[0])
					}
					if err = ValidateRecoveredRun(database, stored, []Step{step}); err != nil {
						t.Fatalf("preserved gate cannot recover: %v", err)
					}
				}
			})
		}
	}
}
