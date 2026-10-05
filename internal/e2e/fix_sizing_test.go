//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFixSizingThroughCLI(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversized=%t", oversized), func(t *testing.T) {
			description := "one small cause"
			if oversized {
				description = strings.Repeat("x", 8192)
			}
			quoted, _ := json.Marshal(description)
			scenario := filepath.Join(t.TempDir(), "sizing.yaml")
			initial := fmt.Sprintf(`actions:
  - match: "Review the code changes and return structured findings"
    structured:
      findings: [{id: A, severity: error, action: ask-user, file: feature.txt, description: %s}]
      summary: "one cause"
      risk_level: low
      risk_rationale: "fixture"
      risk_scope: source-or-external
`, quoted)
			if err := os.WriteFile(scenario, []byte(initial), 0644); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "review_agent_timeout: 3s\nagent_timeout: 3s"})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			const branch = "feature/fix-sizing"
			h.CommitChange(branch, "feature.txt", "seed", "feature")
			operator := h.AddWorktree(branch)
			h.PushToGate(branch)
			run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 45*time.Second)
			// The asking turn is finished. Subsequent calls receive a real edit
			// action plus an independent clean review and honest Test evidence.
			if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Current repair finding ID: A"
    edits: [{path: A.txt, new: "completed A"}]
    structured: {summary: "repair A"}
  - match: "Review the code changes and return structured findings"
    structured: {findings: [], summary: "independent review", risk_level: low, risk_rationale: "fixture", risk_scope: source-or-external}
  - match: ""
    structured:
      findings: []
      summary: "fixture evidence"
      tested: ["fixture only"]
      testing_summary: "fixture only"
      artifacts: []
      scenarios: [{name: "owner live walk", result: untested, live: false, evidence: "", reason: "fixture"}]
      verdict: no-surface
`), 0644); err != nil {
				t.Fatal(err)
			}
			h.RespondWithFindings(run.ID, types.StepReview, types.ActionFix, []string{"A"})
			if oversized {
				run = waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 45*time.Second)
			} else {
				run = waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 45*time.Second)
			}
			fixCalls := 0
			for _, call := range h.AgentInvocations() {
				if strings.Contains(call.Prompt, "Current repair finding ID: A") {
					fixCalls++
				}
			}
			wantCalls := 1
			if oversized {
				wantCalls = 0
			}
			if fixCalls != wantCalls {
				t.Fatalf("fix calls=%d want=%d", fixCalls, wantCalls)
			}
			gate := paths.WithRoot(h.NMHome).RepoDir(run.RepoID)
			count, err := git.Run(context.Background(), gate, "rev-list", "--count", *run.SubmittedHeadSHA+".."+run.HeadSHA)
			if err != nil || count != fmt.Sprint(wantCalls) {
				t.Fatalf("correction commits=%q error=%v", count, err)
			}
			if oversized {
				status, err := h.RunInDir(operator, "axi", "status")
				if err != nil {
					t.Fatalf("status: %v\n%s", err, status)
				}
				for _, want := range []string{"fix-estimate-exceeds-deadline", "unmeasured size estimate", "fixer was not launched", "safety margin"} {
					if !strings.Contains(status, want) {
						t.Errorf("status omitted %q: %s", want, status)
					}
				}
				// The active parked run is deliberate. This forced stop is scoped
				// by the harness to its inventoried temporary NM_HOME only.
				if out, err := h.Run("daemon", "stop", "--force"); err != nil {
					t.Fatalf("isolated daemon stop: %v\n%s", err, out)
				}
				offline, err := h.RunInDir(operator, "axi", "status", "--run", run.ID)
				if err != nil || !strings.Contains(offline, "fix-estimate-exceeds-deadline") || !strings.Contains(offline, "estimated at 6.75s") {
					t.Fatalf("disconnected status lost sizing refusal: %v\n%s", err, offline)
				}
				configPath := filepath.Join(h.NMHome, "config.yaml")
				configBytes, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				configBytes = []byte(strings.ReplaceAll(string(configBytes), "review_agent_timeout: 3s", "review_agent_timeout: 60m"))
				if err = os.WriteFile(configPath, configBytes, 0600); err != nil {
					t.Fatal(err)
				}
				if out, err := h.Run("daemon", "start"); err != nil {
					t.Fatalf("isolated daemon restart: %v\n%s", err, out)
				}
				h.RespondWithFindings(run.ID, types.StepReview, types.ActionFix, []string{"A"})
				resumed := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 45*time.Second)
				for _, step := range resumed.Steps {
					if step.StepName == types.StepReview && (step.Status != types.StepStatusCompleted || step.FindingsJSON != nil && strings.Contains(*step.FindingsJSON, "fix-estimate-exceeds-deadline")) {
						t.Fatalf("successful retry retained the scheduling warning: %+v", step)
					}
				}
				fixCalls = 0
				for _, call := range h.AgentInvocations() {
					if strings.Contains(call.Prompt, "Current repair finding ID: A") {
						fixCalls++
					}
				}
				if fixCalls != 1 {
					t.Fatalf("retry dispatched %d repairs, want one", fixCalls)
				}
				t.Log("oversized finding: no launch; AXI retained reason online and offline; explicit retry after isolated budget change and daemon restart completed one cause and independent review")
			}
		})
	}
}
