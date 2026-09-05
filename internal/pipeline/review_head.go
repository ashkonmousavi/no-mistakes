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

// bindPostReviewHead sends the run back to Review when a post-review step
// advanced the head. The commit is preserved, never discarded: only the
// authority to publish it is withheld until a Review round has actually read
// it.
func (e *Executor) bindPostReviewHead(ctx context.Context, step Step, sctx *StepContext, outcome *StepOutcome) error {
	if outcome == nil || !isPostReviewMutationStep(step.Name()) || outcome.RestartFrom != "" || sctx.Run.ReviewApprovedHeadSHA == nil {
		return nil
	}
	currentHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s step: %w", step.Name(), err)
	}
	needsRereview, err := ReviewHeadNeedsRereview(e.db, sctx.Run.ID, currentHead, WorktreeGitRunner(ctx, sctx.WorkDir))
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
	outcome.RestartFrom = types.StepReview
	outcome.RestartReason = RestartReasonFinalHeadRereview
	sctx.Log("final_head_rereview: pipeline-authored changes advanced HEAD after review; restarting at Review before publication")
	return nil
}

// enforceRestartBound refuses another final-head rereview once the budget is
// spent, so a step that keeps rewriting the head cannot loop forever.
func (e *Executor) enforceRestartBound(runID string, outcome *StepOutcome) error {
	if outcome == nil || outcome.RestartReason != RestartReasonFinalHeadRereview {
		return nil
	}
	steps, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return fmt.Errorf("count final-head rereviews: %w", err)
	}
	count := 0
	for _, step := range steps {
		// Count only Review rounds that actually started because of this reason.
		// A requesting step can park for a finding and execute more fix rounds
		// before the restart is taken, so counting request rounds would spend the
		// budget before a rereview really happened.
		if step.StepName != types.StepReview {
			continue
		}
		rounds, err := e.db.GetRoundsByStep(step.ID)
		if err != nil {
			return fmt.Errorf("count final-head rereviews for %s: %w", step.StepName, err)
		}
		for _, round := range rounds {
			if round.Trigger == string(RestartReasonFinalHeadRereview) {
				count++
			}
		}
	}
	if count >= maxFinalHeadRereviews {
		return fmt.Errorf("final_head_rereview_limit_exceeded: final HEAD changed after Review %d times; refusing another validation loop", count)
	}
	return nil
}
