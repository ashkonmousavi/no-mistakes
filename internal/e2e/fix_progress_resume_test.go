//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFixProgressResumeTwoNativeCutsThroughCLI(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "resume.yaml")
	initial := `actions:
  - match: "Current repair finding ID: A"
    edits: [{path: A.txt, new: "completed A"}]
    structured: {summary: "repair A"}
  - match: "Current repair finding ID: B"
    edits: [{path: B.txt, new: "completed B"}]
    structured: {summary: "repair B"}
  - match: "Current repair finding ID: C"
    edits:
      - {path: C.txt, new: "unfinished C"}
      - {path: C.bin, new: !!binary AP9D}
    stage: [C.txt]
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
      risk_rationale: fixture
      risk_scope: source-or-external
`
	if err := os.WriteFile(scenario, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "review_agent_timeout: 3s\nagent_timeout: 3s"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init %v\n%s", err, out)
	}
	branch := "feature/resumed-fixes"
	h.CommitChange(branch, "feature.txt", "seed", "feature")
	operator := h.AddWorktree(branch)
	// The existing dirty-caller rerun mode omits clean-head evidence. The caller
	// already had these bytes before submission and must keep them untouched.
	callerFile := filepath.Join(operator, "caller-owned.txt")
	if err := os.WriteFile(callerFile, []byte("pre-existing caller work"), 0644); err != nil {
		t.Fatal(err)
	}
	h.PushToGate(branch)
	first := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 45*time.Second)
	h.RespondWithFindings(first.ID, types.StepReview, types.ActionFix, []string{"A", "B", "C"})
	waitRun := func(id string, match func(*ipc.RunInfo) bool) *ipc.RunInfo {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			r := h.RunInfo(id)
			if match(r) {
				return r
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("run %s did not reach expected state: %+v", id, h.RunInfo(id))
		return nil
	}
	cut := waitRun(first.ID, func(r *ipc.RunInfo) bool { return r.Status.Terminal() })
	if cut.Status != types.RunFailed || cut.FixProgress == nil || cut.FixProgress.Applied != 2 || cut.PartialWork == nil || cut.PartialWork.State != "saved" {
		t.Fatalf("first cut %+v", cut)
	}
	gate := paths.WithRoot(h.NMHome).RepoDir(cut.RepoID)
	refs := []string{cut.PartialWork.Ref}
	for attempt := 0; attempt < 2; attempt++ {
		if out, err := h.Run("daemon", "stop"); err != nil {
			t.Fatalf("isolated daemon stop %v\n%s", err, out)
		}
		status, err := h.RunInDir(operator, "axi", "status", "--run", cut.ID)
		if err != nil || !strings.Contains(status, cut.PartialWork.Ref) {
			t.Fatalf("offline rescue status %v\n%s", err, status)
		}
		t.Log(status)
		if attempt == 1 {
			final := `actions:
  - match: "Current repair finding ID: C"
    edits: [{path: C.txt, old: "unfinished C", new: "complete C"}]
    structured: {summary: "complete cause C"}
  - match: "Review the code changes and return structured findings"
    structured:
      findings: []
      summary: "independent review complete"
      risk_level: low
      risk_rationale: "fixture has no remaining findings"
      risk_scope: source-or-external
  - match: "You are validating a code change"
    structured:
      findings: []
      summary: "no deployed fixture surface"
      tested: ["inspected fixture source"]
      testing_summary: "no running dummy product to drive"
      artifacts: []
      scenarios:
        - {name: "dummy source acceptance", result: untested, live: false, evidence: "", reason: "no running dummy product"}
      verdict: no-surface
  - structured: {summary: "focused verification complete", findings: []}
`
			if err = os.WriteFile(scenario, []byte(final), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := h.Run("daemon", "start"); err != nil {
			t.Fatalf("isolated daemon start %v\n%s", err, out)
		}
		out, err := h.RunInDir(operator, "rerun")
		if err != nil {
			t.Fatalf("rerun %v\n%s", err, out)
		}
		id := regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{26}`).FindString(out)
		if id == "" || id == cut.ID {
			t.Fatalf("rerun ID missing: %s", out)
		}
		if attempt == 0 {
			cut = waitRun(id, func(r *ipc.RunInfo) bool { return r.Status.Terminal() })
			if cut.Status != types.RunFailed || cut.HeadSHA != refsParent(t, gate, refs[0]) || cut.PartialWork == nil || cut.PartialWork.State != "saved" {
				t.Fatalf("second cut %+v", cut)
			}
			refs = append(refs, cut.PartialWork.Ref)
		} else {
			validation := waitRun(id, func(r *ipc.RunInfo) bool {
				step, ok := findStep(r.Steps, types.StepTest)
				return r.Status.Terminal() || ok && step.Status == types.StepStatusAwaitingApproval
			})
			if validation.Status.Terminal() {
				t.Fatalf("continued run ended before independent validation: %+v", validation)
			}
			h.Respond(id, types.StepTest, types.ActionApprove) // Honest no-surface acknowledgement.
			completed := waitRun(id, func(r *ipc.RunInfo) bool { return r.Status.Terminal() })
			if completed.Status != types.RunCompleted {
				t.Fatalf("continued run %+v", completed)
			}
			count, err := git.Run(context.Background(), gate, "rev-list", "--count", *first.SubmittedHeadSHA+".."+completed.HeadSHA)
			if err != nil || count != "3" {
				t.Fatalf("replayed/lost corrections: %q %v", count, err)
			}
			binary, err := git.RunRaw(context.Background(), gate, "show", completed.HeadSHA+":C.bin")
			if err != nil || string(binary) != string([]byte{0, 255, 'C'}) {
				t.Fatalf("restored binary lost: %v %v", binary, err)
			}
		}
	}
	calls := map[string]int{}
	for _, inv := range h.AgentInvocations() {
		for _, id := range []string{"A", "B", "C"} {
			if strings.Contains(inv.Prompt, "Current repair finding ID: "+id) {
				calls[id]++
			}
		}
	}
	if calls["A"] != 1 || calls["B"] != 1 || calls["C"] != 3 {
		t.Fatalf("native repair dispatches %+v", calls)
	}
	for _, ref := range refs {
		if _, err := git.Run(context.Background(), gate, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
			t.Fatal("consumption removed a prior rescue", err)
		}
	}
	if got, err := os.ReadFile(callerFile); err != nil || string(got) != "pre-existing caller work" {
		t.Fatal("rerun changed caller bytes")
	}
}

func refsParent(t *testing.T, gate, ref string) string {
	t.Helper()
	head, err := git.Run(context.Background(), gate, "rev-parse", ref+"^1")
	if err != nil {
		t.Fatal(err)
	}
	return head
}
