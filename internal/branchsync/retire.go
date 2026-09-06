package branchsync

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// Retirement records exactly what a custody retirement released, captured
// before the release so the report names the binding that is now gone rather
// than the empty row that replaced it.
type Retirement struct {
	RunID     string
	Branch    string
	RunStatus string
	// Released is false when the run already held nothing: a repeated
	// retirement is a successful no-op, not a refusal.
	Released bool
	// ReleasedHead is the successful-push provenance the run claimed for this
	// branch, empty when the run never published one.
	ReleasedHead       string
	ReleasedRef        string
	ReleasedTargetKind string
	// CustodyStamped is true when this call recorded the custody return; a run
	// already stamped by a guarded recovery keeps its original moment.
	CustodyStamped bool
}

// RetireCustody withdraws a terminal run's claim on its branch without
// adopting, moving, fetching, or pushing any head. It exists for the states
// where guarded recovery cannot finish: a hand rebase and force-push leaves
// blocked_remote_rewritten, whose binding no live remote can ever satisfy
// again, and a preserved head missing from the gate leaves
// blocked_recover_preserved_head_missing with nothing left to import. Both used
// to be dead ends whose only escape was abandoning the branch.
//
// It refuses on anything that is not provably finished: an unknown run, a run
// from another repository, a run that is not terminal, and a run that still
// carries an in-flight push marker or a live push step. Refusal never writes.
//
// This touches only the run's own database row - never a Git ref, the worktree,
// the local gate, or a remote. The run's head_sha, submitted_head_sha and
// terminal head evidence are preserved, so any commits the pipeline made stay
// exactly as reachable (or unreachable) as they were before the call; the
// operator's own Git objects are the authority on what survives. It takes no
// context because it contacts nothing that can block: one local database row.
func (s *Service) RetireCustody(runID string) (Retirement, error) {
	if s == nil || s.DB == nil {
		return Retirement{}, fmt.Errorf("branch synchronization is unavailable")
	}
	if runID == "" {
		return Retirement{}, fmt.Errorf("a run id is required")
	}
	run, err := s.DB.GetRun(runID)
	if err != nil {
		return Retirement{}, fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return Retirement{}, fmt.Errorf("run %q not found", runID)
	}
	if s.Repo == nil || run.RepoID != s.Repo.ID {
		return Retirement{}, fmt.Errorf("run %s does not belong to this repository", runID)
	}
	if !terminalRunStatus(run.Status) {
		return Retirement{}, fmt.Errorf("run %s is still active (%s); drive it to completion or abort it first - no binding or custody record was changed", runID, run.Status)
	}
	// A terminal status alone does not prove no push is in flight: a crash
	// between the push marker and terminalization leaves the marker set, and
	// releasing a binding under a live push is exactly the race this whole
	// subsystem exists to prevent. Fail closed on either marker.
	if run.PushActive || pushStepRunning(s.DB, runID) {
		return Retirement{}, fmt.Errorf("run %s still carries an in-flight push marker; no binding or custody record was changed", runID)
	}

	result := Retirement{
		RunID:              run.ID,
		Branch:             run.Branch,
		RunStatus:          string(run.Status),
		ReleasedHead:       ptr(run.LastPushedSHA),
		ReleasedRef:        ptr(run.PushRef),
		ReleasedTargetKind: ptr(run.PushTargetKind),
	}
	if alreadyRetired(run) {
		return result, nil
	}
	if err := s.DB.ReleaseRunPushBindingAndCustody(run.ID); err != nil {
		return Retirement{}, err
	}
	result.Released = true
	result.CustodyStamped = run.CustodyReturnedAt == nil
	return result, nil
}

// alreadyRetired reports a run that has nothing left to release: custody is
// stamped and no field of the push binding still claims the branch.
func alreadyRetired(run *db.Run) bool {
	return run.CustodyReturnedAt != nil && run.LastPushedSHA == nil && run.LastPushedAt == nil &&
		run.PushRef == nil && run.PushTargetKind == nil && run.PushTargetFingerprint == nil
}
