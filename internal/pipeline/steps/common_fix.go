package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fixExecutionOptions struct {
	RequirePreviousFindings bool
	MissingFindingsError    string
	LogMessage              string
	Prompt                  string
	SelectedFindings        string
	ErrorPrefix             string
	FallbackSummary         string
	AfterAgentRun           func(*agent.Result) error
	AgentContext            context.Context
	// RunAgent overrides the agent-call seam while leaving preparation and
	// post-agent commit work on the step context. Review uses it to create a
	// fresh review_agent_timeout context at the instant each fixer starts.
	RunAgent func(agent.RunOpts) (*agent.Result, error)
	// SessionRole, when set, runs the fix turn in that durable review-loop
	// session (the review step's fixer role). Steps outside the review loop
	// leave it empty and stay session-isolated.
	SessionRole pipeline.SessionRole
	// Purpose labels the invocation for local performance telemetry.
	Purpose string
	// Workload records the bounded size of the change under fix for local
	// telemetry. Optional; nil leaves the invocation's workload unknown.
	Workload *agent.InvocationWorkload
}

type commitSummary struct {
	Summary string `json:"summary"`
}

var errRejectedCommitSummary = errors.New("rejected commit summary")

const (
	noChangesAppliedSummary = pipeline.FixSummaryNoChangesApplied
	changesAppliedSummary   = pipeline.FixSummaryChangesApplied
)

const fixerRemovalRule = `

Removal-first rule:
- When a problem can be solved by removing a code path that is not strictly required to satisfy the intent - an extra acceptance or matching branch, a fallback, an alias, a second definition of something the code already defines once, or handling for an input nobody intends - fix it by removing that path, not by validating, hardening, or documenting it. Judge what the intent strictly requires against the User intent section when present, otherwise against the change's own stated purpose. Removal is the smallest fix for such a path: hardening it leaves the unrequired path in place for the next review to find another hole in.`

func fixerPrompt(prompt string) string {
	return prompt + fixerRemovalRule
}

var commitSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes))

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == "error" || f.Severity == "warning" {
			return true
		}
	}
	return false
}

// assertPipelineHeadContinuity fails closed when the worktree HEAD is no longer
// equal to or a descendant of the head the pipeline itself last recorded
// (sctx.Run.HeadSHA). Every post-review step calls this guard at entry, and
// commitAgentFixes calls it around commits that advance the recorded head.
//
// The pipeline advances HEAD only through its own commits, each of which updates
// sctx.Run.HeadSHA in lockstep. If HEAD has diverged from that recorded head -
// e.g. a concurrent process reset the shared worktree to a different commit -
// then the reviewed change the pipeline approved is no longer in HEAD's history,
// and continuing would ship an unreviewed tree. The whole job of this tool is
// to not lose people's code, so we refuse rather than proceed.
//
// Anchor integrity: sctx.Run.HeadSHA is the correct, un-clobberable anchor. It
// is the *recorded* head the pipeline itself produced at its last commit - held
// in the single daemon process's in-memory Run struct (one shared pointer per
// run, never re-read from the DB mid-pipeline) and written only by no-mistakes
// commit code (commit_fix / rebase / ci_fix / push). An out-of-band `git reset`
// mutates the worktree HEAD on disk but cannot touch this field, so at the check
// point the anchor still holds the reviewed head even after a clobber. The guard
// deliberately compares the *recorded* head against the *live* worktree HEAD
// (git.HeadSHA); it never derives the anchor from the mutable worktree, which
// would be circular and defeatable. Because the guard runs at every post-review
// step entry and at the very top of commitAgentFixes - before any commit that
// would advance sctx.Run.HeadSHA - the next pipeline boundary after a clobber is
// caught while the anchor is still the pre-clobber reviewed head; the anchor can
// never be advanced into a clobbered lineage without first passing this check.
//
// This is what happened in run 01KXC3SD5NZYMERGDS68Z1C8ER: the review step
// committed a correct fix, a sibling worktree sharing the bare repo reset HEAD
// to a divergent commit that lacked it, and the document step committed on the
// clobber and shipped it. A forward-only agent commit (git rebase --continue,
// etc.) keeps the recorded head as an ancestor and is allowed; a divergent
// (sibling) reset or a backward reset both trip this guard. On any failure the
// step and the whole run abort (executor.failRun) before doing more work -
// nothing is committed or shipped.
func assertPipelineHeadContinuity(sctx *pipeline.StepContext, stepName types.StepName) error {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	currentHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head before %s step: %w", stepName, err)
	}
	if currentHead == recorded {
		return nil
	}
	// Fail closed: refuse unless the recorded head is genuinely an ancestor of the
	// live HEAD (a legitimate forward move). A non-ancestor result OR any git error
	// (e.g. an unknown recorded object) aborts rather than proceeds.
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", recorded, currentHead); err != nil {
		return fmt.Errorf("refusing to run %s step: worktree HEAD %s is not a descendant of the pipeline's recorded head %s; "+
			"the reviewed change was rewritten out-of-band and would be lost - aborting to protect it",
			stepName, currentHead, recorded)
	}
	return nil
}

// commitPipelineCorrection creates a pipeline-authored correction commit with
// hook verification bypassed, and is the single owner of that bypass.
//
// A correction commit is machine-authored: the pipeline records the change its
// own agents or its own formatter produced, inside the throwaway run
// worktree. That worktree is freshly carved from the bare gate repo, so tracked
// hooks that depend on generated untracked runtime files cannot run there - the
// canonical case is a repository whose shared config sets core.hooksPath=.husky
// while a tracked .husky hook sources the generated .husky/_/husky.sh that no
// install step ever created in this worktree. The hook exits nonzero, the
// correction commit fails, and the whole run dies on setup state that says
// nothing about the change under review.
//
// --no-verify alone is not enough, because Git gates only pre-commit and
// commit-msg on it and always runs prepare-commit-msg (builtin/commit.c
// prepare_to_commit), so a repository carrying a legacy .husky
// prepare-commit-msg hook - commitizen and ticket-prefix setups are the common
// ones - still fails the exact commit this helper exists to complete. Pointing
// core.hooksPath at a freshly created empty directory for this one invocation
// covers the whole commit hook family; --no-verify is kept so the intent stays
// explicit at the call. The override lives only in this process argument list
// and the directory is removed afterwards, so nothing persists in the
// repository, the user's configuration, or the daemon's environment.
//
// Reach is deliberately narrow. Only commitAgentFixes (Review, Test, Document,
// Lint) and the Push step's leftover-worktree commit route here, because those
// are the two commits the pipeline authors from its own agents' and formatter's
// output.
// CI repair commits, the generic git runner, and every user-authored commit keep
// hook verification; the Review, Test, Document, Lint, Push, PR, and CI gates
// remain the authoritative quality checks for what these commits contain.
func commitPipelineCorrection(ctx context.Context, workDir, message string, logf func(string)) error {
	return commitPipelineCorrectionWithCleanup(ctx, workDir, message, logf, os.RemoveAll)
}

func commitPipelineCorrectionWithCleanup(
	ctx context.Context,
	workDir, message string,
	logf func(string),
	cleanup func(string) error,
) error {
	emptyHooksDir, err := os.MkdirTemp("", "no-mistakes-correction-hooks-")
	if err != nil {
		return fmt.Errorf("prepare hook-free commit environment: %w", err)
	}
	_, commitErr := git.Run(ctx, workDir, "-c", "core.hooksPath="+emptyHooksDir, "commit", "--no-verify", "-m", message)
	if cleanupErr := cleanup(emptyHooksDir); cleanupErr != nil {
		if logf != nil {
			logf(fmt.Sprintf("warning: failed to remove temporary hook-free commit directory %s: %v", emptyHooksDir, cleanupErr))
		} else {
			slog.Warn("failed to remove temporary hook-free commit directory", "path", emptyHooksDir, "error", cleanupErr)
		}
	}
	return commitErr
}

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	_, err := commitAgentFixesWithResult(sctx, stepName, summary, fallbackSummary)
	return err
}

func commitAgentFixesWithResult(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) (bool, error) {
	ctx := sctx.Ctx
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return false, err
	}
	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check %s changes: %w", stepName, err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no agent changes to commit")
		if sctx.CurrentFixUnit != nil {
			return false, sctx.RecordFixUnitHead(sctx.Run.HeadSHA)
		}
		return false, nil
	}
	if summary == "" {
		summary = fallbackSummary
	}
	if summary == "" {
		summary = "apply fixes"
	}
	commitMessage, err := sctx.Config.Commit.RenderFixMessage(stepName, summary)
	if err != nil {
		return false, fmt.Errorf("render %s fix commit message: %w", stepName, err)
	}
	if err := stagePipelineChanges(sctx); err != nil {
		return false, fmt.Errorf("stage %s changes: %w", stepName, err)
	}
	if err := commitPipelineCorrection(ctx, sctx.WorkDir, commitMessage, sctx.Log); err != nil {
		return false, fmt.Errorf("commit %s changes: %w", stepName, err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head after %s commit: %w", stepName, err)
	}
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return false, err
	}
	if sctx.CurrentFixUnit != nil {
		parent := sctx.Run.HeadSHA
		if err := updateNonSharedBranchRef(sctx, headSHA); err != nil {
			return false, err
		}
		if err := sctx.RecordFixUnitHead(headSHA); err != nil {
			return false, err
		}
		if stepName == types.StepReview {
			pipeline.PersistUncertifiedPipelineRange(sctx, parent, headSHA)
		}
	} else {
		ref := normalizedBranchRef(sctx.Run.Branch)
		if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, headSHA); err != nil {
			return false, fmt.Errorf("update local branch ref: %w", err)
		}
		startingHead := strings.TrimSpace(sctx.ReviewStartingHeadSHA)
		if startingHead == "" {
			startingHead = sctx.Run.HeadSHA
		}
		sctx.Run.HeadSHA = headSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
			return false, err
		}
		if stepName == types.StepReview {
			pipeline.PersistUncertifiedPipelineRange(sctx, startingHead, headSHA)
		}
	}
	sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	return true, nil
}

func fixResultSummary(committed bool) string {
	if committed {
		return changesAppliedSummary
	}
	return noChangesAppliedSummary
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if !utf8.Valid(result.Output) {
		return "", fmt.Errorf("%w: agent output must contain valid UTF-8", errRejectedCommitSummary)
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	if len(summary.Summary) > config.MaxFixMessageSummaryBytes {
		return "", fmt.Errorf("%w: commit summary must not exceed %d bytes", errRejectedCommitSummary, config.MaxFixMessageSummaryBytes)
	}
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	return cleaned, nil
}

func executeFixMode(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) (string, error) {
	var selected types.Findings
	if sctx.Fixing && (stepName == types.StepReview || stepName == types.StepTest) && sctx.PreviousFindings != "" {
		raw := sctx.PreviousFindings
		if opts.SelectedFindings != "" {
			raw = opts.SelectedFindings
		}
		if err := json.Unmarshal([]byte(raw), &selected); err != nil {
			return "", err
		}
	}
	selected = types.NormalizeFindings(selected, string(stepName))
	if len(selected.Items) > 0 {
		var err error
		selected, err = sctx.PrepareFixContinuation(stepName, selected)
		if err != nil {
			return "", err
		}
	}
	if len(selected.Items) == 0 || (len(selected.Items) == 1 && sctx.StepResultID == "") {
		return executeFixTurn(sctx, stepName, opts)
	}
	defer func() { sctx.CurrentFixUnit = nil; sctx.FixSelectionID = "" }()
	changed := false
	for i, finding := range selected.Items {
		if sctx.FixAppliedOrdinals[i+1] {
			continue
		}
		if err := sctx.BeginFixUnit(stepName, finding, i+1, len(selected.Items)); err != nil {
			return "", err
		}
		unitOpts := opts
		unitJSON, _ := json.Marshal(finding)
		unitOpts.Prompt = opts.Prompt
		if len(selected.Items) > 1 {
			unitOpts.Prompt = strings.ReplaceAll(unitOpts.Prompt, "- Apply all the fixes you intend to make first; do not run any verification in between individual fixes.", "- Apply only the authorized cause and its sibling sites in this turn.")
			unitOpts.Prompt = strings.ReplaceAll(unitOpts.Prompt, "- After all fixes are applied, run one focused verification limited to the changed area (the specific package, file, or test you touched) at the end of the fix round to confirm the fixes hold.", "- The pipeline runs one focused verification for the union after the final checkpoint.")
		}
		unitOpts.Prompt += "\n\nCurrent repair finding ID: " + finding.ID + "\nCurrent checkpoint head: " + sctx.Run.HeadSHA + " (the original context above names the batch starting head).\nAuthorized repair finding (other selected/deferred findings remain context only):\n" + string(unitJSON) + "\nClose this finding's cause at every sibling site. Do not commit, rebase, reset or push."
		if len(selected.Items) > 1 {
			unitOpts.Prompt += "\nThis is an edit-only unit. Do not run verification in this turn; one focused verification for the union follows all checkpoints."
		}
		summary, err := executeFixTurn(sctx, stepName, unitOpts)
		if err != nil {
			return "", err
		}
		changed = changed || summary == changesAppliedSummary
	}
	sctx.CurrentFixUnit = nil
	if stepName == types.StepReview && len(selected.Items) > 1 {
		if err := verifyFixBatch(sctx, opts, "review-fix-verification"); err != nil {
			return "", err
		}
	}
	sctx.CompletedFixSelectionID = sctx.FixSelectionID
	return fixResultSummary(changed), nil
}

// Verification remains a separate bounded turn after all local checkpoints.
func verifyFixBatch(sctx *pipeline.StepContext, opts fixExecutionOptions, purpose string) error {
	runOpts := agent.RunOpts{CWD: sctx.WorkDir, Prompt: fixerPrompt("Run one focused verification for the union of the completed repair causes below. Do not edit files or run the full suite. Return a concise JSON summary of the checks actually run and their results.\n" + opts.Prompt), JSONSchema: commitSummarySchema, OnChunk: sctx.LogChunk, Purpose: purpose}
	var result *agent.Result
	var err error
	if opts.RunAgent != nil {
		result, err = opts.RunAgent(runOpts)
	} else {
		result, err = sctx.RunAgent(runOpts)
	}
	if err != nil {
		return err
	}
	var summary string
	if result != nil {
		summary, err = extractCommitSummary(result)
	} else {
		err = fmt.Errorf("agent returned no structured summary")
	}
	if purpose == "ci-fix-verification" && (result == nil || len(result.Output) == 0 || err == nil && summary == "") {
		sctx.Log("focused CI verification returned no structured summary; required head validation remains pending")
	} else if err != nil || summary == "" {
		return fmt.Errorf("invalid focused-verification summary: %v", err)
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return err
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if head != sctx.Run.HeadSHA || status != "" {
		return fmt.Errorf("focused verification changed saved repair work; retain for reconciliation")
	}
	return nil
}

func executeFixTurn(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	if opts.RequirePreviousFindings && sctx.PreviousFindings == "" {
		return "", errors.New(opts.MissingFindingsError)
	}
	if opts.LogMessage != "" {
		sctx.Log(opts.LogMessage)
	}
	purpose := opts.Purpose
	if purpose == "" {
		purpose = string(stepName) + "-fix"
	}
	runOpts := agent.RunOpts{
		Prompt:     fixerPrompt(opts.Prompt),
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
		Workload:   opts.Workload,
	}
	var result *agent.Result
	var err error
	if opts.RunAgent != nil {
		result, err = opts.RunAgent(runOpts)
	} else {
		agentCtx := sctx.Ctx
		if opts.AgentContext != nil {
			agentCtx = opts.AgentContext
		}
		result, err = sctx.RunAgentSessionContext(agentCtx, opts.SessionRole, runOpts)
	}
	if err != nil {
		if opts.ErrorPrefix == "" {
			return "", err
		}
		return "", fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
	}
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return "", err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		if sctx.CurrentFixUnit != nil {
			return "", fmt.Errorf("invalid repair-unit summary: %w", err)
		}
		if errors.Is(err, errRejectedCommitSummary) {
			return "", fmt.Errorf("validate %s fix summary: %w", stepName, err)
		}
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	if sctx.CurrentFixUnit != nil {
		if summary == "" {
			return "", fmt.Errorf("empty repair-unit summary")
		}
		sctx.CurrentFixUnit.Summary = summary
	}
	committed, err := commitAgentFixesWithResult(sctx, stepName, summary, opts.FallbackSummary)
	if err != nil {
		return "", err
	}
	return fixResultSummary(committed), nil
}

func updateNonSharedBranchRef(sctx *pipeline.StepContext, headSHA string) error {
	shared, err := worktreeSharesGateRefs(sctx)
	if err != nil || shared {
		return err
	}
	if _, err := stepGitRun(sctx, "update-ref", normalizedBranchRef(sctx.Run.Branch), headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	return nil
}

func worktreeSharesGateRefs(sctx *pipeline.StepContext) (bool, error) {
	if strings.TrimSpace(sctx.GateDir) == "" {
		return false, nil
	}
	gateInfo, err := os.Stat(sctx.GateDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect gate ref storage: %w", err)
	}
	commonDir, err := stepGitRun(sctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("resolve worktree ref storage: %w", err)
	}
	commonInfo, err := os.Stat(commonDir)
	if err != nil {
		return false, fmt.Errorf("inspect worktree ref storage: %w", err)
	}
	return os.SameFile(gateInfo, commonInfo), nil
}
