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
	"github.com/BlackbirdWorks/copilot-autocode/resolver"
	"github.com/google/go-github/v68/github"
)

// issueDisplayInfo holds the human-readable status for one issue, computed
// entirely from live GitHub data during a single tick (never persisted between
// ticks).  This keeps the app stateless: restarting it produces exactly the
// same display because all data is re-read from GitHub.
type issueDisplayInfo struct {
	current      string    // e.g. "Waiting on coding agent to start"
	next         string    // e.g. "Poke Copilot"
	nextActionAt time.Time // when 'next' fires; zero = no countdown
}

// State is the poller's high-level understanding of a single issue.
type State struct {
	Issue         *github.Issue
	PR            *github.PullRequest
	Status        string    // "queue" | "coding" | "review"
	Message       string    // last action taken
	CurrentStatus string    // human-readable current phase
	NextAction    string    // human-readable next action label
	NextActionAt  time.Time // when NextAction fires; zero = no countdown
}

// Event is sent on the Events channel after every poll tick.
type Event struct {
	Queue    []*State
	Coding   []*State
	Review   []*State
	LastRun  time.Time
	Err      error
	Warnings []string // non-fatal warnings, e.g. Copilot assignment failures
}

// Poller orchestrates the Copilot workflow state machine.
type Poller struct {
	cfg    *config.Config
	gh     *ghclient.Client
	token  string // GitHub PAT — used only for local git operations
	Events chan Event
}

// New creates a Poller ready to Start.
func New(cfg *config.Config, gh *ghclient.Client, token string) *Poller {
	return &Poller{
		cfg:    cfg,
		gh:     gh,
		token:  token,
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

	// displayInfo is a per-tick map populated from live GitHub data by each
	// processing step and consumed by snapshot().  It is never persisted
	// between ticks so the app remains fully stateless across restarts.
	displayInfo := make(map[int]*issueDisplayInfo)

	// Step 1: Queue → Coding.
	warnings, err := p.promoteFromQueue(ctx)
	if err != nil {
		evt.Err = fmt.Errorf("promote from queue: %w", err)
	}
	evt.Warnings = warnings

	// Step 2: Coding → Review (detect completed Copilot runs / PR ready).
	if err := p.moveCodingToReview(ctx, displayInfo); err != nil && evt.Err == nil {
		evt.Err = fmt.Errorf("coding→review: %w", err)
	}

	// Step 2.5: Nudge coding issues where Copilot has not started within the
	// configured timeout (no PR opened, no active agent run).
	if err := p.nudgeStuckCodingIssues(ctx, displayInfo); err != nil && evt.Err == nil {
		evt.Err = fmt.Errorf("nudge stuck issues: %w", err)
	}

	// Step 3+4+5: Handle all review-stage PRs.
	if err := p.processReviewPRs(ctx, displayInfo); err != nil && evt.Err == nil {
		evt.Err = fmt.Errorf("review PRs: %w", err)
	}

	// Collect current snapshot for the TUI, enriched with per-issue status.
	evt.Queue, evt.Coding, evt.Review = p.snapshot(ctx, displayInfo)

	// Non-blocking send; drop stale event if channel is full.
	select {
	case p.Events <- evt:
	default:
		<-p.Events
		p.Events <- evt
	}
}

// promoteFromQueue moves issues from ai-queue → ai-coding up to the concurrency limit.
func (p *Poller) promoteFromQueue(ctx context.Context) (warnings []string, err error) {
	coding, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	if err != nil {
		return nil, err
	}
	slots := p.cfg.MaxConcurrentIssues - len(coding)
	if slots <= 0 {
		return nil, nil
	}

	queued, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelQueue)
	if err != nil {
		return nil, err
	}
	// Process oldest-first (lowest issue number = opened earliest).
	sortIssuesAsc(queued)

	for i := 0; i < slots && i < len(queued); i++ {
		issue := queued[i]
		num := issue.GetNumber()
		if err := p.gh.RemoveLabel(ctx, num, p.cfg.LabelQueue); err != nil {
			return warnings, err
		}
		if err := p.gh.AddLabel(ctx, num, p.cfg.LabelCoding); err != nil {
			return warnings, err
		}
		if err := p.gh.AssignCopilot(ctx, num); err != nil {
			// Non-fatal – the copilot user may not exist in the repo.
			// Surface as a TUI warning so it remains visible in alt-screen mode.
			msg := fmt.Sprintf(
				"could not assign %q to issue #%d: %v — verify copilot_user in config.yaml",
				p.cfg.CopilotUser, num, err,
			)
			log.Printf("warning: %s", msg)
			warnings = append(warnings, msg)
		}
	}
	return warnings, nil
}

// moveCodingToReview checks ai-coding issues and moves them to ai-review once
// the associated PR is no longer a draft (Copilot finished initial coding).
// It also populates displayInfo for issues that remain in the coding state so
// the TUI can show an accurate status sub-line.
func (p *Poller) moveCodingToReview(ctx context.Context, displayInfo map[int]*issueDisplayInfo) error {
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
			displayInfo[num] = &issueDisplayInfo{
				current: "Copilot is writing code",
				next:    "Waiting for PR to be ready",
			}
			continue
		}
		// Also wait until no active agent run is still in progress.
		running, err := p.gh.ActiveCopilotAgentRunning(ctx, pr.GetHead().GetSHA())
		if err == nil && running {
			displayInfo[num] = &issueDisplayInfo{
				current: "Agent finalizing code",
				next:    "Waiting for agent to complete",
			}
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

// nudgeStuckCodingIssues detects issues in ai-coding where the Copilot coding
// agent has not opened a PR within the configured timeout, and re-triggers it.
// The orchestrator determines "last activity" as the later of:
//   - when the ai-coding label was applied (read from GitHub issue events), and
//   - when the most recent nudge comment was posted (read from GitHub comments).
//
// Both timestamps come from GitHub, so the countdown is accurate after a
// process restart — the app never stores timing state in memory.
//
// If the number of nudges for the current coding cycle reaches
// CopilotInvokeMaxRetries the issue is returned to ai-queue with an
// explanatory comment so a human can investigate.
func (p *Poller) nudgeStuckCodingIssues(ctx context.Context, displayInfo map[int]*issueDisplayInfo) error {
	coding, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	if err != nil {
		return err
	}

	timeout := time.Duration(p.cfg.CopilotInvokeTimeoutSeconds) * time.Second

	for _, issue := range coding {
		num := issue.GetNumber()

		// Skip issues where Copilot has already opened a PR (handled by
		// moveCodingToReview which already set their displayInfo entry).
		pr, err := p.gh.OpenPRForIssue(ctx, num)
		if err != nil {
			log.Printf("warning: nudge check: could not look up PR for issue #%d: %v", num, err)
			continue
		}
		if pr != nil {
			continue
		}

		// Set a fallback status (no countdown) in case timing data is
		// unavailable; overwritten below when we have full GitHub data.
		displayInfo[num] = &issueDisplayInfo{
			current: "Waiting on coding agent to start",
			next:    "Poke Copilot",
		}

		// Read the label-applied timestamp from GitHub issue events so the
		// countdown is accurate even after a process restart.
		codingAt, err := p.gh.CodingLabeledAt(ctx, num, p.cfg.LabelCoding)
		if err != nil || codingAt.IsZero() {
			if err != nil {
				log.Printf("warning: nudge check: could not determine coding label time for issue #%d: %v", num, err)
			}
			continue
		}

		// Count nudges sent since the coding label was applied (so that the
		// counter resets if the issue cycles back through the queue).
		nudgeCount, err := p.gh.CountNudgesSince(ctx, num, codingAt)
		if err != nil {
			log.Printf("warning: nudge check: could not count nudges for issue #%d: %v", num, err)
			continue
		}

		// Use the most recent nudge comment timestamp as "last activity" so
		// the cooldown window resets after each nudge attempt.
		// Both this and codingAt are read from GitHub — fully stateless.
		lastNudge, err := p.gh.LastNudgeAt(ctx, num)
		if err != nil {
			log.Printf("warning: nudge check: could not fetch last nudge time for issue #%d: %v", num, err)
			continue
		}

		lastActivity := codingAt
		if lastNudge.After(lastActivity) {
			lastActivity = lastNudge
		}

		deadline := lastActivity.Add(timeout)
		if time.Since(lastActivity) < timeout {
			// Still within the wait window — set accurate status with countdown.
			displayInfo[num] = &issueDisplayInfo{
				current:      "Waiting on coding agent to start",
				next:         "Poke Copilot",
				nextActionAt: deadline,
			}
			continue
		}

		if nudgeCount >= p.cfg.CopilotInvokeMaxRetries {
			// Exhausted all nudge attempts — return the issue to the queue.
			log.Printf("issue #%d: Copilot did not start after %d nudge attempt(s); returning to ai-queue",
				num, nudgeCount)
			displayInfo[num] = &issueDisplayInfo{
				current: fmt.Sprintf("No response after %d nudge(s) — returning to queue", nudgeCount),
			}
			notice := fmt.Sprintf(
				"⚠️ copilot-autocode: Copilot has not started after %d nudge attempt(s). "+
					"Returning this issue to the queue for manual review. "+
					"Check that `copilot_user` in config.yaml is correct and that "+
					"the GitHub Copilot coding agent is enabled for this repository.",
				nudgeCount,
			)
			if err := p.gh.PostComment(ctx, num, notice); err != nil {
				log.Printf("warning: could not post exhaustion notice on issue #%d: %v", num, err)
			}
			if err := p.gh.RemoveLabel(ctx, num, p.cfg.LabelCoding); err != nil {
				return err
			}
			if err := p.gh.AddLabel(ctx, num, p.cfg.LabelQueue); err != nil {
				return err
			}
			continue
		}

		// Send a nudge: post a visible comment that @-mentions Copilot (which
		// re-triggers the coding agent) and re-assign to fire a fresh event.
		log.Printf("issue #%d: no Copilot activity detected after %s; nudging (attempt %d of %d)",
			num, timeout, nudgeCount+1, p.cfg.CopilotInvokeMaxRetries)
		displayInfo[num] = &issueDisplayInfo{
			current: fmt.Sprintf("Nudging Copilot (attempt %d of %d)", nudgeCount+1, p.cfg.CopilotInvokeMaxRetries),
			next:    "Waiting for response",
		}
		nudgeBody := formatFallbackPrompt(p.cfg.FallbackIssueInvokePrompt, issue)
		comment := fmt.Sprintf(
			"@%s %s\n%s",
			p.cfg.CopilotUser,
			nudgeBody,
			ghclient.CopilotNudgeCommentMarker,
		)
		if err := p.gh.PostComment(ctx, num, comment); err != nil {
			return err
		}
		if err := p.gh.ReassignCopilot(ctx, num); err != nil {
			log.Printf("warning: could not re-assign %q to issue #%d during nudge: %v",
				p.cfg.CopilotUser, num, err)
		}
	}
	return nil
}

// processReviewPRs runs steps 3, 4, and 5 for every ai-review issue.
func (p *Poller) processReviewPRs(ctx context.Context, displayInfo map[int]*issueDisplayInfo) error {
	reviewing, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelReview)
	if err != nil {
		return err
	}
	for _, issue := range reviewing {
		if err := p.processOne(ctx, issue, displayInfo); err != nil {
			log.Printf("warning: error processing issue #%d: %v", issue.GetNumber(), err)
		}
	}
	return nil
}

// processOne handles a single ai-review issue through steps 3, 4, and 5.
// It also populates displayInfo[issueNum] so the TUI can show the current
// phase and next action for this issue.
func (p *Poller) processOne(ctx context.Context, issue *github.Issue, displayInfo map[int]*issueDisplayInfo) error {
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
		// Wait if an agent is already working on this.
		running, _ := p.gh.ActiveCopilotAgentRunning(ctx, pr.GetHead().GetSHA())
		if running {
			displayInfo[num] = &issueDisplayInfo{
				current: "Agent resolving merge conflicts",
				next:    "Waiting for agent",
			}
			return nil
		}

		// Count how many @copilot merge-conflict prompts have been sent so far
		// (marker embedded in each comment survives process restarts).
		attempts, err := p.gh.CountMergeConflictAttempts(ctx, pr.GetNumber())
		if err != nil {
			return err
		}

		if attempts >= p.cfg.MaxMergeConflictRetries {
			// @copilot has failed too many times — resolve locally with the AI CLI.
			log.Printf("PR#%d: %d merge-conflict @copilot attempt(s) exhausted; "+
				"running local AI resolution via %q",
				pr.GetNumber(), attempts, p.cfg.AIMergeResolverCmd)

			displayInfo[num] = &issueDisplayInfo{
				current: "Running local AI merge resolution",
				next:    "Pushing resolved changes",
			}
			prd := resolver.PRDetails{
				Owner:      p.cfg.GitHubOwner,
				Repo:       p.cfg.GitHubRepo,
				HeadBranch: pr.GetHead().GetRef(),
				BaseBranch: pr.GetBase().GetRef(),
			}
			if err := resolver.RunLocalResolution(ctx, p.token, prd, p.cfg); err != nil {
				log.Printf("warning: local AI merge resolution failed for PR#%d: %v",
					pr.GetNumber(), err)
				return nil
			}
			notice := fmt.Sprintf(
				"ℹ️ Merge conflicts were resolved locally by copilot-autocode using `%s`.",
				p.cfg.AIMergeResolverCmd)
			if err := p.gh.PostComment(ctx, pr.GetNumber(), notice); err != nil {
				log.Printf("warning: failed to post local-resolution notice on PR#%d: %v",
					pr.GetNumber(), err)
			}
			return nil
		}

		// Still within the @copilot retry budget — ask it to fix conflicts.
		// Embed the marker so future ticks can count this attempt.
		displayInfo[num] = &issueDisplayInfo{
			current: "Merge conflicts detected",
			next:    "Asked Copilot to fix",
		}
		comment := p.cfg.MergeConflictPrompt + "\n" + ghclient.MergeConflictCommentMarker
		if err := p.gh.PostComment(ctx, pr.GetNumber(), comment); err != nil {
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
		displayInfo[num] = &issueDisplayInfo{
			current: "Copilot agent is working",
			next:    "Waiting for agent to complete",
		}
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
		displayInfo[num] = &issueDisplayInfo{
			current: "CI failing",
			next:    "Asked Copilot to fix",
		}
		body := p.buildCIFixMessage(workflowName, failedJobs)
		if err := p.gh.PostReviewComment(ctx, pr.GetNumber(), body); err != nil {
			return err
		}
		return nil
	}

	if !allOK {
		// Still running or no runs yet.
		displayInfo[num] = &issueDisplayInfo{
			current: "Waiting for CI",
			next:    "Re-checking next poll",
		}
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
		displayInfo[num] = &issueDisplayInfo{
			current: fmt.Sprintf("CI passing · refinement %d of %d", sent+1, p.cfg.MaxRefinementRounds),
			next:    "Waiting for Copilot feedback",
		}
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
	displayInfo[num] = &issueDisplayInfo{
		current: "All checks passed",
		next:    "Approving and merging",
	}
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

// snapshot builds the current state for the TUI, enriching each State with
// the human-readable status computed by the processing steps above.
func (p *Poller) snapshot(ctx context.Context, displayInfo map[int]*issueDisplayInfo) ([]*State, []*State, []*State) {
	queueIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelQueue)
	codingIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	reviewIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelReview)

	toStates := func(issues []*github.Issue, status string) []*State {
		states := make([]*State, 0, len(issues))
		for _, i := range issues {
			s := &State{Issue: i, Status: status}
			if status == "queue" {
				// Queue items have a fixed status — no GitHub data needed.
				s.CurrentStatus = "Waiting to be assigned"
				s.NextAction = "Assign Copilot"
			} else if info, ok := displayInfo[i.GetNumber()]; ok {
				s.CurrentStatus = info.current
				s.NextAction = info.next
				s.NextActionAt = info.nextActionAt
			}
			states = append(states, s)
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

// formatFallbackPrompt expands the well-known placeholders in the configured
// FallbackIssueInvokePrompt with live data from the given issue:
//
//	{issue_number} → issue number (e.g. "42")
//	{issue_title}  → issue title
//	{issue_url}    → HTML URL of the issue on GitHub
func formatFallbackPrompt(template string, issue *github.Issue) string {
	r := strings.NewReplacer(
		"{issue_number}", fmt.Sprintf("%d", issue.GetNumber()),
		"{issue_title}", issue.GetTitle(),
		"{issue_url}", issue.GetHTMLURL(),
	)
	return r.Replace(template)
}
