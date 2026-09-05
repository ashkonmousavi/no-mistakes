package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestDocumentStep_ReadOnly_ReportsFindingsWithoutTouchingWorktree proves the
// happy path: the agent only reports findings and never mutates the worktree,
// and the outcome carries those findings unchanged.
func TestDocumentStep_ReadOnly_ReportsFindingsWithoutTouchingWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","file":"README.md","line":3,"description":"stale install instructions","action":"ask-user"}],"summary":"README stale"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 agent call, got %d", callCount)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval for a reported documentation finding")
	}
	if outcome.AutoFixable {
		t.Error("expected no auto-fix loop for the read-only document step")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree, got %q", status)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Error("expected HeadSHA to stay unchanged since the read-only step never commits")
	}
}

// TestDocumentStep_ReadOnly_AgentMutationFailsTheStep proves the read-only
// contract is enforced, not merely requested: an agent that edits a file after
// being told this is a read-only review fails the step with a clear error
// instead of silently discarding the edit and passing on zero findings.
func TestDocumentStep_ReadOnly_AgentMutationFailsTheStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected the document step to fail when the agent mutates the worktree")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %v, want a read-only violation message", err)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected the mutation to be discarded after the failure, got %q", status)
	}
}

// TestDocumentStep_ReadOnly_UntrackedFileFailsTheStep proves the mutation check
// also catches a new untracked file, not only edits to tracked ones.
func TestDocumentStep_ReadOnly_UntrackedFileFailsTheStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "NOTES.md"), []byte("scratch notes\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no gaps"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected the document step to fail when the agent leaves an untracked file")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected the untracked file to be cleaned up after the failure, got %q", status)
	}
}

// TestDocumentStep_PreexistingDirtyWorktreeIsNotMisattributedToTheAgent proves
// the read-only verdict is a difference against the entry state, not "is the
// worktree clean". An earlier step's uncommitted work (the Test step's evidence
// agent is told to write focused tests and leaves them uncommitted) reaches
// Document routinely, so it must neither fail the step nor be attributed to
// this agent, and it must survive the pass untouched.
func TestDocumentStep_PreexistingDirtyWorktreeIsNotMisattributedToTheAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# already dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover_test.go"), []byte("package leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirtyBefore := gitStatusPorcelain(t, dir)
	if dirtyBefore == "" {
		t.Fatal("test setup failed to dirty the worktree")
	}

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no gaps"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("an earlier step's uncommitted work must not fail the read-only pass: %v", err)
	}
	if outcome.NeedsApproval {
		t.Error("a clean read-only pass must not gate on another step's leftover changes")
	}
	if got := gitStatusPorcelain(t, dir); got != dirtyBefore {
		t.Fatalf("pre-existing worktree changes must survive untouched, before=%q after=%q", dirtyBefore, got)
	}
	if got := readTestFile(t, filepath.Join(dir, "leftover_test.go")); got != "package leftover\n" {
		t.Fatalf("leftover_test.go = %q, want the earlier step's content preserved", got)
	}
}

// TestDocumentStep_MutationOnTopOfPreexistingDirtyWorktreeStillFails proves the
// entry-state comparison did not weaken the guarantee: the agent's own mutation
// is still a failed step, and because another step's work shares the worktree
// nothing is discarded to clean up after it.
func TestDocumentStep_MutationOnTopOfPreexistingDirtyWorktreeStillFails(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	if err := os.WriteFile(filepath.Join(dir, "leftover_test.go"), []byte("package leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# rewritten by the agent\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"fixed the docs"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected the agent's mutation to fail the read-only document step")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %v, want a read-only violation", err)
	}
	if got := readTestFile(t, filepath.Join(dir, "leftover_test.go")); got != "package leftover\n" {
		t.Fatalf("leftover_test.go = %q, want the earlier step's work preserved, not discarded", got)
	}
	if got := readTestFile(t, filepath.Join(dir, "README.md")); got != "# rewritten by the agent\n" {
		t.Fatalf("README.md = %q, want the tree left untouched when another step's work shares it", got)
	}
}

// TestDocumentStep_EditToAPreexistingDirtyFileIsDetected proves the verdict
// compares content, not porcelain status lines: editing a file that was already
// untracked leaves its "??" line identical, so a status-only comparison would
// report a clean read-only pass.
func TestDocumentStep_EditToAPreexistingDirtyFileIsDetected(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	leftover := filepath.Join(dir, "leftover_test.go")
	if err := os.WriteFile(leftover, []byte("package leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := gitStatusPorcelain(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(leftover, []byte("package leftover // edited by the agent\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no gaps"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if got := gitStatusPorcelain(t, dir); got != statusBefore {
		t.Fatalf("setup invalidated the premise: status changed before the agent ran")
	}

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected an edit to an already-dirty file to fail the read-only document step")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %v, want a read-only violation", err)
	}
	if got := gitStatusPorcelain(t, dir); got != statusBefore {
		t.Fatalf("premise check: porcelain status must be unchanged by the agent's edit, before=%q after=%q", statusBefore, got)
	}
}

// TestDocumentStep_EditToAPreexistingDirtyQuotedPathFileIsDetected proves the
// verdict survives git's core.quotepath escaping: a pre-existing untracked
// file whose name needs quoting (non-ASCII bytes) still moves the fingerprint
// when the agent edits its content, even though its porcelain status line -
// quoted and escaped either way - never moves.
func TestDocumentStep_EditToAPreexistingDirtyQuotedPathFileIsDetected(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	leftover := filepath.Join(dir, "café.go")
	if err := os.WriteFile(leftover, []byte("package leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := gitStatusPorcelain(t, dir)
	if !strings.Contains(statusBefore, `"`) {
		t.Fatalf("setup invalidated the premise: expected core.quotepath to escape the non-ASCII path, got %q", statusBefore)
	}

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(leftover, []byte("package leftover // edited by the agent\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no gaps"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if got := gitStatusPorcelain(t, dir); got != statusBefore {
		t.Fatalf("setup invalidated the premise: status changed before the agent ran")
	}

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected an edit to an already-dirty quoted-path file to fail the read-only document step")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %v, want a read-only violation", err)
	}
	if got := gitStatusPorcelain(t, dir); got != statusBefore {
		t.Fatalf("premise check: porcelain status must be unchanged by the agent's edit, before=%q after=%q", statusBefore, got)
	}
}

func TestDocumentStep_AgentManaged_UnresolvedFindingsNeedApprovalWithoutAutoFixLoop(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"config docs conflict, needs human decision","action":"ask-user"}],"summary":"docs mostly updated"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval for unresolved documentation findings")
	}
	if outcome.AutoFixable {
		t.Error("expected unresolved documentation findings not to trigger an auto-fix round")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings.Items)
	}
}

// TestDocumentStep_PromptAppliesPlacementPolicy pins the placement-policy
// prompt contract from the 121-PR audit: each fact has one authoritative
// owner, stale duplicates are removed or reduced to pointers (not
// synchronized), AGENTS.md never receives incident narratives (invariant +
// regression-test pointer instead), no new surfaces for perceived gaps, and
// the scope stays on documentation this change made stale. The old
// exhaustive-corpus-synchronization incentives must be gone.
func TestDocumentStep_PromptAppliesPlacementPolicy(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		// One owner per fact; duplicates become pointers, never synced copies.
		"exactly one authoritative owner document",
		"remove the duplicate or reduce it to a short pointer to the owner",
		"never synchronize prose copies",
		// No new surfaces, no AGENTS.md postmortems; invariants + test pointers.
		"Do not create a new documentation surface merely to close a perceived gap",
		"Do not add incident narratives or postmortems to AGENTS.md",
		"point to the regression test or authoritative implementation",
		// Ownership map for the standard surfaces.
		"README.md owns the user-facing product introduction",
		"CONTRIBUTING.md owns contribution mechanics",
		"Code comments own non-obvious local intent",
		// Scope discipline: only what this change made stale.
		"Only touch documentation this change made stale",
		"Do not opportunistically rewrite, expand, or restructure unrelated documentation",
		"report one finding proposing the follow-up instead of multiplying edits",
		// Changed behavior must still land in its authoritative location.
		"Changed user-facing behavior must leave its authoritative user documentation accurate",
		// Genuinely read-only: report every defect, never edit.
		"This is a read-only review",
		"do not modify, create, or delete any file",
		"naming the file and line number",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected document prompt to contain %q\nprompt:\n%s", want, prompt)
		}
	}
	// The exhaustive-synchronization incentives from the pre-audit prompt
	// must be gone: they are what produced doc commits in 90 of 121 PRs.
	for _, forbidden := range []string{
		"Be exhaustive",
		"resolve every gap you can in this run",
		"Enumerate all docs",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("document prompt still carries corpus-sweep incentive %q", forbidden)
		}
	}
	// The agent must never be told to fix anything itself, nor told to withhold
	// a gap it already "fixed" - either one lets a compliant agent report zero
	// findings while the discarded edit leaves the real defect unreported.
	for _, forbidden := range []string{
		"fix each stale fact",
		"Fix in the authoritative location",
		"Do not report gaps you already fixed",
		"Update each altered fact in its owner document.",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("document prompt still instructs editing or withholding fixed gaps: %q", forbidden)
		}
	}
}

// TestDocumentStep_TrustedPolicyInstructionsAugmentPrompt proves a
// repository's own ownership map (config document.instructions, loaded only
// from the trusted default branch) reaches the prompt as an augmentation of
// the built-in defaults, and that no-policy repositories keep the built-in
// policy alone.
func TestDocumentStep_TrustedPolicyInstructionsAugmentPrompt(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Document.Instructions = "docs/architecture.md owns the daemon lifecycle facts."

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "docs/architecture.md owns the daemon lifecycle facts.") {
		t.Fatalf("expected trusted repo policy in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "augments the defaults above and cannot weaken them") {
		t.Fatal("expected the repo policy to be framed as augmenting, not replacing, the defaults")
	}
	// The built-in defaults remain active alongside the custom policy.
	if !strings.Contains(prompt, "exactly one authoritative owner document") {
		t.Fatal("expected built-in placement policy to remain with custom instructions present")
	}
}

func TestDocumentStep_UserFix_PassesPreviousFindingsIntoPrompt(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"address config docs"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"doc-1 =======","severity":"warning","file":"docs/config.md >>>>>>> prompt","description":"config section stale <<<<<<< HEAD"}],"summary":"config docs stale"}`

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval once the agent reports no remaining findings")
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "Previous findings to address") {
		t.Error("expected user-fix prompt to include previous findings section")
	}
	if !strings.Contains(prompt, "config section stale") {
		t.Error("expected user-fix prompt to carry the previous finding description")
	}
	if strings.Contains(prompt, "doc-1 =======") || strings.Contains(prompt, "<<<<<<< HEAD") {
		t.Error("expected user-fix prompt to sanitize finding fields and merge markers")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree, got %q", status)
	}
}

func TestDocumentStep_NoChanges_SkipsAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"noop"}`)}, nil
		},
	}
	// Point head at base so there are no changed files.
	sctx := newTestContext(t, ag, dir, baseSHA, baseSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 0 {
		t.Fatalf("expected no agent call when nothing changed, got %d", callCount)
	}
	if outcome.NeedsApproval || outcome.AutoFixable {
		t.Error("expected a clean no-op outcome when nothing changed")
	}
}

// TestDocumentStep_MalformedOutput_FailsClosedWithoutMutation keeps upstream's
// fail-closed analyzer validation intact under the read-only document step: an
// agent that mutates nothing but returns unparsable output still fails the step
// rather than certifying opaque output.
func TestDocumentStep_MalformedOutput_FailsClosedWithoutMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{
				Output: json.RawMessage(`{not valid json`),
				Text:   "I inspected the docs",
			}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "validate document analyzer findings") {
		t.Fatalf("Execute() error = %v, want malformed document analyzer output", err)
	}
	if outcome != nil {
		t.Fatalf("Execute() outcome = %+v, want no outcome", outcome)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree, got %q", status)
	}
}

// TestDocumentStep_MutationTakesPriorityOverMalformedOutput proves the
// read-only check runs before structured-output parsing: an agent that both
// mutates the worktree and returns unparsable output is reported as a
// read-only violation, so the mutation is never hidden behind the analyzer
// error.
func TestDocumentStep_MutationTakesPriorityOverMalformedOutput(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Partial\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{
				Output: json.RawMessage(`{not valid json`),
				Text:   "I updated the docs",
			}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected a mutation alongside malformed output to fail the step")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %v, want a read-only violation message", err)
	}
}

func TestDocumentStep_NoStructuredOutput_FailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "docs status unavailable"}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "document analyzer returned no structured findings") {
		t.Fatalf("Execute() error = %v, want missing document analyzer output", err)
	}
	if outcome != nil {
		t.Fatalf("Execute() outcome = %+v, want no outcome", outcome)
	}
}

func TestDocumentStep_HangingAgentFailsRunAfterTimeout(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "hanging-document-agent",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update docs"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.AgentTimeout = 20 * time.Millisecond

	exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&DocumentStep{}}, nil)
	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil {
		t.Fatal("expected hanging document agent to fail the run")
	}

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want %s", run.Status, types.RunFailed)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "agent timed out after 20ms") {
		var got string
		if run.Error != nil {
			got = *run.Error
		}
		t.Fatalf("run error = %q, want timeout diagnostic", got)
	}
}

func TestDocumentStep_SuccessfulReturnAfterTimeoutFailsWithoutCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{
		name: "late-document-agent",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# late\n"), 0o644); err != nil {
				return nil, err
			}
			<-ctx.Done()
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.AgentTimeout = 20 * time.Millisecond

	if _, err := (&DocumentStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("late successful return error = %v, want timeout", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want unchanged %s", got, headSHA)
	}
}

// TestDocumentStep_FileAddedInsideAPreexistingUntrackedDirectoryIsDetected
// covers the collapsed-directory case: git's default porcelain output reports a
// wholly untracked directory as a single line, so a file the agent adds inside
// it moves neither that line nor the directory's own hash.
func TestDocumentStep_FileAddedInsideAPreexistingUntrackedDirectoryIsDetected(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	pkg := filepath.Join(dir, "newpkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "a_test.go"), []byte("package newpkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := gitStatusPorcelain(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(pkg, "b_test.go"), []byte("package newpkg\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no gaps"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected a file added inside an already-untracked directory to fail the read-only document step")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %v, want a read-only violation", err)
	}
	if got := gitStatusPorcelain(t, dir); got != statusBefore {
		t.Fatalf("premise check: default porcelain status must be unchanged by the added file, before=%q after=%q", statusBefore, got)
	}
}

// readTestFile returns a worktree file's content, failing the test if it is
// gone - the discard path this file's tests guard against deletes files.
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
