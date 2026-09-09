package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DocumentStep keeps documentation accurate for the change under its
// placement policy, and - when no deterministic lint command is configured -
// also performs the agent-driven lint duty in the same invocation so the
// pipeline pays one cold agent pass for housekeeping instead of two.
type DocumentStep struct{}

func (s *DocumentStep) Name() types.StepName { return types.StepDocument }

// documentPlacementPolicy is the fail-safe default placement policy. It
// replaces the old exhaustive-synchronization incentive: the agent is
// rewarded for updating each fact's single owner and for consolidation,
// deletion, and pointers - not for synchronizing every prose copy. A trusted
// repository-specific policy (config document.instructions) may narrow or
// clarify these rules but never weaken them.
const documentPlacementPolicy = `Documentation placement policy (fail-safe defaults; repository-specific instructions may narrow or clarify them, never weaken them):
- Every fact or contract has exactly one authoritative owner document. Update the owner; never synchronize prose copies of the same fact.
- When this change leaves an existing duplicate stale, remove the duplicate or reduce it to a short pointer to the owner instead of updating another full copy.
- Do not create a new documentation surface merely to close a perceived gap.
- Do not add incident narratives or postmortems to AGENTS.md. For a durable incident lesson, preserve the operative invariant in its owner document and point to the regression test or authoritative implementation.
- AGENTS.md is only for high-value project-intrinsic knowledge useful to almost every future session.
- README.md owns the user-facing product introduction and common usage.
- CONTRIBUTING.md owns contribution mechanics, not product or architecture inventories.
- Code comments own non-obvious local intent, safety invariants, and external constraints - never prose that merely restates code.
- Deep reference docs own detailed conditional material; link to them instead of copying them into always-loaded guidance.
- Generated or schema-backed facts must be generated from their authoritative source and checked for drift, never hand-copied.`

// documentScopeDiscipline bounds the pass to documentation this change made
// stale, replacing the old "be exhaustive across the corpus" instruction.
const documentScopeDiscipline = `Scope discipline:
- Only touch documentation this change made stale, plus direct contradictions that analysis reveals.
- Do not opportunistically rewrite, expand, or restructure unrelated documentation, and do not perform a broad documentation architecture migration here.
- When a larger consolidation is warranted but out of scope, leave this change safe and report one finding proposing the follow-up instead of multiplying edits.
- Preserve load-bearing user guidance, security rationale, compatibility constraints, and onboarding material. A long document is not a defect by itself; duplication and wrong placement are.
- Prefer consolidation, deletion, and pointers to the owner over addition and synchronization.`

// documentClassPolicy is the classification contract. It is the answer to the
// failure this step's gate actually produced: every finding arrived with
// action ask-user regardless of what correcting it protected, so a reviewer's
// wording preference blocked a release exactly as hard as a contract row the
// implementation contradicts.
//
// The classification is by EFFECT, never by file extension or severity label.
// A `.md` file that a validator reads, that a generator consumes, or that
// carries delivery authority is behavioural, and a beautifully formatted
// heading in contracts/ is still editorial.
const documentClassPolicy = `Classification (required on every documentation finding, in the "class" field):
- "editorial" - a preference: optional wording, cosmetic formatting, a suggested rephrasing, a style nit, a consolidation you would like but nothing depends on. Recorded as a note on the pull request. It NEVER blocks the change, so use it whenever the documentation is not actually wrong.
- "substantive" - the documentation misinforms a reader who relies on it: it contradicts a required specification or the implementation, gives an operator an instruction that would not work, omits evidence a specification requires, or claims work is complete that is not. Blocks until corrected or explicitly accepted.
- "behavioural" - a substantive defect in a file that influences executable behaviour, generated output, or delivery authority: a specification a validator or generator actually reads, a contract or records file a check verifies, a policy file that steers an agent or a gate. Blocks, and the correction additionally re-runs this project's own test command.

Rules for classifying:
- Classify by what correcting the finding protects, NOT by the file extension and NOT by the severity you assigned. A ".md" file can be behavioural, and an "error" severity does not by itself make a finding substantive.
- When a finding is only a suggestion you would not hold a release for, it is editorial. Do not inflate it to substantive to make sure someone sees it: an editorial note is published on the pull request either way.
- When you are unsure between substantive and behavioural, choose behavioural - the only cost is re-running the test command.
- When you are unsure between editorial and substantive, choose substantive.

Action, separately from class:
- Set "action" to "auto-fix" when your description states the correction precisely enough that another agent could apply it from the description and the file alone. The pipeline then corrects it inside this run, editing only the files your findings name and only inside the documentation-and-records paths listed above.
- Set "action" to "ask-user" only when the correction needs a judgement someone else has to make (which of two contradicting documents is right, whether a documented behaviour should change).
- Set "action" to "no-op" for a finding that needs no change at all.`

// housekeepingLintSection adds the agent-driven lint duty to the combined
// document+lint pass. Read-only, like the document duty: the agent discovers
// and runs the relevant checks but reports every issue instead of fixing it.
const housekeepingLintSection = `

Combined lint duty (same pass - no separate lint agent will run):
- Discover the configured linters and formatters for this repository.
- Run the relevant checks, preferring only the changed files when possible.
- Do not run tests or broader behavioral validation.
- This is a read-only review: do not apply any fix. Report every lint, format, or static-analysis issue you find as a finding with "category" set to "lint", naming the file and line.

Set "category" on every finding: "documentation" for documentation findings, "lint" for lint findings. The "class" field above is required on every documentation finding; omit it on lint findings.`

// documentFindingsSchema is findingsSchema plus the required per-finding
// class. It is a separate schema rather than an extension of the shared one
// because only the document step classifies; lint and rebase share
// findingsSchema and must not be told to emit a field they have no vocabulary
// for.
var documentFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]},
					"class": {"type": "string", "enum": ["editorial", "substantive", "behavioural"], "description": "what correcting this protects: editorial (a preference, never blocks), substantive (the documentation misinforms), behavioural (the file influences executable behaviour, generated output, or delivery authority). Classify by effect, never by file extension or severity."}
				},
				"required": ["severity", "description", "action", "class"]
			}
		},
		"summary": {"type": "string"}
	},
	"required": ["findings", "summary"]
}`)

// housekeepingFindingsSchema extends findingsSchema with the per-finding
// category that routes combined-pass findings to their owning gates, and with
// the document class. The class stays out of "required" here because one
// combined array carries both duties and a lint finding legitimately has no
// class; unmarshalRequiredDocumentFindings enforces it on the documentation
// half, where the flat schema cannot.
var housekeepingFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]},
					"category": {"type": "string", "enum": ["documentation", "lint"]},
					"class": {"type": "string", "enum": ["editorial", "substantive", "behavioural"], "description": "required on every documentation finding, omitted on lint findings: what correcting this protects - editorial (a preference, never blocks), substantive (the documentation misinforms), behavioural (the file influences executable behaviour, generated output, or delivery authority). Classify by effect, never by file extension or severity."}
				},
				"required": ["severity", "description", "action", "category"]
			}
		},
		"summary": {"type": "string"}
	},
	"required": ["findings", "summary"]
}`)

func (s *DocumentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	classPatterns := pipeline.DocumentCorrectionPaths(sctx.Config)

	// The bounded correction runs FIRST, before anything reads the head, so
	// everything after it - the changed-file scan, the analyzer prompt, the
	// read-only verdict - describes the corrected tree. That ordering is what
	// makes the rest of this function the correction's own re-check rather
	// than a separate pass someone has to remember to add.
	correction, err := applyBoundedDocumentCorrection(sctx, classPatterns)
	if err != nil {
		return nil, err
	}

	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	// Combine the agent-driven lint duty into this pass when no deterministic
	// lint command is configured; the lint step then consumes the result
	// instead of paying its own cold agent invocation.
	combinedLint := sctx.Config.Commands.Lint == ""
	if combinedLint {
		sctx.Shared.ClearHousekeepingLint()
	}

	// Skip entirely when nothing the agent would document has changed. No
	// lint result is stashed, so the lint step falls back to its own pass -
	// neither duty is ever silently skipped.
	changedFiles, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	if !hasNonIgnoredDocumentChanges(changedFiles, sctx.Config.IgnorePatterns) {
		sctx.Log("no changes to document")
		// A correction this round already committed still has to be reported,
		// even when every remaining changed path is ignored: it advanced the
		// branch, and a step that returned an empty outcome here would leave
		// the run's own history claiming nothing happened.
		return &pipeline.StepOutcome{
			Findings:   correctionOnlyFindings(correction),
			FixSummary: fixResultSummary(correction.Applied),
		}, nil
	}

	if combinedLint {
		sctx.Log("housekeeping: updating documentation and linting in one pass...")
	} else {
		sctx.Log("updating documentation...")
	}

	// The mutation check below can only attribute a change to this agent by
	// comparing against what was already dirty before it ran. A dirty entry
	// tree is a normal pipeline state, not an anomaly: the Test step's evidence
	// agent is told to write focused tests and its new files reach Document
	// uncommitted (detectNewTestFiles finds them for exactly that reason). So
	// record the entry state rather than refusing to run - pre-existing paths
	// are preserved, excluded from the read-only verdict, and never swept up by
	// the failure cleanup, because they are not this step's to delete.
	entryStatus, entryFingerprint, err := documentWorktreeFingerprint(ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if entryStatus != "" {
		sctx.Log("document step: worktree already carries changes from an earlier step; they are preserved and excluded from the read-only check")
	}

	prompt := s.buildPrompt(sctx, baseSHA, ignorePatterns, classPatterns, combinedLint)
	schema := documentFindingsSchema
	purpose := "document"
	if combinedLint {
		schema = housekeepingFindingsSchema
		purpose = "housekeeping"
	}

	result, err := sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: schema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
	})
	if err != nil {
		return nil, fmt.Errorf("agent document: %w", err)
	}

	// The ANALYSIS turn is genuinely read-only, and stays so even in a round
	// that just corrected something: the correction is a separate, bounded,
	// path-scoped turn that already committed, so by the time this one runs the
	// only changes in the tree are an earlier step's. The earlier fork patch
	// kept a prompt that told the analyzer to fix documentation and never
	// report what it had already fixed, then silently discarded the agent's
	// edits before computing approval from the remaining findings: a compliant
	// agent that fixed and reported nothing produced a passing run while the
	// stale documentation it "fixed" was thrown away unreported. Any worktree
	// mutation after the analyzer returns - tracked or untracked - is a failed
	// step with a clear error; discarding it is cleanup after the failure is
	// recorded, never a silent pass. This is checked before the
	// structured-output validation below, so a pass that both mutated the
	// worktree and returned opaque output reports the mutation rather than
	// hiding it, and a combined-mode mutation can never reach the lint stash.
	_, exitFingerprint, err := documentWorktreeFingerprint(ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if exitFingerprint != entryFingerprint {
		label := "document"
		if combinedLint {
			label = "housekeeping"
		}
		mutations := documentMutationDetail(entryFingerprint, exitFingerprint)
		if entryStatus == "" {
			sctx.Log(label + " step is read-only: agent mutated the worktree, discarding and failing the step")
			if _, cerr := git.Run(ctx, sctx.WorkDir, "checkout", "--", "."); cerr != nil {
				return nil, fmt.Errorf("discard document-step mutation: %w", cerr)
			}
			if _, cerr := git.Run(ctx, sctx.WorkDir, "clean", "-fd"); cerr != nil {
				return nil, fmt.Errorf("discard document-step untracked mutation: %w", cerr)
			}
		} else {
			// An earlier step's uncommitted work shares this worktree, and a
			// path-scoped discard cannot separate the two once the agent has
			// touched a path that was already dirty. Failing without discarding
			// is the safe half of the contract: nothing publishes from a failed
			// step, and no other step's work is destroyed to tidy up this one.
			sctx.Log(label + " step is read-only: agent mutated the worktree; leaving the tree untouched because an earlier step's changes are present")
		}
		return nil, fmt.Errorf("%s analysis must be read-only but the agent modified the worktree:\n%s", label, mutations)
	}

	// Without trustworthy structured output we cannot confirm the agent
	// resolved every gap. Fail the step rather than creating an approval gate:
	// unattended AXI modes can resolve a gate, but must never certify opaque
	// analyzer output.
	var findings Findings
	if result.Output == nil {
		return nil, fmt.Errorf("document analyzer returned no structured findings")
	} else if err := unmarshalRequiredDocumentFindings(result.Output, &findings, combinedLint); err != nil {
		return nil, fmt.Errorf("validate document analyzer findings: %w", err)
	}

	docFindings := findings
	if combinedLint {
		var lintFindings Findings
		docFindings, lintFindings = splitHousekeepingFindings(findings)
		lintJSON, err := types.MarshalFindingsJSON(lintFindings)
		if err == nil {
			sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{
				FindingsJSON: lintJSON,
				Summary:      findings.Summary,
			})
			sctx.Log(fmt.Sprintf("housekeeping lint result recorded for the lint step: %d unresolved items", len(lintFindings.Items)))
		}
	}

	docFindings, editorial, gating := classifyDocumentFindings(docFindings)
	if editorial > 0 {
		sctx.Log(fmt.Sprintf("document findings: %d editorial note(s) recorded on the pull request without gating", editorial))
	}
	if len(correction.Paths) > 0 {
		docFindings.CorrectedPaths = correction.Paths
		sctx.Log(fmt.Sprintf("documentation correction committed in this run: %s", strings.Join(correction.Paths, ", ")))
	}

	// Only a finding whose file the correction may actually reach can be
	// resolved by another round, so offering an auto-fix loop for anything else
	// would burn agent passes to change nothing - the exact shape of the two
	// --action fix rounds that applied no edits in run
	// 01M23AB8T1WKJAG8FMJKG583HP.
	// The correctable set is computed from the auto-fixable findings alone -
	// the exact subset autoFixableFindingsJSON will hand the next round - so a
	// correctable file belonging to some other finding can never be mistaken
	// for work that round could do.
	correctable, _ := correctableDocumentPaths(types.AutoFixableFindings(docFindings), classPatterns)
	// The round this step is completing has not been persisted yet, so a
	// correction it just applied is invisible to the durable budget read.
	// Counting it here is what stops the executor from starting one more
	// round that could only re-check and report.
	spent := documentCorrectionRoundsSpent(sctx)
	if correction.Applied {
		spent++
	}
	autoFixable := documentCorrectionEnabled(sctx) &&
		spent < maxDocumentCorrectionRounds &&
		len(correctable) > 0

	findingsJSON := mustMarshalFindings(docFindings)

	sctx.Log(fmt.Sprintf("document findings: %d unresolved items (%d gating, %d editorial)", len(docFindings.Items), gating, editorial))

	return &pipeline.StepOutcome{
		NeedsApproval: gating > 0,
		AutoFixable:   autoFixable,
		Findings:      string(findingsJSON),
		// The analysis turn never changes anything, so the fix summary reports
		// only what the bounded correction turn committed. Recording "changes
		// applied" for a round that applied none would make downstream history
		// claim work happened here.
		FixSummary: fixResultSummary(correction.Applied),
	}, nil
}

// classifyDocumentFindings applies the class contract to the analyzer's own
// answer and returns the findings the step will report, plus how many are
// editorial notes and how many actually gate.
//
// An editorial finding's action is rewritten to no-op, because "never gates"
// has to be true of the payload itself: the executor parks on any ask-user
// finding in the JSON, and the auto-fix selector picks up any auto-fix one.
// Leaving the analyzer's original action in place and merely intending not to
// gate on it would park the run on a wording preference exactly as before. The
// class is preserved, so the finding is still published as the note it is.
func classifyDocumentFindings(findings Findings) (Findings, int, int) {
	editorial, gating := 0, 0
	for i := range findings.Items {
		if findings.Items[i].IsEditorial() {
			findings.Items[i].Class = types.FindingClassEditorial
			findings.Items[i].Action = types.ActionNoOp
			editorial++
			continue
		}
		findings.Items[i].Class = findings.Items[i].ClassOrDefault()
		gating++
	}
	return findings, editorial, gating
}

// correctionOnlyFindings records a correction whose round found nothing left
// to report, so the corrected files still reach the run history and the pull
// request. It returns "" when no correction was applied, which leaves the
// outcome exactly as it was before.
func correctionOnlyFindings(correction documentCorrection) string {
	if len(correction.Paths) == 0 {
		return ""
	}
	return string(mustMarshalFindings(Findings{
		Summary:        "documentation corrected",
		CorrectedPaths: correction.Paths,
	}))
}

// mustMarshalFindings serializes findings for the step outcome. json.Marshal
// cannot fail for this type - every field is a string, int, bool, or a slice
// of those - and the pre-existing call site already discarded the error, so
// this keeps that one behavior in one named place instead of repeating a bare
// underscore at each use.
func mustMarshalFindings(findings Findings) []byte {
	raw, err := json.Marshal(findings)
	if err != nil {
		return []byte(`{"findings":[],"summary":""}`)
	}
	return raw
}

// buildPrompt assembles the document (or combined document+lint) prompt: the
// placement policy, scope discipline, trusted repository-specific policy,
// the task, and - in combined mode - the lint duty.
func (s *DocumentStep) buildPrompt(sctx *pipeline.StepContext, baseSHA, ignorePatterns string, classPatterns []string, combinedLint bool) string {
	historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)

	intro := "Review the project documentation for accuracy after this change. This is a read-only review: do not edit any file."
	if combinedLint {
		intro = "Perform the combined documentation and lint housekeeping pass for this change. This is a read-only review: do not edit any file."
	}

	editRule := "- This is a read-only review: do not modify, create, or delete any file. Report every stale or incorrect statement instead of fixing it."
	if combinedLint {
		editRule = "- This is a read-only review: do not modify, create, or delete any file, including lint or formatting fixes. Report every stale, incorrect, or unresolved issue instead of fixing it."
	}

	prompt := fmt.Sprintf(
		`%s Analyze what the change made stale and report every defect you find, with the file and line of each stale or incorrect statement.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s
- ignore patterns: %s
- documentation-and-records paths this pipeline may correct inside the run: %s

%s

%s

%s%s

Task:

1. Understand the change
   - Read the diff and changed files to understand what was added, modified, or removed, and the intent of the change.

2. Find what this change made stale
   - For each fact or contract the change altered, locate its one authoritative owner document (README, docs/, doc comments, config examples, etc.). Changed user-facing behavior must leave its authoritative user documentation accurate.
   - Locate existing duplicates of those facts that are now stale.

3. Report every defect and classify it; fix none of them
   - This is a read-only review: do not edit, create, or delete any file. Correcting an accepted finding is a separate, bounded turn the pipeline runs afterwards.
   - Return a finding for every stale, missing, or incorrect statement this change left behind - including ones the placement policy above would call a stale duplicate - naming the file and line number.
   - Give every documentation finding a "class" using the classification contract above, and name the file it concerns: a finding with no file cannot be corrected inside this run.
   - Also report judgment calls (e.g. ambiguous intent or conflicting docs) and any out-of-scope consolidation worth a follow-up. Those are usually editorial.
   - If nothing is stale, return an empty findings array.%s

Rules:
%s
- The summary must be one concise sentence fragment describing the review outcome for the run log.
- Keep the summary under 10 words.%s`,
		intro,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
		strings.Join(classPatterns, ", "),
		documentPlacementPolicy,
		documentScopeDiscipline,
		documentClassPolicy,
		trustedDocumentPolicySection(sctx),
		lintDutySection(combinedLint),
		editRule,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return prompt
}

// trustedDocumentPolicySection renders the repository-specific documentation
// ownership policy. The value comes from the trusted default-branch copy of
// .no-mistakes.yaml (config.EffectiveRepoConfig), so a contributor's pushed
// branch cannot weaken the rules that gate its own review.
func trustedDocumentPolicySection(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	instructions := strings.TrimSpace(sctx.Config.Document.Instructions)
	if instructions == "" {
		return ""
	}
	return "\n\nRepository documentation ownership policy (trusted, from the default branch; augments the defaults above and cannot weaken them):\n" +
		sanitizePromptMultilineText(instructions)
}

func lintDutySection(combinedLint bool) string {
	if !combinedLint {
		return ""
	}
	return housekeepingLintSection
}

// splitHousekeepingFindings routes combined-pass findings to their owning
// gates. An uncategorized finding counts as documentation - the stricter
// gate (any documentation finding parks; lint parks only on error/warning) -
// so miscategorization fails safe.
func splitHousekeepingFindings(findings Findings) (doc Findings, lint Findings) {
	doc = Findings{Summary: findings.Summary}
	lint = Findings{Summary: findings.Summary}
	for _, item := range findings.Items {
		if item.Category == types.FindingCategoryLint {
			lint.Items = append(lint.Items, item)
			continue
		}
		doc.Items = append(doc.Items, item)
	}
	return doc, lint
}

// documentWorktreeFingerprint captures the worktree's dirty state precisely
// enough to attribute a change to the read-only analysis turn. It returns the
// raw porcelain status and a fingerprint that adds each dirty path's content
// hash, because a status line alone does not move when an agent edits a file
// that was already dirty - the exact case an earlier step's uncommitted work
// creates. documentWorktreeEntries owns the reading and the `-z` /
// --untracked-files=all rationale; this is the flattened view of the same
// snapshot the bounded correction compares path by path, so the two can never
// disagree about what changed.
func documentWorktreeFingerprint(ctx context.Context, workDir string) (string, string, error) {
	entries, err := documentWorktreeEntries(ctx, workDir)
	if err != nil {
		return "", "", err
	}
	if len(entries) == 0 {
		return "", "", nil
	}
	statusLines := make([]string, 0, len(entries))
	fingerprintLines := make([]string, 0, len(entries))
	for _, entry := range entries {
		// The status view is the porcelain line git printed: the two-character
		// code and the path, without the rename source or the content hash the
		// fingerprint adds on top.
		statusLines = append(statusLines, entry.Code+" "+entry.Path)
		fingerprintLines = append(fingerprintLines, entry.Line)
	}
	return strings.Join(statusLines, "\n"), strings.Join(fingerprintLines, "\n"), nil
}

// documentMutationDetail reports what the read-only pass changed: fingerprint
// entries the agent added or altered, plus any it reverted.
func documentMutationDetail(entryFingerprint, exitFingerprint string) string {
	before := map[string]bool{}
	for _, line := range strings.Split(entryFingerprint, "\n") {
		before[line] = true
	}
	after := map[string]bool{}
	for _, line := range strings.Split(exitFingerprint, "\n") {
		after[line] = true
	}
	var detail []string
	for _, line := range strings.Split(exitFingerprint, "\n") {
		if line != "" && !before[line] {
			detail = append(detail, line)
		}
	}
	for _, line := range strings.Split(entryFingerprint, "\n") {
		if line != "" && !after[line] {
			detail = append(detail, "(reverted) "+line)
		}
	}
	return strings.Join(detail, "\n")
}
func hasNonIgnoredDocumentChanges(changedFiles string, ignorePatterns []string) bool {
	for _, path := range strings.Split(changedFiles, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		ignored := false
		for _, pattern := range ignorePatterns {
			if matchIgnorePattern(path, pattern) {
				ignored = true
				break
			}
		}
		if !ignored {
			return true
		}
	}
	return false
}
