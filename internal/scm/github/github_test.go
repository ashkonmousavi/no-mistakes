package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestRepoSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://github.com/test/repo", "test/repo"},
		{"https with .git suffix", "https://github.com/test/repo.git", "test/repo"},
		{"pr url", "https://github.com/test/repo/pull/42", "test/repo"},
		{"ssh scp form", "git@github.com:test/repo.git", "test/repo"},
		{"ssh scp form no suffix", "git@github.com:test/repo", "test/repo"},
		{"ssh url form", "ssh://git@github.com/test/repo.git", "test/repo"},
		{"https with port", "https://github.com:8443/test/repo", "test/repo"},
		{"already a slug", "test/repo", "test/repo"},
		{"trailing slash", "https://github.com/test/repo/", "test/repo"},
		{"empty", "", ""},
		{"host only", "https://github.com/", ""},
		{"owner only", "https://github.com/onlyowner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoSlug(tc.in); got != tc.want {
				t.Fatalf("RepoSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHostPrefixedSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// github.com inputs keep the plain owner/name format.
		{"github.com https", "https://github.com/test/repo", "test/repo"},
		{"github.com https with .git suffix", "https://github.com/test/repo.git", "test/repo"},
		{"github.com pr url", "https://github.com/test/repo/pull/42", "test/repo"},
		{"github.com ssh scp form", "git@github.com:test/repo.git", "test/repo"},
		{"github.com ssh url form", "ssh://git@github.com/test/repo.git", "test/repo"},
		{"github.com https with port", "https://github.com:8443/test/repo", "test/repo"},
		{"github.com mixed case host", "https://GitHub.com/test/repo.git", "test/repo"},
		{"github.com trailing slash", "https://github.com/test/repo/", "test/repo"},

		// GitHub Enterprise Server inputs get the host prefix gh requires.
		{"ghe https", "https://bbgithub.dev.bloomberg.com/org/repo", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe https with .git suffix", "https://bbgithub.dev.bloomberg.com/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe ssh scp form", "git@bbgithub.dev.bloomberg.com:org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe ssh url form", "ssh://git@bbgithub.dev.bloomberg.com/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe pr url", "https://bbgithub.dev.bloomberg.com/org/repo/pull/42", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe https with port", "https://bbgithub.dev.bloomberg.com:8443/org/repo.git", "bbgithub.dev.bloomberg.com/org/repo"},
		{"ghe trailing slash", "https://bbgithub.dev.bloomberg.com/org/repo/", "bbgithub.dev.bloomberg.com/org/repo"},

		// Empty/malformed inputs return "" so the --repo flag is omitted.
		{"empty", "", ""},
		{"host only ghe", "https://bbgithub.dev.bloomberg.com/", ""},
		{"owner only ghe", "https://bbgithub.dev.bloomberg.com/onlyowner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostPrefixedSlug(tc.in); got != tc.want {
				t.Fatalf("HostPrefixedSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHostPrefixedSlugForHost_SSHAlias(t *testing.T) {
	remote := "git@github-personal:owner/repo.git"
	if got := HostPrefixedSlugForHost(remote, "github.com"); got != "owner/repo" {
		t.Fatalf("HostPrefixedSlugForHost() = %q, want owner/repo", got)
	}
	if got := HostPrefixedSlugForHost(remote, "ghe.example.com"); got != "ghe.example.com/owner/repo" {
		t.Fatalf("HostPrefixedSlugForHost() = %q, want ghe.example.com/owner/repo", got)
	}
}

func TestGetChecksPassesRepoFlag(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("checks = %+v, want single build check", checks)
	}
}

// A failing gh must surface its stderr in the error: a broken gh (e.g. < v2.50
// rejecting `pr checks --json`) is only diagnosable from the step log if the
// provider message survives the error. See the #644 hardening intent.
func TestGetChecksSurfacesGHErrorStderr(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stderr: "flag needs an argument: --json",
			code:   1,
		},
	}), nil, "", "test/repo")

	_, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err == nil {
		t.Fatal("GetChecks() expected the gh failure to propagate")
	}
	if !strings.Contains(err.Error(), "flag needs an argument: --json") {
		t.Fatalf("GetChecks() error = %v, want gh stderr in the error", err)
	}
}

func TestGetChecksIncludesFailedWorkflowRunMissingFromPRRollup(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stdout: `[
				{"name":"clippy","state":"SUCCESS","bucket":"pass"},
				{"name":"request-owner-review","state":"SUCCESS","bucket":"pass"}
			]` + "\n",
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"workflow_runs":[
				{"id":101,"name":"workflow-validation","display_title":"","status":"completed","conclusion":"failure","updated_at":"2026-07-30T12:34:56Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 3 {
		t.Fatalf("GetChecks() returned %d checks, want 3: %+v", len(checks), checks)
	}
	got := checks[2]
	if got.Name != "workflow-validation" || got.Bucket != scm.CheckBucketFail {
		t.Fatalf("workflow run check = %+v, want workflow-validation/fail", got)
	}
	wantCompletedAt := time.Date(2026, 7, 30, 12, 34, 56, 0, time.UTC)
	if !got.CompletedAt.Equal(wantCompletedAt) {
		t.Fatalf("workflow run CompletedAt = %v, want %v", got.CompletedAt, wantCompletedAt)
	}
}

func TestGetChecksIncludesFailedWorkflowRunWhenPRHasNoChecks(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stderr: "no checks reported on the 'feature' branch\n",
			code:   1,
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"workflow_runs":[
				{"id":101,"name":"workflow-validation","display_title":"","status":"completed","conclusion":"failure","updated_at":"2026-07-30T12:34:56Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want 1: %+v", len(checks), checks)
	}
	if got := checks[0]; got.Name != "workflow-validation" || got.Bucket != scm.CheckBucketFail {
		t.Fatalf("workflow run check = %+v, want workflow-validation/fail", got)
	}
}

func TestGetChecksUsesLivePRHeadForWorkflowDiscovery(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "live-head\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]` + "\n",
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=live-head -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":0,"workflow_runs":[]}]` + "\n",
		},
	}), nil, "", "test/repo")

	pr := &scm.PR{Number: "123", HeadSHA: "stale-head"}
	checks, err := host.GetChecks(context.Background(), pr)
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("checks = %+v, want live-head build rollup", checks)
	}
	if pr.HeadSHA != "live-head" {
		t.Fatalf("PR HeadSHA = %q, want live-head", pr.HeadSHA)
	}
}

func TestGetChecksBindsRollupAcrossABAHeadMovement(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "h1\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"h2-build","state":"SUCCESS","bucket":"pass"}]` + "\n",
		},
		githubCommitChecksCommand("", "test/repo", "h1"): {
			stdout: githubCommitChecksResponse(`[{"__typename":"CheckRun","name":"h1-build","status":"COMPLETED","conclusion":"FAILURE"}]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=h1 -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":0,"workflow_runs":[]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "stale"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "h1-build" || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks = %+v, want exact-H1 failed rollup", checks)
	}
}

func TestGetChecksWorkflowCancellationKeepsRerunIdentity(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stdout: "[]\n",
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"workflow_runs":[{"id":101,"name":"build","status":"completed","conclusion":"cancelled"}]}]` + "\n",
		},
		"gh run rerun 101 --repo test/repo": {},
	}), nil, "", "test/repo")

	pr := &scm.PR{Number: "123", HeadSHA: "deadbeef"}
	checks, err := host.GetChecks(context.Background(), pr)
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %+v, want one cancelled workflow", checks)
	}
	check := checks[0]
	if check.Bucket != scm.CheckBucketCancel || check.State != "CANCELLED" || check.Link != "https://github.com/test/repo/actions/runs/101" {
		t.Fatalf("workflow check = %+v, want rerunnable cancelled run", check)
	}
	if err := host.RerunCheck(context.Background(), pr, check); err != nil {
		t.Fatalf("RerunCheck() error = %v", err)
	}
}

func TestGetChecksDoesNotDuplicateWorkflowRunsRepresentedByRollup(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"build","state":"CANCELLED","bucket":"cancel","link":"https://github.com/test/repo/actions/runs/101/job/201"}]` + "\n",
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":2,"workflow_runs":[
				{"id":101,"name":"represented-workflow","status":"completed","conclusion":"cancelled"},
				{"id":102,"name":"workflow-only","status":"completed","conclusion":"failure"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want rollup job plus unrepresented workflow", checks)
	}
	if checks[0].Name != "build" || checks[1].Name != "workflow-only" {
		t.Fatalf("checks = %+v, want build and workflow-only", checks)
	}
}

// The raw commit statusCheckRollup keeps every check run a commit ever had,
// including a same-named run a later run has already superseded (e.g. a CI
// monitor auto-fix push re-triggering the same gate check). Without a
// latest-wins collapse the stale FAILURE stays visible forever even though a
// later SUCCESS at the same head replaced it, which manufactures an
// unrecoverable auto-fix loop. GetChecks must collapse to the newest
// startedAt so the caller sees zero failing checks.
func TestGetChecksCollapsesSupersededSameNameCheckToLatestAtOneHead(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"gate","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/101/job/201"},
				{"__typename":"CheckRun","name":"gate","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:39:44Z","completedAt":"2026-08-26T08:39:50Z","detailsUrl":"https://github.com/test/repo/actions/runs/102/job/202"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":2,"workflow_runs":[
				{"id":101,"workflow_id":1001,"name":"gate","status":"completed","conclusion":"failure","run_started_at":"2026-08-26T08:25:50Z"},
				{"id":102,"workflow_id":1001,"name":"gate","status":"completed","conclusion":"success","run_started_at":"2026-08-26T08:39:44Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "stale"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want the superseded run collapsed away: %+v", len(checks), checks)
	}
	if got := checks[0]; got.Name != "gate" || got.Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want the latest SUCCESS run to win", got)
	}
	for _, c := range checks {
		if c.Bucket == scm.CheckBucketFail {
			t.Fatalf("checks = %+v, want zero failing checks after collapse", checks)
		}
	}
}

// Order matters: appendUnrepresentedWorkflowRuns dedupes the Actions-run
// union against the checks slice by run ID. If collapseLatestByName ran
// BEFORE that union, the superseded run's ID would drop out of the
// "represented" set and the union would re-add the exact same stale run
// under its own workflow run name - resurrecting the failure the collapse
// was supposed to hide. This test pins the union-then-collapse order: both
// the superseded and the winning run are independently visible to the
// workflow-run API (as they would be on a real repo), and the union must
// recognize both as already represented rather than re-adding either.
func TestGetChecksCollapseOrderingDoesNotLetWorkflowRunUnionResurrectSupersededCheck(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"gate","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/101/job/201"},
				{"__typename":"CheckRun","name":"gate","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:39:44Z","completedAt":"2026-08-26T08:39:50Z","detailsUrl":"https://github.com/test/repo/actions/runs/102/job/202"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":2,"workflow_runs":[
				{"id":101,"workflow_id":1001,"name":"gate - synchronize - event 1 (run 101)","status":"completed","conclusion":"failure"},
				{"id":102,"workflow_id":1001,"name":"gate - edited - event 2 (run 102)","status":"completed","conclusion":"success"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want the union to add nothing and the collapse to leave one: %+v", len(checks), checks)
	}
	if got := checks[0]; got.Name != "gate" || got.Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want the latest SUCCESS run to win with no resurrected failure", got)
	}
}

func TestGetChecksPreservesIndependentSameNameWorkflows(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:39:44Z","completedAt":"2026-08-26T08:39:50Z","detailsUrl":"https://github.com/test/repo/actions/runs/102/job/202"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":2,"workflow_runs":[
				{"id":102,"workflow_id":1002,"name":"build","status":"completed","conclusion":"success","run_started_at":"2026-08-26T08:39:44Z"},
				{"id":103,"workflow_id":1003,"name":"build","status":"completed","conclusion":"failure","run_started_at":"2026-08-26T08:25:50Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("GetChecks() returned %d checks, want both independent same-name workflows: %+v", len(checks), checks)
	}
	buckets := map[scm.CheckBucket]int{}
	for _, check := range checks {
		buckets[check.Bucket]++
	}
	if buckets[scm.CheckBucketPass] != 1 || buckets[scm.CheckBucketFail] != 1 {
		t.Fatalf("GetChecks() buckets = %v, want independent passing and failing workflows", buckets)
	}
}

func TestGetChecksPreservesSameNameJobsWithinOneWorkflowRun(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/102/job/201"},
				{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:39:44Z","completedAt":"2026-08-26T08:39:50Z","detailsUrl":"https://github.com/test/repo/actions/runs/102/job/202"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"workflow_runs":[
				{"id":102,"workflow_id":1001,"name":"build","status":"completed","conclusion":"failure","run_started_at":"2026-08-26T08:25:50Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("GetChecks() returned %d checks, want both same-name jobs from one workflow run: %+v", len(checks), checks)
	}
	buckets := map[scm.CheckBucket]int{}
	for _, check := range checks {
		buckets[check.Bucket]++
	}
	if buckets[scm.CheckBucketPass] != 1 || buckets[scm.CheckBucketFail] != 1 {
		t.Fatalf("GetChecks() buckets = %v, want independent passing and failing jobs", buckets)
	}
}

func TestGetChecksPreservesIndependentSameNameExternalCheckRuns(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://ci-one.example.com/build/42"},
				{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:39:44Z","completedAt":"2026-08-26T08:39:50Z","detailsUrl":"https://ci-two.example.com/build/99"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":0,"workflow_runs":[]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("GetChecks() returned %d checks, want both independent external checks: %+v", len(checks), checks)
	}
	buckets := map[scm.CheckBucket]int{}
	for _, check := range checks {
		buckets[check.Bucket]++
	}
	if buckets[scm.CheckBucketPass] != 1 || buckets[scm.CheckBucketFail] != 1 {
		t.Fatalf("GetChecks() buckets = %v, want independent passing and failing external checks", buckets)
	}
}

func TestGetChecksCollapseComparesNewestRunWithEverySameNameCandidate(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"gate","status":"QUEUED","conclusion":null,"startedAt":null,"completedAt":null,"detailsUrl":"https://checks.example.com/runs/pending"},
				{"__typename":"CheckRun","name":"gate","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/101/job/201"},
				{"__typename":"CheckRun","name":"gate","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:39:44Z","completedAt":"2026-08-26T08:39:50Z","detailsUrl":"https://github.com/test/repo/actions/runs/102/job/202"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":2,"workflow_runs":[
				{"id":101,"workflow_id":1001,"name":"gate","status":"completed","conclusion":"failure","run_started_at":"2026-08-26T08:25:50Z"},
				{"id":102,"workflow_id":1001,"name":"gate","status":"completed","conclusion":"success","run_started_at":"2026-08-26T08:39:44Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("GetChecks() returned %d checks, want unordered pending plus newest ordered run: %+v", len(checks), checks)
	}
	buckets := map[scm.CheckBucket]int{}
	for _, check := range checks {
		buckets[check.Bucket]++
	}
	if buckets[scm.CheckBucketPending] != 1 || buckets[scm.CheckBucketPass] != 1 || buckets[scm.CheckBucketFail] != 0 {
		t.Fatalf("GetChecks() buckets = %v, want pending external check plus latest passing Actions run", buckets)
	}
}

func TestGetChecksPreservesSameNameStatusContextAndCheckRun(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:39:44Z","completedAt":"2026-08-26T08:39:50Z","detailsUrl":"https://github.com/test/repo/actions/runs/102/job/202"},
				{"__typename":"StatusContext","context":"build","state":"FAILURE","targetUrl":"https://ci.example.com/build/42"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"workflow_runs":[
				{"id":102,"name":"build","status":"completed","conclusion":"success","run_started_at":"2026-08-26T08:39:44Z","updated_at":"2026-08-26T08:39:50Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("GetChecks() returned %d checks, want both same-name records: %+v", len(checks), checks)
	}
	if checks[0].Kind != scm.CheckKindRun || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks[0] = %+v, want passing check run", checks[0])
	}
	if checks[1].Kind != scm.CheckKindStatus || checks[1].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks[1] = %+v, want failing commit status", checks[1])
	}
}

func TestGetChecksKeepsQueuedReplacementWithEqualStartTime(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"CI","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/101/job/201"},
				{"__typename":"CheckRun","name":"CI","status":"QUEUED","conclusion":null,"startedAt":null,"completedAt":null,"detailsUrl":"https://github.com/test/repo/actions/runs/102/job/202"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":2,"workflow_runs":[
				{"id":101,"workflow_id":1001,"name":"CI","status":"completed","conclusion":"failure","created_at":"2026-08-26T08:25:45Z","updated_at":"2026-08-26T08:25:56Z"},
				{"id":102,"workflow_id":1001,"name":"CI","status":"queued","conclusion":null,"created_at":"2026-08-26T08:25:50Z","updated_at":"2026-08-26T08:25:50Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("GetChecks() returned %d checks, want one: %+v", len(checks), checks)
	}
	if got := checks[0]; got.Name != "CI" || got.Bucket != scm.CheckBucketPending {
		t.Fatalf("checks[0] = %+v, want the queued replacement to supersede the old failure", got)
	}
	wantStartedAt := time.Date(2026, 8, 26, 8, 25, 50, 0, time.UTC)
	if !checks[0].StartedAt.Equal(wantStartedAt) {
		t.Fatalf("checks[0].StartedAt = %v, want workflow creation time %v", checks[0].StartedAt, wantStartedAt)
	}
}

func TestGetChecksPreservesUnorderedExternalPendingReplacement(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"CI","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/101/job/201"},
				{"__typename":"CheckRun","name":"CI","status":"QUEUED","conclusion":null,"startedAt":null,"completedAt":null,"detailsUrl":"https://checks.example.com/runs/replacement"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"workflow_runs":[
				{"id":101,"name":"CI","status":"completed","conclusion":"failure","run_started_at":"2026-08-26T08:25:50Z","updated_at":"2026-08-26T08:25:56Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("GetChecks() returned %d checks, want both unordered records: %+v", len(checks), checks)
	}
	buckets := map[scm.CheckBucket]int{}
	for _, check := range checks {
		buckets[check.Bucket]++
	}
	if buckets[scm.CheckBucketFail] != 1 || buckets[scm.CheckBucketPending] != 1 {
		t.Fatalf("GetChecks() buckets = %v, want one failure and one pending replacement", buckets)
	}
}

func TestGetChecksUsesWorkflowRunStartTimeWhenCollapsingSameNameChecks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		timestamp string
	}{
		{name: "run_started_at", timestamp: `"run_started_at":"2026-08-26T08:39:44Z"`},
		{name: "created_at fallback", timestamp: `"created_at":"2026-08-26T08:39:44Z"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
				githubCommitChecksCommand("", "test/repo", "deadbeef"): {
					stdout: githubCommitChecksResponse(`[
						{"__typename":"CheckRun","name":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/101/job/201"}
					]`),
				},
				"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
					stdout: `[{"total_count":2,"workflow_runs":[
						{"id":101,"workflow_id":1001,"name":"CI","status":"completed","conclusion":"success","run_started_at":"2026-08-26T08:25:50Z","updated_at":"2026-08-26T08:25:56Z"},
						{"id":102,"workflow_id":1001,"name":"CI","status":"completed","conclusion":"failure",` + tc.timestamp + `,"updated_at":"2026-08-26T08:39:50Z"}
					]}]` + "\n",
				},
			}), nil, "", "test/repo")

			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
			if err != nil {
				t.Fatalf("GetChecks() error = %v", err)
			}
			if len(checks) != 1 {
				t.Fatalf("GetChecks() returned %d checks, want one: %+v", len(checks), checks)
			}
			if got := checks[0]; got.Name != "CI" || got.Bucket != scm.CheckBucketFail {
				t.Fatalf("checks[0] = %+v, want the newer failed workflow run to win", got)
			}
			wantStartedAt := time.Date(2026, 8, 26, 8, 39, 44, 0, time.UTC)
			if !checks[0].StartedAt.Equal(wantStartedAt) {
				t.Fatalf("checks[0].StartedAt = %v, want %v", checks[0].StartedAt, wantStartedAt)
			}
		})
	}
}

func TestGetChecksDoesNotTrustUnrelatedWorkflowRunLinks(t *testing.T) {
	t.Parallel()

	for name, link := range map[string]string{
		"third-party host": "https://ci.example.com/test/repo/actions/runs/101/job/201",
		"wrong repository": "https://github.com/other/repo/actions/runs/101/job/201",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
				"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
					stdout: `[{"name":"external","state":"SUCCESS","bucket":"pass","link":"` + link + `"}]` + "\n",
				},
				"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
					stdout: `[{"total_count":1,"workflow_runs":[{"id":101,"name":"failed-workflow","status":"completed","conclusion":"failure"}]}]` + "\n",
				},
			}), nil, "", "test/repo")

			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
			if err != nil {
				t.Fatalf("GetChecks() error = %v", err)
			}
			if len(checks) != 2 || checks[1].Name != "failed-workflow" || checks[1].Bucket != scm.CheckBucketFail {
				t.Fatalf("checks = %+v, want unrelated rollup plus failed workflow", checks)
			}
		})
	}
}

func TestGetChecksIncludesWorkflowRunsFromEveryPage(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo ghe.example.com/test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		"gh pr checks 123 --repo ghe.example.com/test/repo --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"clippy","state":"SUCCESS","bucket":"pass"}]` + "\n",
		},
		"gh api --hostname ghe.example.com --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[
				{"total_count":2,"workflow_runs":[{"id":101,"name":"first-page","status":"completed","conclusion":"success"}]},
				{"total_count":2,"workflow_runs":[{"id":102,"name":"second-page-failure","status":"completed","conclusion":"failure"}]}
			]` + "\n",
		},
	}), nil, "ghe.example.com", "ghe.example.com/test/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 3 {
		t.Fatalf("GetChecks() returned %d checks, want 3: %+v", len(checks), checks)
	}
	if got := checks[2]; got.Name != "second-page-failure" || got.Bucket != scm.CheckBucketFail {
		t.Fatalf("last workflow run check = %+v, want second-page-failure/fail", got)
	}
}

func TestGetChecksRejectsIncompleteWorkflowPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		response   string
		wantErrSub string
	}{
		{
			name:       "truncated",
			response:   `[{"total_count":2,"workflow_runs":[{"id":101,"name":"visible","status":"completed","conclusion":"success"}]}]`,
			wantErrSub: "returned 1 unique runs, want 2",
		},
		{
			name: "inconsistent totals",
			response: `[
				{"total_count":2,"workflow_runs":[{"id":101,"name":"first","status":"completed","conclusion":"success"}]},
				{"total_count":3,"workflow_runs":[{"id":102,"name":"second","status":"completed","conclusion":"success"}]}
			]`,
			wantErrSub: "total_count is 3, want 2",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
				"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
					stdout: `[{"name":"clippy","state":"SUCCESS","bucket":"pass"}]` + "\n",
				},
				"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
					stdout: tc.response + "\n",
				},
			}), nil, "", "test/repo")

			_, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("GetChecks() error = %v, want containing %q", err, tc.wantErrSub)
			}
		})
	}
}

func TestGetPRContentReadsTitleAndBody(t *testing.T) {
	t.Parallel()

	body := "## Pipeline\n\n" + "Updates from no-mistakes\n"
	encoded, err := json.Marshal(map[string]string{"title": "fix: restamp", "body": body})
	if err != nil {
		t.Fatal(err)
	}
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 42 --repo test/repo --json title,body": {stdout: string(encoded) + "\n"},
	}), nil, "", "test/repo")

	got, err := host.GetPRContent(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("GetPRContent() error = %v", err)
	}
	if got.Title != "fix: restamp" || got.Body != body {
		t.Fatalf("GetPRContent() = %+v, want title and body from gh", got)
	}
}

func TestGetPRContentFailsClosedWithoutIdentity(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(nil), nil, "", "test/repo")
	if _, err := host.GetPRContent(context.Background(), &scm.PR{}); err == nil {
		t.Fatal("GetPRContent() with no PR identity: expected error, got nil")
	}
}

func TestGetPRStatePassesRepoFlag(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json state --jq .state": {
			stdout: "MERGED\n",
		},
	}), nil, "", "test/repo")

	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if state != scm.PRStateMerged {
		t.Fatalf("GetPRState() = %q, want %q", state, scm.PRStateMerged)
	}
}

func TestCreatePRStreamsBodyThroughStdin(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n- keep generated pull request bodies postable"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr create --head feature/body-cap --base main --repo test/repo --title fix: cap body --body-file -": {
			stdout:    "https://github.com/test/repo/pull/42\n",
			wantStdin: body,
		},
	}), nil, "", "test/repo")

	pr, err := host.CreatePR(context.Background(), "feature/body-cap", "main", scm.PRContent{
		Title: "fix: cap body",
		Body:  body,
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "42" {
		t.Fatalf("CreatePR() PR = %+v, want #42", pr)
	}
}

func TestCreatePRAddsDraftFlagWhenConfigured(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n- open as draft"
	host := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr create --head feature/draft --base main --repo test/repo --draft --title fix: draft --body-file -": {
			stdout:    "https://github.com/test/repo/pull/7\n",
			wantStdin: body,
		},
	}), nil, "", "test/repo", "", true)

	pr, err := host.CreatePR(context.Background(), "feature/draft", "main", scm.PRContent{
		Title: "fix: draft",
		Body:  body,
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr == nil || pr.Number != "7" {
		t.Fatalf("CreatePR() PR = %+v, want #7", pr)
	}
}

func TestUpdatePRStreamsBodyThroughStdin(t *testing.T) {
	t.Parallel()

	const body = "## What Changed\n\n- update existing pull request bodies without long argv"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr edit 42 --repo test/repo --title fix: cap body --body-file -": {
			wantStdin: body,
		},
	}), nil, "", "test/repo")

	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	updated, err := host.UpdatePR(context.Background(), pr, scm.PRContent{
		Title: "fix: cap body",
		Body:  body,
	})
	if err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if updated != pr {
		t.Fatalf("UpdatePR() = %+v, want original PR", updated)
	}
}

func TestUpdatePROmitsTitleWhenEmpty(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")
	if _, err := host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{
		Body: "marker only",
	}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
	}
	got := strings.Join(recorded[0], " ")
	if strings.Contains(got, "--title") {
		t.Fatalf("body-only UpdatePR must not pass --title, got %v", recorded[0])
	}
	if !strings.Contains(got, "--body-file") {
		t.Fatalf("body-only UpdatePR must still pass --body-file, got %v", recorded[0])
	}
}

// UpdatePR shares the same explicit-PR selector boundary as the read methods:
// when the number is absent it must target the canonical PR URL, never an empty
// positional that makes `gh pr edit` resolve the cwd branch (main) from the
// detached bare gate repo and edit the wrong PR.
func TestUpdatePRTargetsKnownPRByURLWhenNumberMissing(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")

	prURL := "https://github.com/test/repo/pull/123"
	if _, err := host.UpdatePR(context.Background(), &scm.PR{URL: prURL}, scm.PRContent{
		Title: "fix: cap body",
		Body:  "body",
	}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
	}
	got := recorded[0]
	// argv is: gh pr edit <selector> --repo ...
	if len(got) < 4 || got[1] != "pr" || got[2] != "edit" {
		t.Fatalf("unexpected argv: %v", got)
	}
	if selector := got[3]; selector != prURL {
		t.Fatalf("edit selector = %q, want the known PR URL %q (empty selector makes gh resolve the cwd branch)", selector, prURL)
	}
}

// UpdatePR must fail closed exactly like the read methods: with neither number
// nor URL it refuses to shell out rather than running an argument-less
// `gh pr edit` that would edit the inferred cwd branch's PR.
func TestUpdatePRFailsClosedWithoutIdentity(t *testing.T) {
	t.Parallel()

	host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")

	if _, err := host.UpdatePR(context.Background(), &scm.PR{}, scm.PRContent{Title: "t", Body: "b"}); err == nil {
		t.Fatal("UpdatePR() with no PR identity: expected error, got nil")
	}
}

func TestSetPRBaseBranchTargetsKnownPRByURLWhenNumberMissing(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")

	prURL := "https://github.com/test/repo/pull/123"
	if err := host.SetPRBaseBranch(context.Background(), &scm.PR{URL: prURL}, "epic/feature"); err != nil {
		t.Fatalf("SetPRBaseBranch() error = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
	}
	got := recorded[0]
	if len(got) < 4 || got[1] != "pr" || got[2] != "edit" {
		t.Fatalf("unexpected argv: %v", got)
	}
	if selector := got[3]; selector != prURL {
		t.Fatalf("edit selector = %q, want the known PR URL %q", selector, prURL)
	}
	if !containsArg(got, "--base") || !containsArg(got, "epic/feature") {
		t.Fatalf("expected --base epic/feature, got %v", got)
	}
}

func TestSetPRBaseBranchFailsClosedWithoutIdentity(t *testing.T) {
	t.Parallel()

	host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")
	if err := host.SetPRBaseBranch(context.Background(), &scm.PR{}, "epic/feature"); err == nil {
		t.Fatal("SetPRBaseBranch() with no PR identity: expected error, got nil")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestGetChecksFallsBackToStateWhenBucketMissing(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":""},{"name":"tests","state":"PENDING","bucket":""}]` + "\n",
		},
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}
	if checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketFail {
		t.Fatalf("checks[0] = %+v, want failing build check", checks[0])
	}
	if checks[1].Name != "tests" || checks[1].Bucket != scm.CheckBucketPending {
		t.Fatalf("checks[1] = %+v, want pending tests check", checks[1])
	}
}

// recordingCmdFactory captures the argv of every gh invocation into recorded
// and replies with a fixed successful stdout, so tests can assert exactly which
// PR selector reached gh instead of matching a whole command string.
func recordingCmdFactory(stdout string, recorded *[][]string) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*recorded = append(*recorded, append([]string{name}, args...))
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", "recorded")
		cmd.Env = append(os.Environ(),
			"GITHUB_TEST_HELPER=1",
			"GITHUB_TEST_STDOUT="+stdout,
			"GITHUB_TEST_EXIT_CODE=0",
		)
		return cmd
	}
}

// failIfInvokedCmdFactory fails the test if gh is invoked at all. It proves that
// a PR-targeting call fails closed (never shelling out) when the PR identity is
// unknown, instead of running an argument-less gh that infers the cwd branch.
func failIfInvokedCmdFactory(t *testing.T) CmdFactory {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Fatalf("gh should not be invoked without a known PR identity; got: %s %s", name, strings.Join(args, " "))
		return nil
	}
}

// The final CI check lookup must target the exact PR the pipeline already knows.
//
// Trigger: the CI monitor calls GetChecks with a PR the pipeline identifies by
// URL (Number can be empty when the identity was carried as a URL only).
// Masking condition: the daemon runs gh from the detached bare gate repo whose
// HEAD is the default branch (main).
// Symptom: appending an empty pr.Number produced an argument-less
// `gh pr checks --repo <slug>`, so gh fell back to resolving the cwd branch
// (main) and reported "no pull requests found for branch main" even though the
// feature PR's exact-head checks are green — certification could never finish.
//
// The fix passes the canonical PR URL as the explicit selector when the number
// is absent, so the target is always the known PR, never an inferred branch.
func TestGetChecksTargetsKnownPRByURLWhenNumberMissing(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("[]\n", &recorded), nil, "", "test/repo")

	prURL := "https://github.com/test/repo/pull/123"
	if _, err := host.GetChecks(context.Background(), &scm.PR{URL: prURL}); err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
	}
	got := recorded[0]
	// argv is: gh pr checks <selector> --repo ...
	if len(got) < 4 || got[1] != "pr" || got[2] != "checks" {
		t.Fatalf("unexpected argv: %v", got)
	}
	selector := got[3]
	if selector != prURL {
		t.Fatalf("check selector = %q, want the known PR URL %q (empty selector makes gh resolve the cwd branch)", selector, prURL)
	}
}

// Compare with the proven explicit-PR invocation: when the number is known it is
// passed verbatim as the selector, exactly as before.
func TestGetChecksTargetsKnownPRByNumber(t *testing.T) {
	t.Parallel()

	var recorded [][]string
	host := New(recordingCmdFactory("[]\n", &recorded), nil, "", "test/repo")

	if _, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", URL: "https://github.com/test/repo/pull/123"}); err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(recorded) != 1 || len(recorded[0]) < 4 {
		t.Fatalf("unexpected invocations: %v", recorded)
	}
	if selector := recorded[0][3]; selector != "123" {
		t.Fatalf("check selector = %q, want %q", selector, "123")
	}
}

// Missing/invalid PR identity must stop safely rather than checking main or some
// other PR: with neither number nor URL, the PR-targeting reads refuse to shell
// out at all.
func TestPRTargetingReadsFailClosedWithoutIdentity(t *testing.T) {
	t.Parallel()

	host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")
	pr := &scm.PR{}

	if _, err := host.GetChecks(context.Background(), pr); err == nil {
		t.Fatal("GetChecks() with no PR identity: expected error, got nil")
	}
	if _, err := host.GetPRState(context.Background(), pr); err == nil {
		t.Fatal("GetPRState() with no PR identity: expected error, got nil")
	}
	if _, err := host.GetMergeableState(context.Background(), pr); err == nil {
		t.Fatal("GetMergeableState() with no PR identity: expected error, got nil")
	}
}

// GetPRState and GetMergeableState share the same selector boundary as
// GetChecks, so a URL-only PR must target the URL there too.
func TestPRStateAndMergeableTargetKnownPRByURL(t *testing.T) {
	t.Parallel()

	prURL := "https://github.com/test/repo/pull/123"

	var stateArgs [][]string
	stateHost := New(recordingCmdFactory("OPEN\n", &stateArgs), nil, "", "test/repo")
	if _, err := stateHost.GetPRState(context.Background(), &scm.PR{URL: prURL}); err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if len(stateArgs) != 1 || len(stateArgs[0]) < 4 || stateArgs[0][3] != prURL {
		t.Fatalf("GetPRState selector = %v, want %q at argv[3]", stateArgs, prURL)
	}

	var mergeArgs [][]string
	mergeHost := New(recordingCmdFactory("MERGEABLE\n", &mergeArgs), nil, "", "test/repo")
	if _, err := mergeHost.GetMergeableState(context.Background(), &scm.PR{URL: prURL}); err != nil {
		t.Fatalf("GetMergeableState() error = %v", err)
	}
	if len(mergeArgs) != 1 || len(mergeArgs[0]) < 4 || mergeArgs[0][3] != prURL {
		t.Fatalf("GetMergeableState selector = %v, want %q at argv[3]", mergeArgs, prURL)
	}
}

func TestGetChecksParsesCompletedAt(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"build","state":"FAILURE","bucket":"fail","completedAt":"2026-04-24T04:15:00Z"},{"name":"tests","state":"SUCCESS","bucket":"pass","completedAt":"not-a-time"}]` + "\n",
		},
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}

	wantCompletedAt := time.Date(2026, 4, 24, 4, 15, 0, 0, time.UTC)
	if !checks[0].CompletedAt.Equal(wantCompletedAt) {
		t.Fatalf("checks[0].CompletedAt = %v, want %v", checks[0].CompletedAt, wantCompletedAt)
	}
	if !checks[1].CompletedAt.IsZero() {
		t.Fatalf("checks[1].CompletedAt = %v, want zero time for invalid timestamp", checks[1].CompletedAt)
	}
}

func TestGetChecksParsesStateAndLink(t *testing.T) {
	t.Parallel()

	const link = "https://github.com/test/repo/actions/runs/900/job/901"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"build","state":"cancelled","bucket":"cancel","link":"` + link + `"}]` + "\n",
		},
	}), nil, "", "")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	// The state is what tells a cancelled check apart from a failed one, and the
	// link is what identifies the job to re-run. Both are normalized: the state
	// upper-cased so callers can compare it, the link trimmed.
	if checks[0].State != "CANCELLED" {
		t.Fatalf("checks[0].State = %q, want CANCELLED", checks[0].State)
	}
	if checks[0].Link != link {
		t.Fatalf("checks[0].Link = %q, want %q", checks[0].Link, link)
	}
}

// A rerun must target the exact job behind the check so a genuinely failing job
// in the same workflow run is not re-run along with it. Real details URLs carry
// a query (?check_suite_focus=true) or a step fragment (#step:4:12), and neither
// is part of the job identity, so every one of these shapes must reach the same
// single job.
func TestRerunCheckTargetsJobFromCheckLink(t *testing.T) {
	t.Parallel()

	for name, link := range map[string]string{
		"plain":              "https://github.com/test/repo/actions/runs/900/job/901",
		"query string":       "https://github.com/test/repo/actions/runs/900/job/901?check_suite_focus=true",
		"step fragment":      "https://github.com/test/repo/actions/runs/900/job/901#step:4:12",
		"query and fragment": "https://github.com/test/repo/actions/runs/900/job/901?check_suite_focus=true#step:4:12",
		"trailing slash":     "https://github.com/test/repo/actions/runs/900/job/901/",
	} {
		t.Run(name, func(t *testing.T) {
			var recorded [][]string
			host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")

			check := scm.Check{
				Name:   "build (ubuntu-latest)",
				Bucket: scm.CheckBucketCancel,
				State:  "CANCELLED",
				Link:   link,
			}
			if err := host.RerunCheck(context.Background(), &scm.PR{Number: "123"}, check); err != nil {
				t.Fatalf("RerunCheck() error = %v", err)
			}
			if len(recorded) != 1 {
				t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
			}
			want := []string{"gh", "run", "rerun", "--job", "901", "--repo", "test/repo"}
			if strings.Join(recorded[0], " ") != strings.Join(want, " ") {
				t.Fatalf("rerun argv = %v, want %v", recorded[0], want)
			}
		})
	}
}

// One infrastructure allowance covers the classified failed jobs and their
// provider-rerun dependents. The safe bit is issued only by the joined topology
// proof; without it no provider mutation occurs.
func TestRerunCheck_ArtifactInfrastructureRequiresJoinedDependencyProof(t *testing.T) {
	t.Parallel()

	for _, safe := range []bool{false, true} {
		var recorded [][]string
		host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")
		check := scm.Check{
			Name:                    "browser",
			Bucket:                  scm.CheckBucketFail,
			State:                   "FAILURE",
			Link:                    "https://github.com/test/repo/actions/runs/900/job/901",
			InfrastructureFailure:   true,
			InfrastructureGroup:     "github-actions-run:900",
			InfrastructureRerunSafe: safe,
		}
		err := host.RerunCheck(context.Background(), &scm.PR{Number: "123"}, check)
		if !safe {
			if err == nil || len(recorded) != 0 {
				t.Fatalf("unproved dependency family reached provider: err=%v calls=%v", err, recorded)
			}
			continue
		}
		if err != nil {
			t.Fatalf("joined dependency family rerun failed: %v", err)
		}
		want := []string{"gh", "run", "rerun", "900", "--failed", "--repo", "test/repo"}
		if len(recorded) != 1 || strings.Join(recorded[0], " ") != strings.Join(want, " ") {
			t.Fatalf("rerun argv = %v, want %v", recorded, want)
		}
	}
}

func TestRerunCheckTargetsWholeCancelledRun(t *testing.T) {
	t.Parallel()

	for name, link := range map[string]string{
		"run only":       "https://github.com/test/repo/actions/runs/900",
		"trailing slash": "https://github.com/test/repo/actions/runs/900/",
		"with a query":   "https://github.com/test/repo/actions/runs/900?check_suite_focus=true",
	} {
		t.Run(name, func(t *testing.T) {
			var recorded [][]string
			host := New(recordingCmdFactory("", &recorded), nil, "", "test/repo")

			check := scm.Check{
				Name:   "build",
				Bucket: scm.CheckBucketCancel,
				State:  "CANCELLED",
				Link:   link,
			}
			if err := host.RerunCheck(context.Background(), &scm.PR{Number: "123"}, check); err != nil {
				t.Fatalf("RerunCheck() error = %v", err)
			}
			if len(recorded) != 1 {
				t.Fatalf("expected exactly one gh invocation, got %d: %v", len(recorded), recorded)
			}
			want := []string{"gh", "run", "rerun", "900", "--repo", "test/repo"}
			if strings.Join(recorded[0], " ") != strings.Join(want, " ") {
				t.Fatalf("rerun argv = %v, want %v", recorded[0], want)
			}
		})
	}
}

// A link this backend cannot resolve to one job must not be downgraded into a
// whole-run rerun: that would re-run every failed job in the run, including
// genuinely failing ones, on the strength of a link it could not read. It fails
// closed instead, so the check escalates exactly as it would without the policy.
//
// The browser's plural ".../jobs/<n>" form is one of these: that number is a
// per-run display index the API answers with 404, not the job databaseId
// `gh run rerun --job` needs.
func TestRerunCheckFailsClosedWithoutAnActionsJob(t *testing.T) {
	t.Parallel()

	for name, link := range map[string]string{
		"external dashboard":       "https://ci.example.com/builds/17",
		"third-party Actions path": "https://ci.example.com/test/repo/actions/runs/900/job/901",
		"wrong Actions repository": "https://github.com/other/repo/actions/runs/900/job/901",
		"no link":                  "",
		"non-numeric run":          "https://github.com/test/repo/actions/runs/latest",
		"browser display number":   "https://github.com/test/repo/actions/runs/900/jobs/3",
		"display number with args": "https://github.com/test/repo/actions/runs/900/jobs/3?pr=1",
		"non-numeric job segment":  "https://github.com/test/repo/actions/runs/900/job/latest",
		"job segment with a step":  "https://github.com/test/repo/actions/runs/900/job/latest#step:1:1",
		"unknown run subpath":      "https://github.com/test/repo/actions/runs/900/attempts/2",
	} {
		t.Run(name, func(t *testing.T) {
			host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")
			err := host.RerunCheck(context.Background(), &scm.PR{Number: "123"}, scm.Check{Name: "build", Bucket: scm.CheckBucketFail, State: "TIMED_OUT", Link: link})
			if err == nil {
				t.Fatal("RerunCheck() expected an error for a check with no Actions job")
			}
		})
	}
}

// The provider refusing the rerun must reach the caller: the CI step decides
// what to do with a failed request, and silently reporting success would make it
// wait for a re-run that never happens.
func TestRerunCheckPropagatesProviderError(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh run rerun --job 901 --repo test/repo": {
			stderr: "HTTP 403: Unable to retry this workflow run",
			code:   1,
		},
	}), nil, "", "test/repo")

	err := host.RerunCheck(context.Background(), &scm.PR{Number: "123"}, scm.Check{
		Name:   "build",
		Bucket: scm.CheckBucketFail,
		State:  "TIMED_OUT",
		Link:   "https://github.com/test/repo/actions/runs/900/job/901",
	})
	if err == nil {
		t.Fatal("RerunCheck() expected the provider error to propagate")
	}
	if !strings.Contains(err.Error(), "Unable to retry this workflow run") {
		t.Fatalf("RerunCheck() error = %v, want the provider message", err)
	}
}

func TestFetchFailedCheckLogsSelectsMatchingRunForHeadSHA(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh run list --branch feature --commit abc123 --status failure --limit 20 --json databaseId,headSha,name,displayTitle,workflowName": {
			stdout: `[{"databaseId":101,"headSha":"abc123","name":"CI","displayTitle":"feature","workflowName":"CI"},{"databaseId":102,"headSha":"abc123","name":"Lint","displayTitle":"lint","workflowName":"Lint"}]` + "\n",
		},
		"gh run view 101 --json jobs": {
			stdout: `{"jobs":[{"name":"unit","conclusion":"failure"}]}` + "\n",
		},
		"gh run view 102 --json jobs": {
			stdout: `{"jobs":[{"name":"lint","conclusion":"failure"}]}` + "\n",
		},
		"gh run view 102 --log-failed": {
			stdout: "lint failed\n",
		},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "123"}, "feature", "abc123", []string{"lint"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "lint failed" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want %q", logs, "lint failed")
	}
}

func TestFetchFailedCheckTargetLogsSelectsProviderIdentityOverName(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh run list --branch feature --commit abc123 --status failure --limit 20 --json databaseId,headSha,name,displayTitle,workflowName": {
			stdout: `[{"databaseId":102,"name":"CI"}]` + "\n",
		},
		"gh run view 102 --json jobs":     {stdout: `{"jobs":[{"databaseId":201,"name":"build","conclusion":"failure"},{"databaseId":202,"name":"build","conclusion":"failure"}]}` + "\n"},
		"gh run view 102 --job 202 --log": {stdout: "selected build failed\n"},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "feature", "abc123", []scm.CheckTarget{{Name: "build", ProviderID: "github-check-run:202"}})
	if err != nil {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].Output != "selected build failed" {
		t.Fatalf("FetchFailedCheckTargetLogs() = %+v, want selected check's logs", logs)
	}
}

func TestFetchFailedCheckTargetLogsReturnsPartialLogsWithRetrievalError(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh run list --branch feature --commit abc123 --status failure --limit 20 --json databaseId,headSha,name,displayTitle,workflowName": {stdout: `[{"databaseId":102,"name":"CI"}]` + "\n"},
		"gh run view 102 --json jobs":     {stdout: `{"jobs":[{"databaseId":201,"name":"build","conclusion":"failure"},{"databaseId":202,"name":"lint","conclusion":"failure"}]}` + "\n"},
		"gh run view 102 --job 201 --log": {stdout: "build failed\n"},
		"gh run view 102 --job 202 --log": {stderr: "expired", code: 1},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "feature", "abc123", []scm.CheckTarget{{ProviderID: "github-check-run:201"}, {ProviderID: "github-check-run:202"}})
	if err != nil || len(logs) != 2 || logs[0].Output != "build failed" || logs[1].Err == nil || !strings.Contains(logs[1].Err.Error(), "job 202") {
		t.Fatalf("FetchFailedCheckTargetLogs() = (%+v, %v), want retained partial logs and job 202 error", logs, err)
	}
}

func TestFetchFailedCheckTargetLogsReportsMissingSelectedJob(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh run list --branch feature --commit abc123 --status failure --limit 20 --json databaseId,headSha,name,displayTitle,workflowName": {stdout: `[{"databaseId":102,"name":"CI"}]` + "\n"},
		"gh run view 102 --json jobs": {stdout: `{"jobs":[{"databaseId":201,"name":"build","conclusion":"failure"}]}` + "\n"},
	}), nil, "", "")

	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "123"}, "feature", "abc123", []scm.CheckTarget{{ProviderID: "github-check-run:999"}})
	if err != nil || len(logs) != 1 || logs[0].Err == nil || !strings.Contains(logs[0].Err.Error(), "github-check-run:999") {
		t.Fatalf("FetchFailedCheckTargetLogs() = (%+v, %v), want explicit missing-target error", logs, err)
	}
}

// A GitHub Actions action-download outage fails a job inside "Set up job",
// before any repository step runs. PreRunFailures must flag exactly that job -
// read structurally from the setup step's conclusion, never from log text - and
// must never flag a job that cleared setup and failed a later (repository) step.
// The two directions together are the masking-safety contract.
func TestPreRunFailures_FlagsSetupFailureNotGenuine(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/1/jobs -f filter=latest -f per_page=100": {
			stdout: `{"jobs":[` +
				`{"id":2,"name":"build","conclusion":"failure","steps":[{"name":"Set up job","number":1,"conclusion":"failure"}]},` +
				`{"id":3,"name":"unit","conclusion":"failure","steps":[{"name":"Set up job","number":1,"conclusion":"success"},{"name":"Run tests","number":2,"conclusion":"failure"}]}` +
				`]}` + "\n",
		},
	}), nil, "", "test/repo")

	infra, err := host.PreRunFailures(context.Background(), []scm.Check{
		{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"},
		{Name: "unit", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/3"},
	})
	if err != nil {
		t.Fatalf("PreRunFailures() error = %v", err)
	}
	if len(infra) != 2 {
		t.Fatalf("PreRunFailures returned %d results, want 2 parallel to the checks", len(infra))
	}
	if !infra[0] {
		t.Error("PreRunFailures did not flag the setup/action-download failure")
	}
	if infra[1] {
		t.Error("PreRunFailures flagged a genuine test failure that cleared setup (masking)")
	}
}

// A run the provider cannot report on must leave every check unflagged, so an
// unreadable job stays a genuine failure rather than being masked.
func TestPreRunFailures_FailsClosedOnUnreadableRun(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/9/jobs -f filter=latest -f per_page=100": {stderr: "HTTP 404\n", code: 1},
	}), nil, "", "test/repo")

	infra, err := host.PreRunFailures(context.Background(), []scm.Check{
		{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/9/job/2"},
	})
	if err != nil {
		t.Fatalf("PreRunFailures() error = %v", err)
	}
	if len(infra) != 1 || infra[0] {
		t.Fatalf("PreRunFailures = %v, want nothing flagged when the run is unreadable", infra)
	}
}

// These terminal records come from the @actions/artifact 2.3.2 client locked by
// upload-artifact v4.6.2 and download-artifact v4.3.0; the latter action adds
// its own Unable-to-download wrapper.
const (
	uploadArtifact403Record   = "##[error]Failed to FinalizeArtifact: Received non-retryable error: Failed request: (403) Forbidden"
	uploadArtifact503Record   = "##[error]Failed to FinalizeArtifact: Failed to make request after 5 attempts: Failed request: (503) Service Unavailable"
	downloadArtifact403Record = "##[error]Unable to download artifact(s): Failed to ListArtifacts: Received non-retryable error: Failed request: (403) Forbidden: Error from intermediary with HTTP status code 403 \"Forbidden\""
	downloadArtifact503Record = "##[error]Unable to download artifact(s): Failed to ListArtifacts: Failed to make request after 5 attempts: Failed request: (503) Service Unavailable"
)

func observedShardThreeFailure() githubRunJob {
	return githubRunJob{
		ID: 102144122733, HeadSHA: "c02baaa297b37d2210638b55cd57794cbb4abf4c", Name: "repository shard 3 of 4", Status: "completed", Conclusion: "failure",
		Steps: []githubJobStep{
			{Name: "Set up job", Number: 1, Conclusion: "success"},
			{Name: "Set up runner", Number: 2, Conclusion: "success"},
			{Name: "Check out exact event head", Number: 3, Conclusion: "success"},
			{Name: "Set up Python 3.14.7", Number: 4, Conclusion: "success"},
			{Name: "Set up Node 20.19.0", Number: 5, Conclusion: "success"},
			{Name: "Install pinned repository tools", Number: 6, Conclusion: "success"},
			{Name: "Verify immutable event head", Number: 7, Conclusion: "success"},
			{Name: "Download the sealed Dashboard V2 distribution", Number: 8, Conclusion: "failure"},
			{Name: "Verify the downloaded distribution before any test runs", Number: 9, Conclusion: "skipped"},
			{Name: "Prepare candidate-bound browser evidence", Number: 10, Conclusion: "skipped"},
			{Name: "Run this shard of the battery and classify routine skips", Number: 11, Conclusion: "skipped"},
			{Name: "Upload retained shard browser evidence", Number: 12, Conclusion: "failure"},
			{Name: "Upload this shard's manifest and log", Number: 13, Conclusion: "skipped"},
			{Name: "Print verify log tail on failure", Number: 14, Conclusion: "success"},
			{Name: "Upload verify log artifact on failure", Number: 15, Conclusion: "success"},
			{Name: "Prove checkout cleanliness", Number: 16, Conclusion: "skipped"},
			{Name: "Post Set up Node 20.19.0", Number: 29, Conclusion: "skipped"},
			{Name: "Post Set up Python 3.14.7", Number: 30, Conclusion: "skipped"},
			{Name: "Post Check out exact event head", Number: 31, Conclusion: "success"},
			{Name: "Complete runner", Number: 32, Conclusion: "success"},
			{Name: "Complete job", Number: 33, Conclusion: "success"},
		},
	}
}

func observedShardThreeLogs() string {
	return strings.Join([]string{
		"##[group]Run actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		"##[group]Run actions/setup-python@5fda3b95a4ea91299a34e894583c3862153e4b97",
		"##[group]Run actions/setup-node@820762786026740c76f36085b0efc47a31fe5020",
		"##[group]Run install pinned repository tools",
		"##[group]Run verify immutable event head",
		"##[group]Run actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093",
		downloadArtifact403Record,
		"##[group]Run actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"##[error]No files were found with the provided path: /tmp/evidence. No artifacts will be uploaded.",
		"##[group]Run print verify log tail",
		"##[group]Run actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
	}, "\n") + "\n"
}

// This fixture is a bounded reconstruction of attempt-one run 34250655836 / job
// 102144122733 at c02baaa297b37d2210638b55cd57794cbb4abf4c. The source receipt
// was reread with gh-axi: the 2,373-byte structured job receipt hashes to
// cfa13f9b...d2f6ac0 and the 9,024-byte untruncated failed-log receipt hashes to
// 45be30d6...5c8130. Only setup noise is abbreviated here; the pinned action and
// terminal error records are exact. Later required steps are retry dependents,
// not evidence that a product test failed.
func TestArtifactFailureDisposition_ObservedDownloadFailureMarksLaterRequiredStepsAsDependents(t *testing.T) {
	t.Parallel()

	disposition, ok := artifactFailureDisposition(observedShardThreeFailure(), observedShardThreeLogs(), false)
	if !ok || !disposition.Initiating {
		t.Fatalf("observed job disposition = %+v, %v; want initiating infrastructure failure", disposition, ok)
	}
	want := []string{
		"Verify the downloaded distribution before any test runs",
		"Prepare candidate-bound browser evidence",
		"Run this shard of the battery and classify routine skips",
		"Upload retained shard browser evidence",
		"Upload this shard's manifest and log",
		"Prove checkout cleanliness",
	}
	if len(disposition.DependentSteps) != len(want) {
		t.Fatalf("dependent steps = %+v, want %v", disposition.DependentSteps, want)
	}
	for i := range want {
		if disposition.DependentSteps[i].Name != want[i] {
			t.Fatalf("dependent step %d = %+v, want %q", i, disposition.DependentSteps[i], want[i])
		}
	}
}

func TestArtifactFailureDisposition_RealTestFailureAfterInitiatingErrorRefuses(t *testing.T) {
	t.Parallel()

	job := observedShardThreeFailure()
	for i := range job.Steps {
		if job.Steps[i].Name == "Run this shard of the battery and classify routine skips" {
			job.Steps[i].Conclusion = "failure"
		}
	}
	logs := strings.Replace(
		observedShardThreeLogs(),
		"##[group]Run actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"##[group]Run pytest -q\n##[error]test suite failed\n##[group]Run actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		1,
	)
	if got, ok := artifactFailureDisposition(job, logs, false); ok {
		t.Fatalf("real test failure admitted after artifact error: %+v", got)
	}
}

func joinedXAUJobPopulation() []githubRunJob {
	names := []string{
		"verify original run or bounded infrastructure retry",
		"build and seal the Dashboard V2 distribution",
		"journey smoke (chromium / desktop)",
		"journey smoke (firefox / desktop)",
		"journey smoke (webkit / desktop)",
		"journey smoke (chromium / touch390)",
		"journey smoke (webkit / touch390)",
		"look for a proving pull request with the same pushed tree",
		"repository shard 1 of 4",
		"repository shard 2 of 4",
		"repository shard 3 of 4",
		"repository shard 4 of 4",
		"repository checks",
		"repository",
	}
	jobs := make([]githubRunJob, len(names))
	for i, name := range names {
		jobs[i] = githubRunJob{ID: i + 1, HeadSHA: "head-1", Name: name, Status: "completed", Conclusion: "success", Steps: []githubJobStep{{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"}}}
	}
	jobs[7].Conclusion = "skipped"
	jobs[7].Steps = nil
	return jobs
}

// PR 417 at 887f65f06 owns five distinct browser cells. The fork must admit
// exactly that population and the one event-bound PR reuse-check omission;
// missing, duplicated, or substituted browser cells fail closed.
func TestJoinedXAUArtifactRetryTopology_AcceptsFiveCellsAndRejectsPopulationDrift(t *testing.T) {
	t.Parallel()

	valid := joinedXAUJobPopulation()
	if !joinedXAUArtifactRetryTopology("pull_request", valid) {
		t.Fatal("complete five-cell PR topology was rejected")
	}
	for name, mutate := range map[string]func([]githubRunJob) []githubRunJob{
		"missing cell": func(jobs []githubRunJob) []githubRunJob { return append(jobs[:4], jobs[5:]...) },
		"duplicate cell": func(jobs []githubRunJob) []githubRunJob {
			jobs[3].Name = jobs[2].Name
			return jobs
		},
		"wrong engine": func(jobs []githubRunJob) []githubRunJob {
			jobs[4].Name = "journey smoke (edge / desktop)"
			return jobs
		},
	} {
		t.Run(name, func(t *testing.T) {
			jobs := append([]githubRunJob(nil), valid...)
			if joinedXAUArtifactRetryTopology("pull_request", mutate(jobs)) {
				t.Fatalf("%s topology drift was admitted", name)
			}
		})
	}
	if joinedXAUArtifactRetryTopology("push", valid) {
		t.Fatal("PR-only reuse-check omission was admitted on push")
	}
	push := append([]githubRunJob(nil), valid...)
	push[7].Conclusion = "success"
	push[7].Steps = []githubJobStep{{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"}}
	if !joinedXAUArtifactRetryTopology("push", push) {
		t.Fatal("complete push topology with an executed reuse check was rejected")
	}
	if joinedXAUArtifactRetryTopology("schedule", push) {
		t.Fatal("an unsupported event was admitted by the joined topology")
	}
	withUnknownSkip := append([]githubRunJob(nil), valid...)
	withUnknownSkip[2].Steps = append([]githubJobStep(nil), valid[2].Steps...)
	withUnknownSkip[2].Steps = append(withUnknownSkip[2].Steps, githubJobStep{Name: "Skip a required journey assertion", Number: 3, Conclusion: "skipped"})
	if joinedXAUArtifactRetryTopology("pull_request", withUnknownSkip) {
		t.Fatal("successful retained producer with an unknown skipped step was admitted")
	}
}

func TestJoinedXAUAttemptOneOmittedStep_AllowsOnlyExactStepOwnerPairs(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{
		{"journey smoke (chromium / desktop)", joinedXAURetainedBundleStep},
		{"journey smoke (webkit / touch390)", joinedXAURetainedBundleStep},
		{"repository shard 1 of 4", joinedXAURetainedBundleStep},
		{"repository shard 4 of 4", joinedXAURetainedBundleStep},
		{"repository", joinedXAURetainedProofStep},
		{"repository checks", "Print verify log tail on failure"},
		{"repository shard 3 of 4", "Upload verify log artifact on failure"},
	} {
		if !joinedXAUAttemptOneOmittedStep(pair[0], pair[1]) {
			t.Errorf("expected attempt-one omission was refused: job=%q step=%q", pair[0], pair[1])
		}
	}

	for _, pair := range [][2]string{
		{"repository", joinedXAURetainedBundleStep},
		{"repository shard 1 of 4", joinedXAURetainedProofStep},
		{"journey smoke (chromium / desktop)", "Print verify log tail on failure"},
		{"repository checks", joinedXAURetainedBundleStep},
		{"repository shard 3 of 4", "Skip a required journey assertion"},
	} {
		if joinedXAUAttemptOneOmittedStep(pair[0], pair[1]) {
			t.Errorf("unknown or wrongly owned omission was admitted: job=%q step=%q", pair[0], pair[1])
		}
	}

	dependentFanIn := func(omittedStep string) githubRunJob {
		return githubRunJob{ID: 14, HeadSHA: "head-1", Name: "repository", Status: "completed", Conclusion: "failure", Steps: []githubJobStep{
			{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"},
			{Name: "Download every shard's manifest and log", Number: 3, Conclusion: "success"},
			{Name: omittedStep, Number: 4, Conclusion: "skipped"},
			{Name: "Prove every shard ran and together covered the battery", Number: 5, Conclusion: "failure"},
			{Name: "Upload full-lane proof artifact", Number: 6, Conclusion: "skipped"},
		}}
	}
	if dependent, ok := joinedXAURepositoryDependencyFailure(dependentFanIn(joinedXAURetainedProofStep), "fan-in failed"); !ok || len(dependent) != 2 {
		t.Fatalf("dependent fan-in with expected attempt-one omission = %+v, %v; want fan-in and upload obligations", dependent, ok)
	}
	for _, omittedStep := range []string{joinedXAURetainedBundleStep, "Check out exact event head", "Skip an unknown repository assertion"} {
		if dependent, ok := joinedXAURepositoryDependencyFailure(dependentFanIn(omittedStep), "fan-in failed"); ok {
			t.Errorf("dependent fan-in admitted wrong-owner or required-work omission %q: %+v", omittedStep, dependent)
		}
	}
}

func joinedXAUAttemptOneArtifacts(headSHA string) []scm.InfrastructureArtifactReceipt {
	names := []string{
		"dashboard-v2-dist-1",
		"delivery-lane-shard-1-" + headSHA + "-a1",
		"delivery-lane-shard-2-" + headSHA + "-a1",
		"delivery-lane-shard-3-" + headSHA + "-a1",
		"delivery-lane-shard-4-" + headSHA + "-a1",
		"xau-journey-evidence-chromium-desktop-" + headSHA + "-a1",
		"xau-journey-evidence-firefox-desktop-" + headSHA + "-a1",
		"xau-journey-evidence-webkit-desktop-" + headSHA + "-a1",
		"xau-journey-evidence-chromium-touch390-" + headSHA + "-a1",
		"xau-journey-evidence-webkit-touch390-" + headSHA + "-a1",
	}
	artifacts := make([]scm.InfrastructureArtifactReceipt, len(names))
	for i, name := range names {
		artifacts[i] = scm.InfrastructureArtifactReceipt{ID: int64(i + 1), Name: name, Digest: "sha256:" + strings.Repeat("a", 64), ProviderRunID: 1, HeadSHA: headSHA}
	}
	return artifacts
}

func githubArtifactPages(artifacts []scm.InfrastructureArtifactReceipt) string {
	items := make([]string, len(artifacts))
	for i, artifact := range artifacts {
		items[i] = fmt.Sprintf(
			`{"id":%d,"name":%q,"digest":%q,"expired":false,"workflow_run":{"id":%d,"head_sha":%q}}`,
			artifact.ID, artifact.Name, artifact.Digest, artifact.ProviderRunID, artifact.HeadSHA,
		)
	}
	return fmt.Sprintf(`[{"total_count":%d,"artifacts":[%s]}]`, len(items), strings.Join(items, ","))
}

// Pre-dispatch proof must retain every successful producer the XAU recovery
// helper will consume. A producer that is itself legitimately retried may
// publish its replacement later and therefore owes no attempt-one artifact.
func TestXAUArtifactReceiptsProveAttemptOne_RequiresEverySuccessfulProducer(t *testing.T) {
	t.Parallel()

	jobs := joinedXAUJobPopulation()
	artifacts := joinedXAUAttemptOneArtifacts("head-1")
	if !xauArtifactReceiptsProveAttemptOne(artifacts, 1, "head-1", jobs) {
		t.Fatal("complete retained producer set was rejected")
	}
	for name, missingIndex := range map[string]int{"bundle": 0, "shard": 1, "journey": 5} {
		t.Run(name, func(t *testing.T) {
			incomplete := append([]scm.InfrastructureArtifactReceipt(nil), artifacts[:missingIndex]...)
			incomplete = append(incomplete, artifacts[missingIndex+1:]...)
			if xauArtifactReceiptsProveAttemptOne(incomplete, 1, "head-1", jobs) {
				t.Fatalf("missing successful %s producer artifact was admitted", name)
			}
		})
	}
	retriedProducerJobs := append([]githubRunJob(nil), jobs...)
	retriedProducerJobs[8].Conclusion = "failure"
	withoutRetriedShard := append([]scm.InfrastructureArtifactReceipt(nil), artifacts[:1]...)
	withoutRetriedShard = append(withoutRetriedShard, artifacts[2:]...)
	if !xauArtifactReceiptsProveAttemptOne(withoutRetriedShard, 1, "head-1", retriedProducerJobs) {
		t.Fatal("artifact expected only from a legitimately retried producer was required before dispatch")
	}
}

func TestJoinedXAURetryDependentNames_BuildFailureSelectsEveryNonSuccessfulConsumer(t *testing.T) {
	t.Parallel()

	dependents := joinedXAURetryDependentNames(map[string]bool{"build and seal the Dashboard V2 distribution": true})
	want := []string{
		"journey smoke (chromium / desktop)", "journey smoke (firefox / desktop)", "journey smoke (webkit / desktop)",
		"journey smoke (chromium / touch390)", "journey smoke (webkit / touch390)",
		"repository shard 1 of 4", "repository shard 2 of 4", "repository shard 3 of 4", "repository shard 4 of 4", "repository",
	}
	if len(dependents) != len(want) {
		t.Fatalf("build dependents = %v, want %v", dependents, want)
	}
	for _, name := range want {
		if !dependents[name] {
			t.Fatalf("build dependency closure omitted %q: %v", name, dependents)
		}
	}
	if dependents["repository checks"] || dependents[joinedXAUPROnlyOmittedJob] {
		t.Fatalf("independent successful producer entered retry closure: %v", dependents)
	}
}

// GitHub's failed-job rerun expands through the workflow graph. When the
// successful producer guard is followed by an initiating build upload outage,
// every skipped consumer is part of that same bounded recovery family while
// the independent repository-check producer remains untouched.
func TestArtifactInfrastructureFailures_BuildOutageMarksEverySkippedConsumerAsDependent(t *testing.T) {
	t.Parallel()

	jobs := joinedXAUJobPopulation()
	jobs[1] = githubRunJob{ID: 2, HeadSHA: "head-1", Name: "build and seal the Dashboard V2 distribution", Status: "completed", Conclusion: "failure", Steps: []githubJobStep{
		{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"},
		{Name: "Upload the sealed distribution", Number: 3, Conclusion: "failure"},
	}}
	for _, index := range []int{2, 3, 4, 5, 6, 8, 9, 10, 11, 13} {
		jobs[index].Conclusion = "skipped"
		jobs[index].Steps = nil
	}
	jobsJSON, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	checks := make([]scm.Check, len(jobs))
	for i, job := range jobs {
		bucket, state := scm.CheckBucketPass, "SUCCESS"
		switch job.Conclusion {
		case "failure":
			bucket, state = scm.CheckBucketFail, "FAILURE"
		case "skipped":
			bucket, state = scm.CheckBucketSkip, "SKIPPED"
		}
		checks[i] = scm.Check{Name: job.Name, Bucket: bucket, State: state, Link: fmt.Sprintf("https://github.com/test/repo/actions/runs/1/job/%d", job.ID)}
	}
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/1":                                                    {stdout: `{"id":1,"event":"pull_request","head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`},
		"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":14,"jobs":` + string(jobsJSON) + `}]`},
		"gh api --method GET repos/test/repo/actions/jobs/2/logs":                                               {stdout: "##[group]Run retry guard\n##[group]Run actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02\n" + uploadArtifact503Record + "\n"},
		"gh api --method GET repos/test/repo/actions/runs/1/artifacts -f per_page=100 --paginate --slurp":       {stdout: `[{"total_count":0,"artifacts":[]}]`},
		"gh api --method GET repos/test/repo/contents/.github/workflows/xau-ci.yml -f ref=base-1":               {stdout: `{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"file"}`},
		"gh api --method GET repos/test/repo/contents/.github/workflows/xau-ci.yml -f ref=head-1":               {stdout: `{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"file"}`},
		"gh api --method GET repos/test/repo/contents/scripts/ci_check_infrastructure_retry.py -f ref=base-1":   {stdout: `{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","type":"file"}`},
		"gh api --method GET repos/test/repo/contents/scripts/ci_check_infrastructure_retry.py -f ref=head-1":   {stdout: `{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","type":"file"}`},
	}), nil, "", "test/repo")

	got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, checks)
	if err != nil {
		t.Fatal(err)
	}
	if !got[1].Retryable || got[1].RerunSafe || !strings.Contains(got[1].Reason, "identity is not reviewed") {
		t.Fatalf("initiating build disposition = %+v, want classified but identity-gated retry", got[1])
	}
	for _, index := range []int{2, 3, 4, 5, 6, 8, 9, 10, 11, 13} {
		if !got[index].RerunDependent || got[index].RerunSafe || got[index].Group != got[1].Group {
			t.Fatalf("dependent %q disposition = %+v, want same identity-gated group as build", checks[index].Name, got[index])
		}
	}
	if !got[7].RerunOmission || got[12].Group != "" {
		t.Fatalf("event omission or independent producer misclassified: omission=%+v repository_checks=%+v", got[7], got[12])
	}
}

func classifyJoinedXAUAttemptOne(t *testing.T, jobs []githubRunJob, logsByJobID map[int]string, artifacts []scm.InfrastructureArtifactReceipt) []scm.InfrastructureFailure {
	t.Helper()

	jobsJSON, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	checks := make([]scm.Check, len(jobs))
	for i, job := range jobs {
		bucket := scm.CheckBucketPass
		state := "SUCCESS"
		switch job.Conclusion {
		case "failure":
			bucket, state = scm.CheckBucketFail, "FAILURE"
		case "skipped":
			bucket, state = scm.CheckBucketSkip, "SKIPPED"
		}
		checks[i] = scm.Check{Name: job.Name, Bucket: bucket, State: state, Link: fmt.Sprintf("https://github.com/test/repo/actions/runs/1/job/%d", job.ID)}
	}
	responses := map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/1":                                                    {stdout: `{"id":1,"event":"pull_request","head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`},
		"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":14,"jobs":` + string(jobsJSON) + `}]`},
		"gh api --method GET repos/test/repo/actions/runs/1/artifacts -f per_page=100 --paginate --slurp":       {stdout: githubArtifactPages(artifacts)},
		"gh api --method GET repos/test/repo/contents/.github/workflows/xau-ci.yml -f ref=base-1":               {stdout: `{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"file"}`},
		"gh api --method GET repos/test/repo/contents/.github/workflows/xau-ci.yml -f ref=head-1":               {stdout: `{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"file"}`},
		"gh api --method GET repos/test/repo/contents/scripts/ci_check_infrastructure_retry.py -f ref=base-1":   {stdout: `{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","type":"file"}`},
		"gh api --method GET repos/test/repo/contents/scripts/ci_check_infrastructure_retry.py -f ref=head-1":   {stdout: `{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","type":"file"}`},
	}
	for jobID, logs := range logsByJobID {
		responses[fmt.Sprintf("gh api --method GET repos/test/repo/actions/jobs/%d/logs", jobID)] = githubTestResponse{stdout: logs}
	}
	host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
	got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, checks)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The joined contract classifies the two initiating shard failures and the
// genuinely failed repository fan-in as one retry set. GitHub then reruns those
// failed jobs and their dependents; successful producers remain untouched and
// their attempt-one artifacts carry immutable provenance into the receipt.
func TestArtifactInfrastructureFailures_JoinedFiveCellDependencyFamilyIsOneSafeGroup(t *testing.T) {
	t.Parallel()

	jobs := joinedXAUJobPopulation()
	failedShard := func(name string, id int) githubRunJob {
		return githubRunJob{ID: id, HeadSHA: "head-1", Name: name, Status: "completed", Conclusion: "failure", Steps: []githubJobStep{
			{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"},
			{Name: "Install pinned repository tools", Number: 3, Conclusion: "success"},
			{Name: "Download the sealed Dashboard V2 distribution", Number: 4, Conclusion: "failure"},
			{Name: "Verify the downloaded distribution before any test runs", Number: 5, Conclusion: "skipped"},
			{Name: "Run this shard of the battery and classify routine skips", Number: 6, Conclusion: "skipped"},
			{Name: "Upload retained shard browser evidence", Number: 7, Conclusion: "failure"},
			{Name: "Upload this shard's manifest and log", Number: 8, Conclusion: "skipped"},
		}}
	}
	jobs[10] = failedShard("repository shard 3 of 4", 11)
	jobs[11] = failedShard("repository shard 4 of 4", 12)
	jobs[2].Steps = []githubJobStep{
		{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"},
		{Name: "Download the sealed Dashboard V2 distribution", Number: 3, Conclusion: "success"},
		{Name: "Digest-bind and extract the retained Dashboard V2 distribution", Number: 4, Conclusion: "skipped"},
		{Name: "Verify the downloaded distribution before any test runs", Number: 5, Conclusion: "success"},
		{Name: "Run the core dashboard journeys and classify retained browser errors", Number: 6, Conclusion: "success"},
		{Name: "Upload retained journey evidence", Number: 7, Conclusion: "success"},
	}
	jobs[13] = githubRunJob{ID: 14, HeadSHA: "head-1", Name: "repository", Status: "completed", Conclusion: "failure", Steps: []githubJobStep{
		{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"},
		{Name: "Download every shard's manifest and log", Number: 3, Conclusion: "success"},
		{Name: "Digest-bind and extract retained shard and journey evidence", Number: 4, Conclusion: "skipped"},
		{Name: "Prove every shard ran and together covered the battery", Number: 5, Conclusion: "failure"},
		{Name: "Upload full-lane proof artifact", Number: 6, Conclusion: "skipped"},
	}}
	shardLogs := strings.Join([]string{
		"##[group]Run retry guard",
		"##[group]Run install tools",
		"##[group]Run actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093",
		downloadArtifact403Record,
		"##[group]Run actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"##[error]No files were found with the provided path: /tmp/evidence. No artifacts will be uploaded.",
	}, "\n") + "\n"
	allArtifacts := joinedXAUAttemptOneArtifacts("head-1")
	retainedArtifacts := append([]scm.InfrastructureArtifactReceipt(nil), allArtifacts[:3]...)
	retainedArtifacts = append(retainedArtifacts, allArtifacts[5:]...)
	got := classifyJoinedXAUAttemptOne(t, jobs, map[int]string{
		11: shardLogs,
		12: shardLogs,
		14: "##[group]Run retry guard\n##[group]Run actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093\n##[group]Run python -B scripts/ci_check_shard_fanin.py\n##[error]required repository shard jobs did not conclude successfully\n",
	}, retainedArtifacts)
	for _, index := range []int{10, 11, 13} {
		if !got[index].Retryable || got[index].RerunSafe || got[index].Group != "github-actions-run:1" || !strings.Contains(got[index].Reason, "identity is not reviewed") {
			t.Fatalf("failure %q disposition = %+v, want one complete but identity-gated group", jobs[index].Name, got[index])
		}
	}
	if !got[7].RerunOmission || got[7].Group != "github-actions-run:1" {
		t.Fatalf("PR-only reuse omission = %+v, want group-bound omission", got[7])
	}
	receipt := got[10].Evidence
	if len(receipt.LogJobIDs) != 3 || len(receipt.DependentJobs) != 1 || receipt.DependentJobs[0] != 14 || len(receipt.DependentSteps) != 10 {
		t.Fatalf("dependency-family receipt = %+v", receipt)
	}
	if len(receipt.Artifacts) != 8 || receipt.Artifacts[0].Digest == "" || receipt.Artifacts[0].ProviderRunID != 1 || receipt.Artifacts[0].HeadSHA != "head-1" {
		t.Fatalf("producer provenance receipt = %+v", receipt.Artifacts)
	}
}

// A final proof upload can initiate the same bounded infrastructure family
// only after every required producer and fan-in succeeded. This case keeps all
// ten attempt-one producer receipts and no dependent recovery obligations.
func TestArtifactInfrastructureFailures_FinalProofUploadAfterExpectedAttemptOneSkipIsOneSafeGroup(t *testing.T) {
	t.Parallel()

	jobs := joinedXAUJobPopulation()
	jobs[13] = githubRunJob{ID: 14, HeadSHA: "head-1", Name: "repository", Status: "completed", Conclusion: "failure", Steps: []githubJobStep{
		{Name: boundedInfrastructureRetryStep, Number: 2, Conclusion: "success"},
		{Name: "Download every shard's manifest and log", Number: 3, Conclusion: "success"},
		{Name: joinedXAURetainedProofStep, Number: 4, Conclusion: "skipped"},
		{Name: "Prove every shard ran and together covered the battery", Number: 5, Conclusion: "success"},
		{Name: "Upload full-lane proof artifact", Number: 6, Conclusion: "failure"},
	}}
	logs := strings.Join([]string{
		"##[group]Run retry guard",
		"##[group]Run actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093",
		"##[group]Run python -B scripts/ci_check_shard_fanin.py",
		"##[group]Run actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		uploadArtifact503Record,
	}, "\n") + "\n"
	got := classifyJoinedXAUAttemptOne(t, jobs, map[int]string{14: logs}, joinedXAUAttemptOneArtifacts("head-1"))

	if !got[13].Retryable || got[13].RerunSafe || got[13].Group != "github-actions-run:1" || !strings.Contains(got[13].Reason, "identity is not reviewed") {
		t.Fatalf("final proof upload disposition = %+v, want one complete but identity-gated group", got[13])
	}
	receipt := got[13].Evidence
	if len(receipt.LogJobIDs) != 1 || receipt.LogJobIDs[0] != 14 || len(receipt.DependentJobs) != 0 || len(receipt.DependentSteps) != 0 || len(receipt.Artifacts) != 10 {
		t.Fatalf("final proof upload receipt = %+v, want one log, ten producers, and no dependents", receipt)
	}
}

func TestXAURetryRuleLanded_RequiresReviewedIdentityAndUnchangedTrustedFiles(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		workflow string
		verifier string
		want     bool
	}{
		"identical but unreviewed rule": {workflow: strings.Repeat("a", 40), verifier: strings.Repeat("b", 40)},
		"workflow changed on candidate": {workflow: strings.Repeat("c", 40), verifier: strings.Repeat("b", 40)},
		"verifier changed on candidate": {workflow: strings.Repeat("a", 40), verifier: strings.Repeat("d", 40)},
	} {
		t.Run(name, func(t *testing.T) {
			baseWorkflow := strings.Repeat("a", 40)
			baseVerifier := strings.Repeat("b", 40)
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/contents/.github/workflows/xau-ci.yml -f ref=base":             {stdout: fmt.Sprintf(`{"sha":%q,"type":"file"}`, baseWorkflow)},
				"gh api --method GET repos/test/repo/contents/.github/workflows/xau-ci.yml -f ref=head":             {stdout: fmt.Sprintf(`{"sha":%q,"type":"file"}`, tc.workflow)},
				"gh api --method GET repos/test/repo/contents/scripts/ci_check_infrastructure_retry.py -f ref=base": {stdout: fmt.Sprintf(`{"sha":%q,"type":"file"}`, baseVerifier)},
				"gh api --method GET repos/test/repo/contents/scripts/ci_check_infrastructure_retry.py -f ref=head": {stdout: fmt.Sprintf(`{"sha":%q,"type":"file"}`, tc.verifier)},
			}), nil, "", "test/repo")
			got, err := host.xauRetryRuleLanded(context.Background(), "base", "head")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("xauRetryRuleLanded() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestXAURetryContractIdentityReviewed_RequiresExactReviewedPair(t *testing.T) {
	t.Parallel()

	reviewed := []xauRetryContractIdentity{{workflowBlobSHA: strings.Repeat("a", 40), verifierBlobSHA: strings.Repeat("b", 40)}}
	if !xauRetryContractIdentityReviewed(reviewed[0], reviewed) {
		t.Fatal("exact reviewed workflow/verifier pair was rejected")
	}
	for name, identity := range map[string]xauRetryContractIdentity{
		"workflow substitution": {workflowBlobSHA: strings.Repeat("c", 40), verifierBlobSHA: strings.Repeat("b", 40)},
		"verifier substitution": {workflowBlobSHA: strings.Repeat("a", 40), verifierBlobSHA: strings.Repeat("d", 40)},
		"missing verifier":      {workflowBlobSHA: strings.Repeat("a", 40)},
	} {
		if xauRetryContractIdentityReviewed(identity, reviewed) {
			t.Fatalf("%s was accepted as a reviewed pair: %+v", name, identity)
		}
	}
	if len(reviewedXAURetryContractIdentities) != 0 {
		t.Fatalf("production identity allowlist = %+v, want explicit non-dispatch until the corrected XAU join is reviewed", reviewedXAURetryContractIdentities)
	}
}

// An artifact action may fail after every repository-owned step passed. The
// classifier must bind that narrow exception to the exact PR/head/base and the
// first workflow attempt, and must use the log only to prove the action identity
// that GitHub's structured jobs response does not expose.
func TestArtifactInfrastructureFailures_AdmitsArtifactOnlyFailureOnExactFirstAttempt(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/1": {
			stdout: `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}` + "\n",
		},
		"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"jobs":[{"id":2,"run_id":1,"head_sha":"head-1","name":"build","status":"completed","conclusion":"failure","steps":[{"name":"Run tests","number":2,"status":"completed","conclusion":"success"},{"name":"Upload the sealed distribution","number":3,"status":"completed","conclusion":"failure"}]}]}]` + "\n",
		},
		"gh api --method GET repos/test/repo/actions/jobs/2/logs": {
			stdout: "2026-09-08T00:00:00Z ##[group]Run go test ./...\n2026-09-08T00:00:01Z ok\n2026-09-08T00:00:02Z ##[group]Run actions/upload-artifact@v4\n2026-09-08T00:00:03Z " + uploadArtifact503Record + "\n",
		},
		"gh api --method GET repos/test/repo/actions/runs/1/artifacts -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"artifacts":[{"id":91,"name":"sealed-evidence-1","expired":false}]}]`,
		},
	}), nil, "", "test/repo")

	got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{
		{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"},
	})
	if err != nil {
		t.Fatalf("ArtifactInfrastructureFailures() error = %v", err)
	}
	if len(got) != 1 || !got[0].Retryable {
		t.Fatalf("ArtifactInfrastructureFailures() = %+v, want one retryable result", got)
	}
	if got[0].Group != "github-actions-run:1" {
		t.Fatalf("group = %q, want github-actions-run:1", got[0].Group)
	}
	if got[0].Evidence.Attempt != 1 || len(got[0].Evidence.LogJobIDs) != 1 || got[0].Evidence.LogJobIDs[0] != 2 || len(got[0].Evidence.Artifacts) != 1 || got[0].Evidence.Artifacts[0].ID != 91 {
		t.Fatalf("attempt-1 retention receipt = %+v", got[0].Evidence)
	}
}

func TestGetPRTarget_ReadsHeadAndBaseInOneRequest(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 42 --repo test/repo --json headRefOid,baseRefName,baseRefOid": {
			stdout: `{"headRefOid":"head-1","baseRefName":"main","baseRefOid":"base-1"}`,
		},
	}), nil, "", "test/repo")
	target, err := host.GetPRTarget(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if target.HeadSHA != "head-1" || target.BaseBranch != "main" || target.BaseSHA != "base-1" {
		t.Fatalf("target = %+v, want exact live head/base tuple", target)
	}
}

// Each failed structured step must own both its action invocation and its own
// qualifying service error. An earlier recovered 503 cannot authorize a later
// unrelated failure, and one 503 cannot authorize two failed transfers.
func TestArtifactInfrastructureFailures_EachFailedStepOwnsItsActionAndError(t *testing.T) {
	t.Parallel()

	for name, fixture := range map[string]struct {
		steps         string
		logs          string
		wantRetryable bool
	}{
		"recovered earlier 503 then unrelated failure": {
			steps: `[{"name":"Upload prior evidence","number":2,"conclusion":"success"},{"name":"Upload final evidence","number":3,"conclusion":"failure"}]`,
			logs:  "##[group]Run actions/upload-artifact@v4\n" + uploadArtifact503Record + "\nretry succeeded\n##[group]Run actions/upload-artifact@v4\n##[error]Failed to FinalizeArtifact: Received non-retryable error: Failed request: (400) Bad Request\n",
		},
		"two failed transfers with one qualifying error": {
			steps: `[{"name":"Upload first","number":2,"conclusion":"failure"},{"name":"Upload second","number":3,"conclusion":"failure"}]`,
			logs:  "##[group]Run actions/upload-artifact@v4\n" + uploadArtifact503Record + "\n##[group]Run actions/upload-artifact@v4\n##[error]Failed to FinalizeArtifact: Received non-retryable error: Failed request: (400) Bad Request\n",
		},
		"successful permitted request followed by another operation 403": {
			steps: `[{"name":"Download evidence","number":2,"conclusion":"failure"}]`,
			logs:  "##[group]Run actions/download-artifact@v4\nListArtifacts completed: HTTP 200\nDownloadArtifact failed: HTTP 403\n",
		},
		"same action recovered then ended with an unclassified failure": {
			steps: `[{"name":"Upload evidence","number":2,"conclusion":"failure"}]`,
			logs:  "##[group]Run actions/upload-artifact@v4\n" + uploadArtifact503Record + "\nretry succeeded\nartifact transfer failed: connection reset\n",
		},
		"same-line success and unrelated failure are not one request tuple": {
			steps: `[{"name":"Download evidence","number":2,"conclusion":"failure"}]`,
			logs:  "##[group]Run actions/download-artifact@v4\n##[error]ListArtifacts completed: HTTP 200; DownloadArtifact failed: HTTP 403\n",
		},
		"verified download 403 remains admitted": {
			steps:         `[{"name":"Download evidence","number":2,"conclusion":"failure"}]`,
			logs:          "##[group]Run actions/download-artifact@v4\n" + downloadArtifact403Record + "\n",
			wantRetryable: true,
		},
		"verified upload exhausted 503 remains admitted": {
			steps:         `[{"name":"Upload evidence","number":2,"conclusion":"failure"}]`,
			logs:          "##[group]Run actions/upload-artifact@v4\n" + uploadArtifact503Record + "\n",
			wantRetryable: true,
		},
		"conflicting clause before a valid payload is refused": {
			steps: `[{"name":"Download evidence","number":2,"conclusion":"failure"}]`,
			logs:  "##[group]Run actions/download-artifact@v4\n##[error]DownloadArtifact failed: HTTP 403; " + strings.TrimPrefix(downloadArtifact503Record, "##[error]") + "\n",
		},
		"conflicting clause after a valid payload is refused": {
			steps: `[{"name":"Download evidence","number":2,"conclusion":"failure"}]`,
			logs:  "##[group]Run actions/download-artifact@v4\n" + downloadArtifact503Record + "; DownloadArtifact failed: HTTP 403\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/actions/runs/1":                                                    {stdout: `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`},
				"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":1,"jobs":[{"id":2,"head_sha":"head-1","name":"build","status":"completed","conclusion":"failure","steps":` + fixture.steps + `}]}]`},
				"gh api --method GET repos/test/repo/actions/jobs/2/logs":                                               {stdout: fixture.logs},
				"gh api --method GET repos/test/repo/actions/runs/1/artifacts -f per_page=100 --paginate --slurp":       {stdout: `[{"total_count":0,"artifacts":[]}]`},
			}), nil, "", "test/repo")
			got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"}})
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Retryable != fixture.wantRetryable {
				t.Fatalf("Retryable = %v, want %v: %+v", got[0].Retryable, fixture.wantRetryable, got[0])
			}
		})
	}
}

// A failed repository step can coexist with an artifact action in the log. It
// must remain a genuine failure; otherwise the infrastructure exception masks
// the exact test/lint failure it is designed never to delay.
func TestArtifactInfrastructureFailures_RejectsRepositoryFailureEvenWhenArtifactActionRan(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/1": {
			stdout: `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}` + "\n",
		},
		"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"jobs":[{"id":2,"run_id":1,"head_sha":"head-1","name":"build","status":"completed","conclusion":"failure","steps":[{"name":"Run tests","number":2,"status":"completed","conclusion":"failure"},{"name":"Upload artifact","number":3,"status":"completed","conclusion":"success"}]}]}]` + "\n",
		},
		"gh api --method GET repos/test/repo/actions/jobs/2/logs": {
			stdout: "##[group]Run go test ./...\nFAIL\n##[group]Run actions/upload-artifact@v4\nFinalizeArtifact failed: HTTP 503\n",
		},
	}), nil, "", "test/repo")

	got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{
		{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"},
	})
	if err != nil {
		t.Fatalf("ArtifactInfrastructureFailures() error = %v", err)
	}
	if len(got) != 1 || got[0].Retryable {
		t.Fatalf("ArtifactInfrastructureFailures() = %+v, want genuine failure rejected", got)
	}
}

// A service-side FinalizeArtifact/ListArtifacts error is retryable only for a
// 403 or 5xx response. The HTTP class is log evidence because the jobs API does
// not carry it; a 400 therefore stays an ordinary failure.
func TestArtifactInfrastructureFailures_RequiresRetryableArtifactServiceHTTPStatus(t *testing.T) {
	t.Parallel()

	for name, logText := range map[string]string{
		"403":                                   "##[group]Run go test ./...\nok\n##[group]Run actions/upload-artifact@v4\n" + uploadArtifact403Record,
		"503":                                   "##[group]Run go test ./...\nok\n##[group]Run actions/download-artifact@v4\n" + downloadArtifact503Record,
		"400 is not infrastructure":             "##[group]Run go test ./...\nok\n##[group]Run actions/upload-artifact@v4\n##[error]Failed to FinalizeArtifact: Received non-retryable error: Failed request: (400) Bad Request",
		"retryable 500 cannot be non-retryable": "##[group]Run go test ./...\nok\n##[group]Run actions/upload-artifact@v4\n##[error]Failed to FinalizeArtifact: Received non-retryable error: Failed request: (500) Internal Server Error",
		"501 cannot exhaust client retries":     "##[group]Run go test ./...\nok\n##[group]Run actions/upload-artifact@v4\n##[error]Failed to FinalizeArtifact: Failed to make request after 5 attempts: Failed request: (501) Not Implemented",
		"timestamp is not a 5xx":                "##[group]Run go test ./...\nok\n##[group]Run actions/upload-artifact@v4\nFinalizeArtifact failed without an HTTP status",
		"operation without action":              "FinalizeArtifact failed: HTTP 503",
		"action without service 5xx":            "##[group]Run go test ./...\nok\n##[group]Run actions/upload-artifact@v4\ninput path did not match any files",
	} {
		t.Run(name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/actions/runs/1":                                                    {stdout: `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`},
				"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":1,"jobs":[{"id":2,"run_id":1,"head_sha":"head-1","name":"build","status":"completed","conclusion":"failure","steps":[{"name":"Run tests","number":2,"conclusion":"success"},{"name":"Upload artifact","number":3,"conclusion":"failure"}]}]}]`},
				"gh api --method GET repos/test/repo/actions/jobs/2/logs":                                               {stdout: logText},
				"gh api --method GET repos/test/repo/actions/runs/1/artifacts -f per_page=100 --paginate --slurp":       {stdout: `[{"total_count":0,"artifacts":[]}]`},
			}), nil, "", "test/repo")
			got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"}})
			if err != nil {
				t.Fatal(err)
			}
			want := name == "403" || name == "503"
			if got[0].Retryable != want {
				t.Fatalf("Retryable = %v, want %v for log %q", got[0].Retryable, want, logText)
			}
		})
	}
}

// A rerun, stale head, or changed base is not the candidate being certified.
// All three must fail closed before any job log can authorize another attempt.
func TestArtifactInfrastructureFailures_RejectsDifferentCandidateAndSecondAttempt(t *testing.T) {
	t.Parallel()

	for name, runJSON := range map[string]string{
		"different head":            `{"id":1,"head_sha":"other","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"other"},"base":{"ref":"main","sha":"base-1"}}]}`,
		"different base branch":     `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"release","sha":"base-1"}}]}`,
		"same base branch advanced": `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-old"}}]}`,
		"second attempt":            `{"id":1,"head_sha":"head-1","run_attempt":2,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/actions/runs/1": {stdout: runJSON},
			}), nil, "", "test/repo")
			got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"}})
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Retryable {
				t.Fatalf("mismatched run was classified retryable: %+v", got[0])
			}
		})
	}
}

// Malformed structured provider output never becomes an infrastructure
// verdict. Returning an error lets the CI monitor record why it failed closed.
func TestArtifactInfrastructureFailures_MalformedJobsFailClosed(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/1":                                                    {stdout: `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`},
		"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {stdout: `{not-json`},
	}), nil, "", "test/repo")
	got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"}})
	if err == nil {
		t.Fatalf("ArtifactInfrastructureFailures() = %+v, want malformed-output error", got)
	}
}

func TestArtifactInfrastructureFailures_RequiresExactCompleteAttemptPopulation(t *testing.T) {
	t.Parallel()

	const run = `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`
	const eligibleJob = `{"id":2,"head_sha":"head-1","name":"build","status":"completed","conclusion":"failure","steps":[{"name":"Upload sealed evidence","number":2,"conclusion":"failure"}]}`
	check := scm.Check{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"}
	for name, fixture := range map[string]struct {
		pages   string
		wantErr bool
	}{
		"missing job id cannot fall back by name": {pages: `[{"total_count":1,"jobs":[` + strings.ReplaceAll(eligibleJob, `"id":2`, `"id":3`) + `]}]`},
		"partial page":                 {pages: `[{"total_count":2,"jobs":[` + eligibleJob + `]}]`, wantErr: true},
		"unrepresented failed sibling": {pages: `[{"total_count":2,"jobs":[` + eligibleJob + `,{"id":3,"head_sha":"head-1","name":"other","status":"completed","conclusion":"failure","steps":[{"name":"Upload other","number":2,"conclusion":"failure"}]}]}]`},
	} {
		t.Run(name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/actions/runs/1":                                                    {stdout: run},
				"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {stdout: fixture.pages},
			}), nil, "", "test/repo")
			got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{check})
			if fixture.wantErr != (err != nil) {
				t.Fatalf("error = %v, wantErr %v", err, fixture.wantErr)
			}
			if len(got) != 1 || got[0].Retryable {
				t.Fatalf("incomplete or inexact population was admitted: %+v", got)
			}
		})
	}
}

func TestArtifactInfrastructureFailures_PaginatesCompleteAttemptPopulation(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/actions/runs/1":                                                    {stdout: `{"id":1,"head_sha":"head-1","run_attempt":1,"pull_requests":[{"number":42,"head":{"sha":"head-1"},"base":{"ref":"main","sha":"base-1"}}]}`},
		"gh api --method GET repos/test/repo/actions/runs/1/attempts/1/jobs -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":2,"jobs":[{"id":1,"head_sha":"head-1","name":"prerequisite","status":"completed","conclusion":"success","steps":[{"name":"Run checks","number":2,"conclusion":"success"}]}]},{"total_count":2,"jobs":[{"id":2,"head_sha":"head-1","name":"build","status":"completed","conclusion":"failure","steps":[{"name":"Upload sealed evidence","number":2,"conclusion":"failure"}]}]}]`},
		"gh api --method GET repos/test/repo/actions/jobs/2/logs":                                               {stdout: "##[group]Run actions/upload-artifact@v4\n" + uploadArtifact503Record + "\n"},
		"gh api --method GET repos/test/repo/actions/runs/1/artifacts -f per_page=100 --paginate --slurp":       {stdout: `[{"total_count":2,"artifacts":[{"id":91,"name":"sealed-evidence-1","expired":false}]},{"total_count":2,"artifacts":[{"id":92,"name":"journey-evidence-1","expired":false}]}]`},
	}), nil, "", "test/repo")
	got, err := host.ArtifactInfrastructureFailures(context.Background(), &scm.PR{Number: "42", HeadSHA: "head-1", BaseBranch: "main", BaseSHA: "base-1"}, []scm.Check{
		{Name: "prerequisite", Bucket: scm.CheckBucketPass, State: "SUCCESS", Link: "https://github.com/test/repo/actions/runs/1/job/1"},
		{Name: "build", Bucket: scm.CheckBucketFail, State: "FAILURE", Link: "https://github.com/test/repo/actions/runs/1/job/2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[1].Retryable || got[1].RerunSafe {
		t.Fatalf("complete population classification = %+v, want proven but provider-disabled failure", got)
	}
	if len(got[1].Evidence.Artifacts) != 2 || got[1].Evidence.Artifacts[0].ID != 91 || got[1].Evidence.Artifacts[1].ID != 92 {
		t.Fatalf("paginated retention receipt = %+v", got[1].Evidence)
	}
}

func TestFindPRFiltersByBaseBranch(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature/refactor --base release/1.0 --state open --json number,url,baseRefName": {
			stdout: `[{"number":42,"url":"https://github.example.com/org/repo/pull/42","baseRefName":"release/1.0"}]` + "\n",
		},
	}), nil, "", "")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "release/1.0")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want %q", pr.Number, "42")
	}
	if pr.URL != "https://github.example.com/org/repo/pull/42" {
		t.Fatalf("FindPR() URL = %q, want matching base PR", pr.URL)
	}
}

func TestFindPRForkUsesBareHeadAndFiltersOwner(t *testing.T) {
	t.Parallel()

	branch := "feature/refactor"
	host := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head fork-owner:" + branch + " --base main --repo parent/repo --state open --json number,url,baseRefName,headRefName,headRepositoryOwner": {
			stderr: `invalid argument: "--head" does not support "<owner>:<branch>"` + "\n",
			code:   1,
		},
		"gh pr list --head " + branch + " --base main --repo parent/repo --state open --json number,url,baseRefName,headRefName,headRepositoryOwner": {
			stdout: `[` +
				`{"number":40,"url":"https://github.com/parent/repo/pull/40","baseRefName":"main","headRefName":"feature/refactor","headRepositoryOwner":{"login":"other-owner"}},` +
				`{"number":42,"url":"https://github.com/parent/repo/pull/42","baseRefName":"main","headRefName":"feature/refactor","headRepositoryOwner":{"login":"fork-owner"}}` +
				`]` + "\n",
		},
	}), nil, "", "parent/repo", "fork-owner/repo", false)

	pr, err := host.FindPR(context.Background(), branch, "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want fork PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want 42", pr.Number)
	}
	if pr.URL != "https://github.com/parent/repo/pull/42" {
		t.Fatalf("FindPR() URL = %q, want fork-owned parent PR", pr.URL)
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature/refactor --base main --state open --json number,url,baseRefName": {
			stderr: "api unavailable\n",
			code:   1,
		},
	}), nil, "", "")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
	if err == nil {
		t.Fatal("FindPR() error = nil, want CLI error")
	}
	if !strings.Contains(err.Error(), "gh pr list") {
		t.Fatalf("FindPR() error = %v, want gh pr list context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

func TestFindPRRejectsURLForDifferentRepository(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr list --head feature/refactor --base main --repo parent/repo --state open --json number,url,baseRefName": {
			stdout: `[{"number":42,"url":"https://github.com/other/repo/pull/42","baseRefName":"main"}]` + "\n",
		},
	}), nil, "github.com", "parent/repo")

	pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
	if err == nil {
		t.Fatal("FindPR() error = nil, want repository mismatch error")
	}
	if !strings.Contains(err.Error(), "parse gh pr list") {
		t.Fatalf("FindPR() error = %v, want parse context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

func TestFindPRReturnsJSONError(t *testing.T) {
	t.Parallel()

	const findPRListCommand = "gh pr list --head feature/refactor --base main --state open --json number,url,baseRefName"
	valid := `{"number":42,"url":"https://github.example.com/org/repo/pull/42","baseRefName":"main"}`
	for _, output := range []string{
		"[{\n",
		"null\n",
		"[{}]\n",
		"[" + valid + ",{}]\n",
		`[{"number":42,"url":"https://github.example.com/org/repo/pull/43","baseRefName":"main"}]` + "\n",
		`[{"number":-1,"url":"https://github.example.com/org/repo/pull/-1","baseRefName":"main"}]` + "\n",
		`[{"number":0,"url":"https://github.example.com/org/repo/pull/42","baseRefName":"main"}]` + "\n",
		`[{"number":42,"url":"42","baseRefName":"main"}]` + "\n",
		`[{"number":42,"url":"https://github.example.com/org/repo/pull/42?view=files","baseRefName":"main"}]` + "\n",
		`[{"number":42,"url":"https://github.example.com/org/repo/pull/42#discussion","baseRefName":"main"}]` + "\n",
		`[{"number":42,"url":"https://github.example.com/org/repo/pull/%34%32","baseRefName":"main"}]` + "\n",
	} {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			findPRListCommand: {
				stdout: output,
			},
		}), nil, "", "")

		pr, err := host.FindPR(context.Background(), "feature/refactor", "main")
		if err == nil {
			t.Fatal("FindPR() error = nil, want JSON error")
		}
		if !strings.Contains(err.Error(), "parse gh pr list") {
			t.Fatalf("FindPR() error = %v, want parse context", err)
		}
		if pr != nil {
			t.Fatalf("FindPR() PR = %+v, want nil", pr)
		}
	}
}

func TestFindPRForkRejectsMissingHeadIdentity(t *testing.T) {
	t.Parallel()

	branch := "feature/refactor"
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "missing head ref",
			output: `[{"number":42,"url":"https://github.com/parent/repo/pull/42","headRepositoryOwner":{"login":"fork-owner"}}]`,
		},
		{
			name:   "missing head owner",
			output: `[{"number":42,"url":"https://github.com/parent/repo/pull/42","headRefName":"feature/refactor","headRepositoryOwner":null}]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{
				"gh pr list --head " + branch + " --base main --repo parent/repo --state open --json number,url,baseRefName,headRefName,headRepositoryOwner": {
					stdout: tc.output + "\n",
				},
			}), nil, "", "parent/repo", "fork-owner/repo", false)

			pr, err := host.FindPR(context.Background(), branch, "main")
			if err == nil {
				t.Fatal("FindPR() error = nil, want head identity error")
			}
			if !strings.Contains(err.Error(), "parse gh pr list") {
				t.Fatalf("FindPR() error = %v, want parse context", err)
			}
			if pr != nil {
				t.Fatalf("FindPR() PR = %+v, want nil", pr)
			}
		})
	}
}

func TestAvailableScopesAuthToConfiguredHost(t *testing.T) {
	t.Parallel()

	// With a known host, the auth check must be scoped via --hostname so a
	// stale credential on some other configured gh host (e.g. github.com vs
	// a GHE instance) cannot make this repo look unauthenticated. The
	// unscoped form is treated as a failure here to prove the scoped form
	// is the one actually invoked.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status --hostname ghe.example.com": {},
		"gh auth status": {stderr: "github.com: token invalid\n", code: 1},
	}), func() bool { return true }, "ghe.example.com", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil (scoped auth should pass)", err)
	}
}

func TestAvailableFallsBackToUnscopedAuthWhenHostUnknown(t *testing.T) {
	t.Parallel()

	// No host -> behave as before: a bare `gh auth status`.
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status": {},
	}), func() bool { return true }, "", "")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

func TestAvailableReportsDeadlineExceededInsteadOfAuthFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status": {},
	}), func() bool { return true }, "", "")

	err := host.Available(ctx)
	if err == nil {
		t.Fatal("Available() error = nil, want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Available() error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Available() error = %v, want timed out message", err)
	}
	if strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("Available() error = %v, must not report auth failure on timeout", err)
	}
}

func TestAvailableReportsCancellationInsteadOfAuthFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status": {},
	}), func() bool { return true }, "", "")

	err := host.Available(ctx)
	if err == nil {
		t.Fatal("Available() error = nil, want timeout error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Available() error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Available() error = %v, want interrupted message", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Available() error = %v, must not report timeout on cancellation", err)
	}
	if strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("Available() error = %v, must not report auth failure on cancellation", err)
	}
}

func TestAvailableReportsMissingBinaryInsteadOfAuthFailure(t *testing.T) {
	t.Parallel()

	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "no-mistakes-missing-gh-binary")
	}, func() bool { return true }, "", "")

	err := host.Available(context.Background())
	if err == nil {
		t.Fatal("Available() error = nil, want missing-binary error")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Available() error = %v, want exec.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("Available() error = %v, want not on PATH message", err)
	}
	if strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("Available() error = %v, must not report auth failure when gh is missing", err)
	}
}

func TestAvailableReportsCommandFactoryMissingBinaryInsteadOfAuthFailure(t *testing.T) {
	t.Parallel()

	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GITHUB_TEST_HELPER=1")
		cmd.Err = &exec.Error{Name: name, Err: fs.ErrNotExist}
		return cmd
	}, func() bool { return true }, "", "")

	err := host.Available(context.Background())
	if err == nil {
		t.Fatal("Available() error = nil, want missing-binary error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Available() error = %v, want fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("Available() error = %v, want not on PATH message", err)
	}
	if strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("Available() error = %v, must not report auth failure when command factory marks gh missing", err)
	}
}

func TestAvailableWrapsAuthFailureWithStderr(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status": {stderr: "github.com\n  X Failed to log in\n", code: 1},
	}), func() bool { return true }, "", "")

	err := host.Available(context.Background())
	if err == nil {
		t.Fatal("Available() error = nil, want auth failure")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("Available() error = %v, want not authenticated", err)
	}
	if !strings.Contains(err.Error(), "Failed to log in") {
		t.Fatalf("Available() error = %v, want stderr detail", err)
	}
}

type githubTestResponse struct {
	stdout    string
	stderr    string
	wantStdin string
	code      int
}

func githubTestCmdFactory(responses map[string]githubTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok && strings.HasPrefix(key, "gh api ") && strings.Contains(key, " graphql ") {
			for candidate, prResponse := range responses {
				if strings.Contains(candidate, "gh pr checks ") {
					response = normalizedChecksResponse(prResponse)
					ok = true
					break
				}
			}
		}
		if !ok {
			response = githubTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"GITHUB_TEST_HELPER=1",
			"GITHUB_TEST_STDOUT="+response.stdout,
			"GITHUB_TEST_STDERR="+response.stderr,
			"GITHUB_TEST_WANT_STDIN="+response.wantStdin,
			fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func githubCommitChecksCommand(host, repo, headSHA string) string {
	parts := strings.Split(repo, "/")
	args := []string{"gh", "api"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	args = append(args, "graphql", "-f", "query="+commitChecksQuery,
		"-F", "owner="+parts[0], "-F", "name="+parts[1], "-F", "oid="+headSHA)
	return strings.Join(args, " ")
}

func githubCommitChecksResponse(nodes string) string {
	response := map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"object": map[string]any{
					"statusCheckRollup": map[string]any{
						"contexts": map[string]any{
							"nodes":    json.RawMessage(nodes),
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nodes
	}
	return string(encoded) + "\n"
}

func normalizedChecksResponse(response githubTestResponse) githubTestResponse {
	if response.code != 0 && strings.Contains(response.stderr, "no checks reported") {
		return githubTestResponse{stdout: githubCommitChecksResponse("[]")}
	}
	if response.code != 0 {
		return response
	}
	var raw []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
		Link        string `json:"link"`
	}
	if err := json.Unmarshal([]byte(response.stdout), &raw); err != nil {
		return githubTestResponse{stdout: response.stdout}
	}
	nodes := make([]map[string]string, 0, len(raw))
	for _, check := range raw {
		status := "COMPLETED"
		conclusion := check.State
		if check.Bucket == "pending" {
			status = "IN_PROGRESS"
			conclusion = ""
		}
		nodes = append(nodes, map[string]string{
			"__typename": "CheckRun", "name": check.Name, "status": status,
			"conclusion": conclusion, "completedAt": check.CompletedAt, "detailsUrl": check.Link,
		})
	}
	encoded, err := json.Marshal(nodes)
	if err != nil {
		return githubTestResponse{stdout: response.stdout}
	}
	return githubTestResponse{stdout: githubCommitChecksResponse(string(encoded))}
}

func TestGitHubHelperProcess(t *testing.T) {
	if os.Getenv("GITHUB_TEST_HELPER") != "1" {
		return
	}

	if want := os.Getenv("GITHUB_TEST_WANT_STDIN"); want != "" {
		got, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read stdin: %v", err)
			os.Exit(1)
		}
		if string(got) != want {
			fmt.Fprintf(os.Stderr, "stdin = %q, want %q", string(got), want)
			os.Exit(1)
		}
	}
	if _, err := fmt.Fprint(os.Stdout, os.Getenv("GITHUB_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("GITHUB_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("GITHUB_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestHost_GetReviewComments(t *testing.T) {
	t.Parallel()

	firstPage := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
		{"isResolved":true,"comments":{"nodes":[{"databaseId":1,"body":"resolved","path":"pkg/resolved.go","line":4,"url":"https://ghe.example.com/org/repo/pull/7#discussion_r1","createdAt":"2026-08-27T12:00:00Z","author":{"login":"greptile-apps[bot]"}}]}},
		{"isResolved":false,"comments":{"nodes":[{"databaseId":2,"body":"human","path":"pkg/human.go","line":8,"url":"https://ghe.example.com/org/repo/pull/7#discussion_r2","createdAt":"2026-08-27T12:01:00Z","author":{"login":"reviewer"}}]}},
		{"isResolved":false,"comments":{"nodes":[{"databaseId":3,"body":"other bot","path":"pkg/other.go","line":9,"url":"https://ghe.example.com/org/repo/pull/7#discussion_r3","createdAt":"2026-08-27T12:02:00Z","author":{"login":"dependabot[bot]"}}]}},
		{"isResolved":false,"comments":{"nodes":[{"databaseId":12345,"body":"Fix this null pointer","path":"pkg/foo.go","line":42,"url":"https://ghe.example.com/org/repo/pull/7#discussion_r12345","createdAt":"2026-08-27T12:03:00Z","author":{"login":"greptile-apps[bot]"}}]}}
	],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`
	secondPage := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
		{"isResolved":false,"comments":{"nodes":[{"databaseId":12346,"body":"Second page","path":"pkg/bar.go","line":null,"url":"https://ghe.example.com/org/repo/pull/7#discussion_r12346","createdAt":"2026-08-27T12:04:00Z","author":{"login":"greptile-apps"}}]}}
	],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`
	command := func(cursor string) string {
		args := []string{"gh", "api", "--hostname", "ghe.example.com", "graphql", "-f", "query=" + reviewThreadsQuery,
			"-F", "owner=org", "-F", "name=repo", "-F", "number=7"}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		return strings.Join(args, " ")
	}

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		command(""):         {stdout: firstPage},
		command("cursor-1"): {stdout: secondPage},
	}), nil, "ghe.example.com", "ghe.example.com/org/repo")

	comments, err := host.GetReviewComments(context.Background(), &scm.PR{URL: "https://ghe.example.com/org/repo/pull/7"})
	if err != nil {
		t.Fatalf("GetReviewComments failed: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	c := comments[0]
	if c.ID != "12345" || c.Author != "greptile-apps[bot]" || c.Path != "pkg/foo.go" || c.Line != 42 || c.Body != "Fix this null pointer" {
		t.Fatalf("unexpected comment parsed: %#v", c)
	}
	if comments[1].ID != "12346" || comments[1].Line != 0 || comments[1].Author != "greptile-apps" {
		t.Fatalf("unexpected paginated comment: %#v", comments[1])
	}
}
