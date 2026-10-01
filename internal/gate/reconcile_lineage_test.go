package gate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// rewrittenRebaseFixture models the field shape behind the 2026-10-01 mirror
// stalls: a submitted range is rebased onto a moved base (one regenerated file
// resolves differently, so its patch identity changes), and a later fix commit
// on the live side rewrites a line the submitted range introduced. Nothing was
// lost, but no content proof can tell that rewrite from a dropped change.
func rewrittenRebaseFixture(t *testing.T) (work, gateDir string, private []string, liveHead string) {
	t.Helper()
	work = initReconcileRepo(t)
	writeReconcileFile(t, work, "generated.txt", "entries\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "generated index")
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "feature.txt", "line one\nline two\n")
	writeReconcileFile(t, work, "generated.txt", "entries\nfeature\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "feature with regenerated index")
	first := reconcileGit(t, work, "rev-parse", "HEAD")
	writeReconcileFile(t, work, "other.txt", "other\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "patch-identical follow-up")
	second := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "generated.txt", "entries\nsibling\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "sibling regenerated index")
	writeReconcileFile(t, work, "feature.txt", "line one\nline two\n")
	writeReconcileFile(t, work, "generated.txt", "entries\nsibling\nfeature\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "feature with regenerated index")
	writeReconcileFile(t, work, "other.txt", "other\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "patch-identical follow-up")
	writeReconcileFile(t, work, "feature.txt", "line one, reviewed\nline two\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "review fix rewrites a submitted line")
	liveHead = reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir = filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, second+":refs/heads/feature/rewritten")
	return work, gateDir, []string{first, second}, liveHead
}

func assertLineageMirrorUntouched(t *testing.T, gateDir, want string) {
	t.Helper()
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/rewritten"); got != want {
		t.Fatalf("refused plan moved the mirror to %s, want %s", got, want)
	}
	if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("refused plan archived a head: %s", tags)
	}
}

func TestPlanStaleBranchRefusesWholeRebasedRangeWhenLaterCommitRewritesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, private, liveHead := rewrittenRebaseFixture(t)

	_, err := PlanStaleBranchReconciliation(ctx, gateDir, work, "feature/rewritten", liveHead)
	if err == nil {
		t.Fatal("content proof unexpectedly accepted a rebased range a later commit rewrote")
	}
	// The follow-up commit is patch-identical to its rebased copy, yet the
	// failed final-tree survival merge names the whole private-only range.
	for _, commit := range private {
		if !strings.Contains(err.Error(), commit) {
			t.Fatalf("refusal does not name %s: %v", commit, err)
		}
	}
	assertLineageMirrorUntouched(t, gateDir, private[1])
}

func TestPlanStaleBranchAcceptsAnyExactLineageOwnedHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, private, liveHead := rewrittenRebaseFixture(t)

	plan, err := PlanStaleBranchReconciliation(ctx, gateDir, work, "feature/rewritten", liveHead, "", private[0], private[1])
	if err != nil {
		t.Fatalf("exact owned mirror head was refused: %v", err)
	}
	if !plan.Reconcile || plan.PreviousHead != private[1] || plan.ArchiveTag != "refs/tags/no-mistakes-abandoned/feature/rewritten/"+private[1] {
		t.Fatalf("plan = %+v, want reconciliation of %s", plan, private[1])
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/rewritten"); got != private[1] {
		t.Fatalf("planning moved the mirror to %s", got)
	}
	result, err := ApplyStaleBranchReconciliation(ctx, gateDir, plan)
	if err != nil || !result.Reconciled {
		t.Fatalf("apply = %+v, err = %v", result, err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag); got != private[1] {
		t.Fatalf("archive = %s, want %s", got, private[1])
	}
	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature/rewritten")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/rewritten"); got != liveHead {
		t.Fatalf("ordinary push = %s, want %s", got, liveHead)
	}
}

// Ownership never stands in for content: only exact equality with the mirror
// head skips the proof, so a missing, abbreviated, unrelated, or merely
// ancestral owned head leaves the whole range refused.
func TestPlanStaleBranchInexactLineageHeadsKeepTheContentProof(t *testing.T) {
	for _, variant := range []string{"none", "empty", "abbreviated", "unrelated", "ancestor_only"} {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			work, gateDir, private, liveHead := rewrittenRebaseFixture(t)
			var owned []string
			switch variant {
			case "empty":
				owned = []string{"", "  "}
			case "abbreviated":
				owned = []string{private[1][:12]}
			case "unrelated":
				owned = []string{liveHead, reconcileGit(t, work, "rev-parse", liveHead+"~1")}
			case "ancestor_only":
				owned = []string{private[0]}
			}
			_, err := PlanStaleBranchReconciliation(ctx, gateDir, work, "feature/rewritten", liveHead, owned...)
			if err == nil || !strings.Contains(err.Error(), "refusing to reconcile private mirror ref") || !strings.Contains(err.Error(), private[1]) {
				t.Fatalf("inexact owned heads %v skipped the content proof: %v", owned, err)
			}
			assertLineageMirrorUntouched(t, gateDir, private[1])
		})
	}
}

func TestPlanStaleBranchLineageHeadNeverCoversAnUnseenDescendant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, private, liveHead := rewrittenRebaseFixture(t)

	reconcileGit(t, work, "checkout", "-q", "--detach", private[1])
	writeReconcileFile(t, work, "unseen.txt", "work nobody submitted from here\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "unseen mirror commit")
	unseen := reconcileGit(t, work, "rev-parse", "HEAD")
	reconcileGit(t, gateDir, "fetch", work, "+"+unseen+":refs/heads/feature/rewritten")

	for _, plan := range []func(...string) (StaleBranchPlan, error){
		func(owned ...string) (StaleBranchPlan, error) {
			return PlanStaleBranchReconciliation(ctx, gateDir, work, "feature/rewritten", liveHead, owned...)
		},
		func(owned ...string) (StaleBranchPlan, error) {
			return PlanMirrorPublicationReconciliation(ctx, gateDir, work, "feature/rewritten", liveHead, owned...)
		},
	} {
		_, err := plan(private...)
		if err == nil || !strings.Contains(err.Error(), unseen) {
			t.Fatalf("owned ancestor heads excused an unseen descendant: %v", err)
		}
		assertLineageMirrorUntouched(t, gateDir, unseen)
	}
}
