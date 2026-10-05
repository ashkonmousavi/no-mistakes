package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestFixProgressRescueAgentReturnPreservesBeforeStepReturns(t *testing.T) {
	d, _, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	cause := errors.New("working cap reached")
	ag := &hangingAgent{name: "fixture", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		if err := os.WriteFile(filepath.Join(opts.CWD, "unfinished.bin"), []byte{0, 255, 'C'}, 0o644); err != nil {
			t.Fatal(err)
		}
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, cause
	}}
	sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir, Config: &config.Config{}}
	_, err = sctx.RunAgent(agent.RunOpts{CWD: dir, Purpose: "review-fix"})
	if !errors.Is(err, cause) {
		t.Fatalf("original stop lost: %v", err)
	}
	p, err := d.LatestWorkRescue(run.ID)
	if err != nil || p == nil || p.State != "saved" {
		t.Fatalf("partial work unavailable at return: %+v %v", p, err)
	}
	if err = custody.ValidatePartialWork(context.Background(), dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", p.Ref+":unfinished.bin")
	if err != nil || string(got) != string([]byte{0, 255, 'C'}) {
		t.Fatalf("partial bytes = %v %v", got, err)
	}
	if ag.calls != 1 {
		t.Fatalf("stop launched another repair: %d calls", ag.calls)
	}
}
