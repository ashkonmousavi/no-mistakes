package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// maxFinalHeadRereviews bounds how many times one run may restart at Review
// because its final head moved. Deliberately small: a run that cannot settle on
// a head within this budget is looping, and every extra round costs a full
// Review pass. TestExecutor_FinalHeadRereviewLoopIsBounded pins convergence
// against this value, so raising it here without raising that bound turns a
// non-converging loop into a passing test.
const maxFinalHeadRereviews = 3

// maxDocumentationHeadRechecks bounds how many times one run may restart at
// Test because a documentation-and-records-only correction advanced the head.
// Two, because at most two post-review steps can produce such an advance in
// one pass (Document's bounded correction, then Lint's own fix round); a third
// means something is rewriting documentation on every pass and the run is
// looping. TestExecutor_DocumentationHeadRecheckLoopIsBounded pins convergence
// against this value, so raising it here without raising that bound turns a
// non-converging loop into a passing test.
const maxDocumentationHeadRechecks = 2

// GitRunner runs a git command in the caller's own scope and returns trimmed
// stdout. Callers inside a step pass the step-scoped runner so a step-local
// PATH and credential environment stay in effect; the executor passes a plain
// worktree runner.
type GitRunner func(args ...string) (string, error)

// WorktreeGitRunner is the plain worktree runner used outside a step scope.
func WorktreeGitRunner(ctx context.Context, workDir string) GitRunner {
	return func(args ...string) (string, error) { return git.Run(ctx, workDir, args...) }
}

// ReviewApprovedHead returns the run's durable review-approved commit, or ""
// plus the reason it is unusable. It is the single reader of that authority, so
// the pre-publication continuity decision, the post-review head binding, and
// the publication guard itself can never disagree about what "reviewed" means.
func ReviewApprovedHead(run *db.Run, gitRun GitRunner) (string, string) {
	if run == nil || run.ReviewApprovedHeadSHA == nil || strings.TrimSpace(*run.ReviewApprovedHeadSHA) == "" {
		return "", "run has no durably recorded review-approved head"
	}
	approvedHead := strings.TrimSpace(*run.ReviewApprovedHeadSHA)
	if !IsFullGitObjectID(approvedHead) {
		return "", "durable review-approved head is malformed"
	}
	resolved, err := gitRun("rev-parse", "--verify", approvedHead+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(resolved), approvedHead) {
		return "", "durable review-approved head is unreachable"
	}
	return approvedHead, ""
}

// ReviewHeadNeedsRereview checks the durable completed-review authority for a
// proposed local head. Exact equality is publication-ready. A descendant is
// preserved but must receive another full Review before it may be published:
// passing tests, an ancestry proof, and a freshly stamped attestation all
// describe a commit no reviewer ever read. Missing, malformed, unreachable,
// backward, or divergent authority fails closed.
func ReviewHeadNeedsRereview(database *db.DB, runID, proposedHead string, gitRun GitRunner) (bool, error) {
	run, err := database.GetRun(runID)
	if err != nil {
		return false, fmt.Errorf("load durable review approval before publication: %w", err)
	}
	approvedHead, reason := ReviewApprovedHead(run, gitRun)
	if approvedHead == "" {
		return false, fmt.Errorf("refusing publication: %s", reason)
	}
	proposedHead = strings.TrimSpace(proposedHead)
	if !IsFullGitObjectID(proposedHead) {
		return false, fmt.Errorf("refusing publication: proposed head is malformed")
	}
	if strings.EqualFold(proposedHead, approvedHead) {
		return false, nil
	}
	if _, err := gitRun("merge-base", "--is-ancestor", approvedHead, proposedHead); err != nil {
		return false, fmt.Errorf("refusing publication: proposed head %s violates continuity with review-approved head %s (it is not an equal or descendant commit)", ShortObjectID(proposedHead), ShortObjectID(approvedHead))
	}
	return true, nil
}

// IsFullGitObjectID reports whether value is a complete SHA-1 or SHA-256 object
// name. An abbreviated or non-hex value is never accepted as review authority.
func IsFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// ShortObjectID abbreviates an object name for user-facing messages.
func ShortObjectID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

// isPostReviewMutationStep names the steps that run after Review and may
// legitimately advance the local head (Test repairs, Document prose, Lint
// fixes). Push preparation is handled inside the Push step itself, which is the
// last chance to stop a remote mutation.
func isPostReviewMutationStep(name types.StepName) bool {
	switch name {
	case types.StepTest, types.StepDocument, types.StepLint:
		return true
	default:
		return false
	}
}

// bindPostReviewHead decides what a post-review head advance owes before it
// may be published. The commit is preserved, never discarded: only the
// authority to publish it is withheld until the proof it owes has been paid.
//
// Two shapes, and the difference between them is what the advance actually
// changed:
//
//   - An advance that touched anything outside the documentation-and-records
//     class restarts at Review, exactly as before. A reviewer has not read that
//     code, and passing tests, an ancestry proof, and a fresh attestation all
//     describe a commit no reviewer read.
//   - An advance confined to the documentation-and-records class carries the
//     existing review approval forward to the corrected head and restarts at
//     Test instead. Review already read every source file in the candidate and
//     nothing about them changed, so repeating it buys nothing - and repeating
//     it is precisely the failure the read-only document step was introduced to
//     stop: the old housekeeping pass committed prose after review and drove
//     one records-only change to 8 Review rounds. What the corrected head still
//     owes is proof appropriate to what changed, and it pays all of it: the
//     project's own test command and lint re-run at the corrected head, Push
//     writes the exact-head attestation, and CI runs the full battery there.
//
// Carrying the approval forward is a real widening of that authority, so it is
// recorded rather than assumed: the exact file list is logged, and the restart
// carries the durable RestartReasonDocumentationHeadRecheck trigger that
// `axi status` and the run's round history name.
func (e *Executor) bindPostReviewHead(ctx context.Context, step Step, sctx *StepContext, outcome *StepOutcome) error {
	if outcome == nil || !isPostReviewMutationStep(step.Name()) || outcome.RestartFrom != "" || sctx.Run.ReviewApprovedHeadSHA == nil {
		return nil
	}
	currentHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s step: %w", step.Name(), err)
	}
	gitRun := WorktreeGitRunner(ctx, sctx.WorkDir)
	needsRereview, err := ReviewHeadNeedsRereview(e.db, sctx.Run.ID, currentHead, gitRun)
	if err != nil {
		return err
	}
	if !needsRereview {
		return nil
	}
	if currentHead != sctx.Run.HeadSHA {
		sctx.Run.HeadSHA = currentHead
		if err := e.db.UpdateRunHeadSHA(sctx.Run.ID, currentHead); err != nil {
			return fmt.Errorf("record post-review head before rereview: %w", err)
		}
	}

	// ReviewHeadNeedsRereview has already proven the approved head is present,
	// well-formed, and an ancestor of currentHead, so the diff below is the
	// complete set of files this run added on top of what Review read.
	approvedHead := strings.TrimSpace(*sctx.Run.ReviewApprovedHeadSHA)
	documentationOnly, changed, err := DocumentClassAdvance(gitRun, approvedHead, currentHead, DocumentCorrectionPaths(e.config))
	if err != nil {
		return err
	}
	if documentationOnly {
		return e.carryReviewApprovalToDocumentationHead(step, sctx, outcome, approvedHead, currentHead, changed)
	}

	outcome.RestartFrom = types.StepReview
	outcome.RestartReason = RestartReasonFinalHeadRereview
	sctx.Log("final_head_rereview: pipeline-authored changes advanced HEAD after review; restarting at Review before publication")
	return nil
}

// carryReviewApprovalToDocumentationHead extends the run's review authority to
// a documentation-and-records-only descendant and sends the run back to Test
// for the proof that head still owes.
//
// The extension is durable (runs.review_approved_head_sha), because every
// later publication decision - the Push step's strict head-equality check
// included - reads that one field, and leaving it behind would make Push
// refuse a head this rule just authorized. Widening it here is the deliberate
// second writer of that column; it is never inferred from a worktree, a gate
// ref, or a remote branch, and it is only ever moved forward to a proven
// descendant whose entire diff from the approved head is inside the class.
func (e *Executor) carryReviewApprovalToDocumentationHead(step Step, sctx *StepContext, outcome *StepOutcome, approvedHead, currentHead string, changed []string) error {
	if err := e.db.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, currentHead); err != nil {
		return fmt.Errorf("carry review approval to the corrected documentation head: %w", err)
	}
	sctx.Run.ReviewApprovedHeadSHA = &currentHead
	sctx.Log(fmt.Sprintf(
		"documentation_head_recheck: %s advanced HEAD from %s to %s with documentation-and-records changes only (%s); review approval carries forward to the corrected head and Review is not repeated",
		step.Name(), ShortObjectID(approvedHead), ShortObjectID(currentHead), strings.Join(changed, ", "),
	))

	// Restarting needs a strictly earlier step, so an advance produced by Test
	// itself cannot restart at Test. It does not need to: Test runs its own
	// configured command after its fix round, and Document and Lint still run
	// after it in this same pass, so the corrected head already receives
	// everything the restart exists to provide.
	if step.Name() == types.StepTest {
		sctx.Log("documentation_head_recheck: the advance came from Test itself; its own command already ran against the corrected head and Document and Lint still follow, so no restart is needed")
		return nil
	}
	outcome.RestartFrom = types.StepTest
	outcome.RestartReason = RestartReasonDocumentationHeadRecheck
	sctx.Log("documentation_head_recheck: restarting at Test so the project's test command and lint run against the corrected head before publication")
	return nil
}

// enforceRestartBound refuses another automatic restart once that reason's
// budget is spent, so a step that keeps rewriting the head cannot loop forever.
// Each reason is counted against its own target step and its own bound: a
// documentation recheck must not consume the Review rereview budget, and vice
// versa.
func (e *Executor) enforceRestartBound(runID string, outcome *StepOutcome) error {
	if outcome == nil {
		return nil
	}
	switch outcome.RestartReason {
	case RestartReasonFinalHeadRereview:
		return e.enforceRestartReasonBound(runID, types.StepReview, RestartReasonFinalHeadRereview, maxFinalHeadRereviews,
			"final_head_rereview_limit_exceeded: final HEAD changed after Review %d times; refusing another validation loop")
	case RestartReasonDocumentationHeadRecheck:
		return e.enforceRestartReasonBound(runID, types.StepTest, RestartReasonDocumentationHeadRecheck, maxDocumentationHeadRechecks,
			"documentation_head_recheck_limit_exceeded: documentation-only corrections advanced HEAD after Review %d times; refusing another validation loop")
	default:
		return nil
	}
}

// enforceRestartReasonBound counts the rounds target already started because of
// reason and refuses once budget is spent.
func (e *Executor) enforceRestartReasonBound(runID string, target types.StepName, reason RestartReason, budget int, limitFormat string) error {
	steps, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return fmt.Errorf("count %s restarts: %w", reason, err)
	}
	count := 0
	for _, step := range steps {
		// Count only rounds of the restart TARGET that actually started because
		// of this reason. A requesting step can park for a finding and execute
		// more fix rounds before the restart is taken, so counting request
		// rounds would spend the budget before a restart really happened.
		if step.StepName != target {
			continue
		}
		rounds, err := e.db.GetRoundsByStep(step.ID)
		if err != nil {
			return fmt.Errorf("count %s restarts for %s: %w", reason, step.StepName, err)
		}
		for _, round := range rounds {
			if round.Trigger == string(reason) {
				count++
			}
		}
	}
	if count >= budget {
		return fmt.Errorf(limitFormat, count)
	}
	return nil
}
