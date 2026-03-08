// Package ghclient wraps go-github with the operations needed by the poller.
package ghclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

const (
	// RefinementCommentMarker is an invisible HTML comment embedded in every
	// refinement prompt.  The orchestrator counts PR reviews containing this
	// marker to determine how many refinement rounds have already been sent,
	// which means the count survives process restarts.
	RefinementCommentMarker = "<!-- copilot-autocode:refinement -->"

	// MergeConflictCommentMarker is an invisible HTML comment embedded in
	// every merge-conflict @copilot prompt.  Counting these comments on a PR
	// tells the orchestrator how many @copilot attempts have been made so far,
	// which means the count survives process restarts.
	MergeConflictCommentMarker = "<!-- copilot-autocode:merge-conflict -->"
)

// FailedJobInfo describes a single failed CI job.
type FailedJobInfo struct {
	Name   string // display name of the job
	LogURL string // URL to the raw logs (may be empty if unavailable)
}

// Client wraps the GitHub SDK client with the settings from Config.
type Client struct {
	gh          *github.Client
	owner       string
	repo        string
	labelQueue  string
	labelCoding string
	labelReview string
	copilotUser string
	mergeMethod string
	mergeMsg    string
}

// New creates a new Client authenticated with the provided PAT token.
func New(token string, cfg *config.Config) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return &Client{
		gh:          github.NewClient(tc),
		owner:       cfg.GitHubOwner,
		repo:        cfg.GitHubRepo,
		labelQueue:  cfg.LabelQueue,
		labelCoding: cfg.LabelCoding,
		labelReview: cfg.LabelReview,
		copilotUser: cfg.CopilotUser,
		mergeMethod: cfg.MergeMethod,
		mergeMsg:    cfg.MergeCommitMessage,
	}
}

// IssuesByLabel returns all open issues carrying the given label.
func (c *Client) IssuesByLabel(ctx context.Context, label string) ([]*github.Issue, error) {
	var all []*github.Issue
	opts := &github.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
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
		if strings.Contains(err.Error(), "404") {
			return nil
		}
		return err
	}
	return nil
}

// AssignCopilot assigns the configured copilot user to the issue.
func (c *Client) AssignCopilot(ctx context.Context, issueNum int) error {
	_, _, err := c.gh.Issues.AddAssignees(ctx, c.owner, c.repo, issueNum, []string{c.copilotUser})
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

	// Fallback: list recent PRs and look for issue number in body/title.
	prs, _, err := c.gh.PullRequests.List(ctx, c.owner, c.repo, &github.PullRequestListOptions{
		State:       "open",
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

// IsBranchBehindBase returns true when the PR branch is behind the base branch.
func (c *Client) IsBranchBehindBase(ctx context.Context, pr *github.PullRequest) (bool, error) {
	comp, _, err := c.gh.Repositories.CompareCommits(ctx, c.owner, c.repo,
		pr.GetBase().GetSHA(), pr.GetHead().GetSHA(), nil)
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

// CountMergeConflictAttempts returns the number of merge-conflict @copilot
// prompts the orchestrator has already posted on the given PR by counting
// issue comments whose body contains MergeConflictCommentMarker.  Reading
// the count from GitHub means it survives process restarts.
func (c *Client) CountMergeConflictAttempts(ctx context.Context, prNum int) (int, error) {
	count := 0
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
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

// LatestWorkflowRun returns all recent workflow runs for the given commit SHA.
func (c *Client) LatestWorkflowRun(ctx context.Context, sha string) ([]*github.WorkflowRun, error) {
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, c.owner, c.repo, &github.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: github.ListOptions{PerPage: 20},
	})
	if err != nil {
		return nil, err
	}
	return runs.WorkflowRuns, nil
}

// FailedRunDetails finds the first failed workflow run for a commit SHA and
// returns its display name together with the name and log URL of every failed
// job inside that run.  Returns ("", nil, nil) when no failed run is found.
//
// This replaces the old FindFailedRunID + FailedRunLogs pair so callers get
// the workflow title in one call, which is included in the @copilot message so
// Copilot knows exactly where to look.
func (c *Client) FailedRunDetails(ctx context.Context, sha string) (workflowName string, jobs []FailedJobInfo, err error) {
	runs, err := c.LatestWorkflowRun(ctx, sha)
	if err != nil {
		return "", nil, err
	}

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
			ListOptions: github.ListOptions{PerPage: 50},
		})
	if err != nil {
		// Return the workflow name even if job details fail.
		return workflowName, nil, err
	}
	for _, job := range jobsResp.Jobs {
		if job.GetConclusion() != "failure" {
			continue
		}
		logURL := ""
		if u, _, lerr := c.gh.Actions.GetWorkflowJobLogs(ctx, c.owner, c.repo, job.GetID(), 10); lerr == nil {
			logURL = u.String()
		}
		jobs = append(jobs, FailedJobInfo{Name: job.GetName(), LogURL: logURL})
	}
	return workflowName, jobs, nil
}

// ActiveCopilotAgentRunning returns true when any workflow run on the given
// SHA is still in progress or queued.
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

// EnsureLabelsExist creates any missing ai-* labels with default colours.
func (c *Client) EnsureLabelsExist(ctx context.Context) error {
	needed := map[string]string{
		c.labelQueue:  "0075ca",
		c.labelCoding: "e4e669",
		c.labelReview: "d93f0b",
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
