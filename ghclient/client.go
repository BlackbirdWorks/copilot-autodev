// Package ghclient wraps go-github with the operations needed by the poller.
package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

	// CopilotNudgeCommentMarker is an invisible HTML comment embedded in every
	// nudge comment posted when the Copilot coding agent has not started
	// within the configured timeout.  Counting these comments tells the
	// orchestrator how many re-trigger attempts have been made for the current
	// coding cycle so that it can enforce CopilotInvokeMaxRetries.
	CopilotNudgeCommentMarker = "<!-- copilot-autocode:nudge -->"
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
	token       string // PAT used for Copilot API calls
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
		token:       token,
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

// ReassignCopilot removes and then re-adds the Copilot user as an assignee on
// the issue so that a fresh "assigned" webhook event is fired, re-triggering
// the coding agent.  The remove step is best-effort; failure is ignored.
func (c *Client) ReassignCopilot(ctx context.Context, issueNum int) error {
	// Best-effort unassign – ignore errors (e.g. user was never assigned).
	_, _, _ = c.gh.Issues.RemoveAssignees(ctx, c.owner, c.repo, issueNum, []string{c.copilotUser})
	_, _, err := c.gh.Issues.AddAssignees(ctx, c.owner, c.repo, issueNum, []string{c.copilotUser})
	return err
}

// CodingLabeledAt returns the most recent time the given label was applied to
// the issue by scanning the issue's event timeline.  Returns zero time if no
// such event is found.
func (c *Client) CodingLabeledAt(ctx context.Context, issueNum int, label string) (time.Time, error) {
	var latest time.Time
	opts := &github.ListOptions{PerPage: 100}
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
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repo, issueNum, opts)
		if err != nil {
			return 0, fmt.Errorf("list comments (#%d): %w", issueNum, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), CopilotNudgeCommentMarker) {
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

// LastNudgeAt returns the timestamp of the most recent nudge comment on the
// issue, or zero time if none exist.
func (c *Client) LastNudgeAt(ctx context.Context, issueNum int) (time.Time, error) {
	var latest time.Time
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
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
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// copilotAgentJobRequest is the POST body sent to the Copilot API when
// creating a new coding-agent task.
type copilotAgentJobRequest struct {
	ProblemStatement string `json:"problem_statement"`
	EventType        string `json:"event_type"`
}

// InvokeCopilotAgent creates a new Copilot coding-agent task by calling the
// Copilot API directly — the same backend that powers the GitHub Agents tab
// and the `gh agent-task create` CLI command.
//
// This bypasses the unreliable issue-assignment and @-mention comment
// trigger mechanisms and ensures the agent actually starts working.
func (c *Client) InvokeCopilotAgent(ctx context.Context, prompt string) error {
	endpoint := fmt.Sprintf(
		"%s/agents/swe/v1/jobs/%s/%s",
		copilotAPIBase,
		url.PathEscape(c.owner),
		url.PathEscape(c.repo),
	)
	return c.invokeAgentAt(ctx, endpoint, prompt)
}

// invokeAgentAt is the testable core of InvokeCopilotAgent.  It accepts an
// explicit URL so tests can point it at an httptest.Server.
func (c *Client) invokeAgentAt(ctx context.Context, endpoint, prompt string) error {
	body, err := json.Marshal(&copilotAgentJobRequest{
		ProblemStatement: prompt,
		EventType:        "copilot-autocode",
	})
	if err != nil {
		return fmt.Errorf("marshal copilot agent request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build copilot agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Copilot-Integration-Id", "copilot-autocode")
	req.Header.Set("X-GitHub-Api-Version", copilotAPIVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("invoke copilot agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invoke copilot agent: unexpected status %d %s",
			resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return nil
}
