package db

import (
	"crypto/sha1"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// lineageSHA returns a deterministic full object ID for a fixture label, so
// every lineage fixture compares exact 40-hex heads as the gate does.
func lineageSHA(label string) string {
	sum := sha1.Sum([]byte(label))
	return hex.EncodeToString(sum[:])
}

// verifiedTerminalRun records a run on branch submitted at submitted that a
// terminalization verified at head with the given terminal status.
func verifiedTerminalRun(t *testing.T, d *DB, repoID, branch, submitted, head string, status types.RunStatus) *Run {
	t.Helper()
	run, err := d.InsertRun(repoID, branch, submitted, lineageSHA("base"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, status, head); err != nil {
		t.Fatal(err)
	}
	return run
}

func sortedLineageHeads(t *testing.T, d *DB, repoID, branch, continuation, exclude string) []string {
	t.Helper()
	got, err := d.LineageMirrorHeads(repoID, branch, continuation, exclude)
	if err != nil {
		t.Fatalf("LineageMirrorHeads: %v", err)
	}
	sort.Strings(got)
	return got
}

func sortedSHAs(labels ...string) []string {
	shas := make([]string, 0, len(labels))
	for _, label := range labels {
		shas = append(shas, lineageSHA(label))
	}
	sort.Strings(shas)
	return shas
}

func TestLineageMirrorHeadsRequiresATerminalVerifiedRunEndingAtTheExactHead(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.InsertRepo("/home/user/other", "git@github.com:user/other.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	continuation := lineageSHA("continuation")

	// Every terminal status contributes, in either stored branch form.
	failed := verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("s1"), continuation, types.RunFailed)
	published := verifiedTerminalRun(t, d, repo.ID, "refs/heads/feature", lineageSHA("s2"), continuation, types.RunCancelled)
	if err := d.UpdateRunPublication(published.ID, PushBinding{HeadSHA: lineageSHA("p2"), TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(published.ID, types.RunCancelled, continuation); err != nil {
		t.Fatal(err)
	}
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("s3"), continuation, types.RunCompleted)
	verifiedTerminalRun(t, d, repo.ID, "refs/heads/feature", lineageSHA("s4"), continuation, types.RunCIMonitorInterrupted)

	// A qualifying run with no recorded mirror heads contributes nothing.
	nullable := verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("nullable"), continuation, types.RunFailed)
	if _, err := d.sql.Exec(`UPDATE runs SET submitted_head_sha = NULL, last_pushed_sha = NULL WHERE id = ?`, nullable.ID); err != nil {
		t.Fatal(err)
	}
	blank := verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("blank"), continuation, types.RunFailed)
	if _, err := d.sql.Exec(`UPDATE runs SET submitted_head_sha = '  ', last_pushed_sha = '' WHERE id = ?`, blank.ID); err != nil {
		t.Fatal(err)
	}

	// None of these may contribute a head.
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("other-head"), lineageSHA("other-continuation"), types.RunFailed)
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("abbreviated-head"), continuation[:12], types.RunFailed)
	verifiedTerminalRun(t, d, repo.ID, "other-branch", lineageSHA("other-branch"), continuation, types.RunFailed)
	verifiedTerminalRun(t, d, repo.ID, "refs/heads/feature-2", lineageSHA("prefix-branch"), continuation, types.RunFailed)
	verifiedTerminalRun(t, d, other.ID, "feature", lineageSHA("other-repo"), continuation, types.RunFailed)
	active, err := d.InsertRun(repo.ID, "feature", lineageSHA("active"), lineageSHA("base"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(active.ID, continuation); err != nil {
		t.Fatal(err)
	}
	unverified, err := d.InsertRun(repo.ID, "feature", lineageSHA("unverified"), lineageSHA("base"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(unverified.ID, continuation); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(unverified.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	// Crash recovery stamps the verified head just before the run turns
	// terminal; until then it is still active and contributes nothing.
	recovering, err := d.InsertRun(repo.ID, "feature", lineageSHA("recovering"), lineageSHA("base"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(recovering.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRunTerminalHeadEvidence(recovering.ID, continuation); err != nil {
		t.Fatal(err)
	}
	if run, err := d.GetRun(recovering.ID); err != nil || run.TerminalHeadVerifiedAt == nil || run.HeadSHA != continuation {
		t.Fatalf("fixture did not stamp the running row: %+v, %v", run, err)
	}

	want := sortedSHAs("p2", "s1", "s2", "s3", "s4")
	for _, branch := range []string{"feature", "refs/heads/feature", " feature "} {
		if got := sortedLineageHeads(t, d, repo.ID, branch, continuation, ""); !reflect.DeepEqual(got, want) {
			t.Fatalf("lineage heads for %q = %v, want %v", branch, got, want)
		}
	}
	if got, want := sortedLineageHeads(t, d, repo.ID, "refs/heads/feature", continuation, failed.ID), sortedSHAs("p2", "s2", "s3", "s4"); !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage heads excluding %s = %v, want %v", failed.ID, got, want)
	}

	for name, call := range map[string][2]string{
		"empty continuation":       {"feature", ""},
		"blank continuation":       {"feature", "   "},
		"abbreviated continuation": {"feature", continuation[:7]},
		"empty branch":             {"", continuation},
		"bare refs/heads/ branch":  {"refs/heads/", continuation},
	} {
		got, err := d.LineageMirrorHeads(repo.ID, call[0], call[1], "")
		if err != nil || len(got) != 0 {
			t.Fatalf("%s = %v, %v; want no candidates", name, got, err)
		}
	}
}

func TestLineageMirrorHeadsWalksBackThroughVerifiedReruns(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Run zero submitted the mirror head, published p0, and ended at h0; run
	// one resumed h0 and ended at h1; run two resumed h1 and ended at h2.
	zero := verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("mirror"), lineageSHA("h0"), types.RunFailed)
	if err := d.UpdateRunPublication(zero.ID, PushBinding{HeadSHA: lineageSHA("p0"), TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(zero.ID, types.RunFailed, lineageSHA("h0")); err != nil {
		t.Fatal(err)
	}
	verifiedTerminalRun(t, d, repo.ID, "refs/heads/feature", lineageSHA("h0"), lineageSHA("h1"), types.RunCancelled)
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("h1"), lineageSHA("h2"), types.RunFailed)
	// A run that ended at the last-pushed head p0 is not part of this lineage:
	// a published head contributes as a candidate, never as a continuation.
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("reached-only-through-p0"), lineageSHA("p0"), types.RunFailed)

	if got, want := sortedLineageHeads(t, d, repo.ID, "feature", lineageSHA("h1"), ""), sortedSHAs("h0", "mirror", "p0"); !reflect.DeepEqual(got, want) {
		t.Fatalf("two-hop lineage = %v, want %v", got, want)
	}
	if got, want := sortedLineageHeads(t, d, repo.ID, "feature", lineageSHA("h2"), ""), sortedSHAs("h1", "h0", "mirror", "p0"); !reflect.DeepEqual(got, want) {
		t.Fatalf("three-hop lineage = %v, want %v", got, want)
	}

	// Repeated and cyclic recorded heads terminate and contribute each head once.
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("c1"), lineageSHA("c0"), types.RunFailed)
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("c1"), lineageSHA("c0"), types.RunCancelled)
	verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("c0"), lineageSHA("c1"), types.RunFailed)
	got, err := d.LineageMirrorHeads(repo.ID, "feature", lineageSHA("c0"), "")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := sortedSHAs("c0", "c1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("cyclic lineage = %v, want each head once %v", got, want)
	}
}

// Every filter applies again at every hop: an ineligible intermediate row
// contributes nothing and opens no edge to the rows behind it.
func TestLineageMirrorHeadsIneligibleIntermediateHopEndsTheWalk(t *testing.T) {
	for _, blocker := range []string{"unverified", "active_stamped_by_recovery", "wrong_repo", "wrong_branch", "excluded"} {
		t.Run(blocker, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			other, err := d.InsertRepo("/home/user/other", "git@github.com:user/other.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("old-mirror"), lineageSHA("u0"), types.RunFailed)
			var intermediate *Run
			switch blocker {
			case "unverified":
				intermediate, err = d.InsertRun(repo.ID, "feature", lineageSHA("u0"), lineageSHA("base"))
				if err == nil {
					err = d.UpdateRunHeadSHA(intermediate.ID, lineageSHA("u1"))
				}
				if err == nil {
					err = d.UpdateRunStatus(intermediate.ID, types.RunFailed)
				}
			case "active_stamped_by_recovery":
				intermediate, err = d.InsertRun(repo.ID, "feature", lineageSHA("u0"), lineageSHA("base"))
				if err == nil {
					err = d.UpdateRunStatus(intermediate.ID, types.RunRunning)
				}
				if err == nil {
					err = d.RecordRunTerminalHeadEvidence(intermediate.ID, lineageSHA("u1"))
				}
			case "wrong_repo":
				intermediate = verifiedTerminalRun(t, d, other.ID, "feature", lineageSHA("u0"), lineageSHA("u1"), types.RunFailed)
			case "wrong_branch":
				intermediate = verifiedTerminalRun(t, d, repo.ID, "feature-sibling", lineageSHA("u0"), lineageSHA("u1"), types.RunFailed)
			case "excluded":
				intermediate = verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("u0"), lineageSHA("u1"), types.RunFailed)
			}
			if err != nil {
				t.Fatal(err)
			}
			verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("u1"), lineageSHA("u2"), types.RunFailed)

			exclude := ""
			if blocker == "excluded" {
				exclude = intermediate.ID
			}
			if got, want := sortedLineageHeads(t, d, repo.ID, "feature", lineageSHA("u2"), exclude), sortedSHAs("u1"); !reflect.DeepEqual(got, want) {
				t.Fatalf("walk past a %s hop = %v, want %v", blocker, got, want)
			}
		})
	}
}

// A failed read never yields a usable partial candidate list, even when an
// earlier hop already succeeded.
func TestLineageMirrorHeadsFailsClosed(t *testing.T) {
	for _, failure := range []string{"query", "scan_at_first_hop", "scan_after_one_hop"} {
		t.Run(failure, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			first := verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("h0"), lineageSHA("h1"), types.RunFailed)
			second := verifiedTerminalRun(t, d, repo.ID, "feature", lineageSHA("mirror"), lineageSHA("h0"), types.RunFailed)
			switch failure {
			case "query":
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
			case "scan_at_first_hop":
				if _, err := d.sql.Exec(`UPDATE runs SET intent_score = 'not a score' WHERE id = ?`, first.ID); err != nil {
					t.Fatal(err)
				}
			case "scan_after_one_hop":
				if _, err := d.sql.Exec(`UPDATE runs SET intent_score = 'not a score' WHERE id = ?`, second.ID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := d.LineageMirrorHeads(repo.ID, "feature", lineageSHA("h1"), "")
			if err == nil || !strings.HasPrefix(err.Error(), "get lineage mirror heads: ") {
				t.Fatalf("lineage read failure = %v, want a get lineage mirror heads error", err)
			}
			if got != nil {
				t.Fatalf("failed lineage read returned candidates %v", got)
			}
		})
	}
}
