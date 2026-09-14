package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIStep_MergePolicyAppliesToBothConflictPromptForms(t *testing.T) {
	for _, policy := range []string{"sync_strategy", "rebase.strategy"} {
		for _, mixed := range []bool{false, true} {
			name := "conflict_only"
			if mixed {
				name = "failed_check_and_conflict"
			}
			t.Run(policy+"/"+name, func(t *testing.T) {
				dir, baseSHA, _ := setupGitRepo(t)
				upstream := t.TempDir()
				gitCmd(t, upstream, "init", "--bare")
				gitCmd(t, upstream, "config", "gc.auto", "0")
				gitCmd(t, dir, "remote", "add", "origin", upstream)
				gitCmd(t, dir, "checkout", "feature")
				if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "feature.txt")
				gitCmd(t, dir, "commit", "-m", "feature")
				featureHead := gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "checkout", "main")
				if err := os.WriteFile(filepath.Join(dir, "main.txt"), []byte("main\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "main.txt")
				gitCmd(t, dir, "commit", "-m", "main moved")
				baseHead := gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "push", "origin", "main", "feature")
				gitCmd(t, dir, "checkout", "feature")

				var prompt string
				ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					prompt = opts.Prompt
					gitCmd(t, opts.CWD, "merge", "--no-ff", "--no-edit", "main")
					return &agent.Result{}, nil
				}}
				sctx := newTestContext(t, ag, dir, baseSHA, featureHead, config.Commands{})
				sctx.Repo.UpstreamURL = upstream
				sctx.Run.Branch = "refs/heads/feature"
				sctx.Config.CI.RevalidateRepairs = true
				if policy == "sync_strategy" {
					sctx.Config.SyncStrategy = config.SyncStrategyMerge
				} else {
					sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
				}
				sctx.Env = fakeCIGHMergeable(t, "OPEN", `[{"name":"build","state":"FAILURE","bucket":"fail"}]`, "CONFLICTING")
				host, skip := buildHost(sctx, scm.ProviderGitHub)
				if host == nil {
					t.Fatal(skip)
				}
				var failing []string
				if mixed {
					failing = []string{"build"}
				}
				pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
				repair, err := (&CIStep{}).autoFixCI(sctx, host, pr, ciTargetsFor(failing, true))
				if err != nil {
					t.Fatal(err)
				}
				if !repair.HeadAdvanced {
					t.Fatal("merge repair did not advance the recorded head")
				}
				for _, rule := range []string{"Use git merge, not git rebase", "Do not run git rebase, git reset --hard", "or force-push", "merge target commit: " + baseHead} {
					if !strings.Contains(prompt, rule) {
						t.Fatalf("merge policy omitted %q from the agent prompt: %s", rule, prompt)
					}
				}
				if strings.Contains(prompt, "Rebase onto the base branch") || strings.Contains(prompt, "then rebase onto") {
					t.Fatalf("merge policy still instructs a rebase: %s", prompt)
				}
				if got := gitCmd(t, dir, "merge-base", featureHead, sctx.Run.HeadSHA); got != featureHead {
					t.Fatalf("repair lost feature head %s: merge base %s", featureHead, got)
				}
				if got := gitCmd(t, dir, "merge-base", baseHead, sctx.Run.HeadSHA); got != baseHead {
					t.Fatalf("repair lost base head %s: merge base %s", baseHead, got)
				}
			})
		}
	}
}
