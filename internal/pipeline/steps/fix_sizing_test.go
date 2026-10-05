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

func TestFixSizingCIParksWithoutLaunchOrPublish(t *testing.T) {
	f := newCIRepairFixture(t, false, func(string) { t.Fatal("oversized CI fixer launched") })
	bindStepResult(t, f.sctx, types.StepCI)
	seedFixTiming(t, f.sctx, types.StepCI, 29*time.Minute)
	outcome, err := f.run(t)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "fix-estimate-exceeds-deadline") {
		t.Fatalf("CI sizing did not park: %+v %v", outcome, err)
	}
	if got := gitCmd(t, f.dir, "ls-remote", "origin", "refs/heads/feature"); !strings.Contains(got, f.headSHA) {
		t.Fatalf("oversized CI repair published: %s", got)
	}
}

func TestFixSizingHonorsAbsoluteBudgetAndInheritedDeadline(t *testing.T) {
	for _, tc := range []struct {
		name       string
		absolute   time.Duration
		parent     time.Duration
		wantLaunch bool
	}{
		{"absolute_bound", 0, 0, false},
		{"larger_absolute_budget", 30 * time.Minute, 0, true},
		{"absolute_budget_cannot_extend_parent", 30 * time.Minute, 5 * time.Minute, false},
		{"parent_owns_bound", 0, 40 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, context.Canceled
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			bindStepResult(t, sctx, types.StepReview)
			sctx.Fixing = true
			sctx.Config.AgentTimeout = 10 * time.Minute
			// Current fork has one absolute budget, without the newer activity extension.
			if tc.absolute > 0 {
				sctx.Config.AgentTimeout = tc.absolute
			}
			sctx.PreviousFindings = `{"findings":[{"id":"A","description":"one cause"}]}`
			seedFixTiming(t, sctx, types.StepReview, 20*time.Minute)
			if tc.parent > 0 {
				ctx, cancel := context.WithTimeout(sctx.Ctx, tc.parent)
				defer cancel()
				sctx.Ctx = ctx
			}
			finding := types.Finding{ID: "A", Description: "one cause"}
			if err := sctx.BeginFixUnit(types.StepReview, finding, 1, 1); err != nil {
				t.Fatal(err)
			}
			_, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{CWD: dir, Prompt: "repair cause A"})
			outcome := pipeline.FixSizingOutcome(err, sctx)
			if outcome != nil {
				err = nil
			}
			if tc.wantLaunch {
				if calls != 1 || err == nil {
					t.Fatalf("fitting repair was not launched: calls=%d outcome=%+v error=%v", calls, outcome, err)
				}
			} else if calls != 0 || err != nil || outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("effective bound was ignored: calls=%d outcome=%+v error=%v", calls, outcome, err)
			}
		})
	}
}

func TestFixSizingColdOversizedFindingUsesHonestSizeEstimate(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("oversized cold repair launched")
		return nil, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"large","description":"` + strings.Repeat("x", 8192) + `"}]}`
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "unmeasured size estimate") {
		t.Fatalf("cold sizing did not park honestly: %+v %v", outcome, err)
	}
}

func TestFixSizingResumeKeepsCompletedCommitAndRechecksUnfinishedCause(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := map[string]int{}
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "review-fix" {
			id := "A"
			if strings.Contains(opts.Prompt, "Current repair finding ID: B") {
				id = "B"
			}
			calls[id]++
			if err := os.WriteFile(filepath.Join(dir, id+".txt"), []byte("fixed"), 0644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: []byte(`{"summary":"repair cause"}`)}, nil
		}
		if opts.Purpose == "review-fix-verification" {
			return &agent.Result{Output: []byte(`{"summary":"focused checks passed"}`)}, nil
		}
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"reviewed","risk_level":"low","risk_rationale":"fixture","risk_scope":"source-or-external","reviewed_paths":["feature.txt","A.txt","B.txt"]}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"small cause"},{"id":"B","description":"` + strings.Repeat("x", 5120) + `"}]}`
	seedFixTiming(t, sctx, types.StepReview, 8*time.Minute)
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || calls["A"] != 1 || calls["B"] != 0 || gitCmd(t, dir, "rev-list", "--count", head+"..HEAD") != "1" {
		t.Fatalf("park lost completed work: calls=%v outcome=%+v error=%v", calls, outcome, err)
	}
	sctx.PreviousFindings = outcome.Findings // includes the scheduling warning, not another repair cause
	sctx.Config.ReviewAgentTimeout = time.Hour
	outcome, err = (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome.NeedsApproval || calls["A"] != 1 || calls["B"] != 1 || gitCmd(t, dir, "rev-list", "--count", head+"..HEAD") != "2" {
		t.Fatalf("retry replayed repairs or failed to validate: calls=%v outcome=%+v error=%v", calls, outcome, err)
	}
}

func TestFixSizingMarkerAloneDoesNotLaunchARepair(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: []byte(`{"summary":"a warning is not a repair"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"fix-estimate-exceeds-deadline","description":"repair was not launched","action":"ask-user"}]}`
	_, err := (&ReviewStep{}).Execute(sctx)
	if calls != 0 || err == nil {
		t.Fatalf("scheduling warning dispatched as a cause: calls=%d error=%v", calls, err)
	}
}
