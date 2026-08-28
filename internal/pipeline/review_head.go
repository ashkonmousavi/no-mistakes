package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxFinalHeadRereviews = 3

// ReviewHeadNeedsRereview checks the durable completed-review authority for a
// proposed local head. Exact equality is publication-ready. A descendant is
// preserved but must receive another full Review. Missing, malformed,
// unreachable, backward, or divergent authority fails closed.
func ReviewHeadNeedsRereview(ctx context.Context, database *db.DB, runID, workDir, proposedHead string) (bool, error) {
	run, err := database.GetRun(runID)
	if err != nil {
		return false, fmt.Errorf("load durable review approval before publication: %w", err)
	}
	if run == nil || run.ReviewApprovedHeadSHA == nil || strings.TrimSpace(*run.ReviewApprovedHeadSHA) == "" {
		return false, fmt.Errorf("refusing publication: run has no durably recorded review-approved head")
	}
	approvedHead := strings.TrimSpace(*run.ReviewApprovedHeadSHA)
	if !isFullGitObjectID(approvedHead) {
		return false, fmt.Errorf("refusing publication: durable review-approved head is malformed")
	}
	resolved, err := git.Run(ctx, workDir, "rev-parse", "--verify", approvedHead+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(resolved), approvedHead) {
		return false, fmt.Errorf("refusing publication: durable review-approved head is unreachable")
	}
	proposedHead = strings.TrimSpace(proposedHead)
	if !isFullGitObjectID(proposedHead) {
		return false, fmt.Errorf("refusing publication: proposed head is malformed")
	}
	if strings.EqualFold(proposedHead, approvedHead) {
		return false, nil
	}
	if _, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", approvedHead, proposedHead); err != nil {
		return false, fmt.Errorf("refusing publication: proposed head %s violates continuity with review-approved head %s (it is not an equal or descendant commit)", shortObjectID(proposedHead), shortObjectID(approvedHead))
	}
	return true, nil
}

func isFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func shortObjectID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func isPostReviewMutationStep(name types.StepName) bool {
	switch name {
	case types.StepTest, types.StepDocument, types.StepLint:
		return true
	default:
		return false
	}
}

func (e *Executor) bindPostReviewHead(ctx context.Context, step Step, sctx *StepContext, outcome *StepOutcome) error {
	if outcome == nil || !isPostReviewMutationStep(step.Name()) || outcome.RestartFrom != "" || sctx.Run.ReviewApprovedHeadSHA == nil {
		return nil
	}
	currentHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s step: %w", step.Name(), err)
	}
	needsRereview, err := ReviewHeadNeedsRereview(ctx, e.db, sctx.Run.ID, sctx.WorkDir, currentHead)
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
