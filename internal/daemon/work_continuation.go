package daemon

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type rerunRescueKey struct{}

func (m *RunManager) matchingRerunRescue(ctx context.Context, source *db.Run, head, intent string) (*types.PartialWork, error) {
	p, err := m.db.LatestWorkRescue(source.ID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	refuse := func(reason string) (*types.PartialWork, error) {
		return nil, fmt.Errorf("refusing continuation of %s %s: %s", p.Ref, p.Path, reason)
	}
	if p.State != "saved" || p.Version != 1 {
		return refuse("work is retained or its format is unknown")
	}
	if p.RunID != source.ID || p.RepoID != source.RepoID || p.Branch != source.Branch || source.CustodyReturnedAt != nil || !source.Status.Terminal() {
		return refuse("source branch/run/custody no longer matches")
	}
	original := ""
	if source.Intent != nil {
		original = *source.Intent
	}
	if original != intent {
		return refuse("task intent changed")
	}
	if p.ParentHead != head || source.HeadSHA != head {
		return refuse("exact saved parent does not match selected committed progress")
	}
	if p.Step != "review" && p.Step != "test" && p.Step != "ci" {
		return refuse("no matching selected repair owner; keep the snapshot for reconciliation")
	}
	gate := m.paths.RepoDir(source.RepoID)
	if err = custody.ValidatePartialWork(ctx, gate, p); err != nil {
		return refuse(err.Error())
	}
	if err = pipeline.ValidateFixCheckpointRefs(ctx, m.db, source.ID, gate); err != nil {
		return refuse(err.Error())
	}
	remaining, err := pipeline.InheritedFixSelection(m.db, p)
	if err != nil {
		return refuse(err.Error())
	}
	if len(remaining.Items) == 0 {
		return refuse("only validation remained but its turn changed files; reconcile those bytes before retry")
	}
	return p, nil
}

// A rerun's submitted head may itself contain inherited, unpublished fixes.
// Resolve the publication baseline through verified consumption bindings rather
// than treating that submitted head as proof of a remote publication.
func (m *RunManager) resolveContinuationHead(ctx context.Context, gate, branch string, latest *db.Run) (string, error) {
	if err := pipeline.ValidateFixCheckpointRefs(ctx, m.db, latest.ID, gate); err != nil {
		return "", fmt.Errorf("refusing saved repair continuation: %w", err)
	}
	view := *latest
	source := latest
	seen := map[string]bool{}
	for source.LastPushedSHA == nil {
		if seen[source.ID] {
			return "", fmt.Errorf("cyclic saved-work source for %s", latest.ID)
		}
		seen[source.ID] = true
		inherited, err := m.db.InheritedWorkRescue(source.ID)
		if err != nil {
			return "", err
		}
		if inherited == nil {
			view.SubmittedHeadSHA = source.SubmittedHeadSHA
			break
		}
		if err = pipeline.ValidateInheritedWork(ctx, m.db, source, gate, inherited); err != nil {
			return "", err
		}
		source, err = m.db.GetRun(inherited.RunID)
		if err != nil {
			return "", err
		}
		if source == nil {
			return "", fmt.Errorf("saved source run missing at %s", inherited.Ref)
		}
	}
	if source.LastPushedSHA != nil {
		view.SubmittedHeadSHA = source.LastPushedSHA
	}
	if source.ID != latest.ID {
		actual, err := git.Run(ctx, gate, "rev-parse", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			return "", err
		}
		if view.SubmittedHeadSHA == nil || actual != *view.SubmittedHeadSHA && actual != latest.HeadSHA {
			return "", fmt.Errorf("refusing saved continuation: branch tip changed from its publication baseline; inspect preserved run %s", latest.ID)
		}
	}
	return resolveRerunHead(ctx, gate, branch, &view)
}
