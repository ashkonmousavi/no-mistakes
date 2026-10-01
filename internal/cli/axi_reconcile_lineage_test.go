package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTriggerRunReconcilesMirrorHeldByTheRunWhoseHeadIsSubmitted reproduces the
// 2026-10-01 stalls on the fresh-submission path. A terminal run rebased the
// submitted range and its fix commit rewrote a submitted line; the operator
// adopted that run's verified head, but the private mirror still holds the head
// the run itself placed there, and the content proof can never accept it.
//
// Each positive shape runs twice. With a rejecting pre-receive hook a passing
// reconciliation shows as the hook's rejection plus a restored mirror and an
// archive tag. With an accepting gate, a post-receive hook stands in for the
// daemon's admission and the fake daemon reports a run only once the gate
// received the submission, so triggerRun must return a new run bound to the
// submitted head. A refusal shows as the planner's error with the mirror
// untouched and nothing archived.
func TestTriggerRunReconcilesMirrorHeldByTheRunWhoseHeadIsSubmitted(t *testing.T) {
	for _, variant := range []string{
		"submitted_head_adopted/rejected",
		"submitted_head_adopted/admitted",
		"published_head_adopted/rejected",
		"published_head_adopted/admitted",
		"own_rewrite_of_unmoved_submission",
		"unverified_run_head",
		"mirror_moved_past_the_run",
		"custody_not_returned",
		"lineage_read_fails",
	} {
		t.Run(variant, func(t *testing.T) {
			shape, outcome, _ := strings.Cut(variant, "/")
			dir := t.TempDir()
			p := paths.WithRoot(makeSocketSafeTempDir(t))
			t.Setenv("NM_HOME", p.Root())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			commit := func(message string) string {
				t.Helper()
				cliGit(t, dir, "add", "-A")
				cliGit(t, dir, "commit", "-q", "-m", message)
				return cliGit(t, dir, "rev-parse", "HEAD")
			}
			cliGit(t, dir, "init", "-b", "main")
			cliGit(t, dir, "config", "user.name", "Test")
			cliGit(t, dir, "config", "user.email", "test@example.com")
			write("generated.txt", "entries\n")
			commit("base")

			cliGit(t, dir, "checkout", "-q", "-b", "feature")
			write("feature.txt", "line one\nline two\n")
			write("generated.txt", "entries\nfeature\n")
			firstSubmitted := commit("feature with regenerated index")
			write("other.txt", "other\n")
			submitted := commit("patch-identical follow-up")

			repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			gateDir := p.RepoDir(repo.ID)
			cliGit(t, dir, "clone", "-q", "--bare", dir, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)

			// The run rebases onto a moved base and its fix commit rewrites a
			// submitted line, exactly what the field runs did.
			cliGit(t, dir, "checkout", "-q", "main")
			write("generated.txt", "entries\nsibling\n")
			movedBase := commit("sibling regenerated index")
			cliGit(t, dir, "checkout", "-q", "feature")
			cliGit(t, dir, "reset", "-q", "--hard", movedBase)
			write("feature.txt", "line one\nline two\n")
			write("generated.txt", "entries\nsibling\nfeature\n")
			commit("feature with regenerated index")
			write("other.txt", "other\n")
			commit("patch-identical follow-up")
			write("feature.txt", "line one, reviewed\nline two\n")
			runHead := commit("no-mistakes(review): rewrite a submitted line")

			mirrorHead := submitted
			recordedSubmission := submitted
			if shape == "published_head_adopted" {
				// The mirror then holds the run's last pushed head, not its submission.
				recordedSubmission = firstSubmitted
			}
			placed, err := d.InsertRun(repo.ID, "feature", recordedSubmission, movedBase)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "published_head_adopted":
				// The run published its submitted range, then rewrote it again.
				if err := d.UpdateRunPublication(placed.ID, db.PushBinding{HeadSHA: submitted, TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/feature"}); err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatusWithVerifiedHead(placed.ID, types.RunFailed, runHead); err != nil {
					t.Fatal(err)
				}
			case "own_rewrite_of_unmoved_submission":
				// The run changed nothing; the operator rewrote the range alone.
				if err := d.UpdateRunStatusWithVerifiedHead(placed.ID, types.RunFailed, submitted); err != nil {
					t.Fatal(err)
				}
			case "unverified_run_head":
				if err := d.UpdateRunHeadSHA(placed.ID, runHead); err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(placed.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
			case "mirror_moved_past_the_run":
				if err := d.UpdateRunStatusWithVerifiedHead(placed.ID, types.RunFailed, runHead); err != nil {
					t.Fatal(err)
				}
				cliGit(t, dir, "checkout", "-q", "--detach", submitted)
				write("unseen.txt", "pushed to the gate from somewhere else\n")
				mirrorHead = commit("unseen mirror commit")
				cliGit(t, gateDir, "fetch", "-q", dir, "+"+mirrorHead+":refs/heads/feature")
				cliGit(t, dir, "checkout", "-q", "feature")
			default:
				if err := d.UpdateRunStatusWithVerifiedHead(placed.ID, types.RunFailed, runHead); err != nil {
					t.Fatal(err)
				}
			}
			if shape != "custody_not_returned" {
				if err := d.SetRunCustodyReturned(placed.ID); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "lineage_read_fails" {
				// An unreadable predecessor row. Ownership inspection reports an
				// ambiguous context for it, which does not block a fresh
				// submission, so the lineage read is the boundary that must refuse.
				raw, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				_, err = raw.Exec(`UPDATE runs SET intent_score = 'not a score' WHERE id = ?`, placed.ID)
				if closeErr := raw.Close(); err != nil || closeErr != nil {
					t.Fatalf("corrupt predecessor row: %v, close %v", err, closeErr)
				}
			}
			admissionLog := filepath.Join(t.TempDir(), "admission.log")
			if outcome == "admitted" {
				hook := "#!/bin/sh\nwhile read old new ref; do echo \"$old $new $ref\" >> '" + admissionLog + "'; done\n" +
					"i=0\nwhile [ \"$i\" -lt \"${GIT_PUSH_OPTION_COUNT:-0}\" ]; do eval \"echo option \\$GIT_PUSH_OPTION_$i\" >> '" + admissionLog + "'; i=$((i+1)); done\n"
				if err := os.WriteFile(filepath.Join(gateDir, "hooks", "post-receive"), []byte(hook), 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive"), []byte("#!/bin/sh\necho submission-rejected >&2\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			const admittedRunID = "01ADMITTEDRUNFORTHESUBMITTEDHEAD"
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GetRunsResult{}, nil
			})
			srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
				received, _ := os.ReadFile(admissionLog)
				if !strings.Contains(string(received), " "+runHead+" refs/heads/feature") {
					return &ipc.GetActiveRunResult{}, nil
				}
				head := runHead
				return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{ID: admittedRunID, RepoID: repo.ID, Branch: "feature", HeadSHA: runHead, SubmittedHeadSHA: &head, Status: types.RunRunning}}, nil
			})
			done := make(chan error, 1)
			go func() { done <- srv.Serve(p.Socket()) }()
			t.Cleanup(func() { srv.Close(); <-done })
			var client *ipc.Client
			deadline := time.Now().Add(3 * time.Second)
			for {
				client, err = ipc.Dial(p.Socket())
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			defer client.Close()
			chdir(t, dir)
			env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			runID, err := triggerRun(ctx, env, "feature", nil, "", "", false, "")
			archive := "refs/tags/no-mistakes-abandoned/feature/" + mirrorHead
			if outcome == "admitted" {
				if err != nil || runID != admittedRunID {
					t.Fatalf("submission of the run's verified head was not admitted: run=%q err=%v", runID, err)
				}
				if got := cliGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != runHead {
					t.Fatalf("gate branch = %s, want submitted head %s", got, runHead)
				}
				if got := cliGit(t, gateDir, "rev-parse", archive); got != mirrorHead {
					t.Fatalf("archive = %s, want replaced mirror head %s", got, mirrorHead)
				}
				received, readErr := os.ReadFile(admissionLog)
				if readErr != nil || !strings.Contains(string(received), strings.Repeat("0", 40)+" "+runHead+" refs/heads/feature") ||
					!strings.Contains(string(received), "option "+reconciledPreviousHeadPushOptionPrefix+mirrorHead) {
					t.Fatalf("admission did not see a reconciled ordinary push carrying the replaced head: %q (err %v)", received, readErr)
				}
				return
			}
			if runID != "" || err == nil {
				t.Fatalf("rejected or refused submission: run=%q err=%v", runID, err)
			}
			if got := cliGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != mirrorHead {
				t.Fatalf("mirror left at %s, want %s", got, mirrorHead)
			}
			switch shape {
			case "submitted_head_adopted", "published_head_adopted":
				if strings.Contains(err.Error(), "refusing to reconcile private mirror ref") || !strings.Contains(err.Error(), "submission-rejected") {
					t.Fatalf("the run's own mirror head was not reconciled for its recorded head: %v", err)
				}
				if got := cliGit(t, gateDir, "rev-parse", archive); got != mirrorHead {
					t.Fatalf("archive = %s, want %s", got, mirrorHead)
				}
				return
			case "custody_not_returned":
				var ownershipErr *branchOwnershipError
				if !errors.As(err, &ownershipErr) {
					t.Fatalf("still-owned branch was not refused by admission before lineage: %v", err)
				}
			case "lineage_read_fails":
				if want := `prepare private mirror for "feature": get lineage mirror heads: `; !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("lineage read failure = %v, want prefix %q", err, want)
				}
			default:
				if !strings.Contains(err.Error(), "refusing to reconcile private mirror ref") || !strings.Contains(err.Error(), mirrorHead) {
					t.Fatalf("mirror head outside the run's recorded lineage skipped the content proof: %v", err)
				}
			}
			if tags := cliGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
				t.Fatalf("refused submission archived a head: %s", tags)
			}
		})
	}
}
