package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFixSizingLegacyPendingUnitUsesCurrentFindingSize(t *testing.T) {
	d, _, run, _ := setupTest(t)
	finding := types.Finding{ID: "A", Description: "original cause"}
	raw, _ := json.Marshal(finding)
	selection := `{"findings":[{"id":"A","description":"original cause"}]}`
	c := &db.FixCheckpoint{RunID: run.ID, Step: "review", Selection: "saved-selection", Ordinal: 1, Total: 1,
		FindingID: finding.ID, FindingDigest: fmt.Sprintf("%x", sha256.Sum256(raw)), ParentHead: run.HeadSHA, SelectionJSON: selection}
	if err := d.BeginFixCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{DB: d, Run: run, FixSelectionID: "saved-selection", FixSelectionFindings: selection}
	if err := sctx.BeginFixUnit(types.StepReview, finding, 1, 1); err != nil {
		t.Fatal(err)
	}
	if sctx.CurrentFixUnit.FixSize != 1 {
		t.Fatalf("legacy pending unit has no usable size: %+v", sctx.CurrentFixUnit)
	}
}

func TestFixProgressAuthorityCheckpointRefMustStillMatch(t *testing.T) {
	for _, fault := range []string{"moved", "symbolic", "ref without receipt"} {
		t.Run(fault, func(t *testing.T) {
			d, _, run, _ := setupTest(t)
			dir := t.TempDir()
			initGitRepo(t, dir)
			head, err := git.HeadSHA(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			run.HeadSHA = head
			if err = d.UpdateRunHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, WorkDir: dir, PreviousFindings: `{"findings":[{"id":"A","description":"cause"}]}`}
			if err = sctx.BeginFixUnit(types.StepReview, types.Finding{ID: "A", Description: "cause"}, 1, 1); err != nil {
				t.Fatal(err)
			}
			if err = sctx.RecordFixUnitHead(head); err != nil {
				t.Fatal(err)
			}
			c := sctx.CurrentFixUnit
			if err = ValidateFixCheckpointRefs(sctx.Ctx, d, run.ID, dir); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "moved":
				_, err = git.Run(sctx.Ctx, dir, "update-ref", "-d", c.Ref, head)
			case "symbolic":
				_, err = git.Run(sctx.Ctx, dir, "symbolic-ref", c.Ref, "refs/heads/main")
			case "ref without receipt":
				pending := &db.FixCheckpoint{RunID: run.ID, Step: "test", Selection: c.Selection, Ordinal: 1, Total: 1, FindingID: "B", FindingDigest: "b", ParentHead: head}
				if err = d.BeginFixCheckpoint(pending); err != nil {
					t.Fatal(err)
				}
				_, err = git.Run(sctx.Ctx, dir, "update-ref", "refs/no-mistakes/fix/"+run.ID+"/test/"+c.Selection+"/1", head)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateFixCheckpointRefs(sctx.Ctx, d, run.ID, dir); err == nil {
				t.Fatal("invalid private checkpoint accepted")
			}
			stored, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.ReviewApprovedHeadSHA != nil {
				t.Fatal("receipt conferred approval")
			}
		})
	}
}
