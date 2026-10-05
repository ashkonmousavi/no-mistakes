package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// InheritedFixSelection returns only the source's unfinished causes. These are
// explicit continuation context, never evidence that a new reviewer's finding
// with the same positional ID has been fixed.
func InheritedFixSelection(d *db.DB, p *types.PartialWork) (types.Findings, error) {
	units, err := d.GetFixCheckpoints(p.RunID, p.Step, p.Selection)
	if err != nil {
		return types.Findings{}, err
	}
	if len(units) == 0 {
		return types.Findings{}, fmt.Errorf("rescue %s has no verified selected repair scope", p.Ref)
	}
	f, err := types.ParseFindingsJSON(units[0].SelectionJSON)
	if err != nil {
		return f, err
	}
	f = types.NormalizeFindings(f, p.Step)
	if len(f.Items) != units[0].Total {
		return f, fmt.Errorf("rescue %s has incomplete selected scope", p.Ref)
	}
	applied := map[int]bool{}
	for _, c := range units {
		if c.State == "applied" {
			applied[c.Ordinal] = true
		}
	}
	remaining := types.FindingsMetadata(f)
	for i, item := range f.Items {
		if !applied[i+1] {
			remaining.Items = append(remaining.Items, item)
		}
	}
	return remaining, nil
}

func ValidateInheritedWork(ctx context.Context, d *db.DB, run *db.Run, dir string, p *types.PartialWork) error {
	if p.RepoID != run.RepoID || p.Branch != run.Branch || p.ConsumedBy != run.ID {
		return fmt.Errorf("inherited rescue ownership mismatch at %s", p.Ref)
	}
	source, err := d.GetRun(p.RunID)
	if err != nil {
		return err
	}
	if source == nil || source.CustodyReturnedAt != nil {
		return fmt.Errorf("inherited rescue source custody unavailable at %s", p.Ref)
	}
	text := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	if text(source.Intent) != text(run.Intent) {
		return fmt.Errorf("inherited rescue task intent changed at %s", p.Ref)
	}
	if err = custody.ValidatePartialWork(ctx, dir, p); err != nil {
		return err
	}
	return ValidateFixCheckpointRefs(ctx, d, p.RunID, dir)
}

// CompleteInheritedWork consumes only actual local applied receipts at the
// source's scoped identity. Current-head Review/Test/CI authority stays with
// their existing gates. A skip or approval cannot manufacture these receipts.
func CompleteInheritedWork(ctx context.Context, d *db.DB, run *db.Run, dir string, p *types.PartialWork) error {
	if err := ValidateInheritedWork(ctx, d, run, dir, p); err != nil {
		return err
	}
	required, err := InheritedFixSelection(d, p)
	if err != nil {
		return err
	}
	current, err := d.GetFixCheckpoints(run.ID, p.Step, "")
	if err != nil {
		return err
	}
	for _, want := range required.Items {
		found := false
		for _, c := range current {
			if c.State != "applied" {
				continue
			}
			selection, e := types.ParseFindingsJSON(c.SelectionJSON)
			if e != nil {
				return e
			}
			if c.Ordinal < 1 || c.Ordinal > len(selection.Items) {
				return fmt.Errorf("invalid inherited unit ordinal")
			}
			if repairScope(want) == repairScope(selection.Items[c.Ordinal-1]) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unfinished inherited cause %s at %s has no applied receipt; cannot publish or discard it", want.ID, p.Ref)
		}
	}
	if err = ValidateFixCheckpointRefs(ctx, d, run.ID, dir); err != nil {
		return err
	}
	p.ConsumptionCompleted = true
	return d.SaveWorkRescue(p)
}

func (sctx *StepContext) BindInheritedWork() error {
	p, err := sctx.DB.InheritedWorkRescue(sctx.Run.ID)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	if err = ValidateInheritedWork(sctx.Ctx, sctx.DB, sctx.Run, sctx.WorkDir, p); err != nil {
		return err
	}
	units, err := sctx.DB.GetFixCheckpoints(p.RunID, p.Step, p.Selection)
	if err != nil {
		return err
	}
	var context strings.Builder
	fmt.Fprintf(&context, "\n\nSaved repair context from source run %s at parent %s:\n", p.RunID, p.ParentHead)
	for _, c := range units {
		fmt.Fprintf(&context, "- %s: %s (unverified edit receipt), %s\n", c.FindingID, c.State, c.Summary)
	}
	context.WriteString("The restored bytes and source receipts are unfinished context. Independently validate every current finding; reused IDs confer no authority.\n")
	sctx.InheritedRepairContext = context.String()
	if p.Step == string(types.StepCI) && !p.ConsumptionCompleted && len(units) > 0 {
		sctx.CIFixSnapshotJSON = units[0].CISnapshotJSON
	}
	return nil
}
