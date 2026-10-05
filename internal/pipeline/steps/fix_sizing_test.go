package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Seed a successful measured unit through the ordinary checkpoint store, not
// an estimator mock. Legacy readers ignore the new optional payload fields.
func seedFixTiming(t *testing.T, sctx *pipeline.StepContext, step types.StepName, duration time.Duration) {
	t.Helper()
	run, err := sctx.DB.InsertRun(sctx.Run.RepoID, "refs/heads/timing", sctx.Run.HeadSHA, sctx.Run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"RunID": run.ID, "Step": string(step), "Selection": "timing", "Ordinal": 1, "Total": 1,
		"FindingID": "measured", "FindingDigest": "measured", "ParentHead": run.HeadSHA,
		"FixDurationMS": duration.Milliseconds(), "FixSize": 1, "FixAgent": sctx.Agent.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var c db.FixCheckpoint
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if err = sctx.DB.BeginFixCheckpoint(&c); err != nil {
		t.Fatal(err)
	}
	c.State, c.AppliedHead, c.Ref = "applied", run.HeadSHA, "timing-ref"
	if err = sctx.DB.ApplyFixCheckpoint(&c); err != nil {
		t.Fatal(err)
	}
}

func TestFixSizingOversizedFindingParksBeforeLaunch(t *testing.T) {
	for _, step := range []pipeline.Step{&ReviewStep{}, &TestStep{}} {
		t.Run(string(step.Name()), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return &agent.Result{Output: []byte(`{"summary":"should never launch"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			bindStepResult(t, sctx, step.Name())
			sctx.Fixing = true
			sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","action":"auto-fix","file":"feature.txt","description":"one cause"}]}`
			seedFixTiming(t, sctx, step.Name(), 29*time.Minute)
			outcome, err := step.Execute(sctx)
			if err != nil || outcome == nil || !outcome.NeedsApproval || calls != 0 {
				t.Fatalf("oversized finding launched: calls=%d outcome=%+v error=%v", calls, outcome, err)
			}
			for _, want := range []string{"fix-estimate-exceeds-deadline", "29m0s", "27m0s", "measured", `"id":"A"`} {
				if !strings.Contains(outcome.Findings, want) {
					t.Errorf("park omitted %q: %s", want, outcome.Findings)
				}
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("park advanced head: %s", got)
			}
		})
	}
}

func TestFixSizingSmallFindingLaunchesOnceAndRecordsMeasurement(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	fixCalls := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "review-fix" {
			fixCalls++
			if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("fixed"), 0644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: []byte(`{"summary":"repair cause"}`)}, nil
		}
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"reviewed","risk_level":"low","risk_rationale":"fixture","risk_scope":"source-or-external","reviewed_paths":["feature.txt","fixed.txt"]}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","action":"auto-fix","file":"feature.txt","description":"small cause"}]}`
	seedFixTiming(t, sctx, types.StepReview, 8*time.Minute)
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome.NeedsApproval || fixCalls != 1 {
		t.Fatalf("small repair: calls=%d outcome=%+v error=%v", fixCalls, outcome, err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "fix estimate for A: 8m0s") {
		t.Fatalf("measurement not used before launch: %v", logs)
	}
	units, err := sctx.DB.GetFixCheckpoints(sctx.Run.ID, "review", "")
	if err != nil || len(units) != 1 {
		t.Fatalf("receipts=%+v error=%v", units, err)
	}
	raw, _ := json.Marshal(units[0])
	var recorded map[string]any
	_ = json.Unmarshal(raw, &recorded)
	if duration, ok := recorded["FixDurationMS"].(float64); !ok || duration < 1 {
		t.Fatalf("successful unit lost measured timing: %s", raw)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("one finding did not create one commit: %s", got)
	}
}
