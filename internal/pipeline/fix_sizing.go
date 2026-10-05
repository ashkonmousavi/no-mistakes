package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const FixSizingFindingID = "fix-estimate-exceeds-deadline"

type fixSizingError struct {
	finding   string
	estimate  time.Duration
	available time.Duration
	basis     string
}

func (e *fixSizingError) Error() string {
	return fmt.Sprintf("finding %s estimated at %s (%s) exceeds %s available after a 10%% safety margin; fixer was not launched; inspect its scope or adjust the existing invocation budget before explicitly retrying", e.finding, e.estimate, e.basis, e.available)
}

// sizeFixCall uses the same absolute bound as bindAgentDeadline. It neither
// extends a parent deadline nor changes the configured stall/working budgets.
func (sctx *StepContext) sizeFixCall(parent context.Context, timeout, working time.Duration) error {
	if sctx == nil || sctx.CurrentFixUnit == nil {
		return nil
	}
	c := sctx.CurrentFixUnit
	bound := timeout
	if working > timeout {
		bound = working
	}
	if parent != nil {
		if deadline, ok := parent.Deadline(); ok {
			bound = time.Until(deadline)
		}
	}
	available := bound - bound/10
	rate, err := sctx.DB.FixDurationPerSize(sctx.Run.RepoID, c.Step, c.FixAgent)
	if err != nil {
		return fmt.Errorf("read repair sizing evidence before launch: %w", err)
	}
	basis := "measured successful repair maximum for this repository, step and adapter"
	unit := time.Duration(rate) * time.Millisecond
	if rate == 0 {
		// With no measured repair yet, reserve a quarter of the call's bound
		// per 1024 runes, capped at five minutes. This is a size heuristic,
		// explicitly unmeasured, and works with short isolated fixture budgets.
		unit = min(5*time.Minute, max(time.Duration(0), bound/4))
		basis = "unmeasured size estimate"
	}
	const maxDuration = time.Duration(1<<63 - 1)
	estimate := maxDuration
	if rate >= 0 && rate <= int64(maxDuration/time.Millisecond) && c.FixSize > 0 && unit <= maxDuration/time.Duration(c.FixSize) {
		estimate = unit * time.Duration(c.FixSize)
	}
	if sctx.Log != nil {
		sctx.Log(fmt.Sprintf("fix estimate for %s: %s (%s; %d size units), available %s after a 10%% safety margin", c.FindingID, estimate, basis, c.FixSize, available))
	}
	if available <= 0 || estimate > available {
		return &fixSizingError{finding: c.FindingID, estimate: estimate, available: available, basis: basis}
	}
	return nil
}

func HasFixSizingRefusal(raw string) bool {
	return hasFindingID(raw, FixSizingFindingID)
}

// FixSizingOutcome preserves the original selected and deferred findings and
// completed checkpoints. The synthetic warning is a scheduling refusal, never
// another repair cause or evidence that any original finding was resolved.
func FixSizingOutcome(err error, sctx *StepContext) *StepOutcome {
	var sizing *fixSizingError
	if !errors.As(err, &sizing) {
		return nil
	}
	findings, _ := types.ParseFindingsJSON(mergeFindingsJSON(sctx.PreviousFindings, sctx.DeferredFindings))
	findings.Items = append(findings.Items, types.Finding{ID: FixSizingFindingID, Severity: "warning", Action: types.ActionAskUser, Description: sizing.Error()})
	findings.Summary = "Repair parked before launch: estimated work exceeds the invocation deadline"
	raw, _ := types.MarshalFindingsJSON(findings)
	return &StepOutcome{NeedsApproval: true, Findings: raw}
}

// Scheduling warnings are regenerated at every preflight. They are not defects
// for a fixer to edit or for positive file coverage to verify away.
func DropFixSizingRefusal(raw string) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil || !HasFixSizingRefusal(raw) {
		return raw
	}
	kept := findings.Items[:0]
	for _, f := range findings.Items {
		if f.ID != FixSizingFindingID {
			kept = append(kept, f)
		}
	}
	findings.Items = kept
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw
	}
	return encoded
}
