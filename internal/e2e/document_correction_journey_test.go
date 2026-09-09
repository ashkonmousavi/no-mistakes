//go:build e2e

package e2e

import (
	"context"
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
