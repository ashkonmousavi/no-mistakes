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

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// trustedRepoConfigWithDocumentCorrection is the .no-mistakes.yaml a maintainer
// commits to the default branch to allow the bounded in-run documentation
// correction. auto_fix.document is the switch: 0 keeps the strictly
// report-only step.
const trustedRepoConfigWithDocumentCorrection = `allow_repo_commands: true
commands:
  lint: "true"
auto_fix:
  document: 1
document:
  correction_paths:
    - 'docs/**'
    - '*.md'
`

// documentCorrectionScenario drives the whole journey through the real fake
// agent. The document analyzer reports one editorial preference and one
// substantive omission; the bounded correction turn edits the named file; every
// later documentation analysis finds nothing left.
//
// The fake agent matches the FIRST action whose substring appears in the
// prompt, so order is the state machine here. The correction turn, review,
// test, and pull-request drafting are matched first by their own distinctive
// prompt lines. What is left is the document analysis, and the two passes are
// told apart by the round-history section: only a pass that runs after round
// one carries the earlier finding's own text, so matching that text answers
// "you already corrected this" and the initial pass falls through to the
// report.
const documentCorrectionScenario = `actions:
  - match: "Correct the accepted documentation findings below."
    text: "corrected the reference table"
    edits:
      - path: docs/reference.md
        new: "# Reference\n\n| flag | meaning |\n| --- | --- |\n| --carve | carve the widget |\n"
    structured:
      summary: "add the missing reference row"
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "no source risks"
      risk_scope: source-or-external
  - match: "You are validating a code change by driving the product itself."
    text: "tests passed"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "fakeagent: targeted test"
      testing_summary: "targeted validation passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
  - match: "Draft a pull request title and summary for the full branch delta."
    text: "drafted"
    structured:
      title: "feat: add the carve flag"
      body: "## What Changed\n\nAdd the --carve flag and its reference row."
  - match: "the reference table omits the --carve flag this change adds"
    text: "documentation now accurate"
    structured:
      findings: []
      summary: "documentation accurate"
  - match: "stale or incorrect statement"
    text: "documentation reviewed"
    structured:
      summary: "one omission and one preference"
      findings:
        - id: "document-1"
          severity: "error"
          file: "docs/reference.md"
          line: 3
          description: "the reference table omits the --carve flag this change adds"
          action: "auto-fix"
          class: "substantive"
        - id: "document-2"
          severity: "warning"
          file: "docs/reference.md"
          line: 1
          description: "prefer the heading 'CLI reference' over 'Reference'"
          action: "ask-user"
          class: "editorial"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`

// TestDocumentCorrectionJourney is the end-to-end proof of the documentation
// bottleneck fix, through the real daemon, gate transport, agent adapter, and
// persisted run state. It asserts all five statements the correction owes:
//
//  1. an editorial suggestion does not block the change - the run completes
//     without ever parking, even though the analyzer marked it ask-user;
//  2. a substantive documentation error is detected and corrected inside the
//     same run, with no abort and no second run;
//  3. the correction reaches the pushed branch and is reflected truthfully in
//     the pull-request body, which names the corrected file;
//  4. the corrected head receives the project's own validation - Test runs
//     again against it, durably marked documentation_head_recheck - and is the
//     exact head that was published;
//  5. the old documentation-edit/review loop does not return: Review runs
//     exactly once, and unrelated work is not repeated.
func TestDocumentCorrectionJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "document-correction.yaml")
	if err := os.WriteFile(scenario, []byte(documentCorrectionScenario), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	ctx := context.Background()

	// The gh fork-pr stub is how this suite reaches a real PR body: it is the
	// only stub that authenticates, so without it the PR step is skipped and
	// the run's own evidence never gets rendered.
	parentURL := "https://github.com/example/no-mistakes.git"
	forkURL := "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	pushMainRepoConfig(t, h, trustedRepoConfigWithDocumentCorrection)
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}
	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-document-correction.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "document-correction-journey"
	h.CommitChange(branch, "docs/reference.md", "# Reference\n\n| flag | meaning |\n| --- | --- |\n", "add the reference table")
	submitted := h.CommitChange(branch, "internal/widget/carve.go", "package widget\n\n// Carve implements --carve.\nfunc Carve() {}\n", "add the carve flag")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 180*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.AwaitingAgent {
		t.Fatal("the run is still parked: an editorial documentation finding must not gate")
	}
	detail := h.RunInfo(run.ID)

	// (1) and (5): the editorial finding never parked, and the correction never
	// sent the run back through Review.
	review := stepInfo(t, detail, types.StepReview)
	if review.RoundCount != 1 {
		t.Fatalf("review rounds = %d, want exactly 1: a documentation-only correction must not re-enter Review", review.RoundCount)
	}

	// (2) and (4): the corrected head was produced in this run and re-validated.
	if run.HeadSHA == submitted {
		t.Fatalf("head never advanced from the submitted commit %s: the correction was not applied in-run", submitted)
	}
	test := stepInfo(t, detail, types.StepTest)
	if test.RoundCount != 2 || test.RoundTrigger != "documentation_head_recheck" {
		t.Fatalf("test rounds = %d trigger = %q, want 2 rounds durably marked documentation_head_recheck", test.RoundCount, test.RoundTrigger)
	}
	if test.FindingsJSON == nil {
		t.Fatal("final Test recheck has no persisted findings")
	}
	testFindings, err := types.ParseFindingsJSON(*test.FindingsJSON)
	if err != nil {
		t.Fatalf("parse final Test findings: %v", err)
	}
	if testFindings.TestedHeadSHA != run.HeadSHA {
		t.Fatalf("final Test validated head %q, want corrected run head %q", testFindings.TestedHeadSHA, run.HeadSHA)
	}

	// (3): the correction reached the branch that was actually published, and
	// the published head is the one the pipeline validated.
	finalHead, err := h.runGit(ctx, forkDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read final fork head: %v\n%s", err, finalHead)
	}
	if got := strings.TrimSpace(string(finalHead)); got != run.HeadSHA {
		t.Fatalf("published head = %s, want the corrected head %s", got, run.HeadSHA)
	}
	corrected, err := h.runGit(ctx, forkDir, "diff", "--name-only", submitted+"..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read pipeline-authored diff: %v\n%s", err, corrected)
	}
	if got := strings.Fields(string(corrected)); len(got) != 1 || got[0] != "docs/reference.md" {
		t.Fatalf("the pipeline's own commits changed %q, want only docs/reference.md", got)
	}

	body := createdPRBody(t, readGHStubInvocations(t, ghLog))
	for _, want := range []string{
		"Documentation corrected in this run: `docs/reference.md`",
		"editorial note - recorded, not blocking",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("pull-request body does not state %q:\n%s", want, body)
		}
	}
	assertCurrentDocumentCorrectionAttestation(t, body, run.HeadSHA)
}

// TestDocumentCorrectionJourney_ConfiguredCommand proves that the repository's
// own deterministic command runs again on a Document-corrected head. The
// witness lives outside the candidate tree, so recording it cannot itself
// advance HEAD or make a stale derivative look current.
func TestDocumentCorrectionJourney_ConfiguredCommand(t *testing.T) {
	const initial = "# Reference\n\n| flag | meaning |\n| --- | --- |\n"
	for _, tc := range []struct {
		name       string
		fix        bool
		noHeadMove bool
	}{
		{name: "stale_derivative_blocks_push"},
		{name: "fixed_derivative_publishes_only_tested_head", fix: true},
		{name: "unchanged_head_runs_command_once", noHeadMove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenarioText := documentCorrectionScenario
			if tc.fix {
				fixAction := `  - match: "Fix the failing tests in this repository."
    text: "updated the derivative"
    edits:
      - path: docs/derivative.md
        new: "# Reference\n\n| flag | meaning |\n| --- | --- |\n| --carve | carve the widget |\n"
    structured:
      summary: "update the stale derivative"
`
				scenarioText = strings.Replace(scenarioText, "actions:\n", "actions:\n"+fixAction, 1)
			}
			if tc.noHeadMove {
				cleanAction := `  - match: "stale or incorrect statement"
    text: "documentation accurate"
    structured:
      findings: []
      summary: "documentation accurate"
`
				scenarioText = strings.Replace(scenarioText, "actions:\n", "actions:\n"+cleanAction, 1)
			}
			scenario := filepath.Join(t.TempDir(), "document-command.yaml")
			if err := os.WriteFile(scenario, []byte(scenarioText), 0o644); err != nil {
				t.Fatalf("write scenario: %v", err)
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			ctx := context.Background()
			witness := filepath.Join(t.TempDir(), "tested-heads")
			command := "git rev-parse HEAD >> " + shellQuote(witness) + " && cmp -s docs/reference.md docs/derivative.md"
			config := strings.Replace(trustedRepoConfigWithDocumentCorrection,
				"  lint: \"true\"\n", "  lint: \"true\"\n  test: "+fmt.Sprintf("%q", command)+"\n", 1)
			if tc.fix {
				config = strings.Replace(config, "  document: 1\n", "  document: 1\n  test: 1\n", 1)
			}
			pushMainRepoConfig(t, h, config)
			forkDir, ghLog := configureDocumentCorrectionFork(t, h)
			if out, err := h.Run("init", "--fork-url", "https://github.com/example-fork/no-mistakes.git"); err != nil {
				t.Fatalf("nm init: %v\n%s", err, out)
			}

			branch := "document-command-" + strings.ReplaceAll(tc.name, "_", "-")
			h.CommitChange(branch, "docs/reference.md", initial, "add the reference table")
			h.CommitChange(branch, "docs/derivative.md", initial, "add its derivative")
			submitted := h.CommitChange(branch, "internal/widget/carve.go", "package widget\n\n// Carve implements --carve.\nfunc Carve() {}\n", "add the carve flag")
			h.PushToGate(branch)

			if !tc.fix && !tc.noHeadMove {
				gated := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 180*time.Second)
				if gated == nil {
					t.Fatal("stale derivative did not gate Test")
				}
				assertDocumentCommandHeads(t, witness, submitted, gated.HeadSHA, false)
				if _, err := h.runGit(ctx, forkDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
					t.Fatal("stale derivative was published before Test passed")
				}
				if _, err := os.Stat(ghLog); err == nil {
					if len(readGHStubInvocations(t, ghLog)) != 0 {
						t.Fatal("stale derivative reached the forge before Test passed")
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("inspect forge stub log: %v", err)
				}
				return
			}

			run := h.WaitForRun(branch, 180*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
			}
			heads := assertDocumentCommandHeads(t, witness, submitted, run.HeadSHA, tc.noHeadMove)
			detail := h.RunInfo(run.ID)
			if got := stepInfo(t, detail, types.StepReview).RoundCount; got != 1 {
				t.Fatalf("Review rounds = %d, want 1 for documentation-only changes", got)
			}
			if tc.noHeadMove {
				if got := stepInfo(t, detail, types.StepTest).RoundCount; got != 1 {
					t.Fatalf("Test rounds = %d, want one with no head change", got)
				}
			} else if heads[1] == run.HeadSHA {
				t.Fatal("stale corrected head became the final published head")
			}
			test := stepInfo(t, detail, types.StepTest)
			if test.FindingsJSON == nil {
				t.Fatal("final Test has no persisted findings")
			}
			findings, err := types.ParseFindingsJSON(*test.FindingsJSON)
			if err != nil || findings.TestedHeadSHA != run.HeadSHA {
				t.Fatalf("final Test head = %q, want published head %s: %v", findings.TestedHeadSHA, run.HeadSHA, err)
			}
			finalHead, err := h.runGit(ctx, forkDir, "rev-parse", "refs/heads/"+branch)
			if err != nil || strings.TrimSpace(string(finalHead)) != run.HeadSHA {
				t.Fatalf("published head = %q, want final tested head %s: %v", strings.TrimSpace(string(finalHead)), run.HeadSHA, err)
			}
			if !tc.noHeadMove {
				body := createdPRBody(t, readGHStubInvocations(t, ghLog))
				assertCurrentDocumentCorrectionAttestation(t, body, run.HeadSHA)
			}
		})
	}
}

func assertDocumentCommandHeads(t *testing.T, path, submitted, final string, noHeadMove bool) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read configured-command witness: %v", err)
	}
	heads := strings.Fields(string(data))
	if len(heads) == 0 || heads[0] != submitted {
		t.Fatalf("first configured command ran on heads %q, want submitted %s first", heads, submitted)
	}
	if noHeadMove {
		if len(heads) != 1 || final != submitted {
			t.Fatalf("unchanged head executed command on %q; final=%s submitted=%s", heads, final, submitted)
		}
		return heads
	}
	if len(heads) < 2 || heads[1] == submitted {
		t.Fatalf("Document changed head but configured command saw %q, want a second distinct head", heads)
	}
	if len(heads) == 2 && heads[1] != final {
		t.Fatalf("second configured command ran on %s, want corrected head %s", heads[1], final)
	}
	if len(heads) > 2 && heads[len(heads)-1] != final {
		t.Fatalf("last configured command ran on %s, want final %s", heads[len(heads)-1], final)
	}
	return heads
}

func configureDocumentCorrectionFork(t *testing.T, h *Harness) (string, string) {
	t.Helper()
	ctx := context.Background()
	const parentURL = "https://github.com/example/no-mistakes.git"
	const forkURL = "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}
	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-document-command.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")
	return forkDir, ghLog
}

// assertCurrentDocumentCorrectionAttestation proves the external consumer view
// is bound to the same corrected head as the durable Test result. Do not infer
// this from the human-readable Testing section: the hidden attestation is what
// downstream policy reads.
func assertCurrentDocumentCorrectionAttestation(t *testing.T, body, headSHA string) {
	t.Helper()
	const (
		prefix = "<!-- no-mistakes-pipeline-attestation:v1 "
		suffix = " -->"
	)
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("PR body has no pipeline attestation:\n%s", body)
	}
	payloadStart := start + len(prefix)
	end := strings.Index(body[payloadStart:], suffix)
	if end < 0 {
		t.Fatalf("PR body has an unterminated pipeline attestation:\n%s", body)
	}
	var attestation struct {
		HeadSHA string `json:"head_sha"`
		Steps   []struct {
			Step   types.StepName   `json:"step"`
			Status types.StepStatus `json:"status"`
		} `json:"steps"`
		LiveValidation *struct {
			Verdict string `json:"verdict"`
			Live    int    `json:"live"`
			Total   int    `json:"total"`
		} `json:"live_validation"`
	}
	if err := json.Unmarshal([]byte(body[payloadStart:payloadStart+end]), &attestation); err != nil {
		t.Fatalf("parse pipeline attestation: %v", err)
	}
	if attestation.HeadSHA != headSHA {
		t.Fatalf("attestation head = %q, want corrected run head %q", attestation.HeadSHA, headSHA)
	}
	foundTest := false
	for _, step := range attestation.Steps {
		if step.Step == types.StepTest {
			foundTest = true
			if step.Status != types.StepStatusCompleted {
				t.Fatalf("attested Test status = %q, want completed", step.Status)
			}
		}
	}
	if !foundTest {
		t.Fatal("attestation has no Test step")
	}
	if got := attestation.LiveValidation; got == nil || got.Verdict != types.TestVerdictGo || got.Live != 1 || got.Total != 1 {
		t.Fatalf("attested live_validation = %+v, want verdict=go live=1 total=1", got)
	}
}

func stepInfo(t *testing.T, run *ipc.RunInfo, name types.StepName) ipc.StepResultInfo {
	t.Helper()
	for _, step := range run.Steps {
		if step.StepName == name {
			return step
		}
	}
	t.Fatalf("run has no %s step", name)
	return ipc.StepResultInfo{}
}
