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

// housekeepingLintSection adds the agent-driven lint duty to the combined
// document+lint pass. Read-only, like the document duty: the agent discovers
// and runs the relevant checks but reports every issue instead of fixing it.
const housekeepingLintSection = `

Combined lint duty (same pass - no separate lint agent will run):
- Discover the configured linters and formatters for this repository.
- Run the relevant checks, preferring only the changed files when possible.
- Do not run tests or broader behavioral validation.
- This is a read-only review: do not apply any fix. Report every lint, format, or static-analysis issue you find as a finding with "category" set to "lint", naming the file and line.

Set "category" on every finding: "documentation" for documentation findings, "lint" for lint findings.`

// housekeepingFindingsSchema extends findingsSchema with the per-finding
// category that routes combined-pass findings to their owning gates.
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
					"category": {"type": "string", "enum": ["documentation", "lint"]}
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
		return &pipeline.StepOutcome{}, nil
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

	prompt := s.buildPrompt(sctx, baseSHA, ignorePatterns, combinedLint)
	schema := findingsSchema
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

	// Genuinely read-only. The earlier fork patch kept a prompt that told the
	// agent to fix documentation and never report what it had already fixed,
	// then silently discarded the agent's edits before computing approval from
	// the remaining findings: a compliant agent that fixed and reported nothing
	// produced a passing run while the stale documentation it "fixed" was
	// thrown away unreported. Any worktree mutation after the agent returns -
	// tracked or untracked - is now a failed step with a clear error;
	// discarding it is cleanup after the failure is recorded, never a silent
	// pass. This is checked before the structured-output validation below, so a
	// pass that both mutated the worktree and returned opaque output reports
	// the mutation rather than hiding it, and a combined-mode mutation can
	// never reach the lint stash.
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
		return nil, fmt.Errorf("%s step must be read-only but the agent modified the worktree:\n%s", label, mutations)
	}

	// Without trustworthy structured output we cannot confirm the agent
	// resolved every gap. Fail the step rather than creating an approval gate:
	// unattended AXI modes can resolve a gate, but must never certify opaque
	// analyzer output.
	var findings Findings
	if result.Output == nil {
		return nil, fmt.Errorf("document analyzer returned no structured findings")
	} else if err := unmarshalRequiredFindings(result.Output, &findings, true); err != nil {
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

	needsApproval := len(docFindings.Items) > 0
	findingsJSON, _ := json.Marshal(docFindings)

	sctx.Log(fmt.Sprintf("document findings: %d unresolved items", len(docFindings.Items)))

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    docFindings.Summary,
	}, nil
}

// buildPrompt assembles the document (or combined document+lint) prompt: the
// placement policy, scope discipline, trusted repository-specific policy,
// the task, and - in combined mode - the lint duty.
func (s *DocumentStep) buildPrompt(sctx *pipeline.StepContext, baseSHA, ignorePatterns string, combinedLint bool) string {
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

%s

%s%s

Task:

1. Understand the change
   - Read the diff and changed files to understand what was added, modified, or removed, and the intent of the change.

2. Find what this change made stale
   - For each fact or contract the change altered, locate its one authoritative owner document (README, docs/, doc comments, config examples, etc.). Changed user-facing behavior must leave its authoritative user documentation accurate.
   - Locate existing duplicates of those facts that are now stale.

3. Report every defect; fix none of them
   - This is a read-only review: do not edit, create, or delete any file.
   - Return a finding for every stale, missing, or incorrect statement this change left behind - including ones the placement policy above would call a stale duplicate - naming the file and line number.
   - Also report judgment calls (e.g. ambiguous intent or conflicting docs) and any out-of-scope consolidation worth a follow-up.
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
		documentPlacementPolicy,
		documentScopeDiscipline,
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
// enough to attribute a change to the read-only agent. It returns the raw
// porcelain status and a fingerprint that adds each dirty path's content hash,
// because a status line alone does not move when an agent edits a file that was
// already dirty - the exact case an earlier step's uncommitted work creates.
// Deletions, renames, and unreadable paths fall back to the status line, which
// already records them.
func documentWorktreeFingerprint(ctx context.Context, workDir string) (string, string, error) {
	// --untracked-files=all matters: git's default collapses a wholly untracked
	// directory to one "?? dir/" line, and a file the agent adds inside it would
	// leave that line - and so the fingerprint - byte-identical.
	//
	// -z is used to read the records: without it, git's default core.quotepath
	// escapes any path containing non-ASCII bytes, backslashes, or double quotes
	// into a C-style quoted string, which would then fail to open at that literal
	// path and silently fall back to the (unmoving) status line alone.
	rawZ, err := git.RunRaw(ctx, workDir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return "", "", fmt.Errorf("check worktree status for the read-only document pass: %w", err)
	}
	records := strings.Split(strings.TrimSuffix(string(rawZ), "\x00"), "\x00")
	if len(records) == 1 && records[0] == "" {
		return "", "", nil
	}
	var statusLines []string
	var entries []string
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 {
			continue
		}
		code := record[:2]
		path := record[3:]
		statusLines = append(statusLines, code+" "+path)
		entry := code + " " + path
		// A rename/copy (R/C) emits the destination record followed by a second
		// NUL-terminated record holding the source path, rather than the
		// "old -> new" arrow the non-`-z` format uses.
		if strings.ContainsAny(code, "RC") && i+1 < len(records) {
			i++
			entry += " <- " + records[i]
		}
		if strings.Contains(code, "D") {
			entries = append(entries, entry)
			continue
		}
		hash, herr := git.Run(ctx, workDir, "hash-object", "--", path)
		if herr != nil {
			entries = append(entries, entry)
			continue
		}
		entries = append(entries, entry+" "+strings.TrimSpace(hash))
	}
	status := strings.Join(statusLines, "\n")
	if status == "" {
		return "", "", nil
	}
	return status, strings.Join(entries, "\n"), nil
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
