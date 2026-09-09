// Package github implements scm.Host backed by the gh CLI.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to GitHub through the gh CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	host         string // repo's GitHub hostname; scopes the auth check
	repo         string // "owner/name" slug for --repo; empty when unknown
	forkOwner    string // fork owner for cross-repository PR heads
	draft        bool   // open created PRs as drafts (gh pr create --draft)
	// assetHTTP and assetUploadPrefix override the unofficial user-attachments
	// upload transport in tests. Production leaves both nil/empty and uses
	// http.DefaultClient against uploads.github.com (or uploads.<ghec-host>).
	assetHTTP         *http.Client
	assetUploadPrefix string
}

// New builds a Host. cliAvailable reports whether the gh binary is
// resolvable on the caller's PATH (possibly overridden by env). host is the
// repo's GitHub hostname; when set the availability check is scoped to it via
// --hostname so a stale credential for an unrelated configured gh host cannot
// make this repo look unauthenticated. repo is the "owner/name" slug; when set
// it is passed via --repo to every PR/run command so they resolve the right
// repository regardless of the process working directory. The daemon runs from
// a fixed, non-repo working dir, so without this gh cannot infer the repo (or
// branch) and fails on every poll. host is optional; empty reproduces the
// legacy unscoped auth-check behavior.
func New(cmd CmdFactory, cliAvailable func() bool, host, repo string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		host:         strings.TrimSpace(host),
		repo:         strings.TrimSpace(repo),
	}
}

// NewWithFork builds a Host that opens PRs on repo using forkRepo as the head
// repository owner. forkRepo is an "owner/name" slug; only the owner is needed
// because gh pr create expects --head <owner>:<branch>. host is optional; see
// New for its role in scoping the auth check. draft opens created PRs as drafts.
func NewWithFork(cmd CmdFactory, cliAvailable func() bool, host, repo, forkRepo string, draft bool) *Host {
	h := New(cmd, cliAvailable, host, repo)
	h.forkOwner = repoOwner(forkRepo)
	h.draft = draft
	return h
}

// RepoSlug extracts the "owner/name" identifier from a GitHub remote or PR
// URL. Longer paths such as PR links are reduced to their leading two segments.
func RepoSlug(remoteURL string) string {
	parts := strings.Split(scm.RepoPath(remoteURL), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// HostPrefixedSlug returns "host/owner/name" for GitHub Enterprise Server
// instances and plain "owner/name" for github.com. This is the format that
// the gh CLI's --repo flag requires for GHE.
func HostPrefixedSlug(remoteURL string) string {
	return HostPrefixedSlugForHost(remoteURL, scm.ExtractHost(remoteURL))
}

// HostPrefixedSlugForHost is HostPrefixedSlug using an already-resolved host.
// This lets callers honor SSH HostName aliases without rewriting the remote.
func HostPrefixedSlugForHost(remoteURL, host string) string {
	slug := RepoSlug(remoteURL)
	if slug == "" {
		return ""
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || strings.EqualFold(host, "github.com") {
		return slug
	}
	return host + "/" + slug
}

// repoArgs returns the --repo flag pair when the slug is known, so gh commands
// resolve the right repository regardless of the process working directory.
func (h *Host) repoArgs() []string {
	if h.repo == "" {
		return nil
	}
	return []string{"--repo", h.repo}
}

// prSelector returns the explicit gh PR selector for pr, preferring the numeric
// PR number and falling back to the canonical PR URL; both name the exact pull
// request to gh regardless of the process working directory. It fails closed
// when neither is known: an empty positional makes `gh pr <verb>` fall back to
// resolving the current branch of the cwd, and the daemon runs from a detached
// bare gate repo whose HEAD is the default branch (main), so an inferred
// selector silently targets the wrong PR (or none — "no pull requests found for
// branch main") instead of the feature PR the pipeline already knows.
func prSelector(pr *scm.PR) (string, error) {
	if pr != nil {
		if n := strings.TrimSpace(pr.Number); n != "" {
			return n, nil
		}
		if u := strings.TrimSpace(pr.URL); u != "" {
			return u, nil
		}
	}
	return "", errors.New("no PR number or URL known; refusing to run gh with a cwd-inferred branch")
}

func (h *Host) headRef(branch string) string {
	if h.forkOwner == "" {
		return branch
	}
	return h.forkOwner + ":" + branch
}

func repoOwner(slug string) string {
	owner, _, ok := strings.Cut(strings.TrimSpace(slug), "/")
	if !ok {
		return ""
	}
	return strings.TrimSpace(owner)
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitHub }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true, ReviewComments: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("gh CLI is not installed")
	}
	// Scope the auth check to this repo's host. Unscoped `gh auth status`
	// checks every authenticated account and exits non-zero if ANY of them has
	// a stale/expired token, even when this repo's own host is fully
	// authenticated. Passing --hostname keeps an unrelated bad credential from
	// poisoning availability for this repo. When the host is unknown we fall
	// back to the unscoped check (fail-safe: same behavior as before).
	authArgs := []string{"auth", "status"}
	if h.host != "" {
		authArgs = append(authArgs, "--hostname", h.host)
	}
	cmd := h.cmd(ctx, "gh", authArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Keep timeout / missing-binary failures distinct from auth failure so a
		// cancelled reconcile context is not reported as "log in again".
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("gh auth status timed out: %w", ctx.Err())
		}
		if ctx.Err() != nil {
			return fmt.Errorf("gh auth status interrupted: %w", ctx.Err())
		}
		if isMissingExecutable(err) {
			return fmt.Errorf("gh CLI is not on PATH: %w", err)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("gh CLI is not authenticated: %s: %w", detail, err)
		}
		return fmt.Errorf("gh CLI is not authenticated: %w", err)
	}
	return nil
}

func isMissingExecutable(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errors.Is(execErr.Err, exec.ErrNotFound) || errors.Is(execErr.Err, fs.ErrNotExist)
	}
	return false
}

func parsePullRequestURL(raw, expectedHost, expectedRepo string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return 0, errors.New("expected absolute GitHub pull request URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return 0, errors.New("expected HTTP GitHub pull request URL")
	}
	if expectedHost != "" && !strings.EqualFold(parsed.Hostname(), expectedHost) {
		return 0, fmt.Errorf("URL host %q does not match GitHub host %q", parsed.Hostname(), expectedHost)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 4 || segments[2] != "pull" {
		return 0, errors.New("expected GitHub /owner/repo/pull/number URL")
	}
	for _, segment := range segments[:2] {
		if segment == "" || segment == "." || segment == ".." {
			return 0, errors.New("expected unambiguous GitHub owner/repository path")
		}
	}
	actualRepo := segments[0] + "/" + segments[1]
	expectedRepo = strings.Trim(strings.TrimSpace(expectedRepo), "/")
	if expectedRepo != "" && !strings.EqualFold(actualRepo, expectedRepo) {
		return 0, fmt.Errorf("URL repository %q does not match GitHub repository %q", actualRepo, expectedRepo)
	}
	number, err := strconv.Atoi(segments[3])
	if err != nil || number <= 0 {
		return 0, errors.New("expected positive GitHub pull request number")
	}
	escapedSegments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(escapedSegments) != len(segments) || escapedSegments[len(escapedSegments)-1] != strconv.Itoa(number) {
		return 0, errors.New("expected canonical GitHub pull request number path")
	}
	if parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(trimmed, "#") {
		return 0, errors.New("expected GitHub pull request URL without query or fragment")
	}
	return number, nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"pr", "list", "--head", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--base", base)
	}
	args = append(args, h.repoArgs()...)
	jsonFields := "number,url,baseRefName"
	if h.forkOwner != "" {
		jsonFields = "number,url,baseRefName,headRefName,headRepositoryOwner"
	}
	args = append(args, "--state", "open", "--json", jsonFields)
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var prs []struct {
		Number              int    `json:"number"`
		URL                 string `json:"url"`
		BaseRefName         string `json:"baseRefName"`
		HeadRefName         string `json:"headRefName"`
		HeadRepositoryOwner *struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list JSON: %w", err)
	}
	if prs == nil {
		return nil, errors.New("parse gh pr list JSON: expected array")
	}
	if len(prs) == 0 {
		return nil, nil
	}
	prNumbers := make([]string, len(prs))
	for i, candidate := range prs {
		if candidate.Number <= 0 {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing positive PR number", i)
		}
		url := strings.TrimSpace(candidate.URL)
		if url == "" {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing PR URL", i)
		}
		number, err := parsePullRequestURL(url, h.host, h.repoSlug())
		if err != nil {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d invalid PR URL: %w", i, err)
		}
		if candidate.Number != number {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d PR number %d does not match URL number %d", i, candidate.Number, number)
		}
		prNumbers[i] = strconv.Itoa(candidate.Number)
		if h.forkOwner != "" {
			if strings.TrimSpace(candidate.HeadRefName) == "" {
				return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing headRefName", i)
			}
			if candidate.HeadRepositoryOwner == nil || strings.TrimSpace(candidate.HeadRepositoryOwner.Login) == "" {
				return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing headRepositoryOwner login", i)
			}
		}
	}
	for i, candidate := range prs {
		if !h.matchesHead(candidate.HeadRefName, candidate.HeadRepositoryOwner, branch) {
			continue
		}
		pr := &scm.PR{
			Number:     prNumbers[i],
			URL:        strings.TrimSpace(candidate.URL),
			BaseBranch: strings.TrimSpace(candidate.BaseRefName),
		}
		return pr, nil
	}
	return nil, nil
}

func (h *Host) matchesHead(headRefName string, owner *struct {
	Login string `json:"login"`
}, branch string) bool {
	if h.forkOwner == "" {
		return true
	}
	if strings.TrimSpace(headRefName) != "" && headRefName != branch {
		return false
	}
	if owner == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(owner.Login), h.forkOwner)
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := append([]string{"pr", "create",
		"--head", h.headRef(branch),
		"--base", base,
	}, h.repoArgs()...)
	if h.draft {
		args = append(args, "--draft")
	}
	args = append(args, "--title", content.Title, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	url := strings.TrimSpace(string(out))
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	args := append([]string{"pr", "edit", selector}, h.repoArgs()...)
	if strings.TrimSpace(content.Title) != "" {
		args = append(args, "--title", content.Title)
	}
	args = append(args, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gh pr edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

var _ scm.PRContentReader = (*Host)(nil)

func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return scm.PRContent{}, err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "title,body")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return scm.PRContent{}, fmt.Errorf("gh pr view: %w", err)
	}
	var parsed struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return scm.PRContent{}, fmt.Errorf("parse gh pr view: %w", err)
	}
	return scm.PRContent{Title: parsed.Title, Body: parsed.Body}, nil
}

func (h *Host) SetPRBaseBranch(ctx context.Context, pr *scm.PR, baseBranch string) error {
	selector, err := prSelector(pr)
	if err != nil {
		return err
	}
	args := append([]string{"pr", "edit", selector}, h.repoArgs()...)
	args = append(args, "--base", baseBranch)
	cmd := h.cmd(ctx, "gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr edit --base: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "state", "--jq", ".state")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return normalizePRState(strings.TrimSpace(string(out))), nil
}

func (h *Host) GetPRBaseBranch(ctx context.Context, pr *scm.PR) (string, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "baseRefName", "--jq", ".baseRefName")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view base branch: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *Host) GetPRTarget(ctx context.Context, pr *scm.PR) (scm.PRTarget, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return scm.PRTarget{}, err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "headRefOid,baseRefName,baseRefOid")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return scm.PRTarget{}, fmt.Errorf("gh pr view target: %w", err)
	}
	var payload struct {
		HeadSHA    string `json:"headRefOid"`
		BaseBranch string `json:"baseRefName"`
		BaseSHA    string `json:"baseRefOid"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return scm.PRTarget{}, fmt.Errorf("parse gh pr target: %w", err)
	}
	target := scm.PRTarget{
		HeadSHA:    strings.TrimSpace(payload.HeadSHA),
		BaseBranch: strings.TrimSpace(payload.BaseBranch),
		BaseSHA:    strings.TrimSpace(payload.BaseSHA),
	}
	if target.HeadSHA == "" || target.BaseBranch == "" || target.BaseSHA == "" {
		return scm.PRTarget{}, fmt.Errorf("gh pr target is incomplete")
	}
	return target, nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	headSHA := ""
	if strings.TrimSpace(pr.HeadSHA) != "" {
		headSHA, err = h.getPRHeadSHA(ctx, selector)
		if err != nil {
			return nil, err
		}
		pr.HeadSHA = headSHA
	}
	var checks []scm.Check
	if headSHA != "" {
		checks, err = h.getCommitChecks(ctx, headSHA)
	} else {
		checks, err = h.getPRChecks(ctx, selector)
	}
	if err != nil {
		return nil, err
	}
	if headSHA != "" {
		runs, err := h.getWorkflowRunChecks(ctx, headSHA)
		if err != nil {
			return nil, err
		}
		checks = h.appendUnrepresentedWorkflowRuns(checks, runs)
		checks = h.collapseLatestByName(checks)
		currentHeadSHA, err := h.getPRHeadSHA(ctx, selector)
		if err != nil {
			return nil, err
		}
		if currentHeadSHA != headSHA {
			return nil, fmt.Errorf("PR head changed during check discovery from %s to %s", headSHA, currentHeadSHA)
		}
	}
	return checks, nil
}

func (h *Host) getPRChecks(ctx context.Context, selector string) ([]scm.Check, error) {
	args := append([]string{"pr", "checks", selector}, h.repoArgs()...)
	args = append(args, "--json", "name,state,bucket,completedAt,link")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no checks reported") {
			out = []byte("[]")
		} else {
			return nil, fmt.Errorf("gh pr checks: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	var raw []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
		Link        string `json:"link"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, r := range raw {
		var completedAt time.Time
		if r.CompletedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, r.CompletedAt); parseErr == nil {
				completedAt = parsed
			}
		}
		checks = append(checks, scm.Check{
			Name:        r.Name,
			Bucket:      normalizeCheckBucket(r.Bucket, r.State),
			State:       strings.ToUpper(strings.TrimSpace(r.State)),
			CompletedAt: completedAt,
			Link:        strings.TrimSpace(r.Link),
		})
	}
	return checks, nil
}

const commitChecksQuery = `query($owner:String!,$name:String!,$oid:String!,$cursor:String){repository(owner:$owner,name:$name){object(expression:$oid){... on Commit{statusCheckRollup{contexts(first:100,after:$cursor){nodes{__typename ... on CheckRun{name status conclusion completedAt startedAt detailsUrl} ... on StatusContext{context state targetUrl}} pageInfo{hasNextPage endCursor}}}}}}}`

const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){nodes{isResolved comments(first:100){nodes{databaseId body path line url createdAt author{login}}}} pageInfo{hasNextPage endCursor}}}}}`

func (h *Host) getCommitChecks(ctx context.Context, headSHA string) ([]scm.Check, error) {
	repo := h.repoSlug()
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("resolve GitHub repository for commit checks: invalid repository %q", repo)
	}
	var checks []scm.Check
	cursor := ""
	for {
		args := []string{"api"}
		if h.host != "" {
			args = append(args, "--hostname", h.host)
		}
		args = append(args, "graphql", "-f", "query="+commitChecksQuery,
			"-F", "owner="+parts[0], "-F", "name="+parts[1], "-F", "oid="+strings.TrimSpace(headSHA))
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("gh api checks for head commit: %s: %w", strings.TrimSpace(string(out)), err)
		}
		var response struct {
			Data struct {
				Repository *struct {
					Object *struct {
						Rollup *struct {
							Contexts struct {
								Nodes []struct {
									Type        string `json:"__typename"`
									Name        string `json:"name"`
									Status      string `json:"status"`
									Conclusion  string `json:"conclusion"`
									CompletedAt string `json:"completedAt"`
									StartedAt   string `json:"startedAt"`
									DetailsURL  string `json:"detailsUrl"`
									Context     string `json:"context"`
									State       string `json:"state"`
									TargetURL   string `json:"targetUrl"`
								} `json:"nodes"`
								PageInfo struct {
									HasNextPage bool   `json:"hasNextPage"`
									EndCursor   string `json:"endCursor"`
								} `json:"pageInfo"`
							} `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"object"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("parse checks for head commit: %w", err)
		}
		if response.Data.Repository == nil || response.Data.Repository.Object == nil {
			return nil, errors.New("head commit check discovery returned no commit")
		}
		if response.Data.Repository.Object.Rollup == nil {
			return checks, nil
		}
		contexts := response.Data.Repository.Object.Rollup.Contexts
		for _, node := range contexts.Nodes {
			check := scm.Check{}
			switch node.Type {
			case "CheckRun":
				check.Kind = scm.CheckKindRun
				check.Name = strings.TrimSpace(node.Name)
				check.State = strings.ToUpper(strings.TrimSpace(node.Conclusion))
				if check.State == "" {
					check.State = strings.ToUpper(strings.TrimSpace(node.Status))
				}
				check.Bucket = normalizeCheckBucket("", node.Conclusion)
				if check.Bucket == "" {
					check.Bucket = normalizeCheckBucket("", node.Status)
				}
				check.Link = strings.TrimSpace(node.DetailsURL)
				if parsed, parseErr := time.Parse(time.RFC3339, node.CompletedAt); parseErr == nil {
					check.CompletedAt = parsed
				}
				if parsed, parseErr := time.Parse(time.RFC3339, node.StartedAt); parseErr == nil {
					check.StartedAt = parsed
				}
			case "StatusContext":
				check.Kind = scm.CheckKindStatus
				check.Name = strings.TrimSpace(node.Context)
				check.State = strings.ToUpper(strings.TrimSpace(node.State))
				check.Bucket = normalizeCheckBucket("", node.State)
				check.Link = strings.TrimSpace(node.TargetURL)
			default:
				return nil, fmt.Errorf("head commit check discovery returned unsupported context type %q", node.Type)
			}
			if check.Name == "" || check.Bucket == "" {
				return nil, errors.New("head commit check discovery returned an incomplete context")
			}
			checks = append(checks, check)
		}
		if !contexts.PageInfo.HasNextPage {
			return checks, nil
		}
		if contexts.PageInfo.EndCursor == "" || contexts.PageInfo.EndCursor == cursor {
			return nil, errors.New("head commit check discovery returned an invalid page cursor")
		}
		cursor = contexts.PageInfo.EndCursor
	}
}

func (h *Host) repoSlug() string {
	repo := strings.TrimSpace(h.repo)
	if prefix := strings.TrimSpace(h.host) + "/"; h.host != "" && len(repo) > len(prefix) && strings.EqualFold(repo[:len(prefix)], prefix) {
		repo = repo[len(prefix):]
	}
	return repo
}

func (h *Host) appendUnrepresentedWorkflowRuns(checks, runs []scm.Check) []scm.Check {
	represented := make(map[string][]int, len(checks))
	for i, check := range checks {
		if runID := h.actionsRunID(check.Link); runID != "" {
			represented[runID] = append(represented[runID], i)
		}
	}
	for _, run := range runs {
		runID := h.actionsRunID(run.Link)
		if indices := represented[runID]; runID != "" && len(indices) > 0 {
			for _, i := range indices {
				if checks[i].StartedAt.IsZero() && !run.StartedAt.IsZero() {
					checks[i].StartedAt = run.StartedAt
				}
				checks[i].WorkflowID = run.WorkflowID
			}
			continue
		}
		checks = append(checks, run)
		if runID != "" {
			represented[runID] = []int{len(checks) - 1}
		}
	}
	return checks
}

// collapseLatestByName collapses orderable same-name reruns of one workflow
// to the most recently started one. Independent workflows and records whose
// provider metadata cannot establish an order remain visible. GitHub's raw
// commit statusCheckRollup returns every check run ever attached to the commit,
// including runs a later same-named run has
// already superseded - e.g. a CI monitor's auto-fix push re-triggers the
// same gate check, and the rollup keeps both the old FAILURE and the new
// SUCCESS forever. Without this collapse the superseded failure stays
// visible even after the later run at the same head turns green, which
// manufactures an unrecoverable auto-fix loop (see AGENTS.md "CI Monitor
// Lifecycle"). This restores the semantics `gh pr checks` already applies
// (collapse by startedAt) to the commit-rollup path, which never had it.
//
// Must run AFTER appendUnrepresentedWorkflowRuns, never before: that call
// dedupes by Actions run ID against the FULL uncollapsed rollup. Collapsing
// first would drop a superseded run's ID out of the "represented" set the
// union checks against, letting the union re-add the same stale run under
// its own workflow run name - resurrecting exactly the failure this is
// meant to hide.
func (h *Host) collapseLatestByName(checks []scm.Check) []scm.Check {
	collapsed := make([]scm.Check, 0, len(checks))
	for _, check := range checks {
		keep := true
		for i := 0; i < len(collapsed); {
			other := collapsed[i]
			if !h.sameCheckReplacementGroup(check, other) {
				i++
				continue
			}
			after, ordered := h.checkStartedAfter(check, other)
			if !ordered {
				i++
				continue
			}
			if !after {
				keep = false
				break
			}
			collapsed = append(collapsed[:i], collapsed[i+1:]...)
		}
		if keep {
			collapsed = append(collapsed, check)
		}
	}
	return collapsed
}

func (h *Host) sameCheckReplacementGroup(a, b scm.Check) bool {
	if a.Kind != scm.CheckKindRun || b.Kind != scm.CheckKindRun || a.Name != b.Name {
		return false
	}
	// Only distinct runs of the same known workflow establish rerun identity.
	// Missing workflow/run identities may be independent external checks, while
	// equal run identities may be independent same-name jobs within one run.
	// Collapsing either case could hide a failing requirement.
	aRunID := h.actionsRunID(a.Link)
	bRunID := h.actionsRunID(b.Link)
	return a.WorkflowID != 0 && a.WorkflowID == b.WorkflowID &&
		aRunID != "" && bRunID != "" && aRunID != bRunID
}

// checkStartedAfter reports whether a is newer and whether the available
// provider metadata establishes an order between the checks.
func (h *Host) checkStartedAfter(a, b scm.Check) (bool, bool) {
	if !a.StartedAt.IsZero() && !b.StartedAt.IsZero() && !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt), true
	}
	if aID, aErr := strconv.ParseUint(h.actionsRunID(a.Link), 10, 64); aErr == nil {
		if bID, bErr := strconv.ParseUint(h.actionsRunID(b.Link), 10, 64); bErr == nil && aID != bID {
			return aID > bID, true
		}
	}
	if a.StartedAt.IsZero() != b.StartedAt.IsZero() {
		return false, false
	}
	if !a.CompletedAt.IsZero() && !b.CompletedAt.IsZero() && !a.CompletedAt.Equal(b.CompletedAt) {
		return a.CompletedAt.After(b.CompletedAt), true
	}
	return false, false
}

func (h *Host) getPRHeadSHA(ctx context.Context, selector string) (string, error) {
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "headRefOid", "--jq", ".headRefOid")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view head commit: %w", err)
	}
	headSHA := strings.TrimSpace(string(out))
	if headSHA == "" {
		return "", errors.New("gh pr view returned an empty head commit")
	}
	return headSHA, nil
}

func (h *Host) getWorkflowRunChecks(ctx context.Context, headSHA string) ([]scm.Check, error) {
	repo := h.repoSlug()
	endpoint := "repos/{owner}/{repo}/actions/runs"
	if repo != "" {
		endpoint = "repos/" + repo + "/actions/runs"
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint,
		"-f", "head_sha="+strings.TrimSpace(headSHA),
		"-f", "per_page=100",
		"--paginate", "--slurp",
	)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api workflow runs for head commit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	type workflowRun struct {
		ID           int64  `json:"id"`
		WorkflowID   int64  `json:"workflow_id"`
		Name         string `json:"name"`
		DisplayName  string `json:"display_title"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		RunStartedAt string `json:"run_started_at"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
		HTMLURL      string `json:"html_url"`
	}
	var pages []struct {
		TotalCount   *int          `json:"total_count"`
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse workflow runs for head commit: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("workflow run discovery returned no pages")
	}
	var raw []workflowRun
	totalCount := -1
	runIDs := make(map[int64]struct{})
	for pageIndex, page := range pages {
		if page.TotalCount == nil || *page.TotalCount < 0 {
			return nil, fmt.Errorf("workflow run page %d has no valid total_count", pageIndex+1)
		}
		if totalCount == -1 {
			totalCount = *page.TotalCount
		} else if *page.TotalCount != totalCount {
			return nil, fmt.Errorf("workflow run page %d total_count is %d, want %d", pageIndex+1, *page.TotalCount, totalCount)
		}
		for _, run := range page.WorkflowRuns {
			if run.ID == 0 {
				return nil, fmt.Errorf("workflow run page %d contains a run without an id", pageIndex+1)
			}
			if _, exists := runIDs[run.ID]; exists {
				return nil, fmt.Errorf("workflow run id %d appears more than once", run.ID)
			}
			runIDs[run.ID] = struct{}{}
			raw = append(raw, run)
		}
	}
	if len(runIDs) != totalCount {
		return nil, fmt.Errorf("workflow run discovery returned %d unique runs, want %d", len(runIDs), totalCount)
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, run := range raw {
		name := strings.TrimSpace(run.Name)
		if name == "" {
			name = strings.TrimSpace(run.DisplayName)
		}
		if name == "" {
			name = "GitHub Actions workflow"
		}
		var startedAt time.Time
		for _, timestamp := range []string{run.RunStartedAt, run.CreatedAt} {
			if parsed, parseErr := time.Parse(time.RFC3339, timestamp); parseErr == nil {
				startedAt = parsed
				break
			}
		}
		var completedAt time.Time
		if run.UpdatedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, run.UpdatedAt); parseErr == nil {
				completedAt = parsed
			}
		}
		bucket := normalizeCheckBucket("", run.Conclusion)
		if bucket == "" {
			bucket = normalizeCheckBucket("", run.Status)
		}
		if bucket == "" {
			// An unrecognized or incomplete run state must not certify the
			// commit as green. Keep monitoring until GitHub reports a terminal
			// state that can be classified.
			bucket = scm.CheckBucketPending
		}
		state := strings.ToUpper(strings.TrimSpace(run.Conclusion))
		if state == "" {
			state = strings.ToUpper(strings.TrimSpace(run.Status))
		}
		link := strings.TrimSpace(run.HTMLURL)
		if link == "" {
			host := strings.TrimSpace(h.host)
			if host == "" {
				host = "github.com"
			}
			link = fmt.Sprintf("https://%s/%s/actions/runs/%d", host, repo, run.ID)
		}
		checks = append(checks, scm.Check{Name: name, Bucket: bucket, Kind: scm.CheckKindRun, State: state, CompletedAt: completedAt, StartedAt: startedAt, WorkflowID: run.WorkflowID, Link: link})
	}
	return checks, nil
}

// RerunCheck re-runs the Actions work behind check for the same commit, so a
// check the provider cancelled rather than failed can be retried without a new
// push. The job is identified from the check's details link, which is the only
// run/job identity `gh pr checks` reports: a link naming a job re-runs just that
// job (and its dependencies), and a cancelled check naming only a run re-runs
// the whole run. Anything else - a third-party status pointing at an external
// dashboard, or a run path this backend cannot read - names no re-runnable work,
// and the error says so rather than falling back to a wider rerun.
func (h *Host) RerunCheck(ctx context.Context, _ *scm.PR, check scm.Check) error {
	rerunArgs, ok := h.rerunTargetArgs(check)
	if !ok {
		return fmt.Errorf("check %q has no GitHub Actions job to re-run", check.Name)
	}
	args := append([]string{"run", "rerun"}, rerunArgs...)
	args = append(args, h.repoArgs()...)
	cmd := h.cmd(ctx, "gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh run rerun: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// rerunTargetArgs turns a check into the `gh run rerun` arguments that re-run
// exactly that work, or reports that the link names nothing this backend can
// re-run.
func (h *Host) rerunTargetArgs(check scm.Check) ([]string, bool) {
	runID, jobID, ok := h.actionsRerunTarget(check.Link)
	switch {
	case !ok:
		return nil, false
	case check.InfrastructureFailure:
		// GitHub's failed-jobs primitive also re-runs dependent jobs. It is
		// authorized only after the classifier has bound the full failed family,
		// its one PR-only omission, the joined in-workflow guard, and retained
		// producer provenance to this exact run.
		if !check.InfrastructureRerunSafe {
			return nil, false
		}
		return []string{runID, "--failed"}, true
	case jobID != "":
		return []string{"--job", jobID}, true
	case strings.EqualFold(strings.TrimSpace(check.State), "CANCELLED"):
		return []string{runID}, true
	default:
		return []string{runID, "--failed"}, true
	}
}

func (h *Host) actionsRunID(link string) string {
	segments, ok := h.actionsRunSegments(link)
	if !ok {
		return ""
	}
	return segments[0]
}

func (h *Host) actionsRunSegments(link string) ([]string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return nil, false
	}
	host := strings.TrimSpace(h.host)
	if host == "" {
		host = "github.com"
	}
	if !strings.EqualFold(parsed.Hostname(), host) {
		return nil, false
	}
	repo := h.repoSlug()
	if repo == "" {
		return nil, false
	}
	runsPrefix := "/" + strings.Trim(repo, "/") + "/actions/runs/"
	if len(parsed.Path) <= len(runsPrefix) || !strings.EqualFold(parsed.Path[:len(runsPrefix)], runsPrefix) {
		return nil, false
	}
	segments := strings.Split(strings.Trim(parsed.Path[len(runsPrefix):], "/"), "/")
	if len(segments) == 0 || !isNumericID(segments[0]) {
		return nil, false
	}
	return segments, true
}

// actionsRerunTarget resolves the Actions run, and where possible the exact job,
// that a check's details URL names. It parses the URL and reads only its path:
// a real details URL routinely carries a query (?check_suite_focus=true) or a
// step fragment (#step:4:12), and neither belongs to the job identity.
//
// Only ".../actions/runs/<run-id>/job/<id>" yields a job id, because that `id`
// is the job's databaseId, which is what `gh run rerun --job` requires. Two
// shapes are deliberately rejected rather than downgraded to the whole run:
// the browser's ".../runs/<run-id>/jobs/<n>" form, whose number is a per-run
// display index the API answers with 404, and any other unrecognized path under
// a run. Re-running a run can restart more than one job, so widening one
// check's rerun on an unparsable link is a blast radius this policy must not
// take.
func (h *Host) actionsRerunTarget(link string) (runID, jobID string, ok bool) {
	segments, ok := h.actionsRunSegments(link)
	if !ok {
		return "", "", false
	}
	runID = segments[0]
	switch {
	case len(segments) == 1:
		return runID, "", true
	case len(segments) == 3 && segments[1] == "job" && isNumericID(segments[2]):
		return runID, segments[2], true
	default:
		return "", "", false
	}
}

// PreRunFailures reports which of the given failed checks GitHub Actions failed
// during the job's setup phase - before any repository step ran. Actions
// resolves and downloads every action a job uses inside "Set up job", so an
// action-download outage ("Failed to resolve action download info", HTTP 503)
// fails that step and the job never executes a repository step. It reads the
// job's own step-level conclusions, never log text, and fails closed: a check
// whose job it cannot resolve or read is simply not flagged, so it stays a
// genuine failure. The result is positional (parallel to checks), so a
// same-named genuine failure never inherits another check's infrastructure flag.
func (h *Host) PreRunFailures(ctx context.Context, checks []scm.Check) ([]bool, error) {
	result := make([]bool, len(checks))
	// Cache each run's jobs so several checks from one run cost one API call.
	runJobs := map[string][]githubRunJob{}
	for i, check := range checks {
		runID, jobID, ok := h.actionsRerunTarget(check.Link)
		if !ok {
			continue
		}
		jobs, seen := runJobs[runID]
		if !seen {
			jobs, _ = h.fetchRunJobs(ctx, runID)
			runJobs[runID] = jobs
		}
		job, found := matchRunJob(jobs, jobID, check.Name)
		if found && jobFailedAtSetup(job) {
			result[i] = true
		}
	}
	return result, nil
}

// fetchRunJobs reads a run's jobs (with their steps) from Actions. A run it
// cannot read yields no jobs, so every check on it fails closed to a genuine
// failure rather than being guessed as infrastructure.
func (h *Host) fetchRunJobs(ctx context.Context, runID string) ([]githubRunJob, error) {
	repo := h.repoSlug()
	if repo == "" {
		return nil, errors.New("repository slug is required to read workflow jobs")
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", "repos/"+repo+"/actions/runs/"+runID+"/jobs", "-f", "filter=latest", "-f", "per_page=100")
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api workflow jobs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload githubRunJobsResponse
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("parse workflow jobs: %w", err)
	}
	return payload.Jobs, nil
}

// fetchRunAttemptJobs reads and validates the complete job population for one
// immutable workflow attempt. --paginate --slurp preserves page boundaries so
// total_count can be checked against the union; a missing page, duplicate job,
// or inconsistent count fails closed instead of authorizing a partial retry.
func (h *Host) fetchRunAttemptJobs(ctx context.Context, runID string, attempt int) ([]githubRunJob, error) {
	repo := h.repoSlug()
	if repo == "" {
		return nil, errors.New("repository slug is required to read workflow attempt jobs")
	}
	if attempt <= 0 {
		return nil, errors.New("workflow attempt is required to read jobs")
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	endpoint := fmt.Sprintf("repos/%s/actions/runs/%s/attempts/%d/jobs", repo, runID, attempt)
	args = append(args, "--method", "GET", endpoint, "-f", "per_page=100", "--paginate", "--slurp")
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api workflow attempt jobs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var pages []githubRunJobsResponse
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse workflow attempt jobs: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("workflow attempt jobs response contained no pages")
	}
	total := pages[0].TotalCount
	if total < 0 {
		return nil, errors.New("workflow attempt jobs response has a negative total_count")
	}
	jobs := make([]githubRunJob, 0, total)
	seen := map[int]bool{}
	for _, page := range pages {
		if page.TotalCount != total {
			return nil, errors.New("workflow attempt jobs pages disagree on total_count")
		}
		for _, job := range page.Jobs {
			id := job.databaseID()
			if id <= 0 || seen[id] {
				return nil, errors.New("workflow attempt jobs contain a missing or duplicate job id")
			}
			seen[id] = true
			jobs = append(jobs, job)
		}
	}
	if len(jobs) != total {
		return nil, fmt.Errorf("workflow attempt jobs response is incomplete: got %d of %d jobs", len(jobs), total)
	}
	return jobs, nil
}

// matchRunJob finds the job a check names: by databaseId when the check's link
// carried one, otherwise by job name. A re-run can renumber jobs, so the name
// fallback keeps a check matchable when its link named only the run.
func matchRunJob(jobs []githubRunJob, jobID, checkName string) (githubRunJob, bool) {
	if jobID != "" {
		for _, job := range jobs {
			if strconv.Itoa(job.databaseID()) == jobID {
				return job, true
			}
		}
	}
	for _, job := range jobs {
		if normalizeRunName(job.Name) == normalizeRunName(checkName) {
			return job, true
		}
	}
	return githubRunJob{}, false
}

func matchRunJobExact(jobs []githubRunJob, jobID, checkName string) (githubRunJob, bool) {
	if jobID == "" {
		return githubRunJob{}, false
	}
	for _, job := range jobs {
		if strconv.Itoa(job.databaseID()) == jobID && normalizeRunName(job.Name) == normalizeRunName(checkName) {
			return job, true
		}
	}
	return githubRunJob{}, false
}

// jobFailedAtSetup reports whether a failed job failed in its setup step, before
// any repository step ran. It requires the job itself to be failed and its setup
// step ("Set up job", always step 1) to carry a failure conclusion; a job whose
// setup succeeded and failed a later step is a real failure and is never matched.
func jobFailedAtSetup(job githubRunJob) bool {
	if !isFailedJob(job) {
		return false
	}
	for _, step := range job.Steps {
		if step.Number == 1 || strings.EqualFold(strings.TrimSpace(step.Name), "Set up job") {
			return strings.EqualFold(strings.TrimSpace(step.Conclusion), "failure")
		}
	}
	return false
}

// ArtifactInfrastructureFailures classifies one complete attempt-one failure
// family. GitHub's jobs API proves the exact run/head and every conclusion;
// job logs bind each initiating failure to a pinned artifact action and its
// terminal service error. The only dispatch-safe family is the joined XAU
// topology whose own guard will revalidate attempt one inside every rerun job.
// Missing, ambiguous, stale, or partially joined evidence fails closed.
func (h *Host) ArtifactInfrastructureFailures(ctx context.Context, pr *scm.PR, checks []scm.Check) ([]scm.InfrastructureFailure, error) {
	result := make([]scm.InfrastructureFailure, len(checks))
	if pr == nil || strings.TrimSpace(pr.Number) == "" || strings.TrimSpace(pr.HeadSHA) == "" || strings.TrimSpace(pr.BaseBranch) == "" || strings.TrimSpace(pr.BaseSHA) == "" {
		return result, nil
	}
	prNumber, err := strconv.Atoi(strings.TrimSpace(pr.Number))
	if err != nil {
		return result, nil
	}
	type cachedRun struct {
		metadata       githubWorkflowRun
		jobs           []githubRunJob
		classification artifactRunClassification
		classified     bool
		err            error
	}
	cache := map[string]cachedRun{}
	for i, check := range checks {
		runID, jobID, ok := h.actionsRerunTarget(check.Link)
		if !ok || jobID == "" {
			continue
		}
		entry, seen := cache[runID]
		if !seen {
			entry.metadata, entry.err = h.fetchWorkflowRun(ctx, runID)
			if entry.err == nil && workflowRunMatchesCandidate(entry.metadata, prNumber, pr.HeadSHA, pr.BaseBranch, pr.BaseSHA) {
				entry.jobs, entry.err = h.fetchRunAttemptJobs(ctx, runID, 1)
			}
			cache[runID] = entry
		}
		if entry.err != nil {
			return result, entry.err
		}
		if !workflowRunMatchesCandidate(entry.metadata, prNumber, pr.HeadSHA, pr.BaseBranch, pr.BaseSHA) {
			continue
		}
		job, found := matchRunJobExact(entry.jobs, jobID, check.Name)
		if !found || strings.TrimSpace(job.HeadSHA) != strings.TrimSpace(pr.HeadSHA) {
			continue
		}
		if !entry.classified {
			entry.classification, entry.err = h.classifyArtifactRun(ctx, entry.metadata, entry.jobs, checks, runID, pr.HeadSHA, pr.BaseSHA)
			entry.classified = true
			cache[runID] = entry
		}
		if entry.err != nil {
			return result, entry.err
		}
		classification := entry.classification
		if classification.skippedDependentJobs[job.databaseID()] {
			result[i] = scm.InfrastructureFailure{
				RerunDependent: true,
				Group:          "github-actions-run:" + runID,
				HeadSHA:        strings.TrimSpace(pr.HeadSHA),
				BaseSHA:        strings.TrimSpace(pr.BaseSHA),
				RerunSafe:      classification.rerunSafe,
				Evidence:       classification.evidence,
			}
			continue
		}
		if classification.omittedJobs[job.databaseID()] {
			result[i] = scm.InfrastructureFailure{
				RerunOmission: true,
				Group:         "github-actions-run:" + runID,
				HeadSHA:       strings.TrimSpace(pr.HeadSHA),
				BaseSHA:       strings.TrimSpace(pr.BaseSHA),
				RerunSafe:     classification.rerunSafe,
				Evidence:      classification.evidence,
			}
			continue
		}
		if !classification.failedJobs[job.databaseID()] || !check.Failing() || !strings.EqualFold(strings.TrimSpace(check.State), "FAILURE") {
			continue
		}
		result[i] = scm.InfrastructureFailure{
			Retryable: true,
			Group:     "github-actions-run:" + runID,
			Reason:    artifactInfrastructureReason(classification),
			HeadSHA:   strings.TrimSpace(pr.HeadSHA),
			BaseSHA:   strings.TrimSpace(pr.BaseSHA),
			RerunSafe: classification.rerunSafe,
			Evidence:  classification.evidence,
		}
	}
	return result, nil
}

type artifactRunClassification struct {
	failedJobs           map[int]bool
	omittedJobs          map[int]bool
	skippedDependentJobs map[int]bool
	rerunSafe            bool
	dispatchRefusal      string
	evidence             scm.InfrastructureEvidenceReceipt
}

func artifactInfrastructureReason(classification artifactRunClassification) string {
	reason := "artifact-transfer infrastructure failed; dependent work must pass on recovery"
	if classification.dispatchRefusal != "" {
		return reason + "; dispatch refused: " + classification.dispatchRefusal
	}
	return reason
}

func (h *Host) classifyArtifactRun(ctx context.Context, run githubWorkflowRun, jobs []githubRunJob, checks []scm.Check, runID, headSHA, baseSHA string) (artifactRunClassification, error) {
	classification := artifactRunClassification{failedJobs: map[int]bool{}, omittedJobs: map[int]bool{}, skippedDependentJobs: map[int]bool{}}
	joinedTopology := joinedXAUArtifactRetryTopology(strings.TrimSpace(run.Event), jobs)
	if !h.runAttemptPopulationMatchesChecks(jobs, checks, runID, headSHA, joinedTopology) {
		return classification, nil
	}
	initiatingNames := map[string]bool{}
	potentialDependentJobs := map[int][]scm.InfrastructureStepReceipt{}
	var dependentSteps []scm.InfrastructureStepReceipt
	pinnedInitiatingActions := true
	for _, job := range jobs {
		switch strings.ToLower(strings.TrimSpace(job.Conclusion)) {
		case "failure":
			logs, err := h.fetchWorkflowJobLogs(ctx, job.databaseID())
			if err != nil {
				return classification, err
			}
			classification.evidence.LogJobIDs = append(classification.evidence.LogJobIDs, int64(job.databaseID()))
			if disposition, ok := artifactFailureDisposition(job, logs, joinedTopology); ok {
				initiatingNames[strings.TrimSpace(job.Name)] = true
				pinnedInitiatingActions = pinnedInitiatingActions && disposition.PinnedActions
				dependentSteps = append(dependentSteps, disposition.DependentSteps...)
				classification.failedJobs[job.databaseID()] = true
				continue
			}
			if joinedTopology {
				if steps, ok := joinedXAURepositoryDependencyFailure(job, logs); ok {
					potentialDependentJobs[job.databaseID()] = steps
					continue
				}
			}
			return emptyArtifactRunClassification(), nil
		}
	}
	if len(initiatingNames) == 0 {
		return emptyArtifactRunClassification(), nil
	}
	dependentNames := joinedXAURetryDependentNames(initiatingNames)
	var dependentJobs []int64
	for _, job := range jobs {
		name := strings.TrimSpace(job.Name)
		conclusion := strings.ToLower(strings.TrimSpace(job.Conclusion))
		if dependentNames[name] && !initiatingNames[name] {
			if conclusion == "success" {
				return emptyArtifactRunClassification(), nil
			}
			dependentJobs = append(dependentJobs, int64(job.databaseID()))
			if conclusion == "skipped" {
				classification.skippedDependentJobs[job.databaseID()] = true
				continue
			}
			steps, ok := potentialDependentJobs[job.databaseID()]
			if !ok {
				return emptyArtifactRunClassification(), nil
			}
			dependentSteps = append(dependentSteps, steps...)
			classification.failedJobs[job.databaseID()] = true
			continue
		}
		if conclusion == "skipped" {
			if run.Event != "pull_request" || name != joinedXAUPROnlyOmittedJob {
				return emptyArtifactRunClassification(), nil
			}
			classification.omittedJobs[job.databaseID()] = true
		}
		if _, unresolved := potentialDependentJobs[job.databaseID()]; unresolved {
			return emptyArtifactRunClassification(), nil
		}
	}
	artifacts, err := h.fetchRunArtifacts(ctx, runID)
	if err != nil {
		return classification, err
	}
	classification.evidence.ProviderRunID = runID
	classification.evidence.Attempt = 1
	classification.evidence.DependentJobs = dependentJobs
	classification.evidence.DependentSteps = dependentSteps
	classification.evidence.Artifacts = artifacts
	if joinedTopology && pinnedInitiatingActions && xauArtifactReceiptsProveAttemptOne(artifacts, run.ID, headSHA, jobs) {
		classification.rerunSafe, err = h.xauRetryRuleLanded(ctx, strings.TrimSpace(baseSHA), headSHA)
		if err != nil {
			return classification, err
		}
		if !classification.rerunSafe {
			classification.dispatchRefusal = "joined XAU retry-control identity is not reviewed on the unchanged trusted base"
		}
	}
	return classification, nil
}

func emptyArtifactRunClassification() artifactRunClassification {
	return artifactRunClassification{failedJobs: map[int]bool{}, omittedJobs: map[int]bool{}, skippedDependentJobs: map[int]bool{}}
}

func joinedXAURetryDependentNames(initiating map[string]bool) map[string]bool {
	journeys := []string{
		"journey smoke (chromium / desktop)",
		"journey smoke (firefox / desktop)",
		"journey smoke (webkit / desktop)",
		"journey smoke (chromium / touch390)",
		"journey smoke (webkit / touch390)",
	}
	shards := []string{"repository shard 1 of 4", "repository shard 2 of 4", "repository shard 3 of 4", "repository shard 4 of 4"}
	direct := map[string][]string{
		"verify original run or bounded infrastructure retry": append(append([]string{"build and seal the Dashboard V2 distribution", "repository checks", "repository"}, journeys...), shards...),
		"build and seal the Dashboard V2 distribution":        append(append([]string{"repository"}, journeys...), shards...),
		joinedXAUPROnlyOmittedJob:                             append([]string{"repository checks", "repository"}, shards...),
		"repository checks":                                   {"repository"},
	}
	for _, name := range journeys {
		direct[name] = []string{"repository"}
	}
	for _, name := range shards {
		direct[name] = []string{"repository"}
	}
	result := map[string]bool{}
	queue := make([]string, 0, len(initiating))
	for name := range initiating {
		queue = append(queue, name)
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, dependent := range direct[name] {
			if !result[dependent] {
				result[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	return result
}

var xauRetryRulePaths = []string{
	".github/workflows/xau-ci.yml",
	"scripts/ci_check_infrastructure_retry.py",
}

type xauRetryContractIdentity struct {
	workflowBlobSHA string
	verifierBlobSHA string
}

// reviewedXAURetryContractIdentities is intentionally empty until the XAU
// owner lands a corrected, joined workflow and verifier and their exact blob
// pair passes cross-repository review. Adding one exact pair here is the bounded
// activation mechanism; equality between arbitrary base/head files is not.
var reviewedXAURetryContractIdentities = [...]xauRetryContractIdentity{}

func xauRetryContractIdentityReviewed(identity xauRetryContractIdentity, reviewed []xauRetryContractIdentity) bool {
	if !isHexSHA(identity.workflowBlobSHA) || !isHexSHA(identity.verifierBlobSHA) {
		return false
	}
	for _, candidate := range reviewed {
		if identity == candidate {
			return true
		}
	}
	return false
}

// xauRetryRuleLanded proves the retry guard is established on the trusted base
// with an explicitly reviewed implementation identity, and unchanged on the
// candidate. A branch cannot authorize provider mutation with old, lookalike,
// or independently changed workflow/verifier files.
func (h *Host) xauRetryRuleLanded(ctx context.Context, baseSHA, headSHA string) (bool, error) {
	if baseSHA == "" || headSHA == "" {
		return false, nil
	}
	var identity xauRetryContractIdentity
	for index, path := range xauRetryRulePaths {
		baseBlob, err := h.fetchRepoContentBlob(ctx, path, baseSHA)
		if err != nil {
			return false, err
		}
		headBlob, err := h.fetchRepoContentBlob(ctx, path, headSHA)
		if err != nil {
			return false, err
		}
		if baseBlob == "" || headBlob != baseBlob {
			return false, nil
		}
		if index == 0 {
			identity.workflowBlobSHA = baseBlob
		} else {
			identity.verifierBlobSHA = baseBlob
		}
	}
	return xauRetryContractIdentityReviewed(identity, reviewedXAURetryContractIdentities[:]), nil
}

func (h *Host) fetchRepoContentBlob(ctx context.Context, path, ref string) (string, error) {
	repo := h.repoSlug()
	if repo == "" || strings.TrimSpace(path) == "" || strings.TrimSpace(ref) == "" {
		return "", errors.New("repository, path, and ref are required to read retry-rule identity")
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", "repos/"+repo+"/contents/"+strings.TrimLeft(path, "/"), "-f", "ref="+ref)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh api retry-rule content: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("parse retry-rule content: %w", err)
	}
	if payload.Type != "file" || !isHexSHA(payload.SHA) {
		return "", nil
	}
	return payload.SHA, nil
}

func isHexSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func joinedXAURepositoryDependencyFailure(job githubRunJob, logs string) ([]scm.InfrastructureStepReceipt, bool) {
	if strings.TrimSpace(job.Name) != "repository" || !isFailedJob(job) || strings.TrimSpace(logs) == "" {
		return nil, false
	}
	failed := false
	var dependent []scm.InfrastructureStepReceipt
	for _, step := range job.Steps {
		if jobLifecycleStep(step.Name) {
			continue
		}
		conclusion := strings.ToLower(strings.TrimSpace(step.Conclusion))
		switch conclusion {
		case "success":
			continue
		case "failure":
			if failed || strings.TrimSpace(step.Name) != "Prove every shard ran and together covered the battery" {
				return nil, false
			}
			failed = true
			dependent = append(dependent, scm.InfrastructureStepReceipt{JobID: int64(job.databaseID()), Number: step.Number, Name: strings.TrimSpace(step.Name)})
		case "skipped":
			if !failed {
				return nil, false
			}
			dependent = append(dependent, scm.InfrastructureStepReceipt{JobID: int64(job.databaseID()), Number: step.Number, Name: strings.TrimSpace(step.Name)})
		default:
			return nil, false
		}
	}
	return dependent, failed
}

func xauArtifactReceiptsProveAttemptOne(artifacts []scm.InfrastructureArtifactReceipt, runID int, headSHA string, jobs []githubRunJob) bool {
	if runID <= 0 || strings.TrimSpace(headSHA) == "" {
		return false
	}
	found := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		name := strings.TrimSpace(artifact.Name)
		attemptOneName := name == "dashboard-v2-dist-1" || strings.HasSuffix(name, "-a1")
		if !attemptOneName || found[name] || artifact.ProviderRunID != int64(runID) || artifact.HeadSHA != strings.TrimSpace(headSHA) || !validSHA256Digest(artifact.Digest) {
			return false
		}
		found[name] = true
	}
	for _, job := range jobs {
		if !strings.EqualFold(strings.TrimSpace(job.Conclusion), "success") {
			continue
		}
		required, producer := xauAttemptOneArtifactForProducer(strings.TrimSpace(job.Name), strings.TrimSpace(headSHA))
		if producer && !found[required] {
			return false
		}
	}
	return true
}

func xauAttemptOneArtifactForProducer(jobName, headSHA string) (string, bool) {
	switch jobName {
	case "build and seal the Dashboard V2 distribution":
		return "dashboard-v2-dist-1", true
	case "repository shard 1 of 4":
		return "delivery-lane-shard-1-" + headSHA + "-a1", true
	case "repository shard 2 of 4":
		return "delivery-lane-shard-2-" + headSHA + "-a1", true
	case "repository shard 3 of 4":
		return "delivery-lane-shard-3-" + headSHA + "-a1", true
	case "repository shard 4 of 4":
		return "delivery-lane-shard-4-" + headSHA + "-a1", true
	case "journey smoke (chromium / desktop)":
		return "xau-journey-evidence-chromium-desktop-" + headSHA + "-a1", true
	case "journey smoke (firefox / desktop)":
		return "xau-journey-evidence-firefox-desktop-" + headSHA + "-a1", true
	case "journey smoke (webkit / desktop)":
		return "xau-journey-evidence-webkit-desktop-" + headSHA + "-a1", true
	case "journey smoke (chromium / touch390)":
		return "xau-journey-evidence-chromium-touch390-" + headSHA + "-a1", true
	case "journey smoke (webkit / touch390)":
		return "xau-journey-evidence-webkit-touch390-" + headSHA + "-a1", true
	default:
		return "", false
	}
}

func validSHA256Digest(digest string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+64 {
		return false
	}
	for _, r := range digest[len(prefix):] {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func (h *Host) fetchRunArtifacts(ctx context.Context, runID string) ([]scm.InfrastructureArtifactReceipt, error) {
	repo := h.repoSlug()
	if repo == "" {
		return nil, errors.New("repository slug is required to read workflow artifacts")
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", "repos/"+repo+"/actions/runs/"+runID+"/artifacts", "-f", "per_page=100", "--paginate", "--slurp")
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api workflow artifacts: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var pages []githubRunArtifactsResponse
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse workflow artifacts: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("workflow artifacts response contained no pages")
	}
	total := pages[0].TotalCount
	if total < 0 {
		return nil, errors.New("workflow artifacts response has a negative total_count")
	}
	artifacts := make([]scm.InfrastructureArtifactReceipt, 0, total)
	seen := map[int64]bool{}
	for _, page := range pages {
		if page.TotalCount != total {
			return nil, errors.New("workflow artifact pages disagree on total_count")
		}
		for _, artifact := range page.Artifacts {
			if artifact.ID <= 0 || seen[artifact.ID] || strings.TrimSpace(artifact.Name) == "" || artifact.Expired {
				return nil, errors.New("workflow artifacts contain missing, duplicate, or expired evidence")
			}
			seen[artifact.ID] = true
			artifacts = append(artifacts, scm.InfrastructureArtifactReceipt{
				ID:            artifact.ID,
				Name:          strings.TrimSpace(artifact.Name),
				Digest:        strings.TrimSpace(artifact.Digest),
				ProviderRunID: artifact.WorkflowRun.ID,
				HeadSHA:       strings.TrimSpace(artifact.WorkflowRun.HeadSHA),
			})
		}
	}
	if len(artifacts) != total {
		return nil, fmt.Errorf("workflow artifacts response is incomplete: got %d of %d artifacts", len(artifacts), total)
	}
	return artifacts, nil
}

func (h *Host) fetchWorkflowRun(ctx context.Context, runID string) (githubWorkflowRun, error) {
	var run githubWorkflowRun
	repo := h.repoSlug()
	if repo == "" {
		return run, errors.New("repository slug is required to read workflow run")
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", "repos/"+repo+"/actions/runs/"+runID)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return run, fmt.Errorf("gh api workflow run: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if err := json.Unmarshal(out, &run); err != nil {
		return run, fmt.Errorf("parse workflow run: %w", err)
	}
	return run, nil
}

func (h *Host) fetchWorkflowJobLogs(ctx context.Context, jobID int) (string, error) {
	if jobID <= 0 {
		return "", errors.New("workflow job id is required to read logs")
	}
	repo := h.repoSlug()
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repo, jobID))
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh api workflow job logs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

func workflowRunMatchesCandidate(run githubWorkflowRun, prNumber int, headSHA, baseBranch, baseSHA string) bool {
	if run.RunAttempt != 1 || strings.TrimSpace(run.HeadSHA) != strings.TrimSpace(headSHA) {
		return false
	}
	for _, candidate := range run.PullRequests {
		if candidate.Number == prNumber && strings.TrimSpace(candidate.Head.SHA) == strings.TrimSpace(headSHA) && strings.TrimSpace(candidate.Base.Ref) == strings.TrimSpace(baseBranch) && strings.TrimSpace(candidate.Base.SHA) == strings.TrimSpace(baseSHA) {
			return true
		}
	}
	return false
}

const boundedInfrastructureRetryStep = "Admit only the original run or one verified infrastructure retry"

var joinedXAUArtifactRetryJobNames = map[string]struct{}{
	"verify original run or bounded infrastructure retry":       {},
	"build and seal the Dashboard V2 distribution":              {},
	"journey smoke (chromium / desktop)":                        {},
	"journey smoke (firefox / desktop)":                         {},
	"journey smoke (webkit / desktop)":                          {},
	"journey smoke (chromium / touch390)":                       {},
	"journey smoke (webkit / touch390)":                         {},
	"look for a proving pull request with the same pushed tree": {},
	"repository shard 1 of 4":                                   {},
	"repository shard 2 of 4":                                   {},
	"repository shard 3 of 4":                                   {},
	"repository shard 4 of 4":                                   {},
	"repository checks":                                         {},
	"repository":                                                {},
}

const joinedXAUPROnlyOmittedJob = "look for a proving pull request with the same pushed tree"

// joinedXAUArtifactRetryTopology binds the fork's only safe dependent-job
// rerun route to the same versioned XAU population its in-workflow verifier
// checks. The five browser matrix cells are independent jobs, and the one
// whole-job omission is valid only for the push-only reuse lookup on a PR.
func joinedXAUArtifactRetryTopology(event string, jobs []githubRunJob) bool {
	event = strings.TrimSpace(event)
	if event != "pull_request" && event != "push" {
		return false
	}
	if len(jobs) != len(joinedXAUArtifactRetryJobNames) {
		return false
	}
	seen := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		name := strings.TrimSpace(job.Name)
		if _, ok := joinedXAUArtifactRetryJobNames[name]; !ok || seen[name] {
			return false
		}
		seen[name] = true
		conclusion := strings.ToLower(strings.TrimSpace(job.Conclusion))
		if conclusion == "skipped" {
			if len(job.Steps) != 0 || name == joinedXAUPROnlyOmittedJob && event != "pull_request" {
				return false
			}
			continue
		}
		if conclusion != "success" && conclusion != "failure" {
			return false
		}
		if conclusion == "success" && !joinedXAUSuccessfulJobSteps(job) {
			return false
		}
		guardCount := 0
		for _, step := range job.Steps {
			if strings.TrimSpace(step.Name) == boundedInfrastructureRetryStep && strings.EqualFold(strings.TrimSpace(step.Conclusion), "success") {
				guardCount++
			}
		}
		if guardCount != 1 {
			return false
		}
	}
	for _, job := range jobs {
		if strings.TrimSpace(job.Name) == joinedXAUPROnlyOmittedJob {
			return event != "pull_request" || strings.EqualFold(strings.TrimSpace(job.Conclusion), "skipped")
		}
	}
	return false
}

const (
	joinedXAURetainedBundleStep = "Digest-bind and extract the retained Dashboard V2 distribution"
	joinedXAURetainedProofStep  = "Digest-bind and extract retained shard and journey evidence"
)

// joinedXAUAttemptOneOmittedStep is the complete set of conditional steps the
// joined workflow intentionally skips on attempt one. These exact owner/name
// pairs are structural evidence only: callers also require the complete joined
// topology, and dispatch separately requires a reviewed workflow/verifier blob
// identity before this model can authorize a provider mutation.
func joinedXAUAttemptOneOmittedStep(jobName, stepName string) bool {
	jobName = strings.TrimSpace(jobName)
	stepName = strings.TrimSpace(stepName)
	switch stepName {
	case joinedXAURetainedBundleStep:
		switch jobName {
		case "journey smoke (chromium / desktop)",
			"journey smoke (firefox / desktop)",
			"journey smoke (webkit / desktop)",
			"journey smoke (chromium / touch390)",
			"journey smoke (webkit / touch390)",
			"repository shard 1 of 4",
			"repository shard 2 of 4",
			"repository shard 3 of 4",
			"repository shard 4 of 4":
			return true
		}
	case joinedXAURetainedProofStep:
		return jobName == "repository"
	case "Print verify log tail on failure", "Upload verify log artifact on failure":
		switch jobName {
		case "repository checks",
			"repository shard 1 of 4",
			"repository shard 2 of 4",
			"repository shard 3 of 4",
			"repository shard 4 of 4":
			return true
		}
	}
	return false
}

// joinedXAUSuccessfulJobSteps prevents an otherwise successful producer from
// hiding skipped required work. Only lifecycle bookkeeping and the exact
// attempt-one omissions above may be non-successful in a successful job.
func joinedXAUSuccessfulJobSteps(job githubRunJob) bool {
	for _, step := range job.Steps {
		if jobLifecycleStep(step.Name) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(step.Conclusion)) {
		case "success":
			continue
		case "skipped":
			if joinedXAUAttemptOneOmittedStep(job.Name, step.Name) {
				continue
			}
		}
		return false
	}
	return true
}

type artifactJobDisposition struct {
	Initiating     bool
	PinnedActions  bool
	DependentSteps []scm.InfrastructureStepReceipt
}

const (
	xauUploadArtifactAction   = "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"
	xauDownloadArtifactAction = "actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093"
)

func jobLifecycleStep(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "set up job" || name == "set up runner" || name == "complete runner" || name == "complete job" || strings.HasPrefix(name, "post ")
}

func artifactActionOperation(action string) string {
	switch {
	case strings.HasPrefix(action, "actions/upload-artifact@"):
		return "finalizeartifact"
	case strings.HasPrefix(action, "actions/download-artifact@"):
		return "listartifacts"
	default:
		return ""
	}
}

func consequentialNoFilesFailure(text string) bool {
	errors := 0
	for _, line := range strings.Split(text, "\n") {
		payload, ok := runnerErrorPayload(line)
		if !ok {
			continue
		}
		errors++
		payload = strings.TrimSpace(payload)
		if !strings.HasPrefix(payload, "No files were found with the provided path:") || !strings.HasSuffix(payload, "No artifacts will be uploaded.") {
			return false
		}
	}
	return errors == 1
}

// artifactFailureDisposition admits a job only when its first failed work is
// one pinned artifact client's terminal 403/5xx. Required work skipped after
// that initiating error, plus the narrowly recognized no-files artifact
// consequence, is recorded as recovery-dependent. An executed test failure at
// any point still refuses the infrastructure class.
func artifactFailureDisposition(job githubRunJob, logs string, joinedTopology bool) (artifactJobDisposition, bool) {
	var disposition artifactJobDisposition
	if !isFailedJob(job) || len(job.Steps) == 0 {
		return disposition, false
	}
	segments := actionLogSegments(logs)
	executed := make([]githubJobStep, 0, len(job.Steps))
	for _, step := range job.Steps {
		if !jobLifecycleStep(step.Name) && !strings.EqualFold(strings.TrimSpace(step.Conclusion), "skipped") {
			executed = append(executed, step)
		}
	}
	if len(executed) == 0 || len(segments) != len(executed) {
		return disposition, false
	}
	segmentByNumber := make(map[int]actionLogSegment, len(executed))
	for i, step := range executed {
		segmentByNumber[step.Number] = segments[i]
	}
	initiated := false
	pinnedActions := true
	for _, step := range job.Steps {
		if jobLifecycleStep(step.Name) {
			continue
		}
		conclusion := strings.ToLower(strings.TrimSpace(step.Conclusion))
		switch conclusion {
		case "success":
			continue
		case "skipped":
			if joinedTopology && joinedXAUAttemptOneOmittedStep(job.Name, step.Name) {
				continue
			}
			if !initiated {
				return artifactJobDisposition{}, false
			}
			disposition.DependentSteps = append(disposition.DependentSteps, scm.InfrastructureStepReceipt{JobID: int64(job.databaseID()), Number: step.Number, Name: strings.TrimSpace(step.Name)})
		case "failure", "failed", "error":
			segment, ok := segmentByNumber[step.Number]
			if !ok {
				return artifactJobDisposition{}, false
			}
			operation := artifactActionOperation(segment.action)
			if !initiated {
				if operation == "" || !terminalArtifactRequestFailure(segment.text, operation) {
					return artifactJobDisposition{}, false
				}
				initiated = true
				disposition.Initiating = true
				pinnedActions = segment.action == xauUploadArtifactAction || segment.action == xauDownloadArtifactAction
				continue
			}
			if operation != "finalizeartifact" || !consequentialNoFilesFailure(segment.text) {
				return artifactJobDisposition{}, false
			}
			pinnedActions = pinnedActions && segment.action == xauUploadArtifactAction
			disposition.DependentSteps = append(disposition.DependentSteps, scm.InfrastructureStepReceipt{JobID: int64(job.databaseID()), Number: step.Number, Name: strings.TrimSpace(step.Name)})
		default:
			return artifactJobDisposition{}, false
		}
	}
	disposition.PinnedActions = pinnedActions
	return disposition, initiated
}

func (h *Host) runAttemptPopulationMatchesChecks(jobs []githubRunJob, checks []scm.Check, runID, headSHA string, joinedTopology bool) bool {
	for _, job := range jobs {
		if strings.TrimSpace(job.HeadSHA) != strings.TrimSpace(headSHA) || !strings.EqualFold(strings.TrimSpace(job.Status), "completed") {
			return false
		}
		conclusion := strings.ToLower(strings.TrimSpace(job.Conclusion))
		if conclusion == "skipped" && !joinedTopology {
			return false
		}
		if conclusion != "success" && conclusion != "failure" && conclusion != "skipped" {
			return false
		}
		matches := 0
		for _, check := range checks {
			candidateRun, candidateJob, ok := h.actionsRerunTarget(check.Link)
			if !ok || candidateRun != runID || candidateJob != strconv.Itoa(job.databaseID()) || normalizeRunName(check.Name) != normalizeRunName(job.Name) {
				continue
			}
			matches++
			switch conclusion {
			case "success":
				if check.Bucket != scm.CheckBucketPass || !strings.EqualFold(strings.TrimSpace(check.State), "SUCCESS") {
					return false
				}
			case "failure":
				if !check.Failing() || !strings.EqualFold(strings.TrimSpace(check.State), "FAILURE") {
					return false
				}
			case "skipped":
				if check.Bucket != scm.CheckBucketSkip || !strings.EqualFold(strings.TrimSpace(check.State), "SKIPPED") {
					return false
				}
			}
		}
		if matches != 1 {
			return false
		}
	}
	checksForRun := 0
	for _, check := range checks {
		candidateRun, jobID, ok := h.actionsRerunTarget(check.Link)
		if ok && candidateRun == runID && jobID != "" {
			checksForRun++
		}
	}
	return len(jobs) > 0 && checksForRun == len(jobs)
}

type actionLogSegment struct {
	action string
	text   string
}

func actionLogSegments(logs string) []actionLogSegment {
	const marker = "##[group]Run "
	var segments []actionLogSegment
	for remaining := logs; ; {
		start := strings.Index(remaining, marker)
		if start < 0 {
			break
		}
		remaining = remaining[start+len(marker):]
		lineEnd := strings.IndexByte(remaining, '\n')
		if lineEnd < 0 {
			lineEnd = len(remaining)
		}
		header := strings.TrimSpace(remaining[:lineEnd])
		next := strings.Index(remaining[lineEnd:], marker)
		segmentEnd := len(remaining)
		if next >= 0 {
			segmentEnd = lineEnd + next
		}
		segments = append(segments, actionLogSegment{action: strings.ToLower(header), text: remaining[:segmentEnd]})
		remaining = remaining[segmentEnd:]
	}
	return segments
}

func logsProveArtifactInfrastructureFailure(job githubRunJob, logs string) bool {
	disposition, ok := artifactFailureDisposition(job, logs, false)
	return ok && disposition.Initiating
}

// terminalArtifactRequestFailure binds the permitted operation and retryable
// response to one request line, then requires that request to be the action's
// terminal outcome. This deliberately rejects ambiguous multi-request logs: a
// successful ListArtifacts followed by some other operation's 403, or a 503
// that recovered before an unclassified failure, proves no retryable terminal
// failure for the permitted operation.
func terminalArtifactRequestFailure(text, operation string) bool {
	requestLine := -1
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !strings.Contains(strings.ToLower(line), operation) {
			continue
		}
		if requestLine >= 0 || !artifactRequestRecordFailed(line, operation) {
			return false
		}
		requestLine = i
	}
	if requestLine < 0 {
		return false
	}
	for _, line := range lines[requestLine+1:] {
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if strings.Contains(line, "##[error]") || strings.Contains(line, "error") || strings.Contains(line, "fail") || strings.Contains(line, "success") || strings.Contains(line, "succeed") || strings.Contains(line, "recover") || containsHTTPResponseMarker(line) {
			return false
		}
	}
	return true
}

// artifactRequestRecordFailed recognizes the bounded terminal form emitted by
// @actions/artifact 2.3.2, including download-artifact v4.3.0's wrapper. The
// runner envelope and entire payload are anchored: the parser never seeks
// forward through another clause to find a qualifying operation.
func artifactRequestRecordFailed(line, operation string) bool {
	payload, ok := runnerErrorPayload(line)
	if !ok {
		return false
	}
	payload = strings.ToLower(payload)
	for _, recognized := range []string{
		"createartifact", "finalizeartifact", "listartifacts",
		"getsignedartifacturl", "downloadartifact", "uploadartifact",
	} {
		count := strings.Count(payload, recognized)
		if recognized == operation {
			if count != 1 {
				return false
			}
		} else if count != 0 {
			return false
		}
	}
	prefix := ""
	switch operation {
	case "finalizeartifact":
		prefix = "failed to finalizeartifact: "
	case "listartifacts":
		prefix = "unable to download artifact(s): failed to listartifacts: "
	default:
		return false
	}
	if !strings.HasPrefix(payload, prefix) {
		return false
	}
	result := strings.TrimPrefix(payload, prefix)
	const nonRetryable = "received non-retryable error: failed request: "
	const exhausted = "failed to make request after 5 attempts: failed request: "
	exhaustedRetry := false
	switch {
	case strings.HasPrefix(result, nonRetryable):
		result = strings.TrimPrefix(result, nonRetryable)
	case strings.HasPrefix(result, exhausted):
		result = strings.TrimPrefix(result, exhausted)
		exhaustedRetry = true
	default:
		return false
	}
	code, ok := artifactRequestStatus(result)
	if !ok || code != 403 && (code < 500 || code > 599) {
		return false
	}
	retryableByClient := code == 500 || code == 502 || code == 503 || code == 504
	if exhaustedRetry != retryableByClient {
		return false
	}
	return true
}

func runnerErrorPayload(line string) (string, bool) {
	line = strings.TrimSpace(line)
	const annotation = "##[error]"
	if strings.HasPrefix(line, annotation) {
		payload := strings.TrimPrefix(line, annotation)
		return payload, strings.TrimSpace(payload) != ""
	}
	separator := strings.IndexByte(line, ' ')
	if separator <= 0 {
		return "", false
	}
	if _, err := time.Parse(time.RFC3339Nano, line[:separator]); err != nil {
		return "", false
	}
	remainder := line[separator+1:]
	if !strings.HasPrefix(remainder, annotation) {
		return "", false
	}
	payload := strings.TrimPrefix(remainder, annotation)
	return payload, strings.TrimSpace(payload) != ""
}

func artifactRequestStatus(result string) (int, bool) {
	if len(result) < len("(403) x") || result[0] != '(' {
		return 0, false
	}
	closeParen := strings.IndexByte(result, ')')
	if closeParen != 4 || len(result) <= closeParen+1 || result[closeParen+1] != ' ' {
		return 0, false
	}
	code, err := strconv.Atoi(result[1:closeParen])
	if err != nil {
		return 0, false
	}
	reason := result[closeParen+2:]
	standardReason := artifactHTTPReason[code]
	if reason != standardReason {
		// Captured attempt-one run 34250655836 / job 102144122733 adds
		// this exact intermediary detail to @actions/artifact 2.3.2's 403
		// status text. Keep the observed provider form bounded; no arbitrary
		// suffix or generalized 403 prose is admitted.
		const intermediary403 = "forbidden: error from intermediary with http status code 403 \"forbidden\""
		if code != 403 || reason != intermediary403 {
			return 0, false
		}
	}
	return code, true
}

var artifactHTTPReason = map[int]string{
	403: "forbidden",
	500: "internal server error",
	501: "not implemented",
	502: "bad gateway",
	503: "service unavailable",
	504: "gateway timeout",
	505: "http version not supported",
	507: "insufficient storage",
	508: "loop detected",
	510: "not extended",
	511: "network authentication required",
}

func containsHTTPResponseMarker(text string) bool {
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if field == "http" || field == "status" || field == "response" {
			return true
		}
	}
	return false
}

func isNumericID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "mergeable", "--jq", ".mergeable")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w", err)
	}
	return normalizeMergeableState(strings.TrimSpace(string(out))), nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, _ *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	targets := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		name = normalizeRunName(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	args := []string{"run", "list", "--branch", branch}
	if strings.TrimSpace(headSHA) != "" {
		args = append(args, "--commit", strings.TrimSpace(headSHA))
	}
	args = append(args, h.repoArgs()...)
	args = append(args,
		"--status", "failure",
		"--limit", "20",
		"--json", "databaseId,headSha,name,displayTitle,workflowName",
	)
	listCmd := h.cmd(ctx, "gh", args...)
	listOut, err := listCmd.Output()
	if err != nil {
		return "", nil
	}
	var runs []githubRun
	if err := json.Unmarshal(listOut, &runs); err != nil {
		return "", nil
	}
	for _, run := range runs {
		if !runMatchesTargets(ctx, h, run, targets) {
			continue
		}
		viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
		viewArgs = append(viewArgs, "--log-failed")
		viewCmd := h.cmd(ctx, "gh", viewArgs...)
		out, err := viewCmd.Output()
		if err != nil {
			continue
		}
		logs := strings.TrimSpace(string(out))
		if logs != "" {
			return logs, nil
		}
	}
	return "", nil
}

type githubRun struct {
	DatabaseID   int    `json:"databaseId"`
	HeadSHA      string `json:"headSha"`
	Name         string `json:"name"`
	DisplayTitle string `json:"displayTitle"`
	WorkflowName string `json:"workflowName"`
}

type githubRunView struct {
	Jobs []githubRunJob `json:"jobs"`
}

type githubRunJobsResponse struct {
	TotalCount int            `json:"total_count"`
	Jobs       []githubRunJob `json:"jobs"`
}

type githubRunArtifactsResponse struct {
	TotalCount int `json:"total_count"`
	Artifacts  []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Digest      string `json:"digest"`
		Expired     bool   `json:"expired"`
		WorkflowRun struct {
			ID      int64  `json:"id"`
			HeadSHA string `json:"head_sha"`
		} `json:"workflow_run"`
	} `json:"artifacts"`
}

type githubWorkflowRun struct {
	ID           int    `json:"id"`
	Event        string `json:"event"`
	HeadSHA      string `json:"head_sha"`
	RunAttempt   int    `json:"run_attempt"`
	PullRequests []struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
	} `json:"pull_requests"`
}

type githubRunJob struct {
	DatabaseID int             `json:"databaseId"`
	ID         int             `json:"id"`
	HeadSHA    string          `json:"head_sha"`
	Name       string          `json:"name"`
	Conclusion string          `json:"conclusion"`
	Status     string          `json:"status"`
	Steps      []githubJobStep `json:"steps"`
}

func (j githubRunJob) databaseID() int {
	if j.DatabaseID != 0 {
		return j.DatabaseID
	}
	return j.ID
}

type githubJobStep struct {
	Name       string `json:"name"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func runMatchesTargets(ctx context.Context, h *Host, run githubRun, targets map[string]struct{}) bool {
	for _, candidate := range []string{run.Name, run.DisplayTitle, run.WorkflowName} {
		if _, ok := targets[normalizeRunName(candidate)]; ok {
			return true
		}
	}
	if run.DatabaseID == 0 {
		return false
	}
	viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
	viewArgs = append(viewArgs, "--json", "jobs")
	viewCmd := h.cmd(ctx, "gh", viewArgs...)
	out, err := viewCmd.Output()
	if err != nil {
		return false
	}
	var payload githubRunView
	if err := json.Unmarshal(out, &payload); err != nil {
		return false
	}
	for _, job := range payload.Jobs {
		if !isFailedJob(job) {
			continue
		}
		if _, ok := targets[normalizeRunName(job.Name)]; ok {
			return true
		}
	}
	return false
}

func isFailedJob(job githubRunJob) bool {
	state := strings.ToUpper(strings.TrimSpace(job.Conclusion))
	if state == "" {
		state = strings.ToUpper(strings.TrimSpace(job.Status))
	}
	switch state {
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return true
	default:
		return false
	}
}

func normalizeRunName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OPEN":
		return scm.PRStateOpen
	case "MERGED":
		return scm.PRStateMerged
	case "CLOSED":
		return scm.PRStateClosed
	default:
		return scm.PRState(raw)
	}
}

func normalizeMergeableState(raw string) scm.MergeableState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MERGEABLE":
		return scm.MergeableOK
	case "CONFLICTING":
		return scm.MergeableConflict
	case "UNKNOWN", "":
		return scm.MergeablePending
	default:
		return scm.MergeableState(raw)
	}
}

func normalizeCheckBucket(bucket, state string) scm.CheckBucket {
	if normalized := scm.CheckBucket(strings.TrimSpace(bucket)); normalized != "" {
		return normalized
	}

	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return scm.CheckBucketPass
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return scm.CheckBucketFail
	case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "EXPECTED":
		return scm.CheckBucketPending
	case "CANCELLED":
		return scm.CheckBucketCancel
	case "SKIPPED", "NEUTRAL", "STALE":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}

// GetReviewComments implements scm.ReviewCommentsHost.
func (h *Host) GetReviewComments(ctx context.Context, pr *scm.PR) ([]scm.ReviewComment, error) {
	if pr == nil {
		return nil, errors.New("pr is nil")
	}
	repo := h.repoSlug()
	if repo == "" && pr.URL != "" {
		repo = RepoSlug(pr.URL)
	}
	if repo == "" {
		return nil, errors.New("cannot determine repository for PR review comments")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("resolve GitHub repository for PR review comments: invalid repository %q", repo)
	}
	prNum := strings.TrimSpace(pr.Number)
	if prNum == "" {
		number, parseErr := parsePullRequestURL(pr.URL, h.host, repo)
		if parseErr != nil {
			return nil, parseErr
		}
		prNum = strconv.Itoa(number)
	}
	number, err := strconv.Atoi(prNum)
	if err != nil || number <= 0 {
		return nil, errors.New("expected positive GitHub pull request number")
	}

	var comments []scm.ReviewComment
	cursor := ""
	for {
		args := []string{"api"}
		if h.host != "" {
			args = append(args, "--hostname", h.host)
		}
		args = append(args, "graphql", "-f", "query="+reviewThreadsQuery,
			"-F", "owner="+parts[0], "-F", "name="+parts[1], "-F", "number="+strconv.Itoa(number))
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		out, commandErr := h.cmd(ctx, "gh", args...).CombinedOutput()
		if commandErr != nil {
			return nil, fmt.Errorf("gh api PR review comments: %s: %w", strings.TrimSpace(string(out)), commandErr)
		}
		var response struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads struct {
							Nodes []struct {
								IsResolved bool `json:"isResolved"`
								Comments   struct {
									Nodes []struct {
										ID        int64     `json:"databaseId"`
										Body      string    `json:"body"`
										Path      string    `json:"path"`
										Line      *int      `json:"line"`
										URL       string    `json:"url"`
										CreatedAt time.Time `json:"createdAt"`
										Author    *struct {
											Login string `json:"login"`
										} `json:"author"`
									} `json:"nodes"`
								} `json:"comments"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("decode PR review comments JSON: %w", err)
		}
		if len(response.Errors) > 0 {
			return nil, fmt.Errorf("gh api PR review comments: %s", response.Errors[0].Message)
		}
		if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
			return nil, errors.New("PR review comments response did not contain the pull request")
		}
		threads := response.Data.Repository.PullRequest.ReviewThreads
		for _, thread := range threads.Nodes {
			if thread.IsResolved {
				continue
			}
			for _, raw := range thread.Comments.Nodes {
				if raw.Author == nil || !isSupportedReviewBot(raw.Author.Login) {
					continue
				}
				line := 0
				if raw.Line != nil {
					line = *raw.Line
				}
				comments = append(comments, scm.ReviewComment{
					ID:        strconv.FormatInt(raw.ID, 10),
					Author:    raw.Author.Login,
					Path:      raw.Path,
					Line:      line,
					Body:      raw.Body,
					CreatedAt: raw.CreatedAt,
					URL:       raw.URL,
				})
			}
		}
		if !threads.PageInfo.HasNextPage {
			break
		}
		if threads.PageInfo.EndCursor == "" || threads.PageInfo.EndCursor == cursor {
			return nil, errors.New("PR review comments response returned an invalid page cursor")
		}
		cursor = threads.PageInfo.EndCursor
	}
	return comments, nil
}

func isSupportedReviewBot(login string) bool {
	switch strings.ToLower(strings.TrimSpace(login)) {
	case "greptile-apps[bot]", "greptile-apps":
		return true
	default:
		return false
	}
}
