package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakePreRunHost is a scm.Host that also reports pre-run infrastructure
// failures from a fixed set, so markPreRunInfraFailures can be exercised without
// a live provider. The embedded nil scm.Host is never called: the CI step only
// invokes the detector method here.
type fakePreRunHost struct {
	scm.Host
	flagged map[string]bool
	byLink  map[string]bool
	calls   int
}

type fakeArtifactInfrastructureHost struct {
	scm.Host
	results []scm.InfrastructureFailure
	calls   int
}

type fakeCheckRerunner struct {
	scm.Host
	calls  int
	target scm.PRTarget
}

func (h *fakeCheckRerunner) RerunCheck(context.Context, *scm.PR, scm.Check) error {
	h.calls++
	return nil
}

func (h *fakeCheckRerunner) GetPRTarget(_ context.Context, pr *scm.PR) (scm.PRTarget, error) {
	if h.target.HeadSHA != "" || h.target.BaseBranch != "" || h.target.BaseSHA != "" {
		return h.target, nil
	}
	return scm.PRTarget{HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseBranch, BaseSHA: pr.BaseSHA}, nil
}

func (h *fakeArtifactInfrastructureHost) ArtifactInfrastructureFailures(_ context.Context, _ *scm.PR, _ []scm.Check) ([]scm.InfrastructureFailure, error) {
	h.calls++
	return append([]scm.InfrastructureFailure(nil), h.results...), nil
}

func (h *fakePreRunHost) PreRunFailures(_ context.Context, checks []scm.Check) ([]bool, error) {
	h.calls++
	out := make([]bool, len(checks))
	for i, c := range checks {
		out[i] = h.flagged[c.Name] || h.byLink[c.Link]
	}
	return out, nil
}

func markContext(t *testing.T, rerunBudget int) *pipeline.StepContext {
	t.Helper()
	return &pipeline.StepContext{
		Ctx:    context.Background(),
		Config: &config.Config{CI: config.CI{RerunTransient: rerunBudget}},
		Log:    func(string) {},
	}
}

func markInfrastructureContext(t *testing.T, rerunBudget int) *pipeline.StepContext {
	t.Helper()
	return &pipeline.StepContext{
		Ctx:    context.Background(),
		Config: &config.Config{CI: config.CI{RerunInfrastructure: rerunBudget}},
		Log:    func(string) {},
	}
}

// Artifact-service failures are marked without borrowing the cancellation
// budget or changing their failed bucket. Their separate class is what admits
// exactly the bounded infrastructure path while preserving the original CI
// failure for the run record and any later escalation.
func TestMarkArtifactInfrastructureFailures_UsesSeparateClassAndPreservesFailure(t *testing.T) {
	t.Parallel()

	checks := []scm.Check{{Name: "browser", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "job-link"}}
	host := &fakeArtifactInfrastructureHost{results: []scm.InfrastructureFailure{{Retryable: true, Group: "run:1", Reason: "artifact transfer service"}}}
	pr := &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main"}
	markArtifactInfrastructureFailures(markInfrastructureContext(t, 1), host, pr, checks)

	if host.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1", host.calls)
	}
	if !checks[0].InfrastructureFailure || checks[0].InfrastructureGroup != "run:1" {
		t.Fatalf("check = %+v, want infrastructure evidence", checks[0])
	}
	if checks[0].Bucket != scm.CheckBucketFail || checks[0].State != "FAILURE" {
		t.Fatalf("marking rewrote first failure: %+v", checks[0])
	}
	if got := classifyCheckFailure(checks[0]); got != classInfrastructure {
		t.Fatalf("classifyCheckFailure = %q, want %q", got, classInfrastructure)
	}
}

// The new provider read is opt-in under its own key. Enabling cancelled-check
// retries alone must not make artifact classification or spend its API budget.
func TestMarkArtifactInfrastructureFailures_DoesNotBorrowTransientBudget(t *testing.T) {
	t.Parallel()

	checks := []scm.Check{{Name: "browser", Bucket: scm.CheckBucketFail, State: "FAILURE"}}
	host := &fakeArtifactInfrastructureHost{results: []scm.InfrastructureFailure{{Retryable: true}}}
	sctx := markInfrastructureContext(t, 0)
	sctx.Config.CI.RerunTransient = config.MaxCIRerunTransient
	markArtifactInfrastructureFailures(sctx, host, &scm.PR{Number: "42"}, checks)
	if host.calls != 0 || checks[0].InfrastructureFailure {
		t.Fatalf("transient budget activated infrastructure classifier: calls=%d check=%+v", host.calls, checks[0])
	}
}

// A GitHub Actions action-download outage fails a job in setup, before any
// repository step runs. markPreRunInfraFailures must route that into the
// transient path (re-run, do not fail the run), while a genuine test failure
// that cleared setup stays a real failure. Proving both directions here is the
// masking-safety contract: infra retried, real failure still fails.
func TestMarkPreRunInfraFailures_RetriesInfraButNotGenuine(t *testing.T) {
	t.Parallel()

	infra := scm.Check{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/o/r/actions/runs/1/job/2"}
	genuine := scm.Check{Name: "unit-tests", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/o/r/actions/runs/1/job/3"}
	checks := []scm.Check{infra, genuine}

	host := &fakePreRunHost{flagged: map[string]bool{"build": true}}
	markPreRunInfraFailures(markContext(t, 1), host, checks)

	// The infra failure is re-bucketed out of the failing set and classifies as
	// transient (re-runnable), so it never reaches the fix agent as a failure.
	if !checks[0].PreRunFailure || checks[0].Bucket != scm.CheckBucketCancel {
		t.Fatalf("infra check = %+v, want PreRunFailure with cancel bucket", checks[0])
	}
	if got := classifyCheckFailure(checks[0]); got != classTransient {
		t.Fatalf("infra classifyCheckFailure = %q, want %q", got, classTransient)
	}
	// The genuine failure is untouched: still a fail-bucket, genuine failure that
	// fails the run. This is the no-masking guarantee.
	if checks[1].PreRunFailure || checks[1].Bucket != scm.CheckBucketFail {
		t.Fatalf("genuine check = %+v, want untouched fail bucket", checks[1])
	}
	if got := classifyCheckFailure(checks[1]); got != classGenuine {
		t.Fatalf("genuine classifyCheckFailure = %q, want %q", got, classGenuine)
	}
	if names := failingCheckNames(checks); len(names) != 1 || names[0] != "unit-tests" {
		t.Fatalf("failingCheckNames = %v, want only the genuine failure", names)
	}
}

// The detection is opt-in: with the default rerun budget of 0 the provider is
// never consulted and an action-download outage fails the run exactly as before.
func TestMarkPreRunInfraFailures_OptInGated(t *testing.T) {
	t.Parallel()

	checks := []scm.Check{{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/o/r/actions/runs/1/job/2"}}
	host := &fakePreRunHost{flagged: map[string]bool{"build": true}}

	markPreRunInfraFailures(markContext(t, 0), host, checks)

	if host.calls != 0 {
		t.Fatalf("detector called %d times with reruns disabled, want 0", host.calls)
	}
	if checks[0].PreRunFailure || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("check = %+v, want untouched when opted out", checks[0])
	}
}

// Check names are not unique on a PR: two workflows can both name a job
// "build". If one fails at setup (infra) and the other fails a real repository
// step, only the infra one may be re-bucketed. A positional detector result
// keeps them apart; a name-keyed one would flag both and mask the real failure.
func TestMarkPreRunInfraFailures_SameNameGenuineNotMasked(t *testing.T) {
	t.Parallel()

	infra := scm.Check{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/o/r/actions/runs/1/job/2"}
	genuine := scm.Check{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/o/r/actions/runs/9/job/3"}
	checks := []scm.Check{infra, genuine}

	host := &fakePreRunHost{flagged: map[string]bool{}, byLink: map[string]bool{infra.Link: true}}
	markPreRunInfraFailures(markContext(t, 1), host, checks)

	if !checks[0].PreRunFailure || checks[0].Bucket != scm.CheckBucketCancel {
		t.Fatalf("infra check = %+v, want re-bucketed transient", checks[0])
	}
	if checks[1].PreRunFailure || checks[1].Bucket != scm.CheckBucketFail {
		t.Fatalf("same-named genuine check = %+v, want untouched fail bucket (no masking)", checks[1])
	}
}

func TestClassifyCheckFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		check scm.Check
		want  failureClass
	}{
		{"cancelled job", scm.Check{Name: "test", Bucket: scm.CheckBucketCancel, State: "CANCELLED"}, classTransient},
		{"cancelled job in the fail bucket", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "CANCELLED"}, classTransient},
		{"american spelling", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "CANCELED"}, classTransient},
		{"lowercase state", scm.Check{Name: "test", Bucket: scm.CheckBucketCancel, State: "cancelled"}, classTransient},

		{"job failure", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "FAILURE"}, classGenuine},
		// A failure the provider produced before any repository step ran (a
		// setup/action-download outage) is infrastructure, not a code verdict: it
		// is re-runnable even though its state is still FAILURE.
		{"pre-run infrastructure failure", scm.Check{Name: "test", Bucket: scm.CheckBucketCancel, State: "FAILURE", PreRunFailure: true}, classTransient},
		{"artifact infrastructure failure is separate", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "FAILURE", InfrastructureFailure: true}, classInfrastructure},
		// The same FAILURE state without the pre-run mark is a genuine failure and
		// must never be re-run: this is what keeps a real test failure from being
		// masked as infrastructure.
		{"failure that cleared setup stays genuine", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "FAILURE", PreRunFailure: false}, classGenuine},
		{"job error", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "ERROR"}, classGenuine},
		{"action required", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "ACTION_REQUIRED"}, classGenuine},
		// A workflow that cannot start is reproducible (bad workflow file), not
		// something a rerun clears.
		{"startup failure", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "STARTUP_FAILURE"}, classGenuine},
		// A job that exceeds its own timeout-minutes is usually the branch's own
		// code hanging, and a rerun burns another full timeout window
		// reproducing it.
		{"timed out job", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "TIMED_OUT"}, classGenuine},

		{"failed with no reported state", scm.Check{Name: "test", Bucket: scm.CheckBucketFail}, classUnknown},
		{"failed with an unrecognized state", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "QUARANTINED"}, classUnknown},
		// STALE has one owner: normalizeCheckBucket treats it as skipped, so it
		// is not a terminal failure at all. The fail-bucket pairing a provider
		// could still report must not become a rerun either, or the outcome
		// would depend on whether a bucket was reported.
		{"stale check as the normalizer maps it", scm.Check{Name: "test", Bucket: scm.CheckBucketSkip, State: "STALE"}, classUnknown},
		{"stale check in the fail bucket", scm.Check{Name: "test", Bucket: scm.CheckBucketFail, State: "STALE"}, classUnknown},
		{"still pending", scm.Check{Name: "test", Bucket: scm.CheckBucketPending, State: "IN_PROGRESS"}, classUnknown},
		{"passing", scm.Check{Name: "test", Bucket: scm.CheckBucketPass, State: "SUCCESS"}, classUnknown},
		{"skipped", scm.Check{Name: "test", Bucket: scm.CheckBucketSkip, State: "SKIPPED"}, classUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCheckFailure(tc.check); got != tc.want {
				t.Fatalf("classifyCheckFailure(%+v) = %q, want %q", tc.check, got, tc.want)
			}
		})
	}
}

// A setup failure uses the cancel-shaped policy path, but it is still a failed
// setup rather than a provider cancellation. The approval result must preserve
// that cause after the rerun budget is spent so the user can make the decision
// with an accurate diagnosis.
func TestCIUnresolvedCancelledOutcomePreservesPreRunFailureCause(t *testing.T) {
	t.Parallel()

	outcome := ciObservationOutcome(ciObservationFindings(ciIssues{
		checks:              []scm.Check{{Name: "build", Bucket: scm.CheckBucketCancel, State: "FAILURE", PreRunFailure: true}},
		unresolvedCancelled: []string{"build"},
		reruns:              func(string) int { return 1 },
	}))
	if !outcome.NeedsApproval {
		t.Fatal("pre-run failure after its rerun must require approval")
	}
	if outcome.AutoFixable {
		t.Fatal("a transient check no rerun will replace must never be auto-fixable")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if findings.Summary != "CI checks failed before repository steps ran" {
		t.Fatalf("summary = %q, want setup-failure diagnosis", findings.Summary)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one setup-failure finding", findings.Items)
	}
	description := findings.Items[0].Description
	if !strings.Contains(description, "failed during setup again after its rerun") {
		t.Fatalf("description = %q, want setup-failure diagnosis", description)
	}
	if strings.Contains(description, "provider cancelled") {
		t.Fatalf("description = %q, must not claim the provider cancelled a failed setup", description)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("action = %q, want ask-user parking", findings.Items[0].Action)
	}
	if findings.Items[0].Category != types.FindingCategoryCITransient || findings.Items[0].Check != "build" {
		t.Fatalf("finding = %+v, want a ci-transient finding naming its check", findings.Items[0])
	}
}

// Names identify shared rerun budgets, not unique checks. If two workflows use
// the same job name, the approval result must retain each check's own cause
// instead of letting a setup failure relabel its cancelled sibling.
func TestCIUnresolvedCancelledOutcomeKeepsSameNamedCausesPositional(t *testing.T) {
	t.Parallel()

	outcome := ciObservationOutcome(ciObservationFindings(ciIssues{
		checks: []scm.Check{
			{Name: "build", ProviderID: "github-check-run:41", Bucket: scm.CheckBucketCancel, State: "FAILURE", PreRunFailure: true},
			{Name: "build", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketCancel, State: "CANCELLED"},
		},
		unresolvedCancelled: []string{"build"},
		reruns:              func(string) int { return 1 },
	}))

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if findings.Summary != "CI checks ended without reporting a code verdict" {
		t.Fatalf("summary = %q, want mixed-cause diagnosis", findings.Summary)
	}
	if len(findings.Items) != 2 {
		t.Fatalf("findings = %+v, want one finding per same-named check", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "failed during setup") {
		t.Fatalf("first description = %q, want setup-failure diagnosis", findings.Items[0].Description)
	}
	if !strings.Contains(findings.Items[1].Description, "provider cancelled") {
		t.Fatalf("second description = %q, want cancellation diagnosis", findings.Items[1].Description)
	}
	if findings.Items[0].CheckID != "github-check-run:41" || findings.Items[1].CheckID != "github-check-run:42" {
		t.Fatalf("findings = %+v, want each transient check's provider identity", findings.Items)
	}
	targets, err := parseCIFixTargets(outcome.Findings)
	if err != nil || len(targets.Checks) != 2 || targets.Checks[0].ProviderID == targets.Checks[1].ProviderID {
		t.Fatalf("targets = %+v, %v, want two exact repair targets", targets.Checks, err)
	}
}

// A stale check must also stay non-terminal, so the classifier's verdict for it
// cannot be reached through a path that counts it as a failure.
func TestCheckFailedTerminallyMatchesTheBucketMapping(t *testing.T) {
	t.Parallel()

	for _, check := range []scm.Check{
		{Name: "test", Bucket: scm.CheckBucketSkip, State: "STALE"},
		{Name: "test", Bucket: scm.CheckBucketSkip, State: "SKIPPED"},
		{Name: "test", Bucket: scm.CheckBucketPass, State: "SUCCESS"},
		{Name: "test", Bucket: scm.CheckBucketPending, State: "QUEUED"},
	} {
		if checkFailedTerminally(check) {
			t.Fatalf("checkFailedTerminally(%+v) = true, want false", check)
		}
	}
	for _, check := range []scm.Check{
		{Name: "test", Bucket: scm.CheckBucketFail, State: "FAILURE"},
		{Name: "test", Bucket: scm.CheckBucketCancel, State: "CANCELLED"},
	} {
		if !checkFailedTerminally(check) {
			t.Fatalf("checkFailedTerminally(%+v) = false, want true", check)
		}
	}
}

func TestTransientRerunCandidates(t *testing.T) {
	t.Parallel()

	transient := scm.Check{Name: "test", Bucket: scm.CheckBucketCancel, State: "CANCELLED"}
	genuine := scm.Check{Name: "lint", Bucket: scm.CheckBucketFail, State: "FAILURE"}
	timedOut := scm.Check{Name: "slow", Bucket: scm.CheckBucketFail, State: "TIMED_OUT"}
	unknown := scm.Check{Name: "audit", Bucket: scm.CheckBucketFail}
	passing := scm.Check{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS"}

	cases := []struct {
		name   string
		checks []scm.Check
		limit  int
		spent  map[string]int
		want   []string
	}{
		{
			name:   "cancelled check alongside passing checks",
			checks: []scm.Check{passing, transient},
			limit:  1,
			want:   []string{"test"},
		},
		{
			// The genuine failure needs the fix agent on its first failure, so a
			// cancelled sibling must not buy it another CI cycle.
			name:   "genuine failure present",
			checks: []scm.Check{transient, genuine},
			limit:  1,
			want:   nil,
		},
		{
			name:   "timed-out check present",
			checks: []scm.Check{transient, timedOut},
			limit:  1,
			want:   nil,
		},
		{
			name:   "indeterminate failure present",
			checks: []scm.Check{transient, unknown},
			limit:  1,
			want:   nil,
		},
		{
			name:   "budget already spent",
			checks: []scm.Check{transient},
			limit:  1,
			spent:  map[string]int{"test": 1},
			want:   nil,
		},
		{
			name:   "budget spent for one check only",
			checks: []scm.Check{transient, {Name: "e2e", Bucket: scm.CheckBucketCancel, State: "CANCELLED"}},
			limit:  1,
			spent:  map[string]int{"test": 1},
			want:   []string{"e2e"},
		},
		{
			// Check names are not unique on a PR: the same job name can come
			// from two workflow files, or from a matrix leg the provider reports
			// without a distinguishing suffix. Same-named checks share one budget
			// key, so a single poll must not select both and spend it twice.
			name:   "same name twice shares one budget",
			checks: []scm.Check{transient, transient},
			limit:  1,
			want:   []string{"test"},
		},
		{
			name:   "same name three times with a budget of two",
			checks: []scm.Check{transient, transient, transient},
			limit:  2,
			want:   []string{"test", "test"},
		},
		{
			name:   "same name twice with the budget partly spent",
			checks: []scm.Check{transient, transient},
			limit:  2,
			spent:  map[string]int{"test": 1},
			want:   []string{"test"},
		},
		{
			name:   "larger budget allows a second rerun of the same check",
			checks: []scm.Check{transient},
			limit:  2,
			spent:  map[string]int{"test": 1},
			want:   []string{"test"},
		},
		{
			name:   "reruns disabled",
			checks: []scm.Check{transient},
			limit:  0,
			want:   nil,
		},
		{
			name:   "no terminal failures",
			checks: []scm.Check{passing},
			limit:  1,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := checkRerunBudget{spent: tc.spent}
			got := transientRerunCandidates(tc.checks, &budget, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("candidates = %+v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i].Name != tc.want[i] {
					t.Fatalf("candidates = %+v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Selection and spending together are the whole cap, so they are walked
// together here: no sequence of polls may spend more reruns than the limit for
// one check name, which selecting purely on already-spent counts would allow
// within a single poll.
func TestTransientRerunSelectionCannotExceedTheBudget(t *testing.T) {
	t.Parallel()

	const limit = 2
	// Three same-named cancelled checks, offered again on every poll.
	checks := []scm.Check{
		{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED"},
		{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED"},
		{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED"},
	}

	budget := checkRerunBudget{}
	reruns := 0
	for poll := 0; poll < 5; poll++ {
		for _, candidate := range transientRerunCandidates(checks, &budget, limit) {
			if used := budget.spend(candidate, checks, "head"); used > limit {
				t.Fatalf("poll %d spent rerun %d of a %d rerun budget for %q", poll, used, limit, candidate.Name)
			}
			reruns++
		}
	}
	if reruns != limit {
		t.Fatalf("total reruns = %d, want %d", reruns, limit)
	}
}

// A budget of two or more must not let a lagging rollup buy a second rerun of a
// job that is already re-running: the request either bounces or bills the
// repository a duplicate workflow run. Selection declines while the recorded
// completion is still the one being observed, and the check spends rollup grace
// instead of budget.
func TestTransientRerunSelectionDeclinesWhileTheRollupIsUnrefreshed(t *testing.T) {
	t.Parallel()

	const limit = 2
	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	check := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED", CompletedAt: completed}

	budget := checkRerunBudget{}
	first := transientRerunCandidates([]scm.Check{check}, &budget, limit)
	if len(first) != 1 {
		t.Fatalf("first poll candidates = %+v, want the cancelled check", first)
	}
	budget.spend(first[0], []scm.Check{check}, "head")

	for poll := 1; poll <= rerunRollupGracePolls; poll++ {
		if got := transientRerunCandidates([]scm.Check{check}, &budget, limit); len(got) != 0 {
			t.Fatalf("poll %d selected %+v against an unrefreshed rollup, want none", poll, got)
		}
		// The grace this poll would have spent belongs to cancelledAfterRerun,
		// so selection must not have consumed any of it.
		unresolved, awaiting := budget.cancelledAfterRerun([]scm.Check{check})
		assertNames(t, "unresolved", unresolved, nil)
		assertNames(t, "awaiting", awaiting, []string{"build"})
	}

	// Once the grace is gone the first rerun is presumed never published, and
	// the remaining budget is free to try again.
	after := transientRerunCandidates([]scm.Check{check}, &budget, limit)
	if len(after) != 1 {
		t.Fatalf("candidates after the grace ran out = %+v, want the remaining budget to be usable", after)
	}
	if used := budget.spend(after[0], []scm.Check{check}, "head"); used != limit {
		t.Fatalf("spend after the grace ran out = %d, want %d", used, limit)
	}
	if got := budget.remaining("build", limit); got != 0 {
		t.Fatalf("remaining = %d, want the budget fully spent", got)
	}
}

// A refreshed rollup is a real second cancellation, so the remaining budget is
// available immediately rather than after the grace.
func TestTransientRerunSelectionResumesOnceTheRollupRefreshes(t *testing.T) {
	t.Parallel()

	const limit = 2
	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	check := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED", CompletedAt: completed}

	budget := checkRerunBudget{}
	budget.spend(check, []scm.Check{check}, "head")

	rerun := check
	rerun.CompletedAt = completed.Add(3 * time.Minute)
	got := transientRerunCandidates([]scm.Check{rerun}, &budget, limit)
	if len(got) != 1 {
		t.Fatalf("candidates = %+v, want the refreshed check to be selectable", got)
	}
}

// The accounting primitives only. The cap that matters is enforced by selection,
// which TestTransientRerunSelectionCannotExceedTheBudget covers.
func TestCheckRerunBudgetAccounting(t *testing.T) {
	t.Parallel()

	budget := checkRerunBudget{}
	if got := budget.remaining("test", 1); got != 1 {
		t.Fatalf("remaining before any attempt = %d, want 1", got)
	}
	if got := budget.used("test"); got != 0 {
		t.Fatalf("used before any attempt = %d, want 0", got)
	}
	check := scm.Check{Name: "test"}
	if got := budget.spend(check, []scm.Check{check}, "head"); got != 1 {
		t.Fatalf("spend = %d, want 1", got)
	}
	if got := budget.remaining("test", 1); got != 0 {
		t.Fatalf("remaining after the attempt = %d, want 0", got)
	}
	if got := budget.used("test"); got != 1 {
		t.Fatalf("used after the attempt = %d, want 1", got)
	}
	if got := budget.remaining("lint", 1); got != 1 {
		t.Fatalf("remaining for another check = %d, want 1 (budgets are per check name)", got)
	}
	if got := budget.remaining("test", 0); got != 0 {
		t.Fatalf("remaining with reruns disabled = %d, want 0", got)
	}
}

func TestCancelledChecksAfterRerun(t *testing.T) {
	t.Parallel()

	cancelled := scm.Check{Name: "test", Bucket: scm.CheckBucketCancel, State: "CANCELLED"}

	cases := []struct {
		name           string
		checks         []scm.Check
		spent          map[string]int
		wantUnresolved []string
		wantAwaiting   []string
	}{
		{
			// Nothing was spent on it, so behavior is what it was before this
			// policy existed: a cancelled check is not a failing check.
			name:   "never re-run",
			checks: []scm.Check{cancelled},
		},
		{
			// No completion timestamp on either side is no evidence at all, so
			// the check escalates exactly as it did before rollup lag was
			// accounted for.
			name:           "still cancelled after its rerun with no completion reported",
			checks:         []scm.Check{cancelled},
			spent:          map[string]int{"test": 1},
			wantUnresolved: []string{"test"},
		},
		{
			name:           "same name twice is reported once",
			checks:         []scm.Check{cancelled, cancelled},
			spent:          map[string]int{"test": 1},
			wantUnresolved: []string{"test"},
		},
		{
			name:   "a check that passed after its rerun is not an issue",
			checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketPass, State: "SUCCESS"}},
			spent:  map[string]int{"test": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := checkRerunBudget{spent: tc.spent}
			unresolved, awaiting := budget.cancelledAfterRerun(tc.checks)
			assertNames(t, "unresolved", unresolved, tc.wantUnresolved)
			assertNames(t, "awaiting", awaiting, tc.wantAwaiting)
		})
	}
}

// `gh run rerun` returns as soon as the provider accepts the request, while the
// new attempt replaces the cancelled check asynchronously. A poll that still
// reads the exact completion the rerun was requested for has observed nothing
// new, so it must not escalate a check that was never actually re-run.
func TestCancelledCheckAwaitsItsRerunBeforeBeingCalledUnresolved(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	check := scm.Check{Name: "test", Bucket: scm.CheckBucketCancel, State: "CANCELLED", CompletedAt: completed}

	budget := checkRerunBudget{}
	budget.spend(check, []scm.Check{check}, "head")

	for poll := 1; poll <= rerunRollupGracePolls; poll++ {
		unresolved, awaiting := budget.cancelledAfterRerun([]scm.Check{check})
		assertNames(t, "unresolved", unresolved, nil)
		assertNames(t, "awaiting", awaiting, []string{"test"})
	}

	// A provider that accepted the rerun and never published it must not stall
	// the monitor until its idle timeout.
	unresolved, awaiting := budget.cancelledAfterRerun([]scm.Check{check})
	assertNames(t, "unresolved after the grace ran out", unresolved, []string{"test"})
	assertNames(t, "awaiting after the grace ran out", awaiting, nil)
}

// Once the rerun's own attempt is visible, a check that came back cancelled is
// the provider reporting a second cancellation, not a stale rollup.
func TestRerunThatEndsCancelledAgainIsUnresolvedImmediately(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	check := scm.Check{Name: "test", Bucket: scm.CheckBucketCancel, State: "CANCELLED", CompletedAt: completed}

	budget := checkRerunBudget{}
	budget.spend(check, []scm.Check{check}, "head")

	rerun := check
	rerun.CompletedAt = completed.Add(4 * time.Minute)
	unresolved, awaiting := budget.cancelledAfterRerun([]scm.Check{rerun})
	assertNames(t, "unresolved", unresolved, []string{"test"})
	assertNames(t, "awaiting", awaiting, nil)
}

func TestMergeCheckNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		base  []string
		extra []string
		want  []string
	}{
		{name: "nothing to merge", base: []string{"lint"}, want: []string{"lint"}},
		{name: "joins the existing failing checks", base: []string{"lint"}, extra: []string{"test"}, want: []string{"lint", "test"}},
		{name: "never duplicates a name the base already carries", base: []string{"test"}, extra: []string{"test"}, want: []string{"test"}},
		{name: "deduplicates within extra", extra: []string{"test", "test"}, want: []string{"test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertNames(t, "merged", mergeCheckNames(tc.base, tc.extra), tc.want)
		})
	}
}

func assertNames(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

// A cancelled conclusion does not say who cancelled it, so rerunning is opt-in.
// This guards the default rather than the plumbing: flipping it back to a
// non-zero value would silently restart jobs a maintainer stopped on purpose.
func TestRerunningCancelledChecksIsOffByDefault(t *testing.T) {
	if config.DefaultCIRerunTransient != 0 {
		t.Fatalf("DefaultCIRerunTransient = %d, want 0", config.DefaultCIRerunTransient)
	}
	budget := &checkRerunBudget{}
	check := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED"}
	if got := transientRerunCandidates([]scm.Check{check}, budget, config.DefaultCIRerunTransient); len(got) != 0 {
		t.Fatalf("selected %d candidates at the default budget, want none", len(got))
	}
}

// One infrastructure retry belongs to the exact head/base candidate, not to a
// check name. A genuine sibling blocks the entire retry, and spending once on
// the candidate cannot be reset by another artifact check or daemon restart.
func TestInfrastructureRerunCandidates_OnePerCandidateAndGenuineSiblingBlocks(t *testing.T) {
	t.Parallel()

	infra := scm.Check{Name: "browser", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "job-1", InfrastructureFailure: true, InfrastructureGroup: "run:1", InfrastructureReason: "artifact transfer", InfrastructureHeadSHA: "head-1", InfrastructureBaseSHA: "base-1", InfrastructureRerunSafe: true, InfrastructureEvidence: scm.InfrastructureEvidenceReceipt{ProviderRunID: "1", Attempt: 1, LogJobIDs: []int64{2}}}
	budget := &infrastructureRerunBudget{}
	got := infrastructureRerunCandidates([]scm.Check{infra}, budget, 1, "head-1", "base-1")
	if len(got) != 1 {
		t.Fatalf("first candidate selection = %+v, want one", got)
	}
	budget.spend(got[0], []scm.Check{infra}, "head-1", "main", "base-1")
	if got := infrastructureRerunCandidates([]scm.Check{infra}, budget, 1, "head-1", "base-1"); len(got) != 0 {
		t.Fatalf("same candidate selected again: %+v", got)
	}
	newHead := infra
	newHead.InfrastructureHeadSHA = "head-2"
	if got := infrastructureRerunCandidates([]scm.Check{newHead}, budget, 1, "head-2", "base-1"); len(got) != 1 {
		t.Fatalf("new head candidate selection = %+v, want one", got)
	}

	genuine := scm.Check{Name: "unit", Bucket: scm.CheckBucketFail, State: "FAILURE"}
	if got := infrastructureRerunCandidates([]scm.Check{infra, genuine}, &infrastructureRerunBudget{}, 1, "head-1", "base-1"); len(got) != 0 {
		t.Fatalf("genuine sibling was masked by infrastructure retry: %+v", got)
	}
	omission := scm.Check{Name: "look for a proving pull request with the same pushed tree", Bucket: scm.CheckBucketSkip, State: "SKIPPED", InfrastructureGroup: "run:1", InfrastructureHeadSHA: "head-1", InfrastructureBaseSHA: "base-1", InfrastructureRerunSafe: true, InfrastructureRerunOmission: true}
	if got := infrastructureRerunCandidates([]scm.Check{infra, omission}, &infrastructureRerunBudget{}, 1, "head-1", "base-1"); len(got) != 1 {
		t.Fatalf("group-bound PR-only omission blocked the joined retry family: %+v", got)
	}
	dependent := scm.Check{Name: "repository", Bucket: scm.CheckBucketSkip, State: "SKIPPED", InfrastructureGroup: "run:1", InfrastructureHeadSHA: "head-1", InfrastructureBaseSHA: "base-1", InfrastructureRerunSafe: true, InfrastructureRerunDependent: true}
	if got := infrastructureRerunCandidates([]scm.Check{infra, omission, dependent}, &infrastructureRerunBudget{}, 1, "head-1", "base-1"); len(got) != 1 {
		t.Fatalf("group-bound skipped dependent blocked the joined retry family: %+v", got)
	}
	wrongGroup := omission
	wrongGroup.InfrastructureGroup = "run:2"
	if got := infrastructureRerunCandidates([]scm.Check{infra, wrongGroup}, &infrastructureRerunBudget{}, 1, "head-1", "base-1"); len(got) != 0 {
		t.Fatalf("cross-group omission was admitted: %+v", got)
	}
	for name, outside := range map[string]scm.Check{
		"cancelled": {Name: "browser", Bucket: scm.CheckBucketCancel, State: "CANCELLED"},
		"timed out": {Name: "browser", Bucket: scm.CheckBucketFail, State: "TIMED_OUT"},
		"unknown":   {Name: "browser", Bucket: scm.CheckBucketFail, State: "QUARANTINED"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := infrastructureRerunCandidates([]scm.Check{outside}, &infrastructureRerunBudget{}, 1, "head-1", "base-1"); len(got) != 0 {
				t.Fatalf("outside failure entered infrastructure retry: %+v", got)
			}
		})
	}
	for name, sibling := range map[string]scm.Check{
		"unknown sibling": {Name: "required", State: "QUARANTINED"},
		"pending sibling": {Name: "required", Bucket: scm.CheckBucketPending, State: "IN_PROGRESS"},
		"skipped sibling": {Name: "required", Bucket: scm.CheckBucketSkip, State: "SKIPPED"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := infrastructureRerunCandidates([]scm.Check{infra, sibling}, &infrastructureRerunBudget{}, 1, "head-1", "base-1"); len(got) != 0 {
				t.Fatalf("unproved sibling was masked by infrastructure retry: %+v", got)
			}
		})
	}
}

// The first failure is durable evidence, not a transient log line. Recovery
// must preserve its check, provider link, reason, head/base, and spent budget so
// it cannot issue a second same-candidate rerun after a daemon restart.
func TestInfrastructureRerunBudget_RestartRetainsFirstFailureAndSpentAttempt(t *testing.T) {
	t.Parallel()

	failedAt := time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC)
	check := scm.Check{Name: "browser", Bucket: scm.CheckBucketFail, State: "FAILURE", CompletedAt: failedAt, Link: "job-1", InfrastructureFailure: true, InfrastructureGroup: "run:1", InfrastructureReason: "FinalizeArtifact HTTP 503", InfrastructureHeadSHA: "head-1", InfrastructureBaseSHA: "base-1", InfrastructureRerunSafe: true, InfrastructureEvidence: scm.InfrastructureEvidenceReceipt{
		ProviderRunID: "1", Attempt: 1, LogJobIDs: []int64{2, 3}, DependentJobs: []int64{3, 4},
		DependentSteps: []scm.InfrastructureStepReceipt{{JobID: 2, Number: 7, Name: "Run tests"}, {JobID: 3, Number: 4, Name: "Prove fan-in"}},
		Artifacts:      []scm.InfrastructureArtifactReceipt{{ID: 91, Name: "bundle-a1", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ProviderRunID: 1, HeadSHA: "head-1"}},
	}}
	original := &infrastructureRerunBudget{}
	original.spend(check, []scm.Check{check}, "head-1", "main", "base-1")
	encoded, err := original.marshal()
	if err != nil {
		t.Fatal(err)
	}
	recovered := &infrastructureRerunBudget{}
	if err := recovered.unmarshal(encoded); err != nil {
		t.Fatal(err)
	}
	record, ok := recovered.firstFailure("head-1", "base-1")
	if !ok {
		t.Fatal("recovered budget lost first failure")
	}
	if record.Name != check.Name || record.Link != check.Link || record.Reason != check.InfrastructureReason || record.CompletedAt != failedAt || record.HeadSHA != "head-1" || record.BaseBranch != "main" || record.BaseSHA != "base-1" {
		t.Fatalf("recovered first failure = %+v, want original evidence", record)
	}
	if record.Evidence.ProviderRunID != "1" || record.Evidence.Attempt != 1 || len(record.Evidence.LogJobIDs) != 2 || len(record.Evidence.DependentJobs) != 2 || len(record.Evidence.DependentSteps) != 2 || len(record.Evidence.Artifacts) != 1 || record.Evidence.Artifacts[0].Digest == "" {
		t.Fatalf("recovered retention receipt = %+v", record.Evidence)
	}
	if got := infrastructureRerunCandidates([]scm.Check{check}, recovered, 1, "head-1", "base-1"); len(got) != 0 {
		t.Fatalf("restart handed back a spent candidate retry: %+v", got)
	}
}

// Both policies share the existing opaque run column but retain independent
// budgets. The combined codec must preserve the legacy transient shape and the
// new first-failure record in one restart-safe payload.
func TestCIRerunState_RestartPreservesIndependentTransientAndInfrastructureBudgets(t *testing.T) {
	t.Parallel()

	transient := &checkRerunBudget{}
	cancelled := scm.Check{Name: "lint", Bucket: scm.CheckBucketCancel, State: "CANCELLED", Link: "cancel-link"}
	transient.spend(cancelled, []scm.Check{cancelled}, "head-1")
	infrastructure := &infrastructureRerunBudget{}
	artifact := scm.Check{Name: "browser", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "artifact-link", InfrastructureFailure: true, InfrastructureGroup: "run:1", InfrastructureReason: "FinalizeArtifact HTTP 503", InfrastructureHeadSHA: "head-1", InfrastructureBaseSHA: "base-1", InfrastructureRerunSafe: true, InfrastructureEvidence: scm.InfrastructureEvidenceReceipt{ProviderRunID: "1", Attempt: 1, LogJobIDs: []int64{2}}}
	infrastructure.spend(artifact, []scm.Check{artifact}, "head-1", "main", "base-1")

	encoded, err := marshalCIRerunState(transient, infrastructure)
	if err != nil {
		t.Fatal(err)
	}
	recoveredTransient := &checkRerunBudget{}
	recoveredInfrastructure := &infrastructureRerunBudget{}
	if err := recoveredTransient.unmarshal(encoded); err != nil {
		t.Fatal(err)
	}
	if err := recoveredInfrastructure.unmarshal(encoded); err != nil {
		t.Fatal(err)
	}
	if recoveredTransient.used("lint") != 1 {
		t.Fatalf("transient spent = %d, want 1", recoveredTransient.used("lint"))
	}
	if recoveredInfrastructure.used("head-1", "base-1") != 1 {
		t.Fatalf("infrastructure spent = %d, want 1", recoveredInfrastructure.used("head-1", "base-1"))
	}
}

// A rerun receipt does not make the old failed rollup a new failure. It gets a
// finite wait, while a changed completion is immediately treated as the second
// attempt and may not be hidden from the ordinary CI path.
func TestInfrastructureRerunBudget_WaitsOnlyForExactOldFailureRollup(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC)
	old := scm.Check{Name: "browser", Bucket: scm.CheckBucketFail, State: "FAILURE", CompletedAt: completed, Link: "job-1", InfrastructureFailure: true, InfrastructureGroup: "run:1", InfrastructureHeadSHA: "head-1", InfrastructureBaseSHA: "base-1", InfrastructureRerunSafe: true, InfrastructureEvidence: scm.InfrastructureEvidenceReceipt{ProviderRunID: "1", Attempt: 1, LogJobIDs: []int64{2}}}
	budget := &infrastructureRerunBudget{}
	budget.spend(old, []scm.Check{old}, "head-1", "main", "base-1")
	for poll := 0; poll < rerunRollupGracePolls; poll++ {
		if awaiting := budget.awaitingFailureKeys([]scm.Check{old}, "head-1", "base-1"); !awaiting[checkIdentity(old)] {
			t.Fatalf("poll %d awaiting = %v, want exact old failure", poll+1, awaiting)
		}
	}
	if awaiting := budget.awaitingFailureKeys([]scm.Check{old}, "head-1", "base-1"); len(awaiting) != 0 {
		t.Fatalf("grace was unbounded: %v", awaiting)
	}

	budget = &infrastructureRerunBudget{}
	budget.spend(old, []scm.Check{old}, "head-1", "main", "base-1")
	second := old
	second.CompletedAt = completed.Add(time.Minute)
	if awaiting := budget.awaitingFailureKeys([]scm.Check{second}, "head-1", "base-1"); len(awaiting) != 0 {
		t.Fatalf("new failed attempt was hidden as old rollup: %v", awaiting)
	}
}

// The bounded exception is disabled unless trusted configuration opts in.
func TestRerunningArtifactInfrastructureIsOffByDefault(t *testing.T) {
	t.Parallel()
	if config.DefaultCIRerunInfrastructure != 0 || config.MaxCIRerunInfrastructure != 1 {
		t.Fatalf("infrastructure retry bounds = default %d max %d, want 0 and 1", config.DefaultCIRerunInfrastructure, config.MaxCIRerunInfrastructure)
	}
	check := scm.Check{Name: "browser", Bucket: scm.CheckBucketFail, State: "FAILURE", InfrastructureFailure: true, InfrastructureGroup: "run:1", InfrastructureHeadSHA: "head", InfrastructureBaseSHA: "base", InfrastructureRerunSafe: true}
	if got := infrastructureRerunCandidates([]scm.Check{check}, &infrastructureRerunBudget{}, config.DefaultCIRerunInfrastructure, "head", "base"); len(got) != 0 {
		t.Fatalf("default selected infrastructure retry: %+v", got)
	}
}

func TestInfrastructureFailureWithoutExactRerunBecomesUnifiedCIFinding(t *testing.T) {
	t.Parallel()
	checks := []scm.Check{
		{Name: "artifact", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, State: "FAILURE", InfrastructureFailure: true, InfrastructureRerunSafe: false},
		{Name: "green", Bucket: scm.CheckBucketPass, State: "SUCCESS"},
	}
	got := infrastructureFailuresWithoutExactRerun(checks)
	if len(got) != 1 || got[0] != "artifact" {
		t.Fatalf("unsafe infrastructure failures = %v, want artifact", got)
	}
	outcome := ciFailureOutcome(terminalCheckTargetsForNames(checks, got), false, "provider retry scope is unsafe")
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || len(findings.Items) != 1 {
		t.Fatalf("outcome = %+v findings = %+v, want one parked finding", outcome, findings.Items)
	}
	item := findings.Items[0]
	if item.Action != types.ActionAskUser || item.Category != types.FindingCategoryCICheck || item.Check != "artifact" || item.CheckID != "github-check-run:42" {
		t.Fatalf("finding = %+v, want provider-identified unified CI finding", item)
	}
}

// The monitor may have started on main and then observe the same PR retargeted
// to another branch. That differs from main advancing: the live head/base tuple
// no longer identifies the candidate at all, so classification must stop.
func TestVerifyInfrastructurePRTarget_RetargetDuringMonitorIsRefused(t *testing.T) {
	t.Parallel()
	host := &fakeCheckRerunner{target: scm.PRTarget{HeadSHA: "head-1", BaseBranch: "release", BaseSHA: "release-1"}}
	mismatch, err := verifyInfrastructurePRTarget(context.Background(), host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mismatch, "PR target changed during CI monitoring") || !strings.Contains(mismatch, "release") {
		t.Fatalf("retarget mismatch = %q, want explicit old/new target refusal", mismatch)
	}
}

// A single transient GetPRTarget error must only disable infrastructure
// retry for the poll where it occurred, not for the rest of the run: the CI
// monitor loop calls rearmPRPollIdentity at the top of every poll to restore
// pr.HeadSHA/pr.BaseBranch from the run's known-good values before deciding
// whether to invalidate them again this poll.
func TestRearmPRPollIdentity_RecoversFromPriorPollInvalidation(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}

	invalidateInfrastructurePRTarget(pr)
	if pr.HeadSHA != "" || pr.BaseBranch != "" || pr.BaseSHA != "" {
		t.Fatalf("pr after invalidation = %+v, want all target fields cleared", pr)
	}

	// The next poll resolves pr.BaseSHA from the live base branch tip before
	// rearmPRPollIdentity runs, exactly as the CI monitor loop does.
	pr.BaseSHA = "base-1"
	rearmPRPollIdentity(pr, "head-1", "main")
	if pr.HeadSHA != "head-1" || pr.BaseBranch != "main" || pr.BaseSHA != "base-1" {
		t.Fatalf("pr after rearm = %+v, want head/base/baseSHA restored from the run's known-good values", pr)
	}

	host := &fakeCheckRerunner{}
	mismatch, err := verifyInfrastructurePRTarget(context.Background(), host, pr)
	if err != nil || mismatch != "" {
		t.Fatalf("verifyInfrastructurePRTarget after rearm = mismatch %q err %v, want a clean pass now that the target is restored", mismatch, err)
	}
}

// Infrastructure admission is unavailable until the durable state has been
// read and structurally validated. A read/decode/reservation failure must occur
// before any provider request, and a recovered spend covers a later workflow
// group on the same immutable candidate.
func TestInfrastructureRerunDispatch_FailsClosedAcrossPersistenceBoundaries(t *testing.T) {
	newContext := func(t *testing.T) (*pipeline.StepContext, string) {
		t.Helper()
		dbPath := filepath.Join(t.TempDir(), "state.db")
		database, err := db.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close() })
		repo, err := database.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
		if err != nil {
			t.Fatal(err)
		}
		run, err := database.InsertRun(repo.ID, "refs/heads/feature", "head-1", "base-1")
		if err != nil {
			t.Fatal(err)
		}
		return &pipeline.StepContext{Ctx: context.Background(), DB: database, Run: run, Repo: repo, Config: &config.Config{CI: config.CI{RerunInfrastructure: 1}}, Log: func(string) {}}, dbPath
	}
	checkFor := func(group string) scm.Check {
		return scm.Check{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "job-2", InfrastructureFailure: true, InfrastructureGroup: group, InfrastructureReason: "artifact service", InfrastructureHeadSHA: "head-1", InfrastructureBaseSHA: "base-1", InfrastructureRerunSafe: true, InfrastructureEvidence: scm.InfrastructureEvidenceReceipt{ProviderRunID: "1", Attempt: 1, LogJobIDs: []int64{2}}}
	}
	prepareFreshness := func(step *CIStep) {
		step.publishedHead = func(*pipeline.StepContext) (string, error) { return "head-1", nil }
		step.baseBranchTip = func(context.Context) (string, bool) { return "base-1", true }
	}

	t.Run("failed read", func(t *testing.T) {
		sctx, _ := newContext(t)
		if err := sctx.DB.Close(); err != nil {
			t.Fatal(err)
		}
		step := &CIStep{}
		prepareFreshness(step)
		step.loadRerunBudget(sctx)
		host := &fakeCheckRerunner{}
		step.rerunInfrastructureChecks(sctx, host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{checkFor("run:1")})
		if host.calls != 0 {
			t.Fatalf("provider requests = %d after failed state read, want 0", host.calls)
		}
	})

	t.Run("corrupt and invalid state", func(t *testing.T) {
		valid := &infrastructureRerunBudget{}
		validCheck := checkFor("run:1")
		valid.spend(validCheck, []scm.Check{validCheck}, "head-1", "main", "base-1")
		validPayload, err := valid.marshal()
		if err != nil {
			t.Fatal(err)
		}
		var invalidCountPayload persistedInfrastructureRerunBudget
		if err := json.Unmarshal([]byte(validPayload), &invalidCountPayload); err != nil {
			t.Fatal(err)
		}
		key := infrastructureCandidateKey("head-1", "base-1")
		invalidCount := invalidCountPayload.Infrastructure[key]
		invalidCount.Used = -1
		invalidCountPayload.Infrastructure[key] = invalidCount
		invalidCountJSON, err := json.Marshal(invalidCountPayload)
		if err != nil {
			t.Fatal(err)
		}
		for name, encoded := range map[string]string{
			"corrupt":       `{not-json`,
			"invalid count": string(invalidCountJSON),
		} {
			t.Run(name, func(t *testing.T) {
				sctx, _ := newContext(t)
				if err := sctx.DB.SetRunCIRerunState(sctx.Run.ID, encoded); err != nil {
					t.Fatal(err)
				}
				step := &CIStep{}
				prepareFreshness(step)
				step.loadRerunBudget(sctx)
				host := &fakeCheckRerunner{}
				step.rerunInfrastructureChecks(sctx, host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{checkFor("run:1")})
				if host.calls != 0 {
					t.Fatalf("provider requests = %d after invalid state, want 0", host.calls)
				}
			})
		}
	})

	t.Run("failed reservation", func(t *testing.T) {
		sctx, _ := newContext(t)
		step := &CIStep{infrastructureStateAvailable: true}
		prepareFreshness(step)
		if err := sctx.DB.Close(); err != nil {
			t.Fatal(err)
		}
		host := &fakeCheckRerunner{}
		step.rerunInfrastructureChecks(sctx, host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{checkFor("run:1")})
		if host.calls != 0 {
			t.Fatalf("provider requests = %d after failed reservation, want 0", host.calls)
		}
	})

	t.Run("spent candidate survives recovery and changed workflow group", func(t *testing.T) {
		sctx, _ := newContext(t)
		spent := &infrastructureRerunBudget{}
		first := checkFor("run:1")
		spent.spend(first, []scm.Check{first}, "head-1", "main", "base-1")
		encoded, err := marshalCIRerunState(&checkRerunBudget{}, spent)
		if err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.SetRunCIRerunState(sctx.Run.ID, encoded); err != nil {
			t.Fatal(err)
		}
		step := &CIStep{}
		prepareFreshness(step)
		step.loadRerunBudget(sctx)
		host := &fakeCheckRerunner{}
		step.rerunInfrastructureChecks(sctx, host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{checkFor("run:2")})
		if host.calls != 0 {
			t.Fatalf("provider requests = %d after recovered spend, want 0", host.calls)
		}
	})

	t.Run("same base branch advanced", func(t *testing.T) {
		sctx, _ := newContext(t)
		step := &CIStep{infrastructureStateAvailable: true}
		step.publishedHead = func(*pipeline.StepContext) (string, error) { return "head-1", nil }
		step.baseBranchTip = func(context.Context) (string, bool) { return "base-2", true }
		host := &fakeCheckRerunner{}
		issued, outcome := step.rerunInfrastructureChecks(sctx, host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{checkFor("run:1")})
		if issued || host.calls != 0 || outcome != nil {
			t.Fatalf("same-branch advance = issued %v, calls %d, outcome %+v; want a quiet freshness refusal distinct from retarget", issued, host.calls, outcome)
		}
	})

	t.Run("PR retargeted before dispatch", func(t *testing.T) {
		sctx, _ := newContext(t)
		step := &CIStep{infrastructureStateAvailable: true}
		prepareFreshness(step)
		host := &fakeCheckRerunner{target: scm.PRTarget{HeadSHA: "head-1", BaseBranch: "release", BaseSHA: "base-1"}}
		issued, outcome := step.rerunInfrastructureChecks(sctx, host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{checkFor("run:1")})
		if issued || host.calls != 0 {
			t.Fatalf("retargeted PR dispatched retry: issued=%v calls=%d", issued, host.calls)
		}
		if outcome == nil || !strings.Contains(outcome.Findings, "PR target changed during CI monitoring") {
			t.Fatalf("retargeted PR outcome = %+v, want explicit monitor-target refusal", outcome)
		}
	})

	t.Run("published feature branch advanced", func(t *testing.T) {
		sctx, _ := newContext(t)
		step := &CIStep{infrastructureStateAvailable: true}
		step.publishedHead = func(*pipeline.StepContext) (string, error) { return "head-2", nil }
		step.baseBranchTip = func(context.Context) (string, bool) { return "base-1", true }
		host := &fakeCheckRerunner{}
		step.rerunInfrastructureChecks(sctx, host, &scm.PR{HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{checkFor("run:1")})
		if host.calls != 0 {
			t.Fatalf("provider requests = %d after branch advance, want 0", host.calls)
		}
	})
}

// The budget has to survive a daemon restart. Before it was durable, a
// recovered run started from zero spent and could issue another rerun past the
// documented limit.
func TestRerunBudgetSurvivesARestart(t *testing.T) {
	completed := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	original := &checkRerunBudget{}
	check := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, CompletedAt: completed, Link: "https://github.com/test/repo/actions/runs/1/job/10"}
	original.spend(check, []scm.Check{check}, "head-1")

	encoded, err := original.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if encoded == "" {
		t.Fatal("a spent budget marshalled to nothing")
	}

	recovered := &checkRerunBudget{}
	if err := recovered.unmarshal(encoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := recovered.used("build"); got != 1 {
		t.Fatalf("recovered used = %d, want 1", got)
	}
	if got := recovered.remaining("build", 1); got != 0 {
		t.Fatalf("recovered remaining = %d, want 0: the restart handed back a spent rerun", got)
	}
	if !recovered.rollupUnchanged(scm.Check{Name: "build", CompletedAt: completed}) {
		t.Fatal("recovered budget lost the completion the rerun was requested for")
	}
	if recovered.rollup["build"].headSHA != "head-1" {
		t.Fatalf("recovered head = %q, want head-1", recovered.rollup["build"].headSHA)
	}
	if !recovered.rollup["build"].observedLinks[check.Link] {
		t.Fatal("recovered budget lost the provider link observed before the rerun")
	}
	retired, err := recovered.retireResolvedReruns([]scm.Check{{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS", Link: check.Link}}, "head-1", func(*checkRerunBudget) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if retired {
		t.Fatal("recovered budget retired a rerun while its head was current")
	}
	retired, err = recovered.retireResolvedReruns(nil, "head-2", func(*checkRerunBudget) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !retired {
		t.Fatal("recovered budget did not retire after the pipeline head advanced")
	}
}

// An empty budget writes nothing, so a run that never spent a rerun does not
// persist a payload.
func TestUnspentRerunBudgetMarshalsToNothing(t *testing.T) {
	encoded, err := (&checkRerunBudget{}).marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if encoded != "" {
		t.Fatalf("unspent budget marshalled to %q, want empty", encoded)
	}
}

// A transitional rollup can omit the check a rerun was requested for. Tracking
// only what the response carries dropped it from both buckets, so the run could
// read as green while its replacement was still unknown.
func TestOutstandingRerunMissingFromTheRollupStaysTracked(t *testing.T) {
	completed := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	budget := &checkRerunBudget{}
	check := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, CompletedAt: completed, Link: "https://github.com/test/repo/actions/runs/1/job/10"}
	budget.spend(check, []scm.Check{check}, "head")

	// The rollup no longer mentions the check at all.
	for poll := 1; poll <= rerunRollupGracePolls; poll++ {
		unresolved, awaiting := budget.cancelledAfterRerun(nil)
		if len(unresolved) != 0 {
			t.Fatalf("poll %d: unresolved = %v, want none while grace remains", poll, unresolved)
		}
		if len(awaiting) != 1 || awaiting[0] != "build" {
			t.Fatalf("poll %d: awaiting = %v, want [build]", poll, awaiting)
		}
	}

	// Grace is bounded: a replacement that never appears becomes unresolved
	// rather than stalling the run forever.
	unresolved, awaiting := budget.cancelledAfterRerun(nil)
	if len(awaiting) != 0 {
		t.Fatalf("awaiting = %v after grace ran out, want none", awaiting)
	}
	if len(unresolved) != 1 || unresolved[0] != "build" {
		t.Fatalf("unresolved = %v, want [build]", unresolved)
	}
}

// A check that came back in a non-cancelled bucket is published, not missing:
// it must not consume the missing-check grace.
func TestOutstandingRerunPublishedInAnotherBucketIsNotTreatedAsMissing(t *testing.T) {
	completed := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	budget := &checkRerunBudget{}
	check := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, CompletedAt: completed, Link: "https://github.com/test/repo/actions/runs/1/job/10"}
	budget.spend(check, []scm.Check{check}, "head")

	unresolved, awaiting := budget.cancelledAfterRerun([]scm.Check{
		{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS", Link: "https://github.com/test/repo/actions/runs/1/job/11"},
	})
	if len(unresolved) != 0 || len(awaiting) != 0 {
		t.Fatalf("unresolved = %v, awaiting = %v, want both empty for a republished check", unresolved, awaiting)
	}
}

func TestRetireResolvedReruns(t *testing.T) {
	completed := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	originalLink := "https://github.com/test/repo/actions/runs/1/job/10"
	siblingLink := "https://github.com/test/repo/actions/runs/2/job/20"
	replacementLink := "https://github.com/test/repo/actions/runs/1/job/11"
	newBudget := func(recordedHead string) *checkRerunBudget {
		b := &checkRerunBudget{}
		cancelled := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, CompletedAt: completed, Link: originalLink}
		sibling := scm.Check{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS", Link: siblingLink}
		b.spend(cancelled, []scm.Check{cancelled, sibling}, recordedHead)
		return b
	}

	tests := []struct {
		name         string
		recordedHead string
		currentHead  string
		checks       []scm.Check
		wantRetired  bool
	}{
		{
			name:         "new conclusive link retires on the current head",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks:       []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS", Link: replacementLink}},
			wantRetired:  true,
		},
		{
			name:         "advanced head retires the record",
			recordedHead: "head-1",
			currentHead:  "head-2",
			wantRetired:  true,
		},
		{
			name:         "spend-time link does not retire",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks:       []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS", Link: siblingLink}},
			wantRetired:  false,
		},
		{
			name:         "empty link does not retire",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks:       []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS"}},
			wantRetired:  false,
		},
		{
			name:         "transient failure stays outstanding",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks:       []scm.Check{{Name: "build", Bucket: scm.CheckBucketFail, State: "CANCELLED", Link: replacementLink}},
			wantRetired:  false,
		},
		{
			name:         "american-spelled transient failure stays outstanding",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks:       []scm.Check{{Name: "build", Bucket: scm.CheckBucketFail, State: "CANCELED", Link: replacementLink}},
			wantRetired:  false,
		},
		{
			name:         "pending check stays outstanding",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks:       []scm.Check{{Name: "build", Bucket: scm.CheckBucketPending, State: "IN_PROGRESS", Link: replacementLink}},
			wantRetired:  false,
		},
		{
			name:         "unrecognized check stays outstanding",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks:       []scm.Check{{Name: "build", Bucket: scm.CheckBucket(""), State: "UNKNOWN", Link: replacementLink}},
			wantRetired:  false,
		},
		{
			name:         "same-named pending observation blocks retirement",
			recordedHead: "head-1",
			currentHead:  "head-1",
			checks: []scm.Check{
				{Name: "build", Bucket: scm.CheckBucketPass, State: "SUCCESS", Link: replacementLink},
				{Name: "build", Bucket: scm.CheckBucketPending, State: "QUEUED", Link: "https://github.com/test/repo/actions/runs/3/job/30"},
			},
			wantRetired: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget := newBudget(tt.recordedHead)
			got, err := budget.retireResolvedReruns(tt.checks, tt.currentHead, func(*checkRerunBudget) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.wantRetired {
				t.Fatalf("retireResolvedReruns = %v, want %v", got, tt.wantRetired)
			}
			if budget.used("build") != 1 {
				t.Fatalf("spent budget = %d, want the rerun to stay spent", budget.used("build"))
			}
			// A retired record is no longer an outstanding rerun, so a rollup
			// that stops reporting the check must not re-open it.
			unresolved, awaiting := budget.cancelledAfterRerun(nil)
			if tt.wantRetired {
				if len(unresolved) != 0 || len(awaiting) != 0 {
					t.Fatalf("unresolved = %v, awaiting = %v, want both empty for a retired rerun", unresolved, awaiting)
				}
				return
			}
			if len(awaiting) != 1 || awaiting[0] != "build" {
				t.Fatalf("awaiting = %v, want [build] while the rerun is unanswered", awaiting)
			}
		})
	}
}

func TestRetireResolvedRerunsRetriesAfterPersistenceFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", "head-2", "base")
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{DB: database, Run: run}
	step := &CIStep{}
	cancelled := scm.Check{
		Name:        "build",
		Bucket:      scm.CheckBucketCancel,
		State:       "CANCELLED",
		CompletedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Link:        "https://github.com/test/repo/actions/runs/1/job/10",
	}
	step.transientReruns.spend(cancelled, []scm.Check{cancelled}, "head-1")
	if err := step.persistRerunBudget(sctx); err != nil {
		t.Fatal(err)
	}
	before := step.transientReruns.rollup["build"]
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	retired, err := step.retireResolvedReruns(sctx, nil)
	if err == nil || retired {
		t.Fatalf("retirement with a closed database = (%v, %v), want (false, error)", retired, err)
	}
	after := step.transientReruns.rollup["build"]
	if !after.completedAt.Equal(before.completedAt) || after.graceRemaining != before.graceRemaining || after.headSHA != before.headSHA || !after.observedLinks[cancelled.Link] {
		t.Fatalf("failed persistence changed the outstanding record: before=%+v after=%+v", before, after)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	sctx.DB = database
	encoded, err := database.GetRunCIRerunState(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := &checkRerunBudget{}
	if err := recovered.unmarshal(encoded); err != nil {
		t.Fatal(err)
	}
	recoveredState, ok := recovered.rollup["build"]
	if !ok || recoveredState.graceRemaining != before.graceRemaining || recoveredState.headSHA != before.headSHA || !recoveredState.observedLinks[cancelled.Link] {
		t.Fatalf("durable state lost the outstanding record after failed retirement: %+v", recoveredState)
	}

	retired, err = step.retireResolvedReruns(sctx, nil)
	if err != nil || !retired {
		t.Fatalf("retried retirement = (%v, %v), want (true, nil)", retired, err)
	}
	encoded, err = database.GetRunCIRerunState(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered = &checkRerunBudget{}
	if err := recovered.unmarshal(encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := recovered.rollup["build"]; ok {
		t.Fatal("successful retry did not durably retire the outstanding record")
	}
	if recovered.used("build") != 1 {
		t.Fatalf("successful retry changed spent budget to %d", recovered.used("build"))
	}
}
