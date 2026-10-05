package pipeline

import (
	"crypto/sha256"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PrepareFixContinuation binds a retry to its original selected causes. Routing
// actions may change at a timeout gate; cause identity and human instructions
// cannot. Receipt completion excludes dispatch, never independent validation.
func (sctx *StepContext) PrepareFixContinuation(step types.StepName, requested types.Findings) (types.Findings, error) {
	requested = types.NormalizeFindings(requested, string(step))
	raw, err := types.MarshalFindingsJSON(requested)
	if err != nil {
		return requested, err
	}
	sctx.FixSelectionID = ""
	sctx.CompletedFixSelectionID = ""
	sctx.FixStartingHead = sctx.Run.HeadSHA
	sctx.FixSelectionFindings = raw
	sctx.FixAppliedOrdinals = nil
	progress, err := sctx.DB.FixProgress(sctx.Run.ID)
	if err != nil {
		return requested, err
	}
	p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
	if err != nil {
		return requested, err
	}
	if p != nil && p.Step != string(step) {
		return requested, fmt.Errorf("unfinished %s work at %s %s must be reconciled before %s repair", p.Step, p.Ref, p.Path, step)
	}
	if progress != nil && progress.Step == string(step) && progress.ValidationPending {
		units, err := sctx.DB.GetFixCheckpoints(sctx.Run.ID, string(step), progress.Selection)
		if err != nil {
			return requested, err
		}
		if err = ValidateFixCheckpointRefs(sctx.Ctx, sctx.DB, sctx.Run.ID, sctx.WorkDir); err != nil {
			return requested, err
		}
		original, err := types.ParseFindingsJSON(units[0].SelectionJSON)
		if err != nil {
			return requested, err
		}
		original = types.NormalizeFindings(original, string(step))
		if len(original.Items) != units[0].Total {
			return requested, fmt.Errorf("saved repair scope is incomplete; retain %s", sctx.WorkDir)
		}
		applied := map[int]bool{}
		for _, u := range units {
			if u.State == "applied" {
				applied[u.Ordinal] = true
			}
		}
		for i, f := range original.Items {
			found := false
			for _, candidate := range requested.Items {
				if repairScope(f) == repairScope(candidate) {
					found = true
				}
			}
			if !applied[i+1] && !found {
				return requested, fmt.Errorf("unfinished cause %s was not selected with its original instructions", f.ID)
			}
		}
		for _, candidate := range requested.Items {
			found := false
			for _, f := range original.Items {
				if repairScope(f) == repairScope(candidate) {
					found = true
				}
			}
			if !found {
				return requested, fmt.Errorf("selected cause %s differs from saved repair scope; inspect its preserved refs", candidate.ID)
			}
		}
		sctx.FixStartingHead = units[0].ParentHead
		sctx.FixSelectionID = progress.Selection
		sctx.FixSelectionFindings = units[0].SelectionJSON
		sctx.FixAppliedOrdinals = applied
		sctx.CIFixSnapshotJSON = units[0].CISnapshotJSON
		requested = original
	}
	if p != nil {
		if p.RunID != sctx.Run.ID || p.RepoID != sctx.Run.RepoID || p.Branch != sctx.Run.Branch || sctx.Run.CustodyReturnedAt != nil {
			return requested, fmt.Errorf("rescue ownership mismatch at %s %s", p.Ref, p.Path)
		}
		if sctx.FixSelectionID != "" && p.Selection != sctx.FixSelectionID {
			return requested, fmt.Errorf("rescue selection does not match saved repair %s", p.Ref)
		}
		if sctx.FixSelectionID == "" && p.Selection != fmt.Sprintf("%x", sha256.Sum256([]byte(sctx.PreviousFindings))) {
			return requested, fmt.Errorf("rescue instructions changed at %s", p.Ref)
		}
		if err = custody.RestorePartialWork(sctx.Ctx, sctx.WorkDir, p); err != nil {
			return requested, err
		}
		p.State = "consumed"
		if err = sctx.DB.SaveWorkRescue(p); err != nil {
			return requested, err
		}
	}
	return requested, nil
}

// The existing finding content key deliberately excludes user instructions for
// review carry. Restore is stricter: it preserves those instructions too.
func repairScope(f types.Finding) types.Finding {
	f.ID = ""
	f.Action = ""
	f.Severity = ""
	f.Source = ""
	return f
}

func (sctx *StepContext) FinishFixValidation(step types.StepName) error {
	return sctx.DB.FinishFixValidation(sctx.Run.ID, string(step), sctx.CompletedFixSelectionID, sctx.Run.HeadSHA)
}

// SavedFixTests returns the exact regression paths captured before prior commits.
func (sctx *StepContext) SavedFixTests(step types.StepName) ([]string, error) {
	if sctx.FixSelectionID == "" {
		return nil, nil
	}
	units, err := sctx.DB.GetFixCheckpoints(sctx.Run.ID, string(step), sctx.FixSelectionID)
	if err != nil {
		return nil, err
	}
	var paths []string
	seen := map[string]bool{}
	for _, u := range units {
		if u.State == "applied" {
			for _, path := range u.NewTests {
				if !seen[path] {
					paths = append(paths, path)
					seen[path] = true
				}
			}
		}
	}
	return paths, nil
}
