package branchsync

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// handRebaseRemote reproduces the operator action that strands a branch: the
// remote feature branch is rewritten off the run's base and force-pushed, so
// the run's recorded pushed head is no longer reachable from the live remote
// and no fetch, rebase, or retry can ever make the binding true again.
func handRebaseRemote(t *testing.T, f *syncFixture) string {
	t.Helper()
	root := filepath.Dir(f.local)
	hand := filepath.Join(root, "hand-rebase")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", f.remote, hand)
	configureIdentity(t, hand)
	mustRun(t, hand, "checkout", "-B", "feature/sync", f.base)
	mustWrite(t, filepath.Join(hand, "file.txt"), "hand rebased\n")
	mustRun(t, hand, "commit", "-am", "hand rebase")
	rewritten := mustRun(t, hand, "rev-parse", "HEAD")
	mustRun(t, hand, "push", "--force", f.remote, "HEAD:refs/heads/feature/sync")
	return rewritten
}

// clearSubmittedHead reproduces a run recorded before submitted_head_sha
// existed. There is deliberately no DB API for this: production code only ever
// writes that column, so the legacy shape is reachable only by editing the row
// the way an old database already holds it.
func clearSubmittedHead(t *testing.T, database *db.DB, root, runID string) {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE runs SET submitted_head_sha = NULL WHERE id = ?`, runID); err != nil {
		t.Fatalf("clear submitted head: %v", err)
	}
	if run := reloadRun(t, database, runID); run.SubmittedHeadSHA != nil {
		t.Fatal("submitted head was not cleared")
	}
}

func reloadRun(t *testing.T, database *db.DB, id string) *db.Run {
	t.Helper()
	run, err := database.GetRun(id)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if run == nil {
		t.Fatalf("run %s disappeared", id)
	}
	return run
}

// TestRemoteRewrittenTerminalRunOffersRetireCustody proves the reported dead
// end is gone: a terminal run whose remote was hand-rebased now names the
// supported exit in next_action instead of blocking with no way forward.
func TestRemoteRewrittenTerminalRunOffersRetireCustody(t *testing.T) {
	f := newSyncFixture(t)
	handRebaseRemote(t, f)

	state := f.service.Refresh(f.ctx)
	if state.Safety != "blocked_remote_rewritten" {
		t.Fatalf("safety = %q, want blocked_remote_rewritten: %#v", state.Safety, state)
	}
	if state.NextAction == nil {
		t.Fatal("a terminal run blocked on a rewritten remote offered no next_action; that is the dead end this command exists to remove")
	}
	if state.NextAction.Code != "retire_custody" {
		t.Fatalf("next_action.code = %q, want retire_custody", state.NextAction.Code)
	}
	if want := "no-mistakes axi retire-custody --run " + f.run.ID; state.NextAction.Command != want {
		t.Fatalf("next_action.command = %q, want %q", state.NextAction.Command, want)
	}
}

// TestRemoteRewrittenActiveRunOffersNoRetireCustody proves the offer is gated
// on proof the run is finished: while a run is active the pipeline may still
// publish, so advertising a command that would refuse anyway is wrong.
func TestRemoteRewrittenActiveRunOffersNoRetireCustody(t *testing.T) {
	f := newSyncFixture(t)
	handRebaseRemote(t, f)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	state := f.service.Refresh(f.ctx)
	if state.Safety != "blocked_remote_rewritten" {
		t.Fatalf("safety = %q, want blocked_remote_rewritten: %#v", state.Safety, state)
	}
	if state.NextAction != nil {
		t.Fatalf("an active run offered next_action %#v; retiring a live run's binding is never supported", state.NextAction)
	}
}

// TestRetireCustodyReleasesStaleBindingAndReportsCustodyReturned is the happy
// path: the released binding is reported, and the branch that was permanently
// blocked reports a clean custody_returned state afterwards.
func TestRetireCustodyReleasesStaleBindingAndReportsCustodyReturned(t *testing.T) {
	f := newSyncFixture(t)
	handRebaseRemote(t, f)
	localHeadBefore := mustRun(t, f.local, "rev-parse", "HEAD")

	retired, err := f.service.RetireCustody(f.run.ID)
	if err != nil {
		t.Fatalf("retire custody: %v", err)
	}
	if !retired.Released || !retired.CustodyStamped {
		t.Fatalf("retirement = %#v, want a released binding and a fresh custody stamp", retired)
	}
	if retired.ReleasedHead != f.pushed {
		t.Fatalf("released pushed_head = %q, want the stale binding %q", retired.ReleasedHead, f.pushed)
	}
	if retired.ReleasedRef != "refs/heads/feature/sync" || retired.Branch != "feature/sync" {
		t.Fatalf("retirement did not name what it released: %#v", retired)
	}

	after := reloadRun(t, f.db, f.run.ID)
	if after.LastPushedSHA != nil || after.PushRef != nil || after.PushTargetKind != nil || after.PushTargetFingerprint != nil || after.LastPushedAt != nil {
		t.Fatalf("push binding survived retirement: %#v", after)
	}
	if after.CustodyReturnedAt == nil {
		t.Fatal("custody was not recorded as returned")
	}
	// Nothing was adopted or pushed: the run's own head evidence and the
	// operator's worktree are untouched.
	if after.HeadSHA != f.pushed || ptr(after.SubmittedHeadSHA) != f.old {
		t.Fatalf("retirement rewrote the run's head evidence: head=%s submitted=%s", after.HeadSHA, ptr(after.SubmittedHeadSHA))
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != localHeadBefore {
		t.Fatalf("retirement moved the local head to %s, want %s", got, localHeadBefore)
	}

	state := f.service.InspectCached(f.ctx)
	if state.State != StateCustodyReturned || state.Safety != "custody_returned" {
		t.Fatalf("post-retirement state = %#v, want a clean custody_returned reading", state)
	}
	if state.Error != "" {
		t.Fatalf("post-retirement state still reports an error: %q", state.Error)
	}
	// Refresh is the surface that was permanently blocked; it must now report
	// the same clean state without contacting a remote at all.
	refreshed := f.service.Refresh(f.ctx)
	if refreshed.State != StateCustodyReturned {
		t.Fatalf("post-retirement refresh = %#v, want custody_returned", refreshed)
	}
}

// TestRetireCustodyReportsNoStalePipelinePushObservation proves the released
// binding stops being claimed as an observed remote head: a pipeline_push
// freshness with no head is a push observation for a binding that is gone.
func TestRetireCustodyReportsNoStalePipelinePushObservation(t *testing.T) {
	f := newSyncFixture(t)
	handRebaseRemote(t, f)
	if _, err := f.service.RetireCustody(f.run.ID); err != nil {
		t.Fatalf("retire custody: %v", err)
	}

	state := f.service.InspectCached(f.ctx)
	if state.Remote.ObservedHead != "" {
		t.Fatalf("observed_head = %q after the binding was released", state.Remote.ObservedHead)
	}
	if state.Remote.Freshness == "pipeline_push" {
		t.Fatalf("freshness = pipeline_push with no pushed head: %#v", state.Remote)
	}
}

// TestRetireCustodyRefusesActiveRun proves the refusal is total: an active run
// keeps every field of its binding and its unstamped custody.
func TestRetireCustodyRefusesActiveRun(t *testing.T) {
	f := newSyncFixture(t)
	for _, status := range []types.RunStatus{types.RunPending, types.RunRunning} {
		if err := f.db.UpdateRunStatus(f.run.ID, status); err != nil {
			t.Fatal(err)
		}
		_, err := f.service.RetireCustody(f.run.ID)
		if err == nil {
			t.Fatalf("retire custody succeeded on a %s run", status)
		}
		if !strings.Contains(err.Error(), "still active") {
			t.Fatalf("refusal for %s did not name the cause: %v", status, err)
		}
		after := reloadRun(t, f.db, f.run.ID)
		if ptr(after.LastPushedSHA) != f.pushed || after.CustodyReturnedAt != nil {
			t.Fatalf("refused retirement still wrote to a %s run: %#v", status, after)
		}
	}
}

// TestRetireCustodyRefusesRunWithInFlightPushMarker proves a terminal status
// alone is not accepted as proof no push is in flight: a crash between the
// push marker and terminalization must fail closed.
func TestRetireCustodyRefusesRunWithInFlightPushMarker(t *testing.T) {
	f := newSyncFixture(t)
	if err := f.db.SetRunPushActive(f.run.ID, true); err != nil {
		t.Fatal(err)
	}

	_, err := f.service.RetireCustody(f.run.ID)
	if err == nil {
		t.Fatal("retire custody released a binding while a push marker was set")
	}
	if !strings.Contains(err.Error(), "in-flight push") {
		t.Fatalf("refusal did not name the cause: %v", err)
	}
	after := reloadRun(t, f.db, f.run.ID)
	if ptr(after.LastPushedSHA) != f.pushed || after.CustodyReturnedAt != nil {
		t.Fatalf("refused retirement still wrote: %#v", after)
	}
}

// TestRetireCustodyRefusesUnknownAndForeignRuns proves the command cannot be
// aimed at a run this repository does not own.
func TestRetireCustodyRefusesUnknownAndForeignRuns(t *testing.T) {
	f := newSyncFixture(t)

	if _, err := f.service.RetireCustody(""); err == nil {
		t.Fatal("retire custody accepted an empty run id")
	}
	if _, err := f.service.RetireCustody("no-such-run"); err == nil {
		t.Fatal("retire custody accepted an unknown run id")
	}

	other, err := f.db.InsertRepo(filepath.Join(filepath.Dir(f.local), "other"), f.remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.db.InsertRun(other.ID, "feature/sync", f.old, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(foreign.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RetireCustody(foreign.ID); err == nil {
		t.Fatal("retire custody accepted a run from another repository")
	}
	if reloadRun(t, f.db, foreign.ID).CustodyReturnedAt != nil {
		t.Fatal("refused retirement stamped custody on another repository's run")
	}
}

// TestRetireCustodyRepeatedCallIsASuccessfulNoOp proves a second invocation
// neither fails nor re-reports a release that already happened.
func TestRetireCustodyRepeatedCallIsASuccessfulNoOp(t *testing.T) {
	f := newSyncFixture(t)
	handRebaseRemote(t, f)

	first, err := f.service.RetireCustody(f.run.ID)
	if err != nil || !first.Released {
		t.Fatalf("first retirement = %#v, err = %v", first, err)
	}
	stampedAt := reloadRun(t, f.db, f.run.ID).CustodyReturnedAt

	second, err := f.service.RetireCustody(f.run.ID)
	if err != nil {
		t.Fatalf("repeated retirement failed: %v", err)
	}
	if second.Released || second.CustodyStamped {
		t.Fatalf("repeated retirement reported a second release: %#v", second)
	}
	if got := reloadRun(t, f.db, f.run.ID).CustodyReturnedAt; got == nil || *got != *stampedAt {
		t.Fatal("repeated retirement moved the original custody moment")
	}
}

// TestRetiredLegacyRunWithoutSubmittedHeadReportsCustodyReturned covers the
// shape the per-shape custody checks used to miss: a legacy row with no
// submitted head fell through to blocked_legacy_unbound, reporting a branch the
// operator had just been handed back as unsynchronizable.
func TestRetiredLegacyRunWithoutSubmittedHeadReportsCustodyReturned(t *testing.T) {
	f := newSyncFixture(t)
	clearSubmittedHead(t, f.db, filepath.Dir(f.local), f.run.ID)

	if _, err := f.service.RetireCustody(f.run.ID); err != nil {
		t.Fatalf("retire custody: %v", err)
	}
	state := f.service.InspectCached(f.ctx)
	if state.State != StateCustodyReturned {
		t.Fatalf("legacy retired run state = %q (safety %q), want custody_returned", state.State, state.Safety)
	}
}

// TestRecoverPreservedHeadMissingOffersRetireCustody proves the second reported
// dead end is gone: a plain --recover that cannot import a head the gate no
// longer holds now names the supported exit instead of refusing with nothing.
func TestRecoverPreservedHeadMissingOffersRetireCustody(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	if err := f.db.UpdateRunHeadSHA(f.run.ID, strings.Repeat("f", 40)); err != nil {
		t.Fatal(err)
	}

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Safety != "blocked_recover_preserved_head_missing" {
		t.Fatalf("plain recover with missing head = %#v", refused)
	}
	if refused.NextAction == nil {
		t.Fatal("a missing preserved head refused with no next_action; that is the dead end this command exists to remove")
	}
	if refused.NextAction.Code != "retire_custody" {
		t.Fatalf("next_action.code = %q, want retire_custody", refused.NextAction.Code)
	}
	if want := "no-mistakes axi retire-custody --run " + f.run.ID; refused.NextAction.Command != want {
		t.Fatalf("next_action.command = %q, want %q", refused.NextAction.Command, want)
	}
	if f.custodyReturned() {
		t.Fatal("the refusal stamped custody")
	}
}

// TestInspectKeepLocalOfferSurvivesTheRetireCustodyOffer pins the precedence:
// where the stricter missing-head proof holds, status still offers the
// keep-local recovery, which settles the local gate branch too. Retirement is
// the fallback for the refusals that offer nothing, never a replacement.
func TestInspectKeepLocalOfferSurvivesTheRetireCustodyOffer(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	if err := f.db.UpdateRunHeadSHA(f.run.ID, strings.Repeat("f", 40)); err != nil {
		t.Fatal(err)
	}

	state := f.service.InspectCached(f.ctx)
	if state.Safety != "blocked_recover_preserved_head_missing" {
		t.Fatalf("safety = %q, want blocked_recover_preserved_head_missing: %#v", state.Safety, state)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("next_action = %#v, want the keep-local recover_custody offer", state.NextAction)
	}
	if state.NextAction.Command != "no-mistakes axi sync --recover --keep-local" {
		t.Fatalf("next_action.command = %q", state.NextAction.Command)
	}
}
