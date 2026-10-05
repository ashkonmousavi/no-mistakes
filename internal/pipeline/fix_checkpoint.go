package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os/exec"
	"unicode/utf8"
)

func (sctx *StepContext) BeginFixUnit(step types.StepName, finding types.Finding, ordinal, total int) error {
	selection := sctx.FixSelectionID
	if selection == "" {
		roundID := ""
		if sctx.StepResultID != "" {
			rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
			if err != nil {
				return err
			}
			for _, r := range rounds {
				if r.SelectedFindingIDs != nil {
					roundID = r.ID
				}
			}
		}
		selection = fmt.Sprintf("%x", sha256.Sum256([]byte(roundID+"\x00"+sctx.PreviousFindings)))
		sctx.FixSelectionID = selection
	}
	raw, err := json.Marshal(finding)
	if err != nil {
		return err
	}
	selectionJSON := sctx.FixSelectionFindings
	if selectionJSON == "" {
		selectionJSON = sctx.PreviousFindings
	}
	c := &db.FixCheckpoint{RunID: sctx.Run.ID, Step: string(step), StepResultID: sctx.StepResultID, Selection: selection, Ordinal: ordinal, Total: total, FindingID: finding.ID, FindingDigest: fmt.Sprintf("%x", sha256.Sum256(raw)), ParentHead: sctx.Run.HeadSHA, SelectionJSON: selectionJSON, CISnapshotJSON: sctx.CIFixSnapshotJSON}
	c.FixSize = (utf8.RuneCount(raw) + 1023) / 1024
	if sctx.Agent != nil {
		c.FixAgent = sctx.Agent.Name()
	}
	if err = sctx.DB.BeginFixCheckpoint(c); err != nil {
		return err
	}
	if c.State == "applied" {
		return fmt.Errorf("saved repair unit %s needs matching continuation", c.FindingID)
	}
	sctx.CurrentFixUnit = c
	return nil
}

// ValidateFixCheckpointRefs refuses cleanup when receipts and their exact
// private non-symbolic anchors disagree, including a ref-first storage crash.
func ValidateFixCheckpointRefs(ctx context.Context, d *db.DB, runID, dir string) error {
	units, err := d.GetFixCheckpoints(runID, "", "")
	if err != nil {
		return err
	}
	for _, c := range units {
		ref := fmt.Sprintf("refs/no-mistakes/fix/%s/%s/%s/%d", c.RunID, c.Step, c.Selection, c.Ordinal)
		if c.State != "applied" {
			_, e := git.Run(ctx, dir, "show-ref", "--verify", "--quiet", ref)
			if e == nil {
				return fmt.Errorf("checkpoint %s has no applied receipt", ref)
			}
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 1 {
				return e
			}
			continue
		}
		if c.Ref != ref {
			return fmt.Errorf("cross-bound checkpoint ref %s", c.Ref)
		}
		if _, e := git.Run(ctx, dir, "symbolic-ref", "-q", ref); e == nil {
			return fmt.Errorf("symbolic checkpoint ref %s", ref)
		}
		actual, e := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
		if e != nil {
			return e
		}
		if actual != c.AppliedHead {
			return fmt.Errorf("checkpoint ref %s moved", ref)
		}
	}
	return nil
}

// RecordFixUnitHead anchors first, then atomically records head and receipt.
func (sctx *StepContext) RecordFixUnitHead(head string) error {
	c := sctx.CurrentFixUnit
	if c == nil {
		return fmt.Errorf("no current repair checkpoint")
	}
	c.Ref = fmt.Sprintf("refs/no-mistakes/fix/%s/%s/%s/%d", c.RunID, c.Step, c.Selection, c.Ordinal)
	if err := custody.PreserveRecoveryAnchor(sctx.Ctx, sctx.WorkDir, c.Ref, head); err != nil {
		return err
	}
	c.AppliedHead = head
	c.State = "applied"
	if err := sctx.DB.ApplyFixCheckpoint(c); err != nil {
		c.State = "pending"
		return err
	}
	sctx.Run.HeadSHA = head
	return nil
}
