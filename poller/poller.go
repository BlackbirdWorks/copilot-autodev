// Package poller implements the state machine that drives the Copilot
// Orchestrator workflow.  It runs as a background goroutine and uses GitHub
// labels, PR states, and workflow-run statuses as the single source of truth.
package poller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/google/go-github/v68/github"
)

// State is the poller's high-level understanding of a single issue.
type State struct {
	Issue   *github.Issue
	PR      *github.PullRequest
	Status  string // "queue" | "coding" | "review"
	Message string // last action taken
}

// Event is sent on the Events channel after every poll tick.
type Event struct {
	Queue   []*State
	Coding  []*State
	Review  []*State
	LastRun time.Time
	Err     error
}

// Poller orchestrates the Copilot workflow state machine.
type Poller struct {
	cfg    *config.Config
	gh     *ghclient.Client
	Events chan Event
}

// New creates a Poller ready to Start.
func New(cfg *config.Config, gh *ghclient.Client) *Poller {
	return &Poller{
		cfg:    cfg,
		gh:     gh,
		Events: make(chan Event, 1),
	}
}

// Start launches the polling goroutine.  It runs until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	go func() {
		// Run once immediately, then on every tick.
		p.tick(ctx)
		ticker := time.NewTicker(time.Duration(p.cfg.PollIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.tick(ctx)
			}
		}
	}()
}

// tick executes one full state-machine cycle and sends an Event.
func (p *Poller) tick(ctx context.Context) {
	evt := Event{LastRun: time.Now()}

	// Step 1: Queue → Coding.
	if err := p.promoteFromQueue(ctx); err != nil {
		evt.Err = fmt.Errorf("promote from queue: %w", err)
	}

	// Step 2: Coding → Review (detect completed Copilot runs / PR ready).
	if err := p.moveCodingToReview(ctx); err != nil && evt.Err == nil {
		evt.Err = fmt.Errorf("coding→review: %w", err)
	}

	// Step 3+4+5: Handle all review-stage PRs.
	if err := p.processReviewPRs(ctx); err != nil && evt.Err == nil {
		evt.Err = fmt.Errorf("review PRs: %w", err)
	}

	// Collect current snapshot for the TUI.
	evt.Queue, evt.Coding, evt.Review = p.snapshot(ctx)

	// Non-blocking send; drop stale event if channel is full.
	select {
	case p.Events <- evt:
	default:
		<-p.Events
		p.Events <- evt
	}
}

// promoteFromQueue moves issues from ai-queue → ai-coding up to the concurrency limit.
func (p *Poller) promoteFromQueue(ctx context.Context) error {
	coding, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	if err != nil {
		return err
	}
	slots := p.cfg.MaxConcurrentIssues - len(coding)
	if slots <= 0 {
		return nil
	}

	queued, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelQueue)
	if err != nil {
		return err
	}
	// Process oldest-first (lowest issue number = opened earliest).
	sortIssuesAsc(queued)

	for i := 0; i < slots && i < len(queued); i++ {
		issue := queued[i]
		num := issue.GetNumber()
		if err := p.gh.RemoveLabel(ctx, num, p.cfg.LabelQueue); err != nil {
			return err
		}
		if err := p.gh.AddLabel(ctx, num, p.cfg.LabelCoding); err != nil {
			return err
		}
		if err := p.gh.AssignCopilot(ctx, num); err != nil {
			// Non-fatal – the copilot user may not exist in the repo.
			log.Printf("warning: failed to assign copilot user to issue #%d: %v", num, err)
		}
	}
	return nil
}

// moveCodingToReview checks ai-coding issues and moves them to ai-review once
// the associated PR is no longer a draft (Copilot finished initial coding).
func (p *Poller) moveCodingToReview(ctx context.Context) error {
	coding, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	if err != nil {
		return err
	}
	for _, issue := range coding {
		num := issue.GetNumber()
		pr, err := p.gh.OpenPRForIssue(ctx, num)
		if err != nil || pr == nil {
			continue
		}
		// Wait until the PR is no longer draft.
		if p.gh.IsPRDraft(pr) {
			continue
		}
		// Also wait until no active agent run is still in progress.
		running, err := p.gh.ActiveCopilotAgentRunning(ctx, pr.GetHead().GetSHA())
		if err == nil && running {
			continue
		}

		// Transition to review.
		if err := p.gh.RemoveLabel(ctx, num, p.cfg.LabelCoding); err != nil {
			return err
		}
		if err := p.gh.AddLabel(ctx, num, p.cfg.LabelReview); err != nil {
			return err
		}
	}
	return nil
}

// processReviewPRs runs steps 3, 4, and 5 for every ai-review issue.
func (p *Poller) processReviewPRs(ctx context.Context) error {
	reviewing, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelReview)
	if err != nil {
		return err
	}
	for _, issue := range reviewing {
		if err := p.processOne(ctx, issue); err != nil {
			log.Printf("warning: error processing issue #%d: %v", issue.GetNumber(), err)
		}
	}
	return nil
}

// processOne handles a single ai-review issue through steps 3, 4, and 5.
func (p *Poller) processOne(ctx context.Context, issue *github.Issue) error {
	num := issue.GetNumber()

	pr, err := p.gh.OpenPRForIssue(ctx, num)
	if err != nil {
		return err
	}
	if pr == nil {
		return nil
	}

	// Step 3: Handle merge-conflict / behind-base.
	upToDate, err := p.gh.PRIsUpToDateWithBase(ctx, pr)
	if err != nil {
		return err
	}
	if !upToDate {
		// Wait if an agent is already fixing this.
		running, _ := p.gh.ActiveCopilotAgentRunning(ctx, pr.GetHead().GetSHA())
		if running {
			return nil
		}
		if err := p.gh.PostComment(ctx, pr.GetNumber(), p.cfg.MergeConflictPrompt); err != nil {
			return err
		}
		return nil
	}

	// If an agent run is still active, wait.
	running, err := p.gh.ActiveCopilotAgentRunning(ctx, pr.GetHead().GetSHA())
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	// Step 4: CI/CD feedback loop.
	allOK, anyFail, err := p.gh.AllRunsSucceeded(ctx, pr.GetHead().GetSHA())
	if err != nil {
		return err
	}

	if anyFail {
		// Post a fix request every time CI fails – no cap, keep retrying.
		// Include the workflow name and failing job names so Copilot knows
		// exactly where to look without having to navigate CI manually.
		workflowName, failedJobs, err := p.gh.FailedRunDetails(ctx, pr.GetHead().GetSHA())
		if err != nil {
			return err
		}
		body := p.buildCIFixMessage(workflowName, failedJobs)
		if err := p.gh.PostReviewComment(ctx, pr.GetNumber(), body); err != nil {
			return err
		}
		return nil
	}

	if !allOK {
		// Still running or no runs yet.
		return nil
	}

	// CI is green.  Send up to MaxRefinementRounds "review against the original
	// issue" prompts before approving and merging.  Read the count from GitHub
	// so it survives process restarts.
	sent, err := p.gh.CountRefinementPromptsSent(ctx, pr.GetNumber())
	if err != nil {
		return err
	}
	if sent < p.cfg.MaxRefinementRounds {
		body := fmt.Sprintf(
			"@copilot CI is passing (refinement check %d of %d). "+
				"%s\n%s",
			sent+1, p.cfg.MaxRefinementRounds,
			p.cfg.RefinementPrompt,
			ghclient.RefinementCommentMarker,
		)
		if err := p.gh.PostReviewComment(ctx, pr.GetNumber(), body); err != nil {
			return err
		}
		return nil
	}

	// Step 5: All CI green and all refinement rounds sent – approve and merge.
	if err := p.gh.ApprovePR(ctx, pr.GetNumber()); err != nil {
		// Non-fatal if we already approved.
		if !strings.Contains(err.Error(), "already approved") &&
			!strings.Contains(err.Error(), "Can not approve your own pull request") {
			return err
		}
	}
	if err := p.gh.MergePR(ctx, pr); err != nil {
		return err
	}
	// Close issue and strip ai-* labels.
	for _, lbl := range []string{p.cfg.LabelReview, p.cfg.LabelCoding, p.cfg.LabelQueue} {
		_ = p.gh.RemoveLabel(ctx, num, lbl)
	}
	return p.gh.CloseIssue(ctx, num)
}

// buildCIFixMessage composes the @copilot comment posted when CI fails.
// It opens with the configurable CIFixPrompt, then immediately names the
// failing workflow and jobs so Copilot knows where to look, and finally
// appends per-job log URLs.
func (p *Poller) buildCIFixMessage(workflowName string, failedJobs []ghclient.FailedJobInfo) string {
	var sb strings.Builder
	sb.WriteString(p.cfg.CIFixPrompt)

	if workflowName != "" {
		sb.WriteString(fmt.Sprintf("\n\n**Failing workflow:** %s", workflowName))
	}
	if len(failedJobs) > 0 {
		names := make([]string, len(failedJobs))
		for i, j := range failedJobs {
			names[i] = j.Name
		}
		sb.WriteString(fmt.Sprintf("\n**Failed jobs:** %s", strings.Join(names, ", ")))
	}
	for _, job := range failedJobs {
		if job.LogURL != "" {
			sb.WriteString(fmt.Sprintf("\n\n**%s** logs: %s", job.Name, job.LogURL))
		}
	}
	return sb.String()
}

// snapshot builds the current state for the TUI without doing extra API calls.
func (p *Poller) snapshot(ctx context.Context) ([]*State, []*State, []*State) {
	queueIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelQueue)
	codingIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	reviewIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelReview)

	toStates := func(issues []*github.Issue, status string) []*State {
		states := make([]*State, 0, len(issues))
		for _, i := range issues {
			states = append(states, &State{Issue: i, Status: status})
		}
		return states
	}

	// Attach PRs to review states.
	reviewStates := toStates(reviewIssues, "review")
	for _, s := range reviewStates {
		pr, err := p.gh.OpenPRForIssue(ctx, s.Issue.GetNumber())
		if err == nil {
			s.PR = pr
		}
	}

	sortIssuesAsc(queueIssues)
	sortIssuesAsc(codingIssues)
	sortIssuesAsc(reviewIssues)

	return toStates(queueIssues, "queue"),
		toStates(codingIssues, "coding"),
		reviewStates
}

// -- helpers -----------------------------------------------------------------

func sortIssuesAsc(issues []*github.Issue) {
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0 && issues[j].GetNumber() < issues[j-1].GetNumber(); j-- {
			issues[j], issues[j-1] = issues[j-1], issues[j]
		}
	}
}
