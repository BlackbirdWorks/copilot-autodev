// Package ghclient wraps go-github with the operations needed by the poller.
package ghclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

const (
	LabelQueue  = "ai-queue"
	LabelCoding = "ai-coding"
	LabelReview = "ai-review"

	CopilotUser = "copilot"

	// MaxRefinementPrompts is the number of times the orchestrator will ask
	// @copilot to review its implementation against the full original issue
	// requirements once CI is green.  Only after all prompts have been sent
	// (and CI remains green) will the PR be approved and merged.
	MaxRefinementPrompts = 3

	// RefinementCommentMarker is an invisible HTML comment embedded in every
	// refinement prompt.  The orchestrator counts PR reviews containing this
	// marker to determine how many refinement rounds have already been sent,
	// which means the count survives process restarts.
	RefinementCommentMarker = "<!-- copilot-autocode:refinement -->"
)

// Client wraps the GitHub SDK client.
type Client struct {
	gh    *github.Client
	owner string
	repo  string
}

// New creates a new Client authenticated with the provided PAT token.
func New(token, owner, repo string) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return &Client{
		gh:    github.NewClient(tc),
		owner: owner,
		repo:  repo,
	}
}

// IssuesByLabel returns all open issues carrying the given label.
func (c *Client) IssuesByLabel(ctx context.Context, label string) ([]*github.Issue, error) {
	var all []*github.Issue
	opts := &github.IssueListByRepoOptions{
		State:  "open",
		Labels: []string{label},
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, c.owner, c.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list issues (label=%s): %w", label, err)
		}
		// Filter out pull requests (GitHub API returns PRs in issue list too).
		for _, i := range issues {
			if i.PullRequestLinks == nil {
				all = append(all, i)
			}
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
		// Ignore "label not found" errors.
		if strings.Contains(err.Error(), "404") {
			return nil
		}
		return err
	}
	return nil
}

// AssignCopilot assigns the copilot user to the issue.
func (c *Client) AssignCopilot(ctx context.Context, issueNum int) error {
	_, _, err := c.gh.Issues.AddAssignees(ctx, c.owner, c.repo, issueNum, []string{CopilotUser})
	return err
}

// CloseIssue closes the issue.
func (c *Client) CloseIssue(ctx context.Context, issueNum int) error {
	closed := "closed"
	_, _, err := c.gh.Issues.Edit(ctx, c.owner, c.repo, issueNum, &github.IssueRequest{State: &closed})
	return err
}

// OpenPRForIssue finds the first open PR whose title or body references the issue.
func (c *Client) OpenPRForIssue(ctx context.Context, issueNum int) (*github.PullRequest, error) {
	// Search for PRs that reference this issue.
	query := fmt.Sprintf("repo:%s/%s is:pr is:open #%d in:body", c.owner, c.repo, issueNum)
	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 10},
	}
	result, _, err := c.gh.Search.Issues(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	for _, issue := range result.Issues {
		if issue.PullRequestLinks != nil && issue.GetNumber() != issueNum {
			pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, issue.GetNumber())
			if err != nil {
				continue
			}
			if pr.GetState() == "open" {
				return pr, nil
			}
		}
	}

	// Fallback: list recent PRs and look for branch / title match.
	prs, _, err := c.gh.PullRequests.List(ctx, c.owner, c.repo, &github.PullRequestListOptions{
		State: "open",
		ListOptions: github.ListOptions{PerPage: 50},
	})
	if err != nil {
		return nil, err
	}
	needle := fmt.Sprintf("#%d", issueNum)
	for _, pr := range prs {
		if strings.Contains(pr.GetBody(), needle) || strings.Contains(pr.GetTitle(), needle) {
			return pr, nil
		}
	}
	return nil, nil
}

// IsPRDraft returns true when the PR is still in draft state.
func (c *Client) IsPRDraft(pr *github.PullRequest) bool {
	return pr.GetDraft()
}

// MarkReadyForReview converts a draft PR to a ready-for-review PR via the REST API.
func (c *Client) MarkReadyForReview(ctx context.Context, pr *github.PullRequest) error {
	nodeID := pr.GetNodeID()
	if nodeID == "" {
		return nil
	}
	// Use the REST endpoint to mark as ready for review.
	body := strings.NewReader(`{"draft":false}`)
	req, err := c.gh.NewRequest("PATCH",
		fmt.Sprintf("repos/%s/%s/pulls/%d", c.owner, c.repo, pr.GetNumber()),
		body)
	if err != nil {
		return err
	}
	// Accept the preview header required for draft PRs.
	req.Header.Set("Accept", "application/vnd.github.shadow-cat-preview+json")
	_, err = c.gh.Do(ctx, req, nil)
	return err
}

// IsBranchBehindBase returns true when the PR branch is behind the base branch.
func (c *Client) IsBranchBehindBase(ctx context.Context, pr *github.PullRequest) (bool, error) {
	comp, _, err := c.gh.Repositories.CompareCommits(ctx, c.owner, c.repo,
		pr.GetBase().GetSHA(), pr.GetHead().GetSHA(), nil)
	if err != nil {
		return false, err
	}
	// "behind" means the head is missing commits from base.
	status := comp.GetStatus()
	return status == "behind" || status == "diverged", nil
}

// PostComment posts a plain comment on the given issue/PR number.
func (c *Client) PostComment(ctx context.Context, issueNum int, body string) error {
	_, _, err := c.gh.Issues.CreateComment(ctx, c.owner, c.repo, issueNum, &github.IssueComment{Body: &body})
	return err
}

// PostReviewComment posts a review comment (requesting changes) on a PR.
func (c *Client) PostReviewComment(ctx context.Context, prNum int, body string) error {
	event := "COMMENT"
	_, _, err := c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, prNum, &github.PullRequestReviewRequest{
		Body:  &body,
		Event: &event,
	})
	return err
}

// CountRefinementPromptsSent returns the number of refinement prompts the
// orchestrator has already posted on the given PR by counting PR reviews whose
// body contains RefinementCommentMarker.  Reading this count from GitHub means
// it survives process restarts.
func (c *Client) CountRefinementPromptsSent(ctx context.Context, prNum int) (int, error) {
	count := 0
	opts := &github.ListOptions{PerPage: 100}
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

// ApprovePR approves the PR.
func (c *Client) ApprovePR(ctx context.Context, prNum int) error {
	event := "APPROVE"
	_, _, err := c.gh.PullRequests.CreateReview(ctx, c.owner, c.repo, prNum, &github.PullRequestReviewRequest{
		Event: &event,
	})
	return err
}

// MergePR squash-merges the PR.
func (c *Client) MergePR(ctx context.Context, pr *github.PullRequest) error {
	_, _, err := c.gh.PullRequests.Merge(ctx, c.owner, c.repo, pr.GetNumber(),
		"Auto-merged by copilot-autocode",
		&github.PullRequestOptions{MergeMethod: "squash"})
	return err
}

// LatestWorkflowRun returns the most recent workflow run for the given SHA.
// Returns nil if no runs exist yet.
func (c *Client) LatestWorkflowRun(ctx context.Context, sha string) ([]*github.WorkflowRun, error) {
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, &github.ListWorkflowRunsOptions{
		HeadSHA: sha,
		ListOptions: github.ListOptions{PerPage: 20},
	})
	if err != nil {
		return nil, err
	}
	return runs.WorkflowRuns, nil
}

// FailedRunLogs returns combined log text for a failed workflow run.
func (c *Client) FailedRunLogs(ctx context.Context, runID int64) (string, error) {
	jobs, _, err := c.gh.Actions.ListWorkflowJobs(ctx, c.owner, c.repo, runID, &github.ListWorkflowJobsOptions{
		Filter: "latest",
		ListOptions: github.ListOptions{PerPage: 50},
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, job := range jobs.Jobs {
		if job.GetConclusion() == "failure" {
			url, _, err := c.gh.Actions.GetWorkflowJobLogs(ctx, c.owner, c.repo, job.GetID(), 10)
			if err != nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("### Job: %s\nLogs: %s\n", job.GetName(), url.String()))
		}
	}
	return sb.String(), nil
}

// ActiveCopilotAgentRunning returns true when there is an in-progress Copilot
// agent run associated with a PR head SHA.  We approximate this by checking
// whether any workflow run on that SHA has status "in_progress" or "queued".
func (c *Client) ActiveCopilotAgentRunning(ctx context.Context, sha string) (bool, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		s := r.GetStatus()
		if s == "in_progress" || s == "queued" || s == "waiting" || s == "requested" {
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
		return false, false, nil
	}

	// Filter to only completed runs.
	allSuccess := true
	anyFailure := false
	for _, r := range runs {
		if r.GetStatus() != "completed" {
			return false, false, nil // still running
		}
		if r.GetConclusion() != "success" && r.GetConclusion() != "skipped" {
			allSuccess = false
		}
		if r.GetConclusion() == "failure" {
			anyFailure = true
		}
	}
	return allSuccess, anyFailure, nil
}

// GetPR fetches a PR by number.
func (c *Client) GetPR(ctx context.Context, prNum int) (*github.PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, prNum)
	return pr, err
}

// GetIssue fetches an issue by number.
func (c *Client) GetIssue(ctx context.Context, issueNum int) (*github.Issue, error) {
	issue, _, err := c.gh.Issues.Get(ctx, c.owner, c.repo, issueNum)
	return issue, err
}

// EnsureLabelsExist creates any missing labels with default colours.
func (c *Client) EnsureLabelsExist(ctx context.Context) error {
	needed := map[string]string{
		LabelQueue:  "0075ca",
		LabelCoding: "e4e669",
		LabelReview: "d93f0b",
	}
	existing, _, err := c.gh.Issues.ListLabels(ctx, c.owner, c.repo, &github.ListOptions{PerPage: 100})
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

// FindFailedRunID returns the run ID of the most recent failed workflow run for a SHA.
func (c *Client) FindFailedRunID(ctx context.Context, sha string) (int64, error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return 0, err
	}
	for _, r := range runs {
		if r.GetConclusion() == "failure" {
			return r.GetID(), nil
		}
	}
	return 0, nil
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
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
