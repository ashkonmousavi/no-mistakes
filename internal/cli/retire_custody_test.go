package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// retireCustodyCall runs the command against the fixture store and reports its
// stdout document plus the exit code an agent would observe.
func retireCustodyCall(t *testing.T, runID string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	err := runAxiRetireCustody(cmd, runID)
	if err == nil {
		return out.String(), 0
	}
	var exit *exitError
	if !errors.As(err, &exit) {
		t.Fatalf("unexpected error type %T: %v\n%s", err, err, out.String())
	}
	return out.String(), exit.code
}

// strandedRun records a terminal run holding a successful push binding on the
// checked-out branch: the shape a hand rebase leaves permanently blocked.
func strandedRun(t *testing.T, database *db.DB, repo *db.Repo, branch string) *db.Run {
	t.Helper()
	run, err := database.InsertRun(repo.ID, branch, "head-submitted", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, "head-pushed"); err != nil {
		t.Fatalf("update head: %v", err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{
		HeadSHA:           "head-pushed",
		TargetKind:        "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(repo.PushURL()),
		Ref:               "refs/heads/" + branch,
	}); err != nil {
		t.Fatalf("bind push: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatalf("terminalize run: %v", err)
	}
	return run
}

// TestAxiRetireCustodyRequiresAnExplicitRun proves the command is never
// inferred from the current branch: releasing a binding is per-run, and a bare
// invocation exits with the usage code rather than guessing a target.
func TestAxiRetireCustodyRequiresAnExplicitRun(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/stranded")
	chdir(t, repoDir)
	stranded := strandedRun(t, database, repo, "feature/stranded")

	out, code := retireCustodyCall(t, "")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a missing required flag:\n%s", code, out)
	}
	if !strings.Contains(out, "--run") {
		t.Fatalf("refusal did not name the missing flag:\n%s", out)
	}
	after, err := database.GetRun(stranded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastPushedSHA == nil || after.CustodyReturnedAt != nil {
		t.Fatalf("a refused invocation still released something: %#v", after)
	}
}

// TestAxiRetireCustodyRefusesAnActiveRun proves the active-run refusal reaches
// the agent as a nonzero exit with the binding and custody record untouched.
func TestAxiRetireCustodyRefusesAnActiveRun(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/live")
	chdir(t, repoDir)
	live := strandedRun(t, database, repo, "feature/live")
	if err := database.UpdateRunStatus(live.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	out, code := retireCustodyCall(t, live.ID)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for an active run:\n%s", code, out)
	}
	if !strings.Contains(out, "still active") {
		t.Fatalf("refusal did not name the cause:\n%s", out)
	}
	after, err := database.GetRun(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastPushedSHA == nil || after.CustodyReturnedAt != nil {
		t.Fatalf("a refused invocation still released something: %#v", after)
	}
}

// TestAxiRetireCustodyRefusesAnUnknownRun proves an unresolvable run id is a
// refusal rather than a silent success against some other run.
func TestAxiRetireCustodyRefusesAnUnknownRun(t *testing.T) {
	repoDir, _, _, _ := setupAxiQueryRepo(t)
	chdir(t, repoDir)

	out, code := retireCustodyCall(t, "no-such-run")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for an unknown run:\n%s", code, out)
	}
	if !strings.Contains(out, "not found") {
		t.Fatalf("refusal did not say the run was not found:\n%s", out)
	}
}

// TestAxiRetireCustodyReleasesTheBindingAndReportsACleanBranch is the happy
// path an agent drives: the released binding is named, and the branch that was
// stranded reads back as custody_returned with a fresh-run next action.
func TestAxiRetireCustodyReleasesTheBindingAndReportsACleanBranch(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/stranded")
	chdir(t, repoDir)
	stranded := strandedRun(t, database, repo, "feature/stranded")

	out, code := retireCustodyCall(t, stranded.ID)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{
		"retired: true",
		stranded.ID,
		"feature/stranded",
		"pushed_head: head-pushed",
		"push_ref: refs/heads/feature/stranded",
		"custody: true",
		"state: custody_returned",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing %q:\n%s", want, out)
		}
	}

	after, err := database.GetRun(stranded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastPushedSHA != nil || after.PushRef != nil || after.PushTargetFingerprint != nil || after.PushTargetKind != nil {
		t.Fatalf("push binding survived: %#v", after)
	}
	if after.CustodyReturnedAt == nil {
		t.Fatal("custody was not recorded as returned")
	}
	// No head was adopted: the run's recorded pipeline head is unchanged.
	if after.HeadSHA != "head-pushed" {
		t.Fatalf("head_sha = %s, want the untouched recorded head", after.HeadSHA)
	}

	// The acceptance criterion: sync reports a clean state afterwards.
	var syncOut bytes.Buffer
	syncCmd := &cobra.Command{}
	syncCmd.SetContext(context.Background())
	syncCmd.SetOut(&syncOut)
	if err := runAxiSync(syncCmd, true, false, false, ""); err != nil {
		t.Fatalf("axi sync --check after retirement: %v\n%s", err, syncOut.String())
	}
	if !strings.Contains(syncOut.String(), "state: custody_returned") {
		t.Fatalf("axi sync did not report a clean branch:\n%s", syncOut.String())
	}
}

// TestAxiRetireCustodyRepeatedCallIsASuccessfulNoOp proves a driving agent can
// safely re-run the offered command without a spurious failure.
func TestAxiRetireCustodyRepeatedCallIsASuccessfulNoOp(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/stranded")
	chdir(t, repoDir)
	stranded := strandedRun(t, database, repo, "feature/stranded")

	if _, code := retireCustodyCall(t, stranded.ID); code != 0 {
		t.Fatalf("first retirement exit = %d", code)
	}
	out, code := retireCustodyCall(t, stranded.ID)
	if code != 0 {
		t.Fatalf("repeated retirement exit = %d, want a successful no-op:\n%s", code, out)
	}
	if !strings.Contains(out, "retired: false") || !strings.Contains(out, "no-op") {
		t.Fatalf("repeated retirement did not report a no-op:\n%s", out)
	}
}
