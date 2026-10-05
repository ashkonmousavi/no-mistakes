package steps

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixProgressCINoCodeCauseParksEarlierLocalCheckpoints(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls, running := 0, 0
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if strings.Contains(opts.Prompt, "Authorized repair finding: B.") {
			return &agent.Result{Output: []byte(`{"summary":"external condition requires an operator","code_change_needed":false}`)}, nil
		}
		if err := os.WriteFile(filepath.Join(dir, "A.txt"), []byte("applied A"), 0644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: []byte(`{"summary":"repair A","code_change_needed":true}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepCI)
	sctx.Config.CI.RevalidateRepairs = true
	sctx.MarkRunning = func() error { running++; return nil }
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"cause A","category":"ci-check","check":"A"},{"id":"B","description":"external B","category":"ci-check","check":"B"}]}`
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "A", Bucket: scm.CheckBucketFail}, {Name: "B", Bucket: scm.CheckBucketFail}}}
	outcome, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "1"})
	if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || running != 0 {
		t.Fatalf("no-code result masqueraded as a published repair: outcome=%+v err=%v running=%d", outcome, err, running)
	}
	p, err := sctx.DB.FixProgress(sctx.Run.ID)
	if err != nil || p.Applied != 1 || p.Total != 2 || calls != 2 {
		t.Fatalf("no-code cause applied/replayed: progress %+v calls=%d err=%v", p, calls, err)
	}
	if gitCmd(t, dir, "rev-list", "--count", head+"..HEAD") != "1" {
		t.Fatal("earlier CI work lost")
	}
}

func TestFixProgressCIEmptyBatchSummaryPreservesPendingValidation(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if opts.Purpose != "ci-fix-verification" {
			id := "A"
			if strings.Contains(opts.Prompt, "Authorized repair finding: B.") {
				id = "B"
			}
			if err := os.WriteFile(filepath.Join(dir, id+".txt"), []byte("repaired"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepCI)
	sctx.Config.CI.RevalidateRepairs = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"cause A","category":"ci-check","check":"A"},{"id":"B","description":"cause B","category":"ci-check","check":"B"}]}`
	targets, err := parseCIFixTargets(sctx.PreviousFindings)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&CIStep{}).autoFixCI(sctx, &mockReviewHost{}, &scm.PR{Number: "1"}, targets)
	if err != nil || !result.Revalidate || calls != 3 {
		t.Fatalf("empty successful batch lost existing fallback: %+v %v calls=%d", result, err, calls)
	}
	p, err := sctx.DB.FixProgress(sctx.Run.ID)
	if err != nil || p.Applied != 2 || p.Total != 2 || !p.ValidationPending {
		t.Fatalf("local turn completion became current-head validation: %+v %v", p, err)
	}
	units, err := sctx.DB.GetFixCheckpoints(sctx.Run.ID, "ci", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		if unit.Summary != "" {
			t.Fatal("empty result invented per-finding summary")
		}
	}
}
