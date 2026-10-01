package steps

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPushStep_RerunRepublishesOverItsPredecessorsPublishedHead reproduces the
// 2026-10-01 rerun stall at the push boundary. Run one published a head, then
// rebased again and failed with that newer head unpublished; the rerun resumed
// exactly that verified head, rebased onto the moved base, and a review fix
// rewrote a published line. The mirror still holds run one's last pushed head,
// which Decision 41-A covered only for run one itself until the lineage walk.
//
// The remaining variants keep every other publication safeguard in force with
// lineage candidates present: review approval, the upstream lease, settlement
// rollback, newer-descendant preservation, and a fail-closed lineage read.
func TestPushStep_RerunRepublishesOverItsPredecessorsPublishedHead(t *testing.T) {
	for _, variant := range []string{
		"rerun_resumes_predecessor_head",
		"rerun_of_a_two_hop_lineage",
		"rerun_of_a_different_head",
		"upstream_advanced_out_of_band",
		"newer_mirror_descendant",
		"settlement_fails_then_retries",
		"lineage_read_fails",
	} {
		t.Run(variant, func(t *testing.T) {
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			dir, baseSHA, _ := setupGitRepo(t)
			write := func(name, content string) {
				t.Helper()
				full := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			commit := func(message string) string {
				t.Helper()
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-q", "-m", message)
				return gitCmd(t, dir, "rev-parse", "HEAD")
			}
			advanceBase := func(content string) {
				t.Helper()
				gitCmd(t, dir, "checkout", "-q", "main")
				write("generated.txt", content)
				commit("sibling regenerated index")
				gitCmd(t, dir, "push", "-q", "origin", "main")
			}

			gitCmd(t, dir, "checkout", "-q", "-B", "feature", baseSHA)
			write("feature.txt", "line one\nline two\n")
			write("generated.txt", "feature\n")
			submitted := commit("feat: feature with regenerated index")
			gitCmd(t, dir, "remote", "add", "origin", upstream)
			gitCmd(t, dir, "push", "-q", "origin", "main")

			// The database lives at a known path so a variant can corrupt a
			// predecessor row the way a damaged state file would.
			first := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, config.Commands{})
			dbPath := filepath.Join(t.TempDir(), "lineage.db")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			first.DB = database
			repo, err := database.InsertRepo(dir, "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			first.Repo = repo
			first.Run, err = database.InsertRun(repo.ID, "refs/heads/feature", submitted, baseSHA)
			if err != nil {
				t.Fatal(err)
			}
			first.Repo.UpstreamURL = upstream
			gateDir := setupGateMirror(t, first)
			gitCmd(t, gateDir, "fetch", dir, submitted+":refs/heads/feature")
			recordReviewApproval(t, first, submitted)
			if _, err := (&PushStep{}).Execute(first); err != nil {
				t.Fatalf("first publication: %v", err)
			}
			published := submitted

			// Run one rebases again after publishing and fails before
			// publishing that head; terminalization verifies it.
			advanceBase("sibling one\n")
			gitCmd(t, dir, "checkout", "-q", "-B", "feature", "main")
			write("feature.txt", "line one\nline two\n")
			write("generated.txt", "sibling one\nfeature\n")
			firstHead := commit("feat: feature with regenerated index")
			if err := database.UpdateRunStatusWithVerifiedHead(first.Run.ID, types.RunFailed, firstHead); err != nil {
				t.Fatal(err)
			}
			predecessor := first.Run.ID

			rerunStart := firstHead
			switch variant {
			case "rerun_of_a_two_hop_lineage":
				// A middle rerun resumed run one's head, added a pipeline commit,
				// and failed unpublished at its own verified head.
				write("middle.txt", "middle rerun pipeline commit\n")
				middleHead := commit("no-mistakes(lint): middle rerun commit")
				middle, err := database.InsertRun(repo.ID, "feature", firstHead, baseSHA)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunStatusWithVerifiedHead(middle.ID, types.RunCancelled, middleHead); err != nil {
					t.Fatal(err)
				}
				rerunStart = middleHead
			case "rerun_of_a_different_head":
				gitCmd(t, dir, "checkout", "-q", "--detach", firstHead)
				write("elsewhere.txt", "a head run one never recorded\n")
				rerunStart = commit("unrelated start")
			}
			rerun, err := database.InsertRun(repo.ID, first.Run.Branch, rerunStart, baseSHA)
			if err != nil {
				t.Fatal(err)
			}
			second := *first
			second.Run = rerun

			// The rerun rebases onto the moved base again and a review fix
			// rewrites a line the published head introduced.
			advanceBase("sibling one\nsibling two\n")
			gitCmd(t, dir, "checkout", "-q", "-B", "feature", "main")
			write("feature.txt", "line one\nline two\n")
			write("generated.txt", "sibling one\nsibling two\nfeature\n")
			switch variant {
			case "rerun_of_a_two_hop_lineage":
				write("middle.txt", "middle rerun pipeline commit\n")
			case "rerun_of_a_different_head":
				write("elsewhere.txt", "a head run one never recorded\n")
			}
			commit("feat: feature with regenerated index")
			write("feature.txt", "line one, reviewed\nline two\n")
			reviewed := commit("no-mistakes(review): rewrite a published line")
			if err := database.UpdateRunHeadSHA(rerun.ID, reviewed); err != nil {
				t.Fatal(err)
			}
			second.Run.HeadSHA = reviewed

			mirrorHead := published
			remoteHead := published
			assertUnchanged := func() {
				t.Helper()
				if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != remoteHead {
					t.Fatalf("upstream moved to %s, want %s", got, remoteHead)
				}
				if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != mirrorHead {
					t.Fatalf("mirror moved to %s, want %s", got, mirrorHead)
				}
				if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
					t.Fatalf("refused publication archived a head: %s", tags)
				}
			}

			// Review authority is still required for the replacement head.
			if _, err := (&PushStep{}).Execute(&second); err == nil || !strings.Contains(err.Error(), "refusing to push") {
				t.Fatalf("unreviewed rerun head was published: %v", err)
			}
			assertUnchanged()
			recordReviewApproval(t, &second, reviewed)

			switch variant {
			case "upstream_advanced_out_of_band":
				outOfBand := t.TempDir()
				gitCmd(t, outOfBand, "clone", "-q", "--branch", "feature", upstream, outOfBand)
				if err := os.WriteFile(filepath.Join(outOfBand, "out-of-band.txt"), []byte("someone else's work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, outOfBand, "add", "-A")
				gitCmd(t, outOfBand, "commit", "-q", "-m", "out-of-band commit")
				gitCmd(t, outOfBand, "push", "-q", "origin", "feature")
				remoteHead = gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
			case "newer_mirror_descendant":
				gitCmd(t, dir, "checkout", "-q", "--detach", reviewed)
				write("newer.txt", "newer work on top of the reviewed head\n")
				mirrorHead = commit("newer mirror commit")
				gitCmd(t, gateDir, "fetch", dir, "+"+mirrorHead+":refs/heads/feature")
				gitCmd(t, dir, "checkout", "-q", "feature")
			case "lineage_read_fails":
				raw, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				_, err = raw.Exec(`UPDATE runs SET intent_score = 'not a score' WHERE id = ?`, predecessor)
				if closeErr := raw.Close(); err != nil || closeErr != nil {
					t.Fatalf("corrupt predecessor row: %v, close %v", err, closeErr)
				}
			case "settlement_fails_then_retries":
				hook := "#!/bin/sh\n[ \"$1\" = prepared ] || exit 0\nwhile read old new ref; do\n" +
					"if [ \"$ref\" = refs/heads/feature ] && [ \"$new\" = " + reviewed + " ]; then exit 1; fi\ndone\nexit 0\n"
				if err := os.WriteFile(filepath.Join(gateDir, "hooks", "reference-transaction"), []byte(hook), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			_, err = (&PushStep{}).Execute(&second)
			switch variant {
			case "rerun_of_a_different_head":
				if err == nil || !strings.Contains(err.Error(), "refusing to reconcile private mirror ref") || !strings.Contains(err.Error(), published) {
					t.Fatalf("a head outside run one's recorded lineage skipped the content proof: %v", err)
				}
				assertUnchanged()
				return
			case "upstream_advanced_out_of_band":
				if err == nil || !strings.Contains(err.Error(), remoteHead[:12]) {
					t.Fatalf("push did not stay leased on the predecessor's published head: %v", err)
				}
				assertUnchanged()
				return
			case "lineage_read_fails":
				if want := "update gate mirror ref refs/heads/feature before push: get lineage mirror heads: "; err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("lineage read failure = %v, want prefix %q", err, want)
				}
				assertUnchanged()
				return
			case "newer_mirror_descendant":
				if err != nil {
					t.Fatalf("publication beneath a newer mirror descendant: %v", err)
				}
				if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != reviewed {
					t.Fatalf("upstream = %s, want reviewed rerun head %s", got, reviewed)
				}
				if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != mirrorHead {
					t.Fatalf("newer mirror descendant rewound to %s, want %s", got, mirrorHead)
				}
				if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
					t.Fatalf("preserved descendant was archived: %s", tags)
				}
				return
			case "settlement_fails_then_retries":
				if err == nil || !strings.Contains(err.Error(), "update gate mirror ref refs/heads/feature to") {
					t.Fatalf("settlement failure = %v", err)
				}
				if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != reviewed {
					t.Fatalf("upstream = %s, want verified push of %s", got, reviewed)
				}
				if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != published {
					t.Fatalf("mirror after settlement failure = %s, want restored %s", got, published)
				}
				if err := os.Remove(filepath.Join(gateDir, "hooks", "reference-transaction")); err != nil {
					t.Fatal(err)
				}
				_, err = (&PushStep{}).Execute(&second)
			}
			if err != nil {
				t.Fatalf("rerun of run one's verified lineage could not replace run one's published mirror head: %v", err)
			}
			for _, repo := range []string{upstream, gateDir} {
				if got := gitCmd(t, repo, "rev-parse", "refs/heads/feature"); got != reviewed {
					t.Fatalf("%s = %s, want reviewed rerun head %s", repo, got, reviewed)
				}
			}
			if got := gitCmd(t, gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/feature/"+published+"^{commit}"); got != published {
				t.Fatalf("replaced published head archived as %s, want %s", got, published)
			}
			run, err := database.GetRun(rerun.ID)
			if err != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != reviewed {
				t.Fatalf("rerun publication not recorded: %+v err=%v", run, err)
			}
		})
	}
}
