package steps

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// maxDocumentCorrectionRoundsPerPass bounds how many rounds of one Document
// pass may edit the worktree. One: a correction that did not land in a single
// round is not converging, and the whole point of the bound is that a
// documentation correction can never become another multi-round edit/review
// chain. A pass ends when its correction's documentation-only head advance
// restarts the run at Test. The re-entered Document step starts a fresh pass,
// because it checks a head that has itself been re-validated, and a finding
// against that head is new work rather than a correction that failed to
// converge. pipeline.DocumentationHeadRecheckBudget is the run-wide cap: a
// correction whose head the run could no longer re-validate is refused. Rounds
// beyond either bound still re-check and still report; they do not edit, and
// their gate says so (refuseFurtherDocumentCorrection).
const maxDocumentCorrectionRoundsPerPass = 1

// documentCorrectionBudgetFindingID identifies the structured refusal a gate
// carries once no further correction can apply. It is a stable wire value, so
// an operator or an agent driving the gate can tell "a fix would not edit
// anything" apart from an ordinary documentation finding.
const documentCorrectionBudgetFindingID = "document-correction-budget-spent"

// documentCorrection records what one bounded correction round committed.
type documentCorrection struct {
	Applied bool
	Paths   []string
}

// documentCorrectionEnabled reports whether this repository allows the
// document step to correct documentation inside the run.
//
// auto_fix.document is the switch, and 0 keeps the step strictly report-only -
// today's behaviour, in which an accepted finding is reported and corrected
// outside the run. It is the same key that bounds every other step's automatic
// fix rounds, so a repository that has already decided "no automatic document
// edits" keeps that decision on this path too, and does not have to learn a
// second vocabulary to keep it.
func documentCorrectionEnabled(sctx *pipeline.StepContext) bool {
	return sctx.Config != nil && sctx.Config.AutoFix.Document > 0
}

// documentCorrectionBudget says whether a correction may run, and why not when
// it may not.
type documentCorrectionBudget struct {
	Allowed bool
	Reason  string
}

// readDocumentCorrectionBudget reads the correction budget from the durable
// round history rather than from memory, so a resumed run cannot spend it
// twice.
//
// Two bounds apply. Per pass: the executor writes the restart reason on the
// round that requested the restart, so a Document round triggered
// documentation_head_recheck closes a pass, and only corrections after the
// most recent such round count against the current one. Per run: a correction
// advances the head, and that head is only publishable after a restart at
// Test, so a correction is refused once the run has no documentation recheck
// left to spend on it.
func readDocumentCorrectionBudget(sctx *pipeline.StepContext) documentCorrectionBudget {
	if sctx.DB == nil || sctx.StepResultID == "" {
		return documentCorrectionBudget{Allowed: true}
	}
	rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
	if err != nil {
		// Fail closed: an unreadable history must not read as "budget unspent".
		return documentCorrectionBudget{Reason: fmt.Sprintf("the correction history could not be read (%v)", err)}
	}
	spentInPass := 0
	for _, round := range rounds {
		if round.FindingsJSON != nil {
			if parsed, err := types.ParseFindingsJSON(*round.FindingsJSON); err == nil && len(parsed.CorrectedPaths) > 0 {
				spentInPass++
			}
		}
		if round.Trigger == string(pipeline.RestartReasonDocumentationHeadRecheck) {
			// This round's head advance restarted the run at Test; its own
			// correction belongs to the pass it closed.
			spentInPass = 0
		}
	}
	if spentInPass >= maxDocumentCorrectionRoundsPerPass {
		return documentCorrectionBudget{Reason: passCorrectionSpentReason(spentInPass)}
	}
	started, limit, err := pipeline.DocumentationHeadRecheckBudget(sctx.DB, sctx.Run.ID)
	if err != nil {
		return documentCorrectionBudget{Reason: fmt.Sprintf("the run's documentation recheck count could not be read (%v)", err)}
	}
	if started >= limit {
		return documentCorrectionBudget{Reason: fmt.Sprintf("the run has already re-validated %d documentation-only head advances from Test (limit %d), so the head another correction produced could not be re-validated", started, limit)}
	}
	return documentCorrectionBudget{Allowed: true}
}

// nextDocumentCorrectionBudget is the budget the round after this one will
// see. The round being completed is not persisted yet, so a correction it just
// applied is invisible to the durable read and is counted here instead.
// Without that, the executor would start one more round that could only
// re-check and report.
func nextDocumentCorrectionBudget(sctx *pipeline.StepContext, correction documentCorrection) documentCorrectionBudget {
	if correction.Applied {
		return documentCorrectionBudget{Reason: passCorrectionSpentReason(maxDocumentCorrectionRoundsPerPass)}
	}
	return readDocumentCorrectionBudget(sctx)
}

// passCorrectionSpentReason states the per-pass bound in the refusal text.
func passCorrectionSpentReason(spent int) string {
	return fmt.Sprintf("this Document pass already committed its correction (%d/%d)", spent, maxDocumentCorrectionRoundsPerPass)
}

// refuseFurtherDocumentCorrection makes a gate whose correction budget is spent
// say so instead of offering a fix that would not apply. Every finding still
// labelled auto-fix is relabelled ask-user, because "the pipeline corrects it
// inside this run" is no longer true of it, and the structured refusal is
// appended so whoever answers the gate reads why. Without it, run
// 01M26NQXH7F4KJ7N0DZG1TZ6B4 answered fix four times against a spent budget,
// each round re-checked without editing, and the run had to be aborted.
func refuseFurtherDocumentCorrection(findings Findings, reason string) Findings {
	relabelled := 0
	kept := findings.Items[:0]
	for _, item := range findings.Items {
		// An analyzer that echoed an earlier refusal back must not leave a
		// second finding with the same stable ID beside the fresh one.
		if item.ID == documentCorrectionBudgetFindingID {
			continue
		}
		if item.ActionOrDefault() == types.ActionAutoFix {
			item.Action = types.ActionAskUser
			relabelled++
		}
		kept = append(kept, item)
	}
	findings.Items = append(kept, types.Finding{
		ID:       documentCorrectionBudgetFindingID,
		Severity: "warning",
		Description: fmt.Sprintf(
			"Document correction budget spent: %s. A fix response will not edit any file in this run, so %d documentation finding(s) labelled auto-fix are now ask-user. Approve to accept the remaining documentation findings as they stand, or abort and correct them outside the run.",
			reason, relabelled),
		Action: types.ActionAskUser,
		Class:  types.FindingClassSubstantive,
	})
	return findings
}

// correctableDocumentPaths returns the files the accepted findings name that
// are inside the documentation-and-records class, plus the named files that
// are outside it.
//
// Both halves matter. The first is the complete allowance handed to the fixer:
// it may edit those files and nothing else. The second is reported rather than
// silently dropped, because a documentation finding that points at code is
// exactly the case this bound exists to refuse - the finding still gates, and
// the operator can see why the correction did not cover it.
func correctableDocumentPaths(findings Findings, classPatterns []string) (allowed []string, outside []string) {
	seen := map[string]bool{}
	for _, item := range findings.Items {
		file := strings.TrimSpace(item.File)
		if file == "" || seen[file] {
			continue
		}
		seen[file] = true
		if pipeline.IsDocumentClassPath(file, classPatterns) {
			allowed = append(allowed, file)
			continue
		}
		outside = append(outside, file)
	}
	sort.Strings(allowed)
	sort.Strings(outside)
	return allowed, outside
}

// applyBoundedDocumentCorrection performs the one correction a Document pass
// is allowed: an agent turn restricted to the files the accepted
// findings name, followed by a single path-scoped commit the pipeline itself
// authors.
//
// Every refusal below fails the round visibly rather than quietly producing a
// clean-looking pass. That direction is deliberate: the failure this replaces
// was a compliant agent "fixing" documentation whose edits were then discarded
// unreported, and the only way a correction step earns the right to edit at
// all is that it can prove exactly what it changed.
func applyBoundedDocumentCorrection(sctx *pipeline.StepContext, classPatterns []string) (documentCorrection, error) {
	if !sctx.Fixing {
		return documentCorrection{}, nil
	}
	if !documentCorrectionEnabled(sctx) {
		sctx.Log("document correction is disabled for this repository (auto_fix.document: 0); this round re-checks the documentation without editing anything")
		return documentCorrection{}, nil
	}
	if budget := readDocumentCorrectionBudget(sctx); !budget.Allowed {
		sctx.Log(fmt.Sprintf("document correction budget spent: %s; this round re-checks the documentation without editing anything", budget.Reason))
		return documentCorrection{}, nil
	}
	accepted, err := types.ParseFindingsJSON(sctx.PreviousFindings)
	if err != nil {
		// A fix round with no readable selection has nothing to correct. The
		// analyzer still runs below and reports whatever is still wrong, so the
		// findings are never lost - only this round's edit is skipped.
		sctx.Log("document correction has no readable accepted findings; this round re-checks without editing anything")
		return documentCorrection{}, nil
	}
	allowed, outside := correctableDocumentPaths(accepted, classPatterns)
	if len(outside) > 0 {
		sctx.Log(fmt.Sprintf("document correction refuses %d accepted finding path(s) outside the documentation-and-records class (%s); those findings stay open for a decision", len(outside), strings.Join(outside, ", ")))
	}
	if len(allowed) == 0 {
		sctx.Log("no accepted documentation finding names a file inside the documentation-and-records class; this round re-checks without editing anything")
		return documentCorrection{}, nil
	}

	ctx := sctx.Ctx
	headBefore, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return documentCorrection{}, fmt.Errorf("resolve head before the document correction: %w", err)
	}
	entryEntries, err := documentWorktreeEntries(ctx, sctx.WorkDir)
	if err != nil {
		return documentCorrection{}, err
	}

	sctx.Log(fmt.Sprintf("applying the bounded documentation correction to %s...", strings.Join(allowed, ", ")))
	result, err := sctx.RunAgentContext(ctx, agentCorrectionOpts(sctx, allowed, classPatterns))
	if err != nil {
		return documentCorrection{}, fmt.Errorf("agent document correction: %w", err)
	}

	headAfter, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return documentCorrection{}, fmt.Errorf("resolve head after the document correction: %w", err)
	}
	if headAfter != headBefore {
		// The pipeline owns this commit: it is the thing that applies the
		// path-class refusal, the protected-paths check, and the repository's
		// own commit-message template. A fixer that commits for itself has
		// bypassed all three, and no later check can reconstruct what it would
		// have refused.
		return documentCorrection{}, fmt.Errorf("document correction must not commit: the fixer advanced HEAD from %s to %s itself; the pipeline commits the correction", pipeline.ShortObjectID(headBefore), pipeline.ShortObjectID(headAfter))
	}

	exitEntries, err := documentWorktreeEntries(ctx, sctx.WorkDir)
	if err != nil {
		return documentCorrection{}, err
	}
	touched := documentChangedPaths(entryEntries, exitEntries)
	if disallowed := disallowedCorrectionPaths(touched, allowed, classPatterns); len(disallowed) > 0 {
		return documentCorrection{}, fmt.Errorf(
			"document correction is bounded to the accepted documentation findings' files (%s) but the fixer changed:\n%s",
			strings.Join(allowed, ", "), strings.Join(disallowed, "\n"))
	}
	if len(touched) == 0 {
		sctx.Log("document correction made no change; the findings are re-checked and stay open")
		return documentCorrection{}, nil
	}

	summary, err := extractCommitSummary(result)
	if err != nil {
		if errors.Is(err, errRejectedCommitSummary) {
			return documentCorrection{}, fmt.Errorf("validate document correction summary: %w", err)
		}
		sctx.Log(fmt.Sprintf("warning: could not parse document correction summary: %v", err))
	}
	if err := commitDocumentCorrection(sctx, touched, summary); err != nil {
		return documentCorrection{}, err
	}

	// "Committed the right files" is not the whole contract: a fixer that also
	// left an unrelated edit behind would ship it through a later step's
	// catch-all commit, attributed to that step. Prove the tree is back to the
	// state this step inherited.
	postEntries, err := documentWorktreeEntries(ctx, sctx.WorkDir)
	if err != nil {
		return documentCorrection{}, err
	}
	if leftover := documentChangedPaths(entryEntries, postEntries); len(leftover) > 0 {
		return documentCorrection{}, fmt.Errorf(
			"document correction committed %s but left uncommitted changes behind:\n%s",
			strings.Join(touched, ", "), strings.Join(leftover, "\n"))
	}
	return documentCorrection{Applied: true, Paths: touched}, nil
}

// agentCorrectionOpts builds the bounded correction turn.
//
// The prompt states the allowance twice - as the explicit file list and as the
// path class - because the enforcement is not in the prompt. Every rule below
// is independently checked after the turn returns and fails the round when it
// was broken, so this text exists to make compliance easy, never to be the
// thing relied on.
func agentCorrectionOpts(sctx *pipeline.StepContext, allowed []string, classPatterns []string) agent.RunOpts {
	prompt := fmt.Sprintf(
		`Correct the accepted documentation findings below. This is a bounded correction: it lands inside the pipeline run that reported them.

Context:
- branch: %s
- target commit: %s
- files you may edit (the accepted findings' own files, and nothing else): %s
- documentation-and-records path class for this repository: %s

%s

%s

Task:
1. Read each accepted finding and the file it names.
2. Make the smallest correct edit that resolves it, applying the placement policy above: update the fact's one authoritative owner, reduce a stale duplicate to a pointer or remove it, and do not add a new documentation surface.
3. Leave every finding you cannot resolve inside the allowed files alone. It stays open for a decision; that is the correct outcome, not a failure.

Rules:
- Edit ONLY the files listed above. Touching any other path - source, configuration, tests, generated output - fails this round and the correction is refused.
- Do not create files, and do not run git commit, git add, git stash, or any other command that changes the repository's history or index. The pipeline commits this correction itself, with the repository's own commit-message template and its protected-path checks.
- Do not "fix" documentation by weakening what it requires. If the document is right and the implementation is wrong, leave the document alone and say so in your summary.
- Return JSON with a single "summary" field describing what you corrected.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		sctx.Run.Branch,
		sctx.Run.HeadSHA,
		strings.Join(allowed, ", "),
		strings.Join(classPatterns, ", "),
		documentPlacementPolicy,
		documentScopeDiscipline,
		executionContextPromptSection(sctx.WorkDir)+roundHistoryPromptSection(sctx)+userIntentPromptSection(sctx),
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Accepted findings to correct:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "document-fix",
	}
}

// disallowedCorrectionPaths returns the touched paths the correction was not
// allowed to change, each with the reason. A path must be BOTH named by an
// accepted finding and inside the documentation-and-records class: the finding
// list bounds the correction to what was actually accepted, and the class
// bounds it to what a documentation correction may touch at all.
func disallowedCorrectionPaths(touched, allowed []string, classPatterns []string) []string {
	permitted := make(map[string]bool, len(allowed))
	for _, file := range allowed {
		permitted[file] = true
	}
	var refused []string
	for _, file := range touched {
		switch {
		case !pipeline.IsDocumentClassPath(file, classPatterns):
			refused = append(refused, file+" (outside the documentation-and-records path class)")
		case !permitted[file]:
			refused = append(refused, file+" (not named by any accepted finding)")
		}
	}
	return refused
}

// commitDocumentCorrection records the correction as one pipeline-authored
// commit containing exactly the corrected paths.
//
// It deliberately does NOT reuse commitAgentFixes: that helper stages the
// whole worktree (`git add -A`), which is right for a step that owns every
// uncommitted change but wrong here. An earlier step's uncommitted work
// routinely reaches Document - the Test step's evidence agent writes focused
// tests and returns without committing them - and sweeping those code files
// into this commit would both misattribute them and destroy the one property
// the whole documentation path depends on: that this commit's diff is
// documentation and records only.
func commitDocumentCorrection(sctx *pipeline.StepContext, paths []string, summary string) error {
	ctx := sctx.Ctx
	if err := assertPipelineHeadContinuity(sctx, types.StepDocument); err != nil {
		return err
	}
	if err := assertNoProtectedPaths(sctx, paths); err != nil {
		return err
	}
	if summary == "" {
		summary = "correct documentation findings"
	}
	commitMessage, err := sctx.Config.Commit.RenderFixMessage(types.StepDocument, summary)
	if err != nil {
		return fmt.Errorf("render document correction commit message: %w", err)
	}
	if _, err := git.Run(ctx, sctx.WorkDir, append([]string{"add", "--"}, paths...)...); err != nil {
		return fmt.Errorf("stage document correction: %w", err)
	}
	if err := commitPipelineCorrection(ctx, sctx.WorkDir, commitMessage, sctx.Log); err != nil {
		return fmt.Errorf("commit document correction: %w", err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after the document correction commit: %w", err)
	}
	if err := assertPipelineHeadContinuity(sctx, types.StepDocument); err != nil {
		return err
	}
	ref := normalizedBranchRef(sctx.Run.Branch)
	if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, headSHA); err != nil {
		return fmt.Errorf("update local branch ref after the document correction: %w", err)
	}
	sctx.Run.HeadSHA = headSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
		return err
	}
	sctx.Log(fmt.Sprintf("committed documentation correction: %s", commitMessage))
	return nil
}

// documentWorktreeEntry is one dirty path plus the fingerprint line that
// identifies its exact content, so an edit to an ALREADY-dirty file is visible
// (its porcelain status line alone would not move).
type documentWorktreeEntry struct {
	Code string
	Path string
	Line string
}

// documentWorktreeEntries reads the worktree's dirty state precisely enough to
// attribute a change to one agent turn.
//
// --untracked-files=all matters: git's default collapses a wholly untracked
// directory to one "?? dir/" line, and a file added inside it would leave that
// line - and so the fingerprint - byte-identical. -z is used to read the
// records because git's default core.quotepath escapes any path containing
// non-ASCII bytes, backslashes, or double quotes into a C-style quoted string,
// which would then fail to open at that literal path and silently fall back to
// the (unmoving) status line alone. Deletions, renames, and unreadable paths
// fall back to the status line, which already records them.
func documentWorktreeEntries(ctx context.Context, workDir string) ([]documentWorktreeEntry, error) {
	rawZ, err := git.RunRaw(ctx, workDir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("check worktree status for the read-only document pass: %w", err)
	}
	records := strings.Split(strings.TrimSuffix(string(rawZ), "\x00"), "\x00")
	if len(records) == 1 && records[0] == "" {
		return nil, nil
	}
	var entries []documentWorktreeEntry
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 {
			continue
		}
		code := record[:2]
		path := record[3:]
		line := code + " " + path
		// A rename/copy (R/C) emits the destination record followed by a second
		// NUL-terminated record holding the source path, rather than the
		// "old -> new" arrow the non-`-z` format uses.
		if strings.ContainsAny(code, "RC") && i+1 < len(records) {
			i++
			line += " <- " + records[i]
		}
		if strings.Contains(code, "D") {
			entries = append(entries, documentWorktreeEntry{Code: code, Path: path, Line: line})
			continue
		}
		hash, herr := git.Run(ctx, workDir, "hash-object", "--", path)
		if herr != nil {
			entries = append(entries, documentWorktreeEntry{Code: code, Path: path, Line: line})
			continue
		}
		entries = append(entries, documentWorktreeEntry{Code: code, Path: path, Line: line + " " + strings.TrimSpace(hash)})
	}
	return entries, nil
}

// documentChangedPaths returns the paths whose dirty state differs between two
// snapshots - added, altered, or reverted - de-duplicated and sorted.
func documentChangedPaths(before, after []documentWorktreeEntry) []string {
	beforeLines := map[string]string{}
	for _, entry := range before {
		beforeLines[entry.Path] = entry.Line
	}
	afterLines := map[string]string{}
	for _, entry := range after {
		afterLines[entry.Path] = entry.Line
	}
	changed := map[string]bool{}
	for path, line := range afterLines {
		if beforeLines[path] != line {
			changed[path] = true
		}
	}
	for path, line := range beforeLines {
		if afterLines[path] != line {
			changed[path] = true
		}
	}
	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
