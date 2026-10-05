package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type restoredProbe struct{ parent string }
type restoredProbeStep struct {
	input    chan restoredProbe
	observed chan error
}

func (*restoredProbeStep) Name() types.StepName { return types.StepReview }
func (s *restoredProbeStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	expected := <-s.input
	var problem error
	selected, err := types.ParseFindingsJSON(ctx.PreviousFindings)
	if err != nil || !ctx.Fixing || len(selected.Items) != 1 || selected.Items[0].ID != "C" {
		problem = errors.New("rerun did not select only the unfinished cause")
	}
	if ctx.Run.HeadSHA != expected.parent {
		problem = errors.New("rerun fell back to submitted head")
	}
	working, err := os.ReadFile(filepath.Join(ctx.WorkDir, "C.txt"))
	if err != nil || string(working) != "unfinished C" {
		problem = errors.New("rerun lost working bytes before its first step")
	}
	staged, err := git.RunRaw(ctx.Ctx, ctx.WorkDir, "show", ":C.txt")
	if err != nil || string(staged) != "staged C" {
		problem = errors.New("rerun lost the separate index")
	}
	binary, err := os.ReadFile(filepath.Join(ctx.WorkDir, "C.bin"))
	if err != nil || string(binary) != string([]byte{0, 255, 'C'}) {
		problem = errors.New("rerun lost nonignored binary")
	}
	s.observed <- problem
	return nil, errors.New("probe intentionally stops before any approval")
}

func savedCauseFixture(t *testing.T, p *paths.Paths, d *db.DB) (*db.Repo, *db.Run, *types.PartialWork) {
	t.Helper()
	repo, head := setupTestGitRepo(t, p, d, "saved-cause")
	run, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateRunIntent(run.ID, db.RunIntent{Summary: "finish these causes", Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(p.WorktreesDir(), repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", wt, head)
	gitCmd(t, wt, "config", "user.name", "test")
	gitCmd(t, wt, "config", "user.email", "test@example.invalid")
	sr, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	scope := `{"findings":[{"id":"A","severity":"error","description":"cause A"},{"id":"B","severity":"error","description":"cause B"},{"id":"C","severity":"error","description":"cause C"}]}`
	sctx := &pipeline.StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, WorkDir: wt, StepResultID: sr.ID, PreviousFindings: scope}
	for i, id := range []string{"A", "B", "C"} {
		if err = sctx.BeginFixUnit(types.StepReview, types.Finding{ID: id, Severity: "error", Description: "cause " + id}, i+1, 3); err != nil {
			t.Fatal(err)
		}
		if id == "C" {
			break
		}
		if err = os.WriteFile(filepath.Join(wt, id+".txt"), []byte("applied"), 0644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, wt, "add", "-A")
		gitCmd(t, wt, "commit", "-m", "repair "+id)
		if err = sctx.RecordFixUnitHead(gitOutput(t, wt, "rev-parse", "HEAD")); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(wt, "C.txt"), []byte("staged C"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, wt, "add", "C.txt")
	if err = os.WriteFile(filepath.Join(wt, "C.txt"), []byte("unfinished C"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(wt, "C.bin"), []byte{0, 255, 'C'}, 0644); err != nil {
		t.Fatal(err)
	}
	saved, err := d.BeginWorkRescue(run, "review", sctx.FixSelectionID, run.HeadSHA, wt)
	if err != nil {
		t.Fatal(err)
	}
	if err = pipeline.PreserveRunWork(context.Background(), d, run, wt, saved, "fixture stop", true); err != nil {
		t.Fatal(err)
	}
	if err = custody.PreserveRecoveryHead(context.Background(), wt, run.ID, run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "saved fixture cleanup")
	if _, err = os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("original cleanup not exercised: %v", err)
	}
	return repo, run, saved
}

func TestFixProgressResumeRerunRestoresBeforeItsFirstStep(t *testing.T) {
	step := &restoredProbeStep{input: make(chan restoredProbe, 1), observed: make(chan error, 1)}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
	repo, source, saved := savedCauseFixture(t, p, d)
	step.input <- restoredProbe{source.HeadSHA}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RerunResult
	if err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main"}, &result); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-step.observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restored step did not start")
	}
	final := waitForRunTerminalState(t, d, result.RunID)
	if final.Status != types.RunFailed || final.HeadSHA != saved.ParentHead || final.ReviewApprovedHeadSHA != nil {
		t.Fatalf("rescue conferred approval: %+v", final)
	}
	if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", saved.Ref); got != saved.SHA {
		t.Fatal("new run removed original rescue")
	}
}

func TestFixProgressFailureRerunRefusesChangedOwnershipBeforeLaunch(t *testing.T) {
	for _, fault := range []string{"intent", "returned custody", "moved ref", "symbolic ref"} {
		t.Run(fault, func(t *testing.T) {
			step := &mockPassStep{name: types.StepReview}
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
			repo, source, saved := savedCauseFixture(t, p, d)
			intent := ""
			switch fault {
			case "intent":
				intent = "a different task"
			case "returned custody":
				if err := d.SetRunCustodyReturned(source.ID); err != nil {
					t.Fatal(err)
				}
			case "moved ref":
				gitCmd(t, p.RepoDir(repo.ID), "update-ref", saved.Ref, source.HeadSHA)
			case "symbolic ref":
				gitCmd(t, p.RepoDir(repo.ID), "symbolic-ref", saved.Ref, "refs/heads/main")
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.RerunResult
			err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main", Intent: intent}, &result)
			if err == nil || !strings.Contains(err.Error(), saved.Ref) {
				t.Fatalf("ownership mismatch silently launched/fell back: %+v %v", result, err)
			}
			runs, e := d.GetRunsByRepo(repo.ID)
			if e != nil || len(runs) != 1 || step.execCnt.Load() != 0 {
				t.Fatalf("refusal mutated runs=%d calls=%d err=%v", len(runs), step.execCnt.Load(), e)
			}
		})
	}
}
