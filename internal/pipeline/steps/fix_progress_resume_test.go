package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
