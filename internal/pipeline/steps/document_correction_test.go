package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// documentCorrectionAgent answers the two turns a document fix round makes:
// the bounded correction turn (Purpose "document-fix"), which may edit the
// worktree, and the read-only analysis turn that re-checks it.
type documentCorrectionAgent struct {
	*mockAgent
	purposes []string
}

func newDocumentCorrectionAgent(recheck string, correct func(agent.RunOpts) (*agent.Result, error)) *documentCorrectionAgent {
	a := &documentCorrectionAgent{mockAgent: &mockAgent{name: "test"}}
	a.mockAgent.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		a.purposes = append(a.purposes, opts.Purpose)
		if opts.Purpose == "document-fix" {
			return correct(opts)
		}
		return &agent.Result{Output: json.RawMessage(recheck)}, nil
	}
	return a
}

// newDocumentCorrectionContext builds a document-step context whose repository
// permits the bounded in-run correction, with the accepted findings already
// selected as a fix round would deliver them.
func newDocumentCorrectionContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA, accepted string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	sctx.Config.AutoFix.Document = 1
	sctx.Fixing = true
	sctx.PreviousFindings = accepted
	return sctx
}

const cleanDocumentRecheck = `{"findings":[],"summary":"documentation accurate"}`

func acceptedFinding(file, description string) string {
	return `{"findings":[{"id":"document-1","severity":"error","file":"` + file + `","description":"` + description + `","action":"auto-fix","class":"substantive"}],"summary":"one substantive gap"}`
}

func writeWorktreeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitWorktreeFile adds a file to the branch under test so a later
// correction edits tracked content rather than creating a new document.
func commitWorktreeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	writeWorktreeFile(t, dir, name, content)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add "+name)
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

// TestDocumentStep_EditorialFindingIsRecordedWithoutGating proves the failure
// the captain named: an editorial suggestion must not block an eligible
// change. The analyzer's own ask-user action is overridden by the class, the
// finding is still published, and the step needs no approval.
func TestDocumentStep_EditorialFindingIsRecordedWithoutGating(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"document-1","severity":"warning","file":"README.md","line":2,"description":"prefer 'install' over 'set up' in the heading","action":"ask-user","class":"editorial"}],"summary":"one wording preference"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("an editorial documentation finding must not park the run for approval")
	}
	if outcome.AutoFixable {
		t.Fatal("an editorial documentation finding must not start a correction round")
	}
	findings := parseStepFindings(t, outcome.Findings)
	if len(findings.Items) != 1 {
		t.Fatalf("editorial finding must still be published, got %d items", len(findings.Items))
	}
	if got := findings.Items[0].Action; got != types.ActionNoOp {
		t.Fatalf("editorial finding action = %q, want %q so no downstream gate parks on it", got, types.ActionNoOp)
	}
	if got := findings.Items[0].Class; got != types.FindingClassEditorial {
		t.Fatalf("editorial class = %q, want it preserved for the pull-request note", got)
	}
}

// TestDocumentStep_SubstantiveAndBehaviouralFindingsStillGate proves the other
// half of the classification: correcting the documentation is still required
// when the documentation actually misinforms, and an unclassified finding
// replayed from an older run keeps gating rather than being demoted.
func TestDocumentStep_SubstantiveAndBehaviouralFindingsStillGate(t *testing.T) {
	t.Parallel()
	for _, class := range []string{types.FindingClassSubstantive, types.FindingClassBehavioural} {
		t.Run(class, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"document-1","severity":"error","file":"contracts/rows.md","line":9,"description":"the contract table omits the row this change adds","action":"ask-user","class":"` + class + `"}],"summary":"one omission"}`)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
			outcome, err := (&DocumentStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval {
				t.Fatalf("a %s documentation finding must still gate", class)
			}
			findings := parseStepFindings(t, outcome.Findings)
			if findings.Items[0].Action != types.ActionAskUser {
				t.Fatalf("gating finding action = %q, want it left as the analyzer reported", findings.Items[0].Action)
			}
		})
	}
}

// TestDocumentStep_MissingClassFailsTheAnalyzerOutput proves the class is a
// validated contract, not advice: an analyzer that omits it cannot un-gate a
// substantive defect by leaving a field out.
func TestDocumentStep_MissingClassFailsTheAnalyzerOutput(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","file":"README.md","description":"stale","action":"ask-user"}],"summary":"one gap"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	_, err := (&DocumentStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "class") {
		t.Fatalf("Execute() error = %v, want the missing class to fail the step", err)
	}
}

// TestDocumentStep_BoundedCorrectionCommitsOnlyTheAcceptedDocumentationFile
// proves the present bottleneck is gone: an accepted substantive documentation
// finding is corrected inside the same run, committed by the pipeline itself,
// and reported truthfully as an applied change naming the corrected file.
func TestDocumentStep_BoundedCorrectionCommitsOnlyTheAcceptedDocumentationFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n")

	ag := newDocumentCorrectionAgent(cleanDocumentRecheck, func(opts agent.RunOpts) (*agent.Result, error) {
		writeWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n| new-row | pipeline |\n")
		return &agent.Result{Output: json.RawMessage(`{"summary":"add the missing contract row"}`)}, nil
	})
	sctx := newDocumentCorrectionContext(t, ag, dir, baseSHA, headSHA, acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds"))

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.FixSummary != changesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q", outcome.FixSummary, changesAppliedSummary)
	}
	correctedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if correctedHead == headSHA {
		t.Fatal("the accepted correction did not reach the branch: HEAD never advanced")
	}
	if sctx.Run.HeadSHA != correctedHead {
		t.Fatalf("recorded head = %s, want the correction commit %s", sctx.Run.HeadSHA, correctedHead)
	}
	if ref := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); ref != correctedHead {
		t.Fatalf("branch ref = %s, want the correction commit %s", ref, correctedHead)
	}
	if changed := gitCmd(t, dir, "diff", "--name-only", headSHA+".."+correctedHead); changed != "contracts/rows.md" {
		t.Fatalf("correction commit changed %q, want only contracts/rows.md", changed)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("correction left the worktree dirty: %q", status)
	}
	findings := parseStepFindings(t, outcome.Findings)
	if len(findings.CorrectedPaths) != 1 || findings.CorrectedPaths[0] != "contracts/rows.md" {
		t.Fatalf("corrected paths = %v, want the evidence to name contracts/rows.md", findings.CorrectedPaths)
	}
	if outcome.NeedsApproval {
		t.Fatal("the re-check found nothing, so the corrected head must not stay parked")
	}
	if len(ag.purposes) != 2 || ag.purposes[0] != "document-fix" || ag.purposes[1] != "document" {
		t.Fatalf("agent turns = %v, want one bounded correction followed by one read-only re-check", ag.purposes)
	}
}

// TestDocumentStep_BoundedCorrectionRefusesAPathOutsideItsAllowance proves the
// correction cannot become the old unbounded documentation editor. Editing
// code, or editing a documentation file no accepted finding named, fails the
// round visibly and commits nothing.
func TestDocumentStep_BoundedCorrectionRefusesAPathOutsideItsAllowance(t *testing.T) {
	t.Parallel()
	for name, stray := range map[string]string{
		"code file":                  "internal/handler.go",
		"unnamed documentation file": "docs/unrelated.md",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := setupGitRepo(t)
			headSHA := commitWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n")

			ag := newDocumentCorrectionAgent(cleanDocumentRecheck, func(opts agent.RunOpts) (*agent.Result, error) {
				writeWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n| new-row | pipeline |\n")
				writeWorktreeFile(t, dir, stray, "stray content\n")
				return &agent.Result{Output: json.RawMessage(`{"summary":"add the missing contract row"}`)}, nil
			})
			sctx := newDocumentCorrectionContext(t, ag, dir, baseSHA, headSHA, acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds"))

			_, err := (&DocumentStep{}).Execute(sctx)
			if err == nil {
				t.Fatal("expected the round to fail when the correction touched a path outside its allowance")
			}
			if !strings.Contains(err.Error(), stray) {
				t.Fatalf("error = %v, want it to name the refused path %s", err, stray)
			}
			if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
				t.Fatal("a refused correction must not commit anything")
			}
		})
	}
}

// TestDocumentStep_BoundedCorrectionRefusesAFixerThatCommitsForItself proves
// the pipeline keeps ownership of the correction commit, which is what applies
// the path-class refusal, the protected-path check, and the repository's own
// commit-message template.
func TestDocumentStep_BoundedCorrectionRefusesAFixerThatCommitsForItself(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n")

	ag := newDocumentCorrectionAgent(cleanDocumentRecheck, func(opts agent.RunOpts) (*agent.Result, error) {
		writeWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n| new-row | pipeline |\n")
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "fixer commits for itself")
		return &agent.Result{Output: json.RawMessage(`{"summary":"add the missing contract row"}`)}, nil
	})
	sctx := newDocumentCorrectionContext(t, ag, dir, baseSHA, headSHA, acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds"))

	_, err := (&DocumentStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "must not commit") {
		t.Fatalf("Execute() error = %v, want a refusal naming the fixer's own commit", err)
	}
}

// TestDocumentStep_ReportOnlyWhenCorrectionIsDisabled proves the previous
// safeguard is still reachable by configuration: with auto_fix.document at 0
// the step keeps today's behaviour and edits nothing, even in a fix round.
func TestDocumentStep_ReportOnlyWhenCorrectionIsDisabled(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n")

	ag := newDocumentCorrectionAgent(acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds"), func(opts agent.RunOpts) (*agent.Result, error) {
		t.Fatal("no correction turn may run while auto_fix.document is 0")
		return nil, nil
	})
	sctx := newDocumentCorrectionContext(t, ag, dir, baseSHA, headSHA, acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds"))
	sctx.Config.AutoFix.Document = 0

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.FixSummary != noChangesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q for a report-only repository", outcome.FixSummary, noChangesAppliedSummary)
	}
	if outcome.AutoFixable {
		t.Fatal("a report-only repository must never be offered a correction round")
	}
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
		t.Fatal("a report-only document step must not advance HEAD")
	}
}

// TestDocumentStep_CorrectionIsBoundedToOneRound proves the correction can
// never become another multi-round edit chain: once a round has committed a
// correction, later rounds re-check and report without editing.
func TestDocumentStep_CorrectionIsBoundedToOneRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n")

	stillOpen := acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds")
	ag := newDocumentCorrectionAgent(stillOpen, func(opts agent.RunOpts) (*agent.Result, error) {
		t.Fatal("the correction budget was already spent; no second correction turn may run")
		return nil, nil
	})
	sctx := newDocumentCorrectionContext(t, ag, dir, baseSHA, headSHA, stillOpen)
	sctx.StepResultID = recordSpentCorrectionRound(t, sctx)

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.FixSummary != noChangesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q once the budget is spent", outcome.FixSummary, noChangesAppliedSummary)
	}
	if outcome.AutoFixable {
		t.Fatal("a spent correction budget must not offer another automatic round")
	}
	if !outcome.NeedsApproval {
		t.Fatal("the still-open substantive finding must keep gating")
	}
}

// recordSpentCorrectionRound persists a completed document round that already
// committed a correction, the durable evidence the budget check reads.
func recordSpentCorrectionRound(t *testing.T, sctx *pipeline.StepContext) string {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	spent := `{"findings":[],"summary":"corrected","corrected_paths":["contracts/rows.md"]}`
	if _, err := sctx.DB.InsertStepRound(step.ID, 1, "auto_fix", &spent, nil, 0); err != nil {
		t.Fatal(err)
	}
	return step.ID
}

func parseStepFindings(t *testing.T, raw string) types.Findings {
	t.Helper()
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse step findings %q: %v", raw, err)
	}
	return findings
}

// TestDocumentStep_CorrectionRoundDoesNotOfferAnotherAutomaticRound proves the
// budget is counted against the round being completed, not only against the
// rounds already persisted. Without that, the executor would start one more
// auto-fix round whose correction is refused and whose only work is a repeated
// analyzer pass - the two --action fix rounds that applied no edits in run
// 01M23AB8T1WKJAG8FMJKG583HP.
func TestDocumentStep_CorrectionRoundDoesNotOfferAnotherAutomaticRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n")

	stillOpen := acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds")
	ag := newDocumentCorrectionAgent(stillOpen, func(opts agent.RunOpts) (*agent.Result, error) {
		writeWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n| new-row | pipeline |\n")
		return &agent.Result{Output: json.RawMessage(`{"summary":"add the missing contract row"}`)}, nil
	})
	sctx := newDocumentCorrectionContext(t, ag, dir, baseSHA, headSHA, stillOpen)

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.FixSummary != changesAppliedSummary {
		t.Fatalf("premise check: this round must have applied the correction, got fix summary %q", outcome.FixSummary)
	}
	if outcome.AutoFixable {
		t.Fatal("the round that spent the correction budget must not offer another automatic round")
	}
	if !outcome.NeedsApproval {
		t.Fatal("the still-open substantive finding must keep gating")
	}
}

// TestDocumentStep_IgnoredRemainingDiffStillReportsTheCorrection proves the
// correction is never silently dropped from the run's history: even when every
// remaining changed path is covered by ignore_patterns and the step has nothing
// left to analyze, the round still reports the files it committed.
func TestDocumentStep_IgnoredRemainingDiffStillReportsTheCorrection(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n")

	ag := newDocumentCorrectionAgent(cleanDocumentRecheck, func(opts agent.RunOpts) (*agent.Result, error) {
		writeWorktreeFile(t, dir, "contracts/rows.md", "| row | owner |\n| new-row | pipeline |\n")
		return &agent.Result{Output: json.RawMessage(`{"summary":"add the missing contract row"}`)}, nil
	})
	sctx := newDocumentCorrectionContext(t, ag, dir, baseSHA, headSHA, acceptedFinding("contracts/rows.md", "the contract table omits the row this change adds"))
	sctx.Config.IgnorePatterns = []string{"*"}

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.FixSummary != changesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q even when nothing remains to analyze", outcome.FixSummary, changesAppliedSummary)
	}
	findings := parseStepFindings(t, outcome.Findings)
	if len(findings.CorrectedPaths) != 1 || findings.CorrectedPaths[0] != "contracts/rows.md" {
		t.Fatalf("corrected paths = %v, want the committed file still named", findings.CorrectedPaths)
	}
}
