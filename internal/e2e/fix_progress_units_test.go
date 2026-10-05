//go:build e2e

package e2e

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFixProgressReviewUnitsThroughCLI(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "units.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Current repair finding ID: A"
    edits: [{path: A.txt, new: "completed A"}]
    structured: {summary: "repair A"}
  - match: "Current repair finding ID: B"
    edits: [{path: B.txt, new: "completed B"}]
    structured: {summary: "repair B"}
  - match: "Current repair finding ID: C"
    edits: [{path: C.bin, new: !!binary AHVuZmluaXNoZWQgQ/8=}]
    delay_after_edits_ms: 60000
    structured: {summary: "never reached"}
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - {id: A, severity: error, action: ask-user, file: feature.txt, description: "cause A"}
        - {id: B, severity: error, action: ask-user, file: feature.txt, description: "cause B"}
        - {id: C, severity: error, action: ask-user, file: feature.txt, description: "cause C"}
      summary: "three causes"
      risk_level: low
      risk_rationale: "fixture"
      risk_scope: source-or-external
`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "review_agent_timeout: 3s\nagent_timeout: 3s"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/unit-progress"
	h.CommitChange(branch, "feature.txt", "seed", "feature")
	operator := h.AddWorktree(branch)
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 45*time.Second)
	h.RespondWithFindings(run.ID, types.StepReview, types.ActionFix, []string{"A", "B", "C"})
	final := h.WaitForRun(branch, 45*time.Second)
	if final.Status != types.RunFailed || final.FixProgress == nil || final.FixProgress.Applied != 2 || final.FixProgress.Total != 3 || final.FixProgress.Current != "C" {
		t.Fatalf("saved progress = %+v", final)
	}
	if final.PartialWork == nil || final.PartialWork.State != "saved" {
		t.Fatalf("partial C missing: %+v", final.PartialWork)
	}
	gate := paths.WithRoot(h.NMHome).RepoDir(final.RepoID)
	count, err := git.Run(context.Background(), gate, "rev-list", "--count", *final.SubmittedHeadSHA+".."+final.HeadSHA)
	if err != nil || count != "2" {
		t.Fatalf("cause commits = %q %v", count, err)
	}
	status, err := h.RunInDir(operator, "axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	t.Log(status)
	for _, want := range []string{"fix_progress", "applied: 2", "total: 3", final.PartialWork.Ref, final.HeadSHA} {
		if !strings.Contains(status, want) {
			t.Fatalf("status omitted %q: %s", want, status)
		}
	}
	if calls := len(h.AgentInvocations()); calls != 4 {
		t.Fatalf("unexpected replay: %d calls", calls)
	}
}
