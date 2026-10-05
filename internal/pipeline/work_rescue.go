package pipeline

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (sctx *StepContext) beginAgentRescue(opts agent.RunOpts) (*types.PartialWork, error) {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || sctx.WorkDir == "" {
		return nil, nil
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	step := "agent"
	if sctx.StepResultID != "" {
		s, e := sctx.DB.GetStepResult(sctx.StepResultID)
		if e != nil {
			return nil, e
		}
		if s != nil {
			step = string(s.StepName)
		}
	} else if purpose := strings.Split(opts.Purpose, "-")[0]; purpose != "" {
		step = purpose
	}
	selection := sha256.Sum256([]byte(sctx.PreviousFindings))
	selectionID := fmt.Sprintf("%x", selection)
	if sctx.FixSelectionID != "" {
		selectionID = sctx.FixSelectionID
	}
	return sctx.DB.BeginWorkRescue(sctx.Run, step, selectionID, head, sctx.WorkDir)
}

func (sctx *StepContext) finishAgentRescue(p *types.PartialWork, invocationErr error, activity *agentActivity) error {
	if p == nil {
		return nil
	}
	if invocationErr == nil {
		p.State = "settled"
		p.Reason = ""
		return sctx.DB.SaveWorkRescue(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if needed, err := custody.WorkNeedsRescue(ctx, sctx.WorkDir); err == nil && !needed {
		p.State = "settled"
		p.Reason = ""
		return sctx.DB.SaveWorkRescue(p)
	}
	activity.mu.Lock()
	quiescent := activity.launched && activity.exited
	activity.mu.Unlock()
	return PreserveRunWork(ctx, sctx.DB, sctx.Run, sctx.WorkDir, p, invocationErr.Error(), quiescent)
}

// PreserveRunWork is the shared stopped-invocation and final-deletion backstop.
// A saved ref is local evidence. A retained state refuses removal and editing.
func PreserveRunWork(ctx context.Context, d *db.DB, run *db.Run, dir string, p *types.PartialWork, reason string, quiescent bool) error {
	if run == nil {
		return fmt.Errorf("cannot preserve worktree %s without a run", dir)
	}
	if p == nil {
		var e error
		head, e := git.HeadSHA(ctx, dir)
		if e != nil {
			return e
		}
		p, e = d.BeginWorkRescue(run, "cleanup", "", head, dir)
		if e != nil {
			return e
		}
	}
	// Scoped sweep uses the existing process owner, and verifies every victim
	// has exited before any content can be called a stable snapshot.
	sweepErr := procreap.Quiesce(ctx, procreap.Options{Worktrees: []procreap.Worktree{{Dir: dir, RepoID: run.RepoID, RunID: run.ID}}, Scopes: []string{dir}})
	if sweepErr != nil {
		quiescent = false
	}
	snapshot, e := custody.PreservePartialWork(ctx, dir, p.RunID, p.Step, p.StopID)
	if snapshot != nil {
		p.Ref = snapshot.Ref
		p.SHA = snapshot.SHA
		p.IndexSHA = snapshot.IndexSHA
		p.State = snapshot.State
		p.Reason = snapshot.Reason
		if snapshot.ParentHead != p.ParentHead {
			e = errors.Join(e, fmt.Errorf("worktree HEAD changed during invocation; retain for continuity reconciliation"))
		}
	}
	if !quiescent {
		e = errors.Join(e, errors.New("writer shutdown could not be verified"), sweepErr)
	}
	if e != nil {
		p.State = "retained"
		p.Reason = e.Error()
	}
	if p.State == "saved" || p.State == "settled" {
		p.Path = ""
	} else {
		p.Path = dir
	}
	if saveErr := d.SaveWorkRescue(p); saveErr != nil {
		return errors.Join(e, fmt.Errorf("persist partial work; retained %s: %w", dir, saveErr))
	}
	if p.State != "saved" && p.State != "settled" {
		return errors.Join(e, fmt.Errorf("partial work retained %s: %s (stop: %s)", dir, p.Reason, reason))
	}
	return nil
}
