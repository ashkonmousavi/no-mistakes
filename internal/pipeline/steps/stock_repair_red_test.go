package steps

import (
	"context"
	"errors"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"strings"
	"testing"
)

func TestFixSizingUnmeasuredOversizedCauseDoesNotLaunch(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return nil, errors.New("oversized fixer launched")
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","action":"auto-fix","description":"` + strings.Repeat("oversized cause ", 400) + `"}]}`
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || calls != 0 || !strings.Contains(outcome.Findings, "fix-estimate-exceeds-deadline") {
		t.Fatalf("oversized unmeasured launch: calls=%d outcome=%+v error=%v", calls, outcome, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("refusal changed head: %s", got)
	}
}
