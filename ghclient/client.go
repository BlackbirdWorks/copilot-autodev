// Package ghclient wraps go-github with the operations needed by the poller.
package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/pkgs/logger"
)

const (
	// RefinementCommentMarker is an invisible HTML comment embedded in every
	// refinement prompt.  The orchestrator counts PR reviews containing this
	// marker to determine many refinement rounds have already been sent,
	// which means the count survives process restarts.
	RefinementCommentMarker = "<!-- copilot-autodev:refinement -->"

	// MergeConflictCommentMarker is an invisible HTML comment embedded in
	// every merge-conflict @copilot prompt.  Counting these comments on a PR
	// tells the orchestrator how many @copilot attempts have been made so far,
	// which means the count survives process restarts.
	MergeConflictCommentMarker = "<!-- copilot-autodev:merge-conflict -->"

	// CopilotNudgeCommentMarker is an invisible HTML comment embedded in every
	// nudge comment posted when the Copilot coding agent has not started
	// within the configured timeout.  Counting these comments tells the
	// orchestrator how many re-trigger attempts have been made for the current
	// coding cycle so that it can enforce CopilotInvokeMaxRetries.
	CopilotNudgeCommentMarker = "<!-- copilot-autodev:nudge -->"

	// AgentContinueCommentMarker is an invisible HTML comment embedded in
	// every "@copilot continue" comment posted when the agent's workflow run
	// times out during coding or refinement.  Counting these comments tells
	// the orchestrator how many continue attempts have been made so far.
	AgentContinueCommentMarker = "<!-- copilot-autodev:agent-continue -->"

	// MergeConflictContinueCommentMarker is an invisible HTML comment embedded
	// in every "@copilot continue" nudge posted while the agent is stuck
	// resolving merge conflicts.  Keeping this separate from
	// AgentContinueCommentMarker gives the merge-conflict phase its own retry
	// budget so it cannot starve the refinement+CI feedback loop.
	MergeConflictContinueCommentMarker = "<!-- copilot-autodev:merge-conflict-continue -->"

	// LocalResolutionCommentMarker is embedded in the notice posted after a
	// local AI merge resolution attempt (both success and failure).
	LocalResolutionCommentMarker = "<!-- copilot-autodev:local-resolution -->"

	// LocalResolutionFailedMarker is embedded only in failure notices.  The
	// orchestrator checks for this marker to avoid retrying after a failure,
	// while still allowing re-runs if a previous resolution succeeded but new
	// conflicts arise later.
	LocalResolutionFailedMarker = "<!-- copilot-autodev:local-resolution-failed -->"

	// PRLinkCommentMarker is embedded in an issue comment to explicitly link it to a PR.
	PRLinkCommentMarker = "<!-- copilot-autodev:pr-link:"

	// IssueLinkCommentMarker is embedded in a PR comment to explicitly link it to an Issue.
	IssueLinkCommentMarker = "<!-- copilot-autodev:issue-link:"

	// CopilotJobIDCommentMarker is embedded in the tracking comment posted
	// after invoking the Copilot API.  It records the job/task ID returned
	// by the API so the orchestrator can avoid duplicate invocations.
	// Format: <!-- copilot-autodev:job-id:UUID -->.
	CopilotJobIDCommentMarker = "<!-- copilot-autodev:job-id:"

	// CIFixCommentMarker is an invisible HTML comment embedded in every
	// CI-fix-only prompt posted after refinement rounds are exhausted but CI
	// is still failing.  Counting these comments tells the orchestrator how
	// many CI-fix attempts have been made.
	CIFixCommentMarker = "<!-- copilot-autodev:ci-fix -->"

	// DeploymentPendingCommentMarker is embedded in the one-time notice
	// posted when a workflow run has conclusion=action_required (environment
	// deployment gate) and the orchestrator's token cannot approve it.
	// Combined with SHAMarker("deployment-pending", sha) for per-SHA dedup.
	DeploymentPendingCommentMarker = "<!-- copilot-autodev:deployment-pending -->"
)

// FailedJobInfo describes a single failed CI job.
type FailedJobInfo struct {
	Name   string // display name of the job
	LogURL string // URL to the raw logs (may be empty if unavailable)
}

// copilotAPIBase is the base URL of the GitHub Copilot API used to create
// agent tasks directly (the same backend that powers the Agents tab).
const copilotAPIBase = "https://api.githubcopilot.com"

// copilotAPIVersion is the API version header sent to the Copilot API.
const copilotAPIVersion = "2026-01-09"

// Pagination page sizes and other numeric constants used throughout the client.
const (
	prStateOpen              = "open"      // GitHub API state value for open issues and PRs
	runStatusCompleted       = "completed" // GitHub Actions workflow run status
	actionRequiredConclusion = "action_required"
	perPageDefault           = 100 // default page size for most paginated list calls
	perPageMedium            = 50  // page size for PR and commit list calls
	perPageSmall             = 20  // page size for workflow run calls
	perPageMin               = 10  // page size for small-result-set calls
	maxLogRedirects          = 10  // maximum HTTP redirects when fetching job log URLs
	housPerDay               = 24  // hours in a day, for relative timestamp formatting
	shortSHALen              = 7   // number of chars used for a shortened commit SHA
)

// Client wraps the GitHub SDK client with the settings from Config.
type workflowRunCacheEntry struct {
	runs      []*github.WorkflowRun
	fetchedAt time.Time
}

type Client struct {
	gh               *github.Client
	owner            string
	repo             string
	labelQueue       string
	labelCoding      string
	labelReview      string
	labelTakeover    string
	mergeMethod      string
	mergeMsg         string
	token            string // PAT used for Copilot API calls
	transport        http.RoundTripper
	workflowRunCache sync.Map // sha → workflowRunCacheEntry
}

// New creates a new Client authenticated with the provided PAT token.
func New(token string, cfg *config.Config) *Client {
	return NewWithTransport(token, cfg, nil)
}

// NewWithTransport creates a new Client with a custom [http.RoundTripper].
func NewWithTransport(token string, cfg *config.Config, transport http.RoundTripper) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	ctx := context.Background()
	if transport != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: transport})
	}
	tc := oauth2.NewClient(ctx, ts)
	return &Client{
		gh:            github.NewClient(tc),
		owner:         cfg.GitHubOwner,
		repo:          cfg.GitHubRepo,
		labelQueue:    cfg.LabelQueue,
		labelCoding:   cfg.LabelCoding,
		labelReview:   cfg.LabelReview,
		labelTakeover: cfg.LabelTakeover,
		mergeMethod:   cfg.MergeMethod,
		mergeMsg:      cfg.MergeCommitMessage,
		token:         token,
		transport:     transport,
	}
}

// NewTestClient creates a minimal Client suitable for unit tests that need to
// call methods directly (e.g. InvokeAgentAt) without a full config.
func NewTestClient(owner, repo, token string) *Client {
	return &Client{
		gh:    github.NewClient(nil),
		owner: owner,
		repo:  repo,
		token: token,
	}
}

// NewTestClientWithGH creates a Client backed by the provided *github.Client,
// used to point at an httptest server in unit tests.
func NewTestClientWithGH(gh *github.Client, owner, repo string) *Client {
	return &Client{gh: gh, owner: owner, repo: repo}
}

func (c *Client) httpClient() *http.Client {
	if c.transport != nil {
		return &http.Client{Transport: c.transport}
	}
	return http.DefaultClient
}

// IssuesByLabel returns all open issues carrying the given label.
func (c *Client) IssuesByLabel(ctx context.Context, label string) ([]*github.Issue, error) {
	var all []*github.Issue
	opts := &github.IssueListByRepoOptions{
		State:       prStateOpen,
		Labels:      []string{label},
		ListOptions: github.ListOptions{PerPage: perPageDefault},
	}
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, c.owner, c.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list issues (label=%s): %w", label, err)
		}
		// Filter out pull requests (GitHub API returns PRs in issue list too).
		for _, i := range issues {
			if i.PullRequestLinks != nil {
				continue
			}
			// Ignore any issue carrying the manual takeover label.
			takenOver := false
			for _, l := range i.Labels {
				if strings.EqualFold(l.GetName(), c.labelTakeover) {
					takenOver = true
					break
				}
			}
			if takenOver {
				continue
			}
			all = append(all, i)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// AddLabel adds a label to the given issue number.
func (c *Client) AddLabel(ctx context.Context, issueNum int, label string) error {
	_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, c.owner, c.repo, issueNum, []string{label})
	return err
}

// RemoveLabel removes a label from the given issue number (ignores 404).
func (c *Client) RemoveLabel(ctx context.Context, issueNum int, label string) error {
	_, err := c.gh.Issues.RemoveLabelForIssue(ctx, c.owner, c.repo, issueNum, label)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil
		}
		return err
	}
	return nil
}

// SwapLabel atomically transitions an issue from one label to another by
// adding the new label first, then removing the old one.  This ordering
// ensures the issue is never label-less: if AddLabel fails the issue keeps
// the old label (safe); if RemoveLabel fails after AddLabel the issue has
// both labels (recoverable on next tick, and IssuesByLabel de-duplicates
// by issue number).
func (c *Client) SwapLabel(ctx context.Context, issueNum int, oldLabel, newLabel string) error {
	if err := c.AddLabel(ctx, issueNum, newLabel); err != nil {
		return fmt.Errorf("swap label: add %q: %w", newLabel, err)
	}
	if err := c.RemoveLabel(ctx, issueNum, oldLabel); err != nil {
		logger.Load(ctx).ErrorContext(ctx, "swap label: added new but failed to remove old",
			slog.String("added", newLabel), slog.String("removed", oldLabel),
			slog.Int("issue", issueNum), slog.Any("err", err))
	}
	return nil
}

// CloseIssue closes the issue.
func (c *Client) CloseIssue(ctx context.Context, issueNum int) error {
	closed := "closed"
	_, _, err := c.gh.Issues.Edit(ctx, c.owner, c.repo, issueNum, &github.IssueRequest{State: &closed})
	return err
}

// IsMatchForIssue returns true if the PR's title, body, or head branch reference
// the given issue number.
func (c *Client) IsMatchForIssue(pr *github.PullRequest, issueNum int) bool {
	bodyRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|\s)(?:fixes|closes|resolves)\s+#%d\b`, issueNum))
	titleRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|\s)#%d\b`, issueNum))
	branchRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|/)(?:issue-?)?%d(?:[-/]|$)`, issueNum))

	if bodyRe.MatchString(pr.GetBody()) || titleRe.MatchString(pr.GetTitle()) {
		return true
	}
	// Copilot Workspace might not link the issue in the body/title yet, but
	// often names branches `copilot/issue-number-description` or similar.
	branch := pr.GetHead().GetRef()
	return branchRe.MatchString(branch)
}

// FindLinkedPRFromComments scans an issue's comments for the PRLinkCommentMarker.
// If found, it parses the PR number and returns it.
func (c *Client) FindLinkedPRFromComments(ctx context.Context, issueNum int) (int, error) {
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return 0, err
		}
		for _, cm := range comments {
			body := cm.GetBody()
			if idx := strings.Index(body, PRLinkCommentMarker); idx != -1 {
				// Parse the number. Format: <!-- copilot-autodev:pr-link:123 -->
				start := idx + len(PRLinkCommentMarker)
				end := strings.Index(body[start:], "-->")
				if end != -1 {
					numStr := strings.TrimSpace(body[start : start+end])
					var prNum int
					if _, err := fmt.Sscanf(numStr, "%d", &prNum); err == nil {
						return prNum, nil
					}
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return 0, nil
}

// OpenPRForIssue finds the first open PR whose title or body references the issue.
// It uses several discovery steps in order of reliability.
func (c *Client) OpenPRForIssue(ctx context.Context, issue *github.Issue) (*github.PullRequest, error) {
	issueNum := issue.GetNumber()

	// 1. Check for explicit comment links on the ISSUE side first.
	if pr, err := c.findPRByIssueComment(ctx, issueNum); err == nil && pr != nil {
		return pr, nil
	}

	// 2. Proactive discovery via Copilot Job ID.
	if pr := c.DiscoverPRViaJobID(ctx, issueNum); pr != nil {
		return pr, nil
	}

	// 3. Search for the explicit link marker on the PR side.
	if pr := c.FindPRByMarkerComment(ctx, issueNum); pr != nil {
		return pr, nil
	}

	// 4. Native GitHub Search by issue number (body/title/comments).
	// Initial discovery (text-matching heuristics).

	if pr := c.FindPRBySearch(ctx, issueNum); pr != nil {
		return pr, nil
	}

	// 5. Fallback: list recent PRs and look for issue number.
	if pr := c.FindPRByListing(ctx, issueNum); pr != nil {
		return pr, nil
	}

	// 6. Issue timeline: scan for cross-reference events from a PR.
	if pr, err := c.FindPRFromTimeline(ctx, issueNum); err == nil && pr != nil {
		c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
		return pr, nil
	} else if err != nil {
		logger.Load(ctx).ErrorContext(ctx, "issue step 6 error", slog.Int("issue", issueNum), slog.Any("err", err))
	}

	logger.Load(ctx).InfoContext(ctx, "OpenPRForIssue: no PR found after all 6 detection steps",
		slog.Int("issue", issueNum))
	return nil, nil
}

func (c *Client) findPRByIssueComment(ctx context.Context, issueNum int) (*github.PullRequest, error) {
	linkedPRNum, err := c.FindLinkedPRFromComments(ctx, issueNum)
	if err != nil {
		logger.Load(ctx).ErrorContext(ctx, "issue step 1 error", slog.Int("issue", issueNum), slog.Any("err", err))
		return nil, err
	}
	if linkedPRNum == 0 {
		return nil, nil
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, linkedPRNum)
	if err == nil && pr.GetState() == prStateOpen {
		c.ensureTwoWayLink(ctx, issueNum, linkedPRNum)
		return pr, nil
	}
	logger.Load(ctx).InfoContext(ctx, "issue step 1 found closed PR",
		slog.Int("issue", issueNum), slog.Int("pr", linkedPRNum), slog.Any("err", err))
	return nil, nil
}

func (c *Client) FindPRByMarkerComment(ctx context.Context, issueNum int) *github.PullRequest {
	markerText := fmt.Sprintf("copilot-autodev:issue-link:%d", issueNum)
	markerQuery := fmt.Sprintf("repo:%s/%s is:pr is:open %s", c.owner, c.repo, markerText)
	markerResult, _, err := c.gh.Search.Issues(ctx, markerQuery, nil)
	if err != nil {
		logger.Load(ctx).ErrorContext(ctx, "issue step 3 error", slog.Int("issue", issueNum), slog.Any("err", err))
		return nil
	}
	if len(markerResult.Issues) == 0 {
		return nil
	}
	sr := markerResult.Issues[0]
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, sr.GetNumber())
	if err == nil && pr.GetState() == prStateOpen {
		c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
		return pr
	}
	return nil
}

func (c *Client) FindPRBySearch(ctx context.Context, issueNum int) *github.PullRequest {
	query := fmt.Sprintf("repo:%s/%s is:pr is:open %d", c.owner, c.repo, issueNum)
	result, _, err := c.gh.Search.Issues(ctx, query, nil)
	if err != nil {
		logger.Load(ctx).ErrorContext(ctx, "issue step 4 error", slog.Int("issue", issueNum), slog.Any("err", err))
		return nil
	}
	if pr := c.FindMatchInSearchResults(ctx, issueNum, result.Issues); pr != nil {
		return pr
	}
	logger.Load(ctx).InfoContext(ctx, "issue step 4 no matches",
		slog.Int("issue", issueNum), slog.Int("results", len(result.Issues)))
	return nil
}

func (c *Client) FindPRByListing(ctx context.Context, issueNum int) *github.PullRequest {
	prs, _, err := c.gh.PullRequests.List(ctx, c.owner, c.repo, &github.PullRequestListOptions{
		State:       prStateOpen,
		ListOptions: github.ListOptions{PerPage: perPageMedium},
	})
	if err != nil {
		logger.Load(ctx).ErrorContext(ctx, "issue step 5 error", slog.Int("issue", issueNum), slog.Any("err", err))
		return nil
	}
	for _, pr := range prs {
		if c.IsMatchForIssue(pr, issueNum) {
			c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
			return pr
		}
	}
	logger.Load(ctx).InfoContext(ctx, "issue step 5 listed prs, none matched",
		slog.Int("issue", issueNum), slog.Int("count", len(prs)))
	return nil
}

func (c *Client) DiscoverPRViaJobID(ctx context.Context, issueNum int) *github.PullRequest {
	jobID, _ := c.LatestCopilotJobID(ctx, issueNum)
	if jobID == "" {
		return nil
	}
	status, err := c.GetCopilotJobStatus(ctx, jobID)
	if err != nil || status == nil || status.PullRequest == nil {
		return nil
	}
	prNum := status.PullRequest.Number
	if prNum == 0 {
		return nil
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, prNum)
	if err == nil && pr.GetState() == prStateOpen {
		logger.Load(ctx).InfoContext(ctx, "proactive discovery via Job ID found PR",
			slog.Int("issue", issueNum), slog.String("job", jobID), slog.Int("pr", prNum))
		c.ensureTwoWayLink(ctx, issueNum, prNum)
		return pr
	}
	return nil
}

// FindPRFromTimeline scans the issue's timeline for cross-reference events
// originating from an open PR. This works immediately when a PR body says
// "Fixes #N", without waiting for search indexing or relying on text matching.
func (c *Client) FindPRFromTimeline(ctx context.Context, issueNum int) (*github.PullRequest, error) {
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		events, resp, err := c.gh.Issues.ListIssueTimeline(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return nil, err
		}
		for _, evt := range events {
			if evt.GetEvent() != "cross-referenced" {
				continue
			}
			src := evt.Source
			// GitHub always returns type:"issue" for cross-reference sources,
			// even when the source is a pull request.  Check PullRequestLinks
			// to distinguish PRs from plain issues.
			if src == nil || src.Issue == nil || src.Issue.PullRequestLinks == nil {
				continue
			}
			prNum := src.Issue.GetNumber()
			if prNum == 0 {
				continue
			}
			pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, prNum)
			if err != nil || pr.GetState() != prStateOpen {
				continue
			}
			return pr, nil
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil, nil
}

// FindMatchInSearchResults iterates GitHub search results and returns the first
// open PR that matches the issue by body/title/branch regex.
func (c *Client) FindMatchInSearchResults(
	ctx context.Context, issueNum int, issues []*github.Issue,
) *github.PullRequest {
	for _, sr := range issues {
		if sr.PullRequestLinks == nil || sr.GetNumber() == issueNum {
			continue
		}
		pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, sr.GetNumber())
		if err != nil {
			continue
		}
		if pr.GetState() == prStateOpen && c.IsMatchForIssue(pr, issueNum) {
			c.ensureTwoWayLink(ctx, issueNum, pr.GetNumber())
			return pr
		}
	}
	return nil
}

// ensureTwoWayLink posts cross-linking comments to the Issue and PR if they
// don't already exist, making it safe to call on every poll iteration.
func (c *Client) ensureTwoWayLink(ctx context.Context, issueNum, prNum int) {
	// Issue side: "Tracking PR #N"
	issueMarker := fmt.Sprintf("%s%d -->", PRLinkCommentMarker, prNum)
	if ok, _, _ := c.HasCommentContaining(ctx, issueNum, issueMarker); !ok {
		body := fmt.Sprintf(
			"copilot-autodev: Tracking PR #%d for this issue.\n%s%d -->",
			prNum, PRLinkCommentMarker, prNum,
		)
		_ = c.PostComment(ctx, issueNum, body)
	}

	// PR side: "addressing Issue #N"
	prMarker := fmt.Sprintf("%s%d -->", IssueLinkCommentMarker, issueNum)
	if ok, _, _ := c.HasCommentContaining(ctx, prNum, prMarker); !ok {
		body := fmt.Sprintf(
			"copilot-autodev: This PR is addressing Issue #%d.\n%s%d -->",
			issueNum, IssueLinkCommentMarker, issueNum,
		)
		_ = c.PostComment(ctx, prNum, body)
	}
}

// IsPRDraft returns true when the PR is still in draft state.
func (c *Client) IsPRDraft(pr *github.PullRequest) bool {
	return pr.GetDraft()
}

// IsBranchBehindBase returns true when the PR branch is behind the base branch.
// It uses the base branch ref name (e.g. "main") rather than the SHA recorded
// on the PR object, which can be stale if the base branch has moved since the
// PR was last synced.
func (c *Client) IsBranchBehindBase(ctx context.Context, pr *github.PullRequest) (bool, error) {
	comp, _, err := c.gh.Repositories.CompareCommits(ctx, c.owner, c.repo,
		pr.GetBase().GetRef(), pr.GetHead().GetSHA(), nil)
	if err != nil {
		return false, err
	}
	status := comp.GetStatus()
	return status == "behind" || status == "diverged", nil
}

// PostComment posts a plain comment on the given issue/PR number.
func (c *Client) PostComment(ctx context.Context, issueNum int, body string) error {
	_, _, err := c.gh.Issues.CreateComment(ctx, c.owner, c.repo, issueNum, &github.IssueComment{Body: &body})
	return err
}

// PostReviewComment posts a review comment on a PR.
func (c *Client) PostReviewComment(ctx context.Context, prNum int, body string) error {
	event := "COMMENT"
	_, _, err := c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, prNum, &github.PullRequestReviewRequest{
		Body:  &body,
		Event: &event,
	})
	return err
}

// CountRefinementPromptsSent returns the number of refinement prompts already
// posted on the given PR by counting reviews containing RefinementCommentMarker.
// Reading the count from GitHub means it survives process restarts.
func (c *Client) CountRefinementPromptsSent(ctx context.Context, prNum int) (int, error) {
	count := 0
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, c.owner, c.repo, prNum, opts)
		if err != nil {
			return 0, err
		}
		for _, r := range reviews {
			if strings.Contains(r.GetBody(), RefinementCommentMarker) {
				count++
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// CountMergeConflictAttempts returns the number of merge-conflict @copilot
// prompts the orchestrator has already posted on the given PR by counting
// issue comments whose body contains MergeConflictCommentMarker.  Reading
// the count from GitHub means it survives process restarts.
func (c *Client) CountMergeConflictAttempts(ctx context.Context, prNum int) (int, error) {
	count := 0
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, prNum, opts)
		if err != nil {
			return 0, err
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), MergeConflictCommentMarker) {
				count++
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// ApprovePR approves the PR.
func (c *Client) ApprovePR(ctx context.Context, prNum int) error {
	event := "APPROVE"
	_, _, err := c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, prNum, &github.PullRequestReviewRequest{
		Event: &event,
	})
	return err
}

// MergePR merges the PR using the method and commit message from Config.
func (c *Client) MergePR(ctx context.Context, pr *github.PullRequest) error {
	_, _, err := c.gh.PullRequests.Merge(ctx, c.owner, c.repo, pr.GetNumber(),
		c.mergeMsg,
		&github.PullRequestOptions{MergeMethod: c.mergeMethod})
	return err
}

// GetPRHeadSHA fetches a PR by number and returns its head commit SHA.
func (c *Client) GetPRHeadSHA(ctx context.Context, prNum int) (string, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, prNum)
	if err != nil {
		return "", fmt.Errorf("get PR #%d: %w", prNum, err)
	}
	return pr.GetHead().GetSHA(), nil
}

// UpdatePRBranch updates the PR branch with latest changes from its base branch
// using the GitHub native "Update branch" API.
func (c *Client) UpdatePRBranch(ctx context.Context, prNum int) error {
	_, resp, err := c.gh.PullRequests.UpdateBranch(ctx, c.owner, c.repo, prNum, nil)
	if err != nil {
		var acceptedErr *github.AcceptedError
		if errors.As(err, &acceptedErr) {
			logger.Load(ctx).InfoContext(ctx, "UpdatePRBranch: update scheduled (202 Accepted)", slog.Int("pr", prNum))
			return nil
		}
		logger.Load(ctx).ErrorContext(ctx, "UpdatePRBranch: API error", slog.Int("pr", prNum), slog.Any("err", err))
		return err
	}
	if resp != nil && resp.StatusCode == http.StatusAccepted {
		logger.Load(ctx).InfoContext(ctx, "UpdatePRBranch: update scheduled (202 Accepted)", slog.Int("pr", prNum))
		return nil
	}
	logger.Load(ctx).InfoContext(ctx, "UpdatePRBranch: update requested successfully",
		slog.Int("pr", prNum), slog.String("status", resp.Status))
	return nil
}

// LatestWorkflowRun returns all recent workflow runs for the given commit SHA.
// Results are cached for 30 s to avoid redundant API calls within a single tick.
func (c *Client) LatestWorkflowRun(ctx context.Context, sha string) ([]*github.WorkflowRun, error) {
	const cacheTTL = 30 * time.Second
	if v, ok := c.workflowRunCache.Load(sha); ok {
		if e, ok := v.(workflowRunCacheEntry); ok && time.Since(e.fetchedAt) < cacheTTL {
			return e.runs, nil
		}
	}
	logger.Load(ctx).InfoContext(ctx, "Listing workflow runs", slog.String("sha", sha))
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, &github.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: github.ListOptions{PerPage: perPageDefault},
	})
	if err != nil {
		return nil, err
	}
	logger.Load(ctx).InfoContext(ctx, "Found workflow runs",
		slog.String("sha", sha), slog.Int("count", len(runs.WorkflowRuns)))
	for _, r := range runs.WorkflowRuns {
		logger.Load(ctx).InfoContext(ctx, "Workflow run details",
			slog.Int64("id", r.GetID()),
			slog.String("name", r.GetName()),
			slog.String("status", r.GetStatus()),
			slog.String("conclusion", r.GetConclusion()),
		)
	}
	c.workflowRunCache.Store(sha, workflowRunCacheEntry{
		runs:      runs.WorkflowRuns,
		fetchedAt: time.Now(),
	})
	return runs.WorkflowRuns, nil
}

// FailedRunDetails finds the first failed workflow run for a commit SHA and
// returns its display name together with the name and log URL of every failed
// job inside that run.  Returns ("", nil, nil) when no failed run is found.
//
// FailedRunDetails returns the name of the failed workflow and a list of failed
// jobs (with optional log URLs) for the given commit SHA.
//
// This replaces the old FindFailedRunID + FailedRunLogs pair so callers get
// the workflow title in one call, which is included in the @copilot message so
// Copilot knows exactly where to look.
func (c *Client) FailedRunDetails(
	ctx context.Context, sha string,
) (string, []FailedJobInfo, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return "", nil, err
	}

	var workflowName string
	var runID int64
	for _, r := range runs {
		if r.GetConclusion() == "failure" {
			workflowName = r.GetName()
			runID = r.GetID()
			break
		}
	}
	if runID == 0 {
		return "", nil, nil
	}

	jobsResp, _, err := c.gh.Actions.ListWorkflowJobs(ctx, c.owner, c.repo, runID,
		&github.ListWorkflowJobsOptions{
			Filter:      "latest",
			ListOptions: github.ListOptions{PerPage: perPageMedium},
		})
	if err != nil {
		// Return the workflow name even if job details fail.
		return workflowName, nil, err
	}
	var jobs []FailedJobInfo
	for _, job := range jobsResp.Jobs {
		if job.GetConclusion() != "failure" {
			continue
		}
		logURL := ""
		u, _, lerr := c.gh.Actions.GetWorkflowJobLogs(
			ctx, c.owner, c.repo, job.GetID(), maxLogRedirects,
		)
		if lerr == nil {
			logURL = u.String()
		}
		jobs = append(jobs, FailedJobInfo{Name: job.GetName(), LogURL: logURL})
	}
	return workflowName, jobs, nil
}

// AnyWorkflowRunActive returns true when any workflow run on the given SHA is
// still in progress or queued.  This is used as a proxy for "is the Copilot
// agent or CI still working" — the Copilot coding agent does not have a
// dedicated status API, so checking for active workflow runs (triggered by the
// agent's pushes) is the best available signal.
func (c *Client) AnyWorkflowRunActive(ctx context.Context, sha string) (bool, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		s := r.GetStatus()
		if s == "in_progress" || s == "queued" || s == "requested" {
			return true, nil
		}
	}
	return false, nil
}

// HasActiveCopilotRun checks whether there are any in-progress or queued
// workflow runs in the repository that appear to be from the Copilot coding
// agent.  It matches runs whose triggering actor login contains "copilot"
// (case-insensitive) or whose workflow name contains "copilot".
//
// This is used as a guard before re-invoking the agent: if a Copilot run
// is already active, we skip re-invocation to avoid duplicate tasks.
func (c *Client) HasActiveCopilotRun(ctx context.Context) (bool, error) {
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, &github.ListWorkflowRunsOptions{
		ListOptions: github.ListOptions{PerPage: perPageSmall},
	})
	if err != nil {
		return false, err
	}
	for _, r := range runs.WorkflowRuns {
		status := r.GetStatus()
		if status != "in_progress" && status != "queued" && status != "requested" {
			continue
		}
		actor := strings.ToLower(r.GetActor().GetLogin())
		name := strings.ToLower(r.GetName())
		if strings.Contains(actor, "copilot") || strings.Contains(name, "copilot") {
			return true, nil
		}
	}
	return false, nil
}

// AllRunsSucceeded returns (allSuccess bool, anyFailure bool, err).
func (c *Client) AllRunsSucceeded(ctx context.Context, sha string) (bool, bool, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return false, false, err
	}
	if len(runs) == 0 {
		return true, false, nil // No runs -> no failures -> inherently successful
	}
	allSuccess := true
	anyFailure := false
	for _, r := range runs {
		status := r.GetStatus()
		conclusion := r.GetConclusion()

		// If any run finished with a failure, we flag it immediately
		// so the agent can start fixing it while others are still running.
		// "action_required" is excluded from anyFailure because it requires
		// human action (deployment gate, GitHub App check), not something
		// Copilot can fix — including it would generate CI-fix prompts that
		// cause an infinite refinement loop.  However, it still blocks
		// allSuccess so the PR won't be merged while it's pending.
		if status == runStatusCompleted &&
			(conclusion != "success" && conclusion != "skipped" &&
				conclusion != "neutral" && conclusion != actionRequiredConclusion) {
			anyFailure = true
			allSuccess = false
		}

		// action_required blocks merge but doesn't count as a CI failure
		// the agent can fix.  Steps 2/2.5 in processOne handle it by
		// re-running the workflow or posting a notice; this is defense-in-depth.
		if status == runStatusCompleted && conclusion == actionRequiredConclusion {
			allSuccess = false
		}

		if status != runStatusCompleted {
			allSuccess = false
		}
	}
	return allSuccess, anyFailure, nil
}

// LatestFailedRunConclusion returns the conclusion string (e.g. "failure",
// "timed_out") and completion time of the most recent completed-but-unsuccessful
// workflow run for the given SHA.  Returns ("", zero, nil) when every run
// succeeded/was skipped, or no runs exist at all.
func (c *Client) LatestFailedRunConclusion(ctx context.Context, sha string) (string, time.Time, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return "", time.Time{}, err
	}
	for _, r := range runs {
		if r.GetStatus() != runStatusCompleted {
			continue
		}
		conc := r.GetConclusion()
		// Only treat "timed_out" as an actionable agent failure.
		// "failure" is typically a normal CI test failure which should
		// be handled by the CI-fix feedback loop instead.
		// Other non-success conclusions (cancelled, neutral, stale) are
		// either user-initiated or transient and should not trigger the
		// "@copilot continue" recovery flow.
		if conc == "timed_out" {
			completedAt := r.GetUpdatedAt().Time
			return conc, completedAt, nil
		}
	}
	return "", time.Time{}, nil
}

// ListActionRequiredRuns returns all workflow runs for the given SHA that are
// pending manual approval via the GitHub fork-PR approval mechanism.
// This covers two pending statuses that can be unblocked by calling
// ApproveWorkflowRun:
//   - status="action_required": first-run from a fork PR awaiting approval.
//   - status="waiting": environment protection rule awaiting a reviewer.
//
// Note: status="completed" conclusion="action_required" is a different state —
// it means the run has pending environment deployments; use
// ApprovePendingDeployments for those.
func (c *Client) ListActionRequiredRuns(ctx context.Context, sha string) ([]*github.WorkflowRun, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return nil, err
	}
	var required []*github.WorkflowRun
	for _, r := range runs {
		status := r.GetStatus()
		if status == "action_required" || status == "waiting" {
			required = append(required, r)
		}
	}
	return required, nil
}

// ListPendingDeploymentRuns returns workflow runs for the given SHA that have
// concluded with "action_required", which means one or more environment
// protection rules are awaiting review approval.  These are approved via
// ApprovePendingDeployments, not the fork-PR approve endpoint.
func (c *Client) ListPendingDeploymentRuns(ctx context.Context, sha string) ([]*github.WorkflowRun, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return nil, err
	}
	var required []*github.WorkflowRun
	for _, r := range runs {
		if r.GetStatus() == runStatusCompleted && r.GetConclusion() == "action_required" {
			required = append(required, r)
		}
	}
	return required, nil
}

// ApprovePendingDeployments approves all environment deployments that the
// current token is allowed to approve for the given workflow run.  It returns
// the number of environments that were approved.
func (c *Client) ApprovePendingDeployments(ctx context.Context, runID int64) (int, error) {
	pending, _, err := c.gh.Actions.GetPendingDeployments(ctx, c.owner, c.repo, runID)
	if err != nil {
		return 0, fmt.Errorf("get pending deployments for run %d: %w", runID, err)
	}
	logger.Load(ctx).InfoContext(ctx, "pending deployments for run",
		slog.Int64("run", runID), slog.Int("count", len(pending)))
	var envIDs []int64
	for _, d := range pending {
		env := d.GetEnvironment()
		logger.Load(ctx).InfoContext(ctx, "pending deployment",
			slog.Int64("run", runID),
			slog.String("env", env.GetName()),
			slog.Int64("env_id", env.GetID()),
			slog.Bool("can_approve", d.GetCurrentUserCanApprove()),
		)
		if d.GetCurrentUserCanApprove() {
			envIDs = append(envIDs, env.GetID())
		}
	}
	if len(envIDs) == 0 {
		return 0, nil
	}
	_, _, err = c.gh.Actions.PendingDeployments(ctx, c.owner, c.repo, runID, &github.PendingDeploymentsRequest{
		EnvironmentIDs: envIDs,
		State:          "approved",
		Comment:        "Auto-approved by copilot-autodev",
	})
	if err != nil {
		return 0, fmt.Errorf("approve pending deployments for run %d: %w", runID, err)
	}
	return len(envIDs), nil
}

// RerunWorkflow re-runs an entire workflow run.  This is used when a run is
// stuck in conclusion=action_required with no pending deployments — the
// action_required state is likely stale or from a GitHub App check suite, so
// re-running the workflow clears it and restarts CI from scratch.
func (c *Client) RerunWorkflow(ctx context.Context, runID int64) error {
	_, err := c.gh.Actions.RerunWorkflowByID(ctx, c.owner, c.repo, runID)
	if err != nil {
		return fmt.Errorf("rerun workflow run %d: %w", runID, err)
	}
	logger.Load(ctx).InfoContext(ctx, "Actions: re-run triggered for workflow run", slog.Int64("run", runID))
	return nil
}

// ApproveWorkflowRun sends a raw API request to approve a pending workflow run.
// (go-github v68 does not expose this specific endpoint).
func (c *Client) ApproveWorkflowRun(ctx context.Context, runID int64) error {
	baseURL := "https://api.github.com/"
	if c.gh.BaseURL != nil {
		baseURL = c.gh.BaseURL.String()
	}
	endpoint := fmt.Sprintf(
		"%srepos/%s/%s/actions/runs/%d/approve",
		baseURL, url.PathEscape(c.owner), url.PathEscape(c.repo), runID,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build approve run request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("approve run %d: %w", runID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		logger.Load(ctx).InfoContext(ctx, "Actions: successfully approved run", slog.Int64("run", runID))
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	logger.Load(ctx).WarnContext(ctx, "Actions: approve run failed",
		slog.Int64("run", runID),
		slog.Int("status", resp.StatusCode),
		slog.String("body", string(body)),
	)
	return fmt.Errorf(
		"approve run %d: unexpected status %d %s",
		runID, resp.StatusCode, http.StatusText(resp.StatusCode),
	)
}

// CountCIFixPromptsSent returns the number of CI-fix-only prompts posted on
// the given PR by counting comments containing CIFixCommentMarker that were
// posted after the most recent successful local merge resolution.
func (c *Client) CountCIFixPromptsSent(ctx context.Context, prNum int) (int, error) {
	lastRes, err := c.LastSuccessfulLocalResolutionAt(ctx, prNum)
	if err != nil {
		return 0, err
	}
	return c.countCommentsWithMarkerSince(ctx, prNum, CIFixCommentMarker, lastRes)
}

// LastSuccessfulLocalResolutionAt returns the timestamp of the most recent
// successful local merge resolution comment.
func (c *Client) LastSuccessfulLocalResolutionAt(ctx context.Context, num int) (time.Time, error) {
	var latest time.Time
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list comments (#%d): %w", num, err)
		}
		for _, cm := range comments {
			body := cm.GetBody()
			if strings.Contains(body, LocalResolutionCommentMarker) &&
				!strings.Contains(body, LocalResolutionFailedMarker) {
				if t := cm.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// CountAgentContinueComments returns the number of "@copilot continue"
// comments (identified by AgentContinueCommentMarker) posted on the given
// issue or PR number.  This covers agent-coding timeouts and refinement nudges.
func (c *Client) CountAgentContinueComments(ctx context.Context, num int) (int, error) {
	return c.countCommentsWithMarker(ctx, num, AgentContinueCommentMarker)
}

// LastAgentContinueAt returns the timestamp of the most recent "@copilot
// continue" comment on the given issue or PR, or zero time if none exist.
func (c *Client) LastAgentContinueAt(ctx context.Context, num int) (time.Time, error) {
	return c.lastCommentWithMarker(ctx, num, AgentContinueCommentMarker)
}

// CountMergeConflictContinueComments returns the number of merge-conflict
// nudge comments (identified by MergeConflictContinueCommentMarker) posted on
// the given PR.  This budget is separate from AgentContinueCommentMarker so
// the merge-conflict phase cannot consume the refinement+CI retry allowance.
func (c *Client) CountMergeConflictContinueComments(ctx context.Context, num int) (int, error) {
	return c.countCommentsWithMarker(ctx, num, MergeConflictContinueCommentMarker)
}

// LastMergeConflictContinueAt returns the timestamp of the most recent
// merge-conflict nudge comment, or zero time if none exist.
func (c *Client) LastMergeConflictContinueAt(ctx context.Context, num int) (time.Time, error) {
	return c.lastCommentWithMarker(ctx, num, MergeConflictContinueCommentMarker)
}

// countCommentsWithMarker counts issue/PR comments containing the given marker.
func (c *Client) countCommentsWithMarker(ctx context.Context, num int, marker string) (int, error) {
	return c.countCommentsWithMarkerSince(ctx, num, marker, time.Time{})
}

// countCommentsWithMarkerSince counts issue/PR comments containing the given marker
// that were created after the given time (or all if since is zero).
func (c *Client) countCommentsWithMarkerSince(
	ctx context.Context,
	num int,
	marker string,
	since time.Time,
) (int, error) {
	count := 0
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return 0, fmt.Errorf("list comments (#%d): %w", num, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), marker) {
				if since.IsZero() || cm.GetCreatedAt().Time.After(since) {
					count++
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// lastCommentWithMarker returns the timestamp of the most recent issue/PR
// comment containing the given marker, or zero time if none exist.
func (c *Client) lastCommentWithMarker(ctx context.Context, num int, marker string) (time.Time, error) {
	var latest time.Time
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list comments (#%d): %w", num, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), marker) {
				if t := cm.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// EnsureLabelsExist creates any missing ai-* labels with default colours.
func (c *Client) EnsureLabelsExist(ctx context.Context) error {
	needed := map[string]string{
		c.labelQueue:  "0075ca",
		c.labelCoding: "e4e669",
		c.labelReview: "d93f0b",
	}
	existing, _, err := c.gh.Issues.ListLabels(ctx, c.owner, c.repo, &github.ListOptions{PerPage: perPageDefault})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, l := range existing {
		have[l.GetName()] = true
	}
	for name, color := range needed {
		if have[name] {
			continue
		}
		n, col := name, color
		if _, _, err := c.gh.Issues.CreateLabel(ctx, c.owner, c.repo, &github.Label{
			Name:  &n,
			Color: &col,
		}); err != nil {
			return fmt.Errorf("create label %q: %w", name, err)
		}
	}
	return nil
}

// MergedPRForIssue finds the first *merged* PR whose title or body references the issue.
func (c *Client) MergedPRForIssue(ctx context.Context, issue *github.Issue) (*github.PullRequest, error) {
	issueNum := issue.GetNumber()
	query := fmt.Sprintf("repo:%s/%s is:pr is:merged %d", c.owner, c.repo, issueNum)
	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: perPageMin},
	}
	result, _, err := c.gh.Search.Issues(ctx, query, opts)
	if err != nil {
		return nil, err
	}

	for _, sr := range result.Issues {
		if sr.PullRequestLinks != nil && sr.GetNumber() != issueNum {
			pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, sr.GetNumber())
			if err != nil {
				continue
			}
			if pr.GetMerged() {
				if c.IsMatchForIssue(pr, issueNum) {
					return pr, nil
				}
			}
		}
	}

	prs, _, err := c.gh.PullRequests.List(ctx, c.owner, c.repo, &github.PullRequestListOptions{
		State:       "closed",
		ListOptions: github.ListOptions{PerPage: perPageMedium},
	})
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if !pr.GetMerged() {
			continue
		}
		if c.IsMatchForIssue(pr, issueNum) {
			return pr, nil
		}
	}
	return nil, nil
}

// PRIsUpToDateWithBase returns true when the PR has no merge conflicts and is
// not behind the base branch.
func (c *Client) PRIsUpToDateWithBase(ctx context.Context, pr *github.PullRequest) (bool, error) {
	if pr.GetMergeableState() == "behind" || pr.GetMergeableState() == "dirty" {
		return false, nil
	}
	behind, err := c.IsBranchBehindBase(ctx, pr)
	if err != nil {
		return false, err
	}
	return !behind, nil
}

// CodingLabeledAt returns the most recent time the given label was applied to
// the issue by scanning the issue's event timeline.  Returns zero time if no
// such event is found.
func (c *Client) CodingLabeledAt(ctx context.Context, issueNum int, label string) (time.Time, error) {
	var latest time.Time
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		events, resp, err := c.gh.Issues.ListIssueEvents(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list issue events (#%d): %w", issueNum, err)
		}
		for _, e := range events {
			if e.GetEvent() == "labeled" && e.GetLabel().GetName() == label {
				if t := e.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// CountNudgesSince returns the number of nudge comments posted on the issue
// after the given time.  Pass zero time to count all nudge comments.
func (c *Client) CountNudgesSince(ctx context.Context, issueNum int, since time.Time) (int, error) {
	count := 0
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return 0, fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), CopilotNudgeCommentMarker) {
				if since.IsZero() || !cm.GetCreatedAt().Time.Before(since) {
					count++
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

// LastNudgeAt returns the timestamp of the most recent nudge comment on the
// issue, or zero time if none exist.
func (c *Client) LastNudgeAt(ctx context.Context, issueNum int) (time.Time, error) {
	var latest time.Time
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), CopilotNudgeCommentMarker) {
				if t := cm.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// TimeAgo returns a short human-readable relative time string.
func TimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/housPerDay))
	}
}

// CopilotAgentJobRequest is the POST body sent to the Copilot API when
// creating a new coding-agent task.
type CopilotAgentJobRequest struct {
	Title            string `json:"title,omitempty"`
	ProblemStatement string `json:"problem_statement"`
	EventType        string `json:"event_type"`
	IssueNumber      int    `json:"issue_number,omitempty"`
	IssueURL         string `json:"issue_url,omitempty"`
}

// CopilotJobStatus contains the status of a scheduled Copilot agent task (e.g., "in_progress", "running", "queued", "completed", "failed").
type CopilotJobStatus struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"` // "running", "completed", etc.

	PullRequest *struct {
		Number int `json:"number"`
	} `json:"pull_request,omitempty"`

	WorkflowRun *struct {
		ID int64 `json:"id"`
	} `json:"workflow_run,omitempty"`
}

// GetCopilotJobStatus queries the Copilot API for the status of a specific job ID.
func (c *Client) GetCopilotJobStatus(ctx context.Context, jobID string) (*CopilotJobStatus, error) {
	endpoint := fmt.Sprintf(
		"%s/agents/swe/v1/jobs/%s/%s/%s",
		copilotAPIBase,
		url.PathEscape(c.owner),
		url.PathEscape(c.repo),
		url.PathEscape(jobID),
	)
	return c.GetJobStatusAt(ctx, endpoint, jobID)
}

// GetJobStatusAt is the testable core of GetCopilotJobStatus.
func (c *Client) GetJobStatusAt(ctx context.Context, endpoint, jobID string) (*CopilotJobStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build copilot job status request: %w", err)
	}
	req.Header.Set("Copilot-Integration-Id", "copilot-autodev")
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("invoke copilot job status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("copilot job %s not found (404)", jobID)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get copilot job status %s: unexpected status %d %s: %s",
			jobID, resp.StatusCode, http.StatusText(resp.StatusCode), string(respBody))
	}

	var status CopilotJobStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode copilot job status response: %w", err)
	}
	return &status, nil
}

// LatestCopilotJobID returns the most recent Copilot task job ID recorded on the
// issue, or an empty string if none exist.
func (c *Client) LatestCopilotJobID(ctx context.Context, issueNum int) (string, error) {
	var latestJobID string
	var latest time.Time

	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return "", fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			body := cm.GetBody()
			idx := strings.Index(body, CopilotJobIDCommentMarker)
			if idx != -1 {
				if t := cm.GetCreatedAt().Time; t.After(latest) {
					start := idx + len(CopilotJobIDCommentMarker)
					end := strings.Index(body[start:], "-->")
					if end != -1 {
						latestJobID = strings.TrimSpace(body[start : start+end])
						latest = t
					}
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latestJobID, nil
}

// InvokeCopilotAgent creates a new Copilot coding-agent task by calling the
// Copilot API directly — the same backend that powers the GitHub Agents tab
// and the `gh agent-task create` CLI command.
//
// The issueNum and issueURL parameters link the agent task to the originating
// issue so the agent creates a PR that references it.  This bypasses the
// unreliable issue-assignment and @-mention trigger mechanisms. We also explicitly
// inject a "Fixes #issueNum" string into the prompt so that the Copilot workspace agent
// will reliably use the issue in branch strings and link it locally.
//
// Returns the job ID from the CAPI response (empty string if not parseable).
func (c *Client) InvokeCopilotAgent(
	ctx context.Context, prompt, issueTitle string, issueNum int, issueURL string,
) (string, error) {
	prompt = fmt.Sprintf("%s\n\nFixes #%d", prompt, issueNum)
	endpoint := fmt.Sprintf(
		"%s/agents/swe/v1/jobs/%s/%s",
		copilotAPIBase,
		url.PathEscape(c.owner),
		url.PathEscape(c.repo),
	)
	return c.InvokeAgentAt(ctx, endpoint, prompt, issueTitle, issueNum, issueURL)
}

// copilotAgentJobResponse captures the fields we care about from the CAPI
// response.  The actual schema may contain more fields; we only parse what
// we need and log the full body for discovery.
type copilotAgentJobResponse struct {
	ID    string `json:"id"`
	JobID string `json:"job_id"`
}

// InvokeAgentAt is the testable core of InvokeCopilotAgent.  It accepts an
// explicit URL so tests can point it at an [httptest.Server].
//
// Returns the job ID extracted from the response (best-effort; empty if the
// response doesn't contain a recognisable ID field).
func (c *Client) InvokeAgentAt(
	ctx context.Context, endpoint, prompt, issueTitle string, issueNum int, issueURL string,
) (string, error) {
	body, err := json.Marshal(&CopilotAgentJobRequest{
		Title:            fmt.Sprintf("[copilot-autodev] #%d: %s", issueNum, issueTitle),
		ProblemStatement: prompt,
		EventType:        "copilot-autodev",
		IssueNumber:      issueNum,
		IssueURL:         issueURL,
	})
	if err != nil {
		return "", fmt.Errorf("marshal copilot agent request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build copilot agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Copilot-Integration-Id", "copilot-autodev")
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("invoke copilot agent: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("invoke copilot agent: unexpected status %d %s: %s",
			resp.StatusCode, http.StatusText(resp.StatusCode), string(respBody))
	}

	// Best-effort parse of the job ID from the response.
	var parsed copilotAgentJobResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		logger.Load(ctx).WarnContext(ctx, "copilot agent: could not parse response body",
			slog.Any("err", err), slog.String("raw", string(respBody)))
		return "", nil
	}
	jobID := parsed.ID
	if jobID == "" {
		jobID = parsed.JobID
	}
	logger.Load(ctx).InfoContext(ctx, "copilot agent: invoked successfully",
		slog.String("job_id", jobID), slog.String("raw", string(respBody)))
	return jobID, nil
}

// MarkPRReady removes draft status from a pull request using the GitHub
// GraphQL API (the REST API does not support changing draft status).
func (c *Client) MarkPRReady(ctx context.Context, pr *github.PullRequest) error {
	nodeID := pr.GetNodeID()
	if nodeID == "" {
		return fmt.Errorf("PR #%d has no node ID", pr.GetNumber())
	}

	const mutation = `mutation($id: ID!) ` +
		`{ markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { id } } }`
	payload := map[string]any{
		"query":     mutation,
		"variables": map[string]string{"id": nodeID},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("mark PR ready: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mark PR ready: unexpected status %d %s",
			resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return nil
}

// HasReviewContaining returns true and the timestamp if any review comment on the PR contains
// the given substring.  Used for SHA-based deduplication of CI-fix and
// refinement prompts.
func (c *Client) HasReviewContaining(ctx context.Context, prNum int, needle string) (bool, time.Time, error) {
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, c.owner, c.repo, prNum, opts)
		if err != nil {
			return false, time.Time{}, err
		}
		for _, r := range reviews {
			if strings.Contains(r.GetBody(), needle) {
				return true, r.GetSubmittedAt().Time, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false, time.Time{}, nil
}

// HasCommentContaining returns true and the timestamp if any issue comment on the given
// issue/PR number contains the given substring.
func (c *Client) HasCommentContaining(ctx context.Context, num int, needle string) (bool, time.Time, error) {
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return false, time.Time{}, err
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), needle) {
				return true, cm.GetCreatedAt().Time, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false, time.Time{}, nil
}

// DeleteCommentContaining finds the first comment on an issue/PR whose body
// contains needle and deletes it.  Returns nil if no matching comment exists.
func (c *Client) DeleteCommentContaining(ctx context.Context, num int, needle string) error {
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, num, opts)
		if err != nil {
			return err
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), needle) {
				_, err := c.gh.Issues.DeleteComment(ctx, c.owner, c.repo, cm.GetID())
				return err
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil
}

// SHAMarker returns a marker string that embeds a commit SHA prefix, used
// to deduplicate per-SHA comments across poll ticks.
func SHAMarker(prefix, sha string) string {
	short := sha
	if len(short) > shortSHALen {
		short = short[:7]
	}
	return fmt.Sprintf("<!-- copilot-autodev:%s:%s -->", prefix, short)
}

// TimelineEntry represents a single event in an issue's activity feed.
type TimelineEntry struct {
	Time   time.Time
	Actor  string
	Event  string
	Detail string
}

// FetchTimeline returns a chronological list of significant events for an issue,
// combining label events and comments (with friendly marker detection).
func (c *Client) FetchTimeline(ctx context.Context, issueNum int) ([]TimelineEntry, error) {
	var entries []TimelineEntry

	// 1. Fetch label events.
	opts := &github.ListOptions{PerPage: perPageDefault}
	for {
		events, resp, err := c.gh.Issues.ListIssueEvents(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return nil, fmt.Errorf("list issue events (#%d): %w", issueNum, err)
		}
		for _, e := range events {
			evt := e.GetEvent()
			switch evt {
			case "labeled", "unlabeled":
				entries = append(entries, TimelineEntry{
					Time:   e.GetCreatedAt().Time,
					Actor:  e.GetActor().GetLogin(),
					Event:  evt,
					Detail: e.GetLabel().GetName(),
				})
			case "assigned", "unassigned":
				entries = append(entries, TimelineEntry{
					Time:   e.GetCreatedAt().Time,
					Actor:  e.GetActor().GetLogin(),
					Event:  evt,
					Detail: e.GetAssignee().GetLogin(),
				})
			case "closed", "reopened":
				entries = append(entries, TimelineEntry{
					Time:  e.GetCreatedAt().Time,
					Actor: e.GetActor().GetLogin(),
					Event: evt,
				})
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	// 2. Fetch comments and detect our markers for friendly labels.
	cmOpts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: perPageDefault}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, cmOpts)
		if err != nil {
			return nil, fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			body := cm.GetBody()
			entry := TimelineEntry{
				Time:  cm.GetCreatedAt().Time,
				Actor: cm.GetUser().GetLogin(),
			}
			switch {
			case strings.Contains(body, RefinementCommentMarker):
				entry.Event = "Refinement posted"
			case strings.Contains(body, CIFixCommentMarker):
				entry.Event = "CI fix requested"
			case strings.Contains(body, MergeConflictCommentMarker):
				entry.Event = "Merge conflict comment"
			case strings.Contains(body, LocalResolutionFailedMarker):
				entry.Event = "Local resolution failed"
			case strings.Contains(body, LocalResolutionCommentMarker):
				entry.Event = "Local resolution succeeded"
			case strings.Contains(body, CopilotNudgeCommentMarker):
				entry.Event = "Agent nudge"
			case strings.Contains(body, AgentContinueCommentMarker):
				entry.Event = "Agent continue"
			default:
				// Truncate long comments.
				detail := body
				if len(detail) > 80 {
					detail = detail[:77] + "..."
				}
				entry.Event = "Comment"
				entry.Detail = detail
			}
			entries = append(entries, entry)
		}
		if resp.NextPage == 0 {
			break
		}
		cmOpts.Page = resp.NextPage
	}

	// Sort by time ascending.
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Time.Before(entries[i].Time) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	return entries, nil
}
