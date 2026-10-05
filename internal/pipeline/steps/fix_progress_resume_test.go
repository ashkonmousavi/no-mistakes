package steps

import (
	"context"
	"errors"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFixProgressResumeReviewTwoCutsNeverReplaysAppliedCauses(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := map[string]int{}
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		for _, id := range []string{"A", "B", "C"} {
			if !strings.Contains(opts.Prompt, "Current repair finding ID: "+id) {
				continue
			}
			calls[id]++
			if id != "C" {
				if calls[id] != 1 {
					t.Fatalf("replayed completed cause %s", id)
				}
				if err := os.WriteFile(filepath.Join(dir, id+".txt"), []byte("applied"), 0644); err != nil {
					t.Fatal(err)
				}
				return &agent.Result{Output: []byte(`{"summary":"repair one cause"}`)}, nil
			}
			if calls[id] > 1 {
				got, e := os.ReadFile(filepath.Join(dir, "partial.bin"))
				if e != nil || string(got) != "unfinished C" {
					t.Fatalf("continuation lost partial bytes: %q %v", got, e)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "partial.bin"), []byte("unfinished C"), 0644); err != nil {
				t.Fatal(err)
			}
			if calls[id] < 3 {
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 123456789})
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 123456789})
				return nil, errors.New("cut C")
			}
			return &agent.Result{Output: []byte(`{"summary":"complete cause C"}`)}, nil
		}
		if opts.Purpose == "review-fix-verification" {
			calls["verification"]++
			return &agent.Result{Output: []byte(`{"summary":"focused check passed"}`)}, nil
		}
		calls["independent review"]++
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"independently verified","risk_level":"low","risk_rationale":"fixture checks the repaired scope","risk_scope":"source-or-external","reviewed_paths":["feature.txt","A.txt","B.txt","partial.bin"]}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepReview)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","severity":"error","description":"cause A","action":"auto-fix"},{"id":"B","severity":"error","description":"cause B","action":"auto-fix"},{"id":"C","severity":"error","description":"cause C","action":"auto-fix"}]}`
	var refs []string
	for attempt := 0; attempt < 3; attempt++ {
		outcome, err := (&ReviewStep{}).Execute(sctx)
		if attempt < 2 {
			if err == nil || !strings.Contains(err.Error(), "cut C") {
				t.Fatalf("cut %d = %+v %v", attempt, outcome, err)
			}
			p, e := sctx.DB.LatestWorkRescue(sctx.Run.ID)
			if e != nil || p == nil || p.State != "saved" {
				t.Fatalf("partial state %+v %v", p, e)
			}
			refs = append(refs, p.Ref)
		} else if err != nil || outcome.NeedsApproval {
			t.Fatalf("completion = %+v %v", outcome, err)
		}
	}
	if calls["A"] != 1 || calls["B"] != 1 || calls["C"] != 3 || calls["verification"] != 1 || calls["independent review"] != 1 {
		t.Fatalf("dispatches %+v", calls)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "3" {
		t.Fatalf("completed commits=%s", got)
	}
	for _, ref := range refs {
		if got := gitCmd(t, dir, "rev-parse", ref); got == "" {
			t.Fatalf("consumed rescue lost %s", ref)
		}
	}
	if p, e := sctx.DB.LatestWorkRescue(sctx.Run.ID); e != nil || p != nil {
		t.Fatalf("unfinished rescue remains %+v %v", p, e)
	}
}

func TestFixProgressTestValidationOnlyRetryRetainsEveryNewTest(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls <= 2 {
			name := string(rune('A'+calls-1)) + "_test.go"
			if err := os.WriteFile(filepath.Join(dir, name), []byte("package fixture\n"), 0644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: []byte(`{"summary":"add selected regression"}`)}, nil
		}
		if calls == 3 {
			return nil, errTestAgentTimeout
		}
		if !strings.Contains(opts.Prompt, "A_test.go") || !strings.Contains(opts.Prompt, "B_test.go") {
			t.Fatalf("retry omitted regression union: %s", opts.Prompt)
		}
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"honest fixture","tested":["inspected local regression paths"],"artifacts":[],"testing_summary":"fixture","scenarios":[{"name":"pending installed validation","result":"untested","live":false,"evidence":"","reason":"fixture cannot drive installed product"}],"verdict":"inconclusive"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepTest)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"first cause"},{"id":"B","description":"second cause"}]}`
	cut, err := (&TestStep{}).Execute(sctx)
	if err != nil || cut == nil || !cut.NeedsApproval {
		t.Fatalf("evidence cut %+v %v", cut, err)
	}
	sctx.PreviousFindings = cut.Findings
	final, err := (&TestStep{}).Execute(sctx)
	if err != nil || final == nil || !final.NeedsApproval {
		t.Fatalf("unsupported fixture became clean pass: %+v %v", final, err)
	}
	if calls != 4 || gitCmd(t, dir, "rev-list", "--count", head+"..HEAD") != "2" {
		t.Fatalf("validation retry repeated repairs: calls %d", calls)
	}
}

func TestFixProgressAuthorityRetryRejectsReusedIDAndChangedInstructions(t *testing.T) {
	for _, replacement := range []string{
		`{"findings":[{"id":"C","description":"a different cause"}]}`,
		`{"findings":[{"id":"C","description":"unfinished cause","user_instructions":"change the design"}]}`,
	} {
		t.Run(replacement, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("saved C"), 0644); err != nil {
					t.Fatal(err)
				}
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 123456789})
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 123456789})
				return nil, errors.New("cut C")
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			bindStepResult(t, sctx, types.StepReview)
			sctx.Fixing = true
			sctx.PreviousFindings = `{"findings":[{"id":"C","description":"unfinished cause"}]}`
			if _, err := (&ReviewStep{}).Execute(sctx); err == nil {
				t.Fatal("cut missing")
			}
			p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
			if err != nil || p == nil {
				t.Fatal(err)
			}
			sctx.PreviousFindings = replacement
			if _, err = (&ReviewStep{}).Execute(sctx); err == nil {
				t.Fatal("changed source scope silently restored")
			}
			if calls != 1 || gitCmd(t, dir, "rev-parse", "HEAD") != head {
				t.Fatal("scope refusal launched work or advanced head")
			}
			if gitCmd(t, dir, "rev-parse", p.Ref) != p.SHA {
				t.Fatal("scope refusal lost original ref")
			}
		})
	}
}

type savedSnapshotHost struct {
	completionSnapshotHost
	reads    int
	expected string
}

func (h *savedSnapshotHost) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	h.reads++
	if pr.HeadSHA != h.expected {
		return nil, errors.New("attempted a fresh snapshot on an unpublished local checkpoint")
	}
	return h.completionSnapshotHost.GetChecks(ctx, pr)
}

func TestFixProgressCIContinuationKeepsOriginalCheckFreshness(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	a, b, verification := 0, 0, 0
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "ci-fix-verification" {
			verification++
			return &agent.Result{Output: []byte(`{"summary":"focused check passed"}`)}, nil
		}
		id := "A"
		if strings.Contains(opts.Prompt, "Authorized repair finding: B.") {
			id = "B"
		}
		if id == "A" {
			a++
			if a != 1 {
				t.Fatal("replayed first CI cause")
			}
		} else {
			b++
		}
		if err := os.WriteFile(filepath.Join(dir, id+".txt"), []byte("repaired"), 0644); err != nil {
			t.Fatal(err)
		}
		if id == "B" && b == 1 {
			opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 123456789})
			<-ctx.Done()
			opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 123456789})
			return nil, ctx.Err()
		}
		return &agent.Result{Output: []byte(`{"summary":"repair check cause","code_change_needed":true}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	bindStepResult(t, sctx, types.StepCI)
	sctx.Config.AgentTimeout = 100 * time.Millisecond
	sctx.Config.CI.RevalidateRepairs = true
	sctx.PreviousFindings = `{"findings":[{"id":"A","description":"cause A","category":"ci-check","check":"A","check_id":"a"},{"id":"B","description":"cause B","category":"ci-check","check":"B","check_id":"b"}]}`
	host := &savedSnapshotHost{expected: head, completionSnapshotHost: completionSnapshotHost{checks: []scm.Check{{Name: "A", ProviderID: "a", ExecutionID: "attempt-one-a", Bucket: scm.CheckBucketFail, CompletedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}, {Name: "B", ProviderID: "b", ExecutionID: "attempt-one-b", Bucket: scm.CheckBucketFail, CompletedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}}}}
	cut, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "1"})
	if err != nil || cut == nil || !cut.NeedsApproval {
		t.Fatalf("CI cut %+v %v", cut, err)
	}
	resumed := &CIStep{}
	sctx.Config.AgentTimeout = time.Second
	result, err := resumed.repairFromFindings(sctx, host, &scm.PR{Number: "1"})
	if err != nil || result == nil || result.RestartFrom != types.StepReview {
		t.Fatalf("resumed CI lost revalidation policy: %+v %v", result, err)
	}
	if host.reads != 1 || a != 1 || b != 2 || verification != 1 {
		t.Fatalf("freshness/replay reads=%d A=%d B=%d verification=%d", host.reads, a, b, verification)
	}
	if got := resumed.observedCompletedAt["id:b"].ExecutionID; got != "attempt-one-b" {
		t.Fatalf("freshness changed: %q", got)
	}
}

func TestFixProgressTestNewRunCarriesSourceRegressionPaths(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sourceCalls := 0
	sourceAgent := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		sourceCalls++
		name := string(rune('A'+sourceCalls-1)) + "_test.go"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package fixture\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if sourceCalls == 3 {
			opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 123456789})
			opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 123456789})
			return nil, errTestAgentTimeout
		}
		return &agent.Result{Output: []byte(`{"summary":"add cause regression"}`)}, nil
	}}
	source := newTestContextWithDBRecords(t, sourceAgent, dir, base, head, config.Commands{})
	bindStepResult(t, source, types.StepTest)
	source.Fixing = true
	source.PreviousFindings = `{"findings":[{"id":"A","severity":"error","description":"cause A"},{"id":"B","severity":"error","description":"cause B"},{"id":"C","severity":"error","description":"cause C"}]}`
	cut, err := (&TestStep{}).Execute(source)
	if err != nil || cut == nil || !cut.NeedsApproval {
		t.Fatalf("source cut %+v %v", cut, err)
	}
	p, err := source.DB.LatestWorkRescue(source.Run.ID)
	if err != nil || p == nil || p.State != "saved" {
		t.Fatalf("source rescue %+v %v", p, err)
	}
	if err = source.DB.UpdateRunStatusWithVerifiedHead(source.Run.ID, types.RunFailed, source.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	next, err := source.DB.InsertRun(source.Run.RepoID, source.Run.Branch, source.Run.HeadSHA, base)
	if err != nil {
		t.Fatal(err)
	}
	p.State = "consumed"
	p.ConsumedBy = next.ID
	if err = source.DB.SaveWorkRescue(p); err != nil {
		t.Fatal(err)
	}
	calls := 0
	source.Agent = &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if got, err := os.ReadFile(filepath.Join(dir, "C_test.go")); err != nil || string(got) != "package fixture\n" {
				t.Fatal("unfinished C not restored")
			}
			if err := os.WriteFile(filepath.Join(dir, "C_test.go"), []byte("package fixture\n// completed C\n"), 0644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: []byte(`{"summary":"complete cause C regression"}`)}, nil
		}
		for _, path := range []string{"A_test.go", "B_test.go", "C_test.go"} {
			if !strings.Contains(opts.Prompt, path) {
				t.Fatalf("new run evidence omitted %s", path)
			}
		}
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"fixture source inspected","tested":["inspected regression files"],"artifacts":[],"testing_summary":"fixture has no deployed surface","scenarios":[{"name":"pending installed acceptance","result":"untested","live":false,"evidence":"","reason":"fixture cannot drive installed product"}],"verdict":"inconclusive"}`)}, nil
	}}
	source.Run = next
	bindStepResult(t, source, types.StepTest)
	source.PreviousFindings = `{"findings":[{"id":"new-C","severity":"error","description":"cause C"}]}`
	if err = source.BindInheritedWork(); err != nil {
		t.Fatal(err)
	}
	completed, err := (&TestStep{}).Execute(source)
	if err != nil || completed == nil || !completed.NeedsApproval {
		t.Fatalf("new run evidence %+v %v", completed, err)
	}
	if sourceCalls != 3 || calls != 2 {
		t.Fatalf("source causes repeated: old=%d new=%d", sourceCalls, calls)
	}
	if err = pipeline.CompleteInheritedWork(source.Ctx, source.DB, source.Run, dir, p); err != nil {
		t.Fatalf("reminted finding identity could not consume its exact scoped cause: %v", err)
	}
}
