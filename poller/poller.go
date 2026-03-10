// Package poller implements the state machine that drives the Copilot
// Orchestrator workflow.  It runs as a background goroutine and uses GitHub
// labels, PR states, and workflow-run statuses as the single source of truth.
package poller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/google/go-github/v68/github"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/pkgs/logger"
)

// IssueDisplayInfo holds the human-readable status for one issue, computed
// entirely from live GitHub data during a single tick (never persisted between
// ticks).  This keeps the app stateless: restarting it produces exactly the
// same display because all data is re-read from GitHub.
type IssueDisplayInfo struct {
	Current         string // e.g. "Waiting on coding agent to start"
	Next            string // e.g. "Poke Copilot"
	NextActionAt    time.Time
	PR              *github.PullRequest
	RefinementCount int
	RefinementMax   int
	AgentStatus     string // "pending" | "success" | "failed"
	MergeLogPath    string // path to the per-PR merge resolution log (if any)
	CICompleted     int    // number of completed CI workflow runs
	CITotal         int    // total number of CI workflow runs
	CIPassed        int    // number of successful CI runs (conclusion=success/skipped/neutral)
	CIFailed        int    // number of failed CI runs
}

// State is the poller's high-level understanding of a single issue.
type State struct {
	Issue           *github.Issue
	PR              *github.PullRequest
	Status          string // "queue" | "coding" | "review"
	Message         string // last action taken
	CurrentStatus   string // human-readable current phase
	NextAction      string // human-readable next action label
	NextActionAt    time.Time
	RefinementCount int
	RefinementMax   int
	AgentStatus     string // "pending" | "success" | "failed"
	MergeLogPath    string // path to the per-PR merge resolution log (if any)
	CICompleted     int    // number of completed CI workflow runs
	CITotal         int    // total number of CI workflow runs
	CIPassed        int    // number of successful CI runs
	CIFailed        int    // number of failed CI runs
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

// Command is a request sent from the TUI (or other callers) to the Poller.
type Command struct {
	Action   string // "retry-merge" | "takeover" | "rerun-ci" | "priority-up" | "priority-down"
	PRNum    int    // the PR number to act on (used by retry-merge, rerun-ci)
	IssueNum int    // the issue number to act on (used by takeover, priority-up/down)
}

// Poller orchestrates the Copilot workflow state machine.
type Poller struct {
	cfg            *config.Config
	gh             *ghclient.Client
	token          string // GitHub PAT — used only for local git operations
	Events         chan Event
	Commands       chan Command
	approveRetries map[int64]int
	priorities     map[int]int // issue number → priority offset (higher = promoted first)
	mu             sync.Mutex  // protects approveRetries and priorities across concurrent calls
}

// New creates a Poller ready to Start.
func New(cfg *config.Config, gh *ghclient.Client, token string) *Poller {
	return &Poller{
		cfg:            cfg,
		gh:             gh,
		token:          token,
		Events:         make(chan Event, 1),
		Commands:       make(chan Command, 10),
		approveRetries: make(map[int64]int),
		priorities:     make(map[int]int),
	}
}

// Cfg returns the config used by this Poller, exposed for testing.
func (p *Poller) Cfg() *config.Config { return p.cfg }

// Start launches the polling goroutine.  It runs until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	go func() {
		// Ensure labels exist before the first tick.
		if err := p.gh.EnsureLabelsExist(ctx); err != nil {
			logger.Load(ctx).WarnContext(ctx, "could not ensure labels exist", slog.Any("err", err))
		}

		// Run once immediately in the background so the UI doesn't hang.
		p.Tick(ctx)

		ticker := time.NewTicker(time.Duration(p.cfg.PollIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-p.Commands:
				p.handleCommand(ctx, cmd)
			case <-ticker.C:
				p.drainCommands(ctx)
				p.Tick(ctx)
			}
		}
	}()
}

// handleCommand processes a single TUI command.
func (p *Poller) handleCommand(ctx context.Context, cmd Command) {
	switch cmd.Action {
	case "retry-merge":
		p.retryMergeResolution(ctx, cmd.PRNum)
	case "takeover":
		p.takeoverIssue(ctx, cmd.IssueNum)
	case "rerun-ci":
		p.rerunCI(ctx, cmd.PRNum)
	case "priority-up":
		p.adjustPriority(ctx, cmd.IssueNum, 1)
	case "priority-down":
		p.adjustPriority(ctx, cmd.IssueNum, -1)
	default:
		logger.Load(ctx).WarnContext(ctx, "unknown command", slog.String("action", cmd.Action))
	}
}

// drainCommands processes any commands queued since the last tick.
func (p *Poller) drainCommands(ctx context.Context) {
	for {
		select {
		case cmd := <-p.Commands:
			p.handleCommand(ctx, cmd)
		default:
			return
		}
	}
}

// retryMergeResolution deletes the failure marker comment from a PR so the
// next tick will re-attempt local AI merge resolution.
func (p *Poller) retryMergeResolution(ctx context.Context, prNum int) {
	logger.Load(ctx).InfoContext(ctx, "retrying local merge resolution", slog.Int("pr", prNum))
	if err := p.gh.DeleteCommentContaining(ctx, prNum, ghclient.LocalResolutionFailedMarker); err != nil {
		logger.Load(ctx).WarnContext(ctx, "failed to delete merge resolution failure marker", slog.Int("pr", prNum), slog.Any("err", err))
	}
}

// takeoverIssue adds the manual-takeover label and removes all workflow labels
// so the orchestrator stops managing the issue.
func (p *Poller) takeoverIssue(ctx context.Context, issueNum int) {
	logger.Load(ctx).InfoContext(ctx, "manual takeover requested", slog.Int("issue", issueNum))
	if err := p.gh.AddLabel(ctx, issueNum, p.cfg.LabelTakeover); err != nil {
		logger.Load(ctx).WarnContext(ctx, "failed to add takeover label", slog.Int("issue", issueNum), slog.Any("err", err))
		return
	}
	for _, lbl := range []string{p.cfg.LabelQueue, p.cfg.LabelCoding, p.cfg.LabelReview} {
		_ = p.gh.RemoveLabel(ctx, issueNum, lbl)
	}
}

// rerunCI looks up the PR's head SHA and re-runs all failed/completed workflow runs.
func (p *Poller) rerunCI(ctx context.Context, prNum int) {
	logger.Load(ctx).InfoContext(ctx, "force re-run CI requested", slog.Int("pr", prNum))
	sha, err := p.gh.GetPRHeadSHA(ctx, prNum)
	if err != nil {
		logger.Load(ctx).WarnContext(ctx, "could not get PR head SHA for rerun", slog.Int("pr", prNum), slog.Any("err", err))
		return
	}
	runs, err := p.gh.LatestWorkflowRun(ctx, sha)
	if err != nil {
		logger.Load(ctx).WarnContext(ctx, "could not list workflow runs for rerun", slog.Int("pr", prNum), slog.Any("err", err))
		return
	}
	for _, r := range runs {
		c := r.GetConclusion()
		if c == "failure" || c == "timed_out" || c == "cancelled" || c == "action_required" {
			if rerunErr := p.gh.RerunWorkflow(ctx, r.GetID()); rerunErr != nil {
				logger.Load(ctx).WarnContext(ctx, "failed to rerun workflow", slog.Int64("run", r.GetID()), slog.Any("err", rerunErr))
			}
		}
	}
}

// adjustPriority changes the priority offset for an issue in the queue.
func (p *Poller) adjustPriority(ctx context.Context, issueNum, delta int) {
	p.mu.Lock()
	p.priorities[issueNum] += delta
	newPri := p.priorities[issueNum]
	if newPri == 0 {
		delete(p.priorities, issueNum)
	}
	p.mu.Unlock()
	logger.Load(ctx).InfoContext(ctx, "priority adjusted", slog.Int("issue", issueNum), slog.Int("priority", newPri))
}

// FetchAllIssues returns the queue, coding, and reviewing issue lists fetched
// from GitHub in a single concurrent batch — three parallel API calls instead
// of the up-to-six sequential calls that the old per-phase fetch pattern used.
func (p *Poller) FetchAllIssues(ctx context.Context) ([]*github.Issue, []*github.Issue, []*github.Issue, error) {
	type result struct {
		issues []*github.Issue
		err    error
	}
	labels := [3]string{p.cfg.LabelQueue, p.cfg.LabelCoding, p.cfg.LabelReview}
	results := [3]result{}
	var wg sync.WaitGroup
	wg.Add(3)
	for i, label := range labels {
		go func(idx int, l string) {
			defer wg.Done()
			issues, fetchErr := p.gh.IssuesByLabel(ctx, l)
			results[idx] = result{issues, fetchErr}
		}(i, label)
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			return nil, nil, nil, fmt.Errorf("fetch %s issues: %w", labels[i], r.err)
		}
	}
	return results[0].issues, results[1].issues, results[2].issues, nil
}

// Tick executes one full state-machine cycle and sends an Event.
func (p *Poller) Tick(ctx context.Context) {
	evt := Event{LastRun: time.Now()}

	// Bulk-fetch all tracked issue lists in a single concurrent batch.
	queue, coding, reviewing, err := p.FetchAllIssues(ctx)
	if err != nil {
		evt.Err = err
		select {
		case p.Events <- evt:
		default:
			<-p.Events
			p.Events <- evt
		}
		return
	}

	// Deduplicate across lists: Priority: reviewing > coding > queue.
	coding, reviewing = DeduplicateIssueLists(queue, coding, reviewing)

	// Process issues in parallel.
	type diEntry struct {
		num  int
		info *IssueDisplayInfo
	}
	entries := make(chan diEntry, len(coding)+len(reviewing))

	var wg sync.WaitGroup

	// Phase 2+2.5: coding issues.
	for _, issue := range coding {
		wg.Add(1)
		go func(i *github.Issue) {
			defer wg.Done()
			local := make(map[int]*IssueDisplayInfo)
			if gErr := p.ProcessCodingIssue(ctx, i, local); gErr != nil {
				logger.Load(ctx).ErrorContext(ctx, "error processing coding issue",
					slog.Int("issue", i.GetNumber()), slog.Any("err", gErr))
			}
			for k, v := range local {
				entries <- diEntry{k, v}
			}
		}(issue)
	}

	// Phase 3+: review PRs.
	for _, issue := range reviewing {
		wg.Add(1)
		go func(i *github.Issue) {
			defer wg.Done()
			local := make(map[int]*IssueDisplayInfo)
			if gErr := p.ProcessOne(ctx, i, local); gErr != nil {
				logger.Load(ctx).ErrorContext(ctx, "error processing issue",
					slog.Int("issue", i.GetNumber()), slog.Any("err", gErr))
			}
			for k, v := range local {
				entries <- diEntry{k, v}
			}
		}(issue)
	}

	wg.Wait()
	close(entries)

	displayInfo := make(map[int]*IssueDisplayInfo, len(coding)+len(reviewing))
	for e := range entries {
		displayInfo[e.num] = e.info
	}

	// Phase 1: Queue → Coding.
	if err := p.PromoteFromQueue(ctx, queue); err != nil {
		evt.Err = fmt.Errorf("promote from queue: %w", err)
	}

	// Collect current snapshot for the TUI.
	evt.Queue, evt.Coding, evt.Review = p.Snapshot(ctx, displayInfo)

	// Non-blocking send.
	select {
	case p.Events <- evt:
	default:
		<-p.Events
		p.Events <- evt
	}
}

// PromoteFromQueue moves issues from ai-queue → ai-coding up to the concurrency limit.
func (p *Poller) PromoteFromQueue(ctx context.Context, queue []*github.Issue) error {
	var nCoding, nReview int
	if coding, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding); err == nil {
		nCoding = len(coding)
	}
	if reviewing, err := p.gh.IssuesByLabel(ctx, p.cfg.LabelReview); err == nil {
		nReview = len(reviewing)
	}
	slots := p.cfg.MaxConcurrentIssues - (nCoding + nReview)
	if slots <= 0 {
		return nil
	}

	p.SortQueueByPriority(queue)

	for i := 0; i < slots && i < len(queue); i++ {
		issue := queue[i]
		num := issue.GetNumber()

		existingPR, err := p.gh.OpenPRForIssue(ctx, issue)
		if err == nil && existingPR != nil {
			logger.Load(ctx).InfoContext(ctx, "found existing PR; skipping invoke",
				slog.Int("issue", num), slog.Int("pr", existingPR.GetNumber()))
			if err := p.gh.SwapLabel(ctx, num, p.cfg.LabelQueue, p.cfg.LabelCoding); err != nil {
				return err
			}
			continue
		}

		if err := p.gh.SwapLabel(ctx, num, p.cfg.LabelQueue, p.cfg.LabelCoding); err != nil {
			return err
		}
		prompt := FormatFallbackPrompt(p.cfg.FallbackIssueInvokePrompt, issue)
		jobID, capiErr := p.gh.InvokeCopilotAgent(ctx, prompt, issue.GetTitle(), num, issue.GetHTMLURL())
		if capiErr != nil {
			logger.Load(ctx).ErrorContext(ctx, "could not invoke copilot agent",
				slog.Int("issue", num), slog.Any("err", capiErr))
		}

		comment := fmt.Sprintf(
			"copilot-autocode: agent task created for issue #%d (initial invoke).\n%s",
			num, ghclient.CopilotNudgeCommentMarker,
		)
		if jobID != "" {
			comment += fmt.Sprintf("\n%s%s -->", ghclient.CopilotJobIDCommentMarker, jobID)
		}
		if err := p.gh.PostComment(ctx, num, comment); err != nil {
			logger.Load(ctx).ErrorContext(ctx, "could not post tracking comment",
				slog.Int("issue", num), slog.Any("err", err))
		}
	}
	return nil
}

// ProcessCodingIssue handles a single ai-coding issue.
func (p *Poller) ProcessCodingIssue(ctx context.Context, issue *github.Issue, displayInfo map[int]*IssueDisplayInfo) error {
	num := issue.GetNumber()
	pr, err := p.gh.OpenPRForIssue(ctx, issue)
	if err != nil {
		logger.Load(ctx).WarnContext(ctx, "could not look up PR", slog.Int("issue", num), slog.Any("err", err))
	}
	if pr != nil {
		if _, ok := displayInfo[num]; !ok {
			displayInfo[num] = &IssueDisplayInfo{PR: pr}
		}
		return p.ProcessCodingPR(ctx, pr, num, displayInfo)
	}

	mergedPR, mErr := p.gh.MergedPRForIssue(ctx, issue)
	if mErr == nil && mergedPR != nil {
		logger.Load(ctx).InfoContext(ctx, "PR merged externally; closing issue",
			slog.Int("issue", num), slog.Int("pr", mergedPR.GetNumber()))
		displayInfo[num] = &IssueDisplayInfo{
			Current:     "PR merged externally — closing issue",
			PR:          mergedPR,
			AgentStatus: "success",
		}
		_ = p.gh.RemoveLabel(ctx, num, p.cfg.LabelCoding)
		return p.gh.CloseIssue(ctx, num)
	}

	return p.NudgeSingleCodingIssue(ctx, issue, displayInfo, time.Duration(p.cfg.CopilotInvokeTimeoutSeconds)*time.Second)
}

// ProcessCodingPR handles the PR-present path of ProcessCodingIssue.
func (p *Poller) ProcessCodingPR(ctx context.Context, pr *github.PullRequest, num int, displayInfo map[int]*IssueDisplayInfo) error {
	sha := pr.GetHead().GetSHA()
	running, err := p.gh.AnyWorkflowRunActive(ctx, sha)
	if err == nil && running {
		label := "Agent finalizing code"
		if p.gh.IsPRDraft(pr) {
			label = "Copilot is writing code"
		}
		displayInfo[num] = &IssueDisplayInfo{
			Current:     label,
			Next:        "Waiting for agent to complete",
			PR:          pr,
			AgentStatus: "pending",
		}
		return nil
	}
	if p.gh.IsPRDraft(pr) {
		return p.ProcessDraftPR(ctx, pr, num, displayInfo)
	}
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelCoding, p.cfg.LabelReview)
}

// ProcessDraftPR promotes a draft PR when the agent is done.
func (p *Poller) ProcessDraftPR(ctx context.Context, pr *github.PullRequest, num int, displayInfo map[int]*IssueDisplayInfo) error {
	t := &PRTask{P: p, PR: pr, Num: num, Sha: pr.GetHead().GetSHA(), DisplayInfo: displayInfo}
	if stop, err := t.HandleTimeout(ctx); err != nil || stop {
		return err
	}
	logger.Load(ctx).InfoContext(ctx, "agent finished draft push; marking ready", slog.Int("pr", pr.GetNumber()))
	if err := p.gh.MarkPRReady(ctx, pr); err != nil {
		logger.Load(ctx).WarnContext(ctx, "could not mark PR as ready", slog.Int("pr", pr.GetNumber()), slog.Any("err", err))
		displayInfo[num] = &IssueDisplayInfo{
			Current:     "Agent completed but PR still draft — retrying",
			Next:        "Retry mark ready next poll",
			PR:          pr,
			AgentStatus: "pending",
		}
		return nil
	}
	displayInfo[num] = &IssueDisplayInfo{
		Current: "Agent completed — PR marked ready",
		Next:    "Transitioning to review",
		PR:      pr,
	}
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelCoding, p.cfg.LabelReview)
}

// NudgeSingleCodingIssue handles timeout/retry/nudge for coding issues without a PR.
func (p *Poller) NudgeSingleCodingIssue(ctx context.Context, issue *github.Issue, displayInfo map[int]*IssueDisplayInfo, timeout time.Duration) error {
	num := issue.GetNumber()
	displayInfo[num] = &IssueDisplayInfo{
		Current: "Agent assigned, awaiting PR",
		Next:    "Nudge if no PR",
	}

	codingAt, err := p.gh.CodingLabeledAt(ctx, num, p.cfg.LabelCoding)
	if err != nil || codingAt.IsZero() {
		return nil
	}

	nudgeCount, err := p.gh.CountNudgesSince(ctx, num, codingAt)
	if err != nil {
		return nil
	}

	lastNudge, err := p.gh.LastNudgeAt(ctx, num)
	if err != nil {
		return nil
	}

	lastActivity := codingAt
	if lastNudge.After(lastActivity) {
		lastActivity = lastNudge
	}
	if lastActivity.IsZero() {
		lastActivity = timeNow()
	}

	deadline := lastActivity.Add(timeout)
	if time.Since(lastActivity) < timeout {
		statusLabel := "Agent assigned, awaiting PR"
		if nudgeCount > 0 {
			statusLabel = fmt.Sprintf("Agent invoked via API (attempt %d)", nudgeCount+1)
		}
		displayInfo[num] = &IssueDisplayInfo{
			Current:      statusLabel,
			Next:         "Nudge if no PR",
			NextActionAt: deadline,
			AgentStatus:  "pending",
		}
		return nil
	}

	if nudgeCount >= p.cfg.CopilotInvokeMaxRetries {
		return p.HandleNudgeExhaustion(ctx, num, nudgeCount, displayInfo)
	}

	if pr, err := p.gh.OpenPRForIssue(ctx, issue); err == nil && pr != nil {
		return nil
	}

	if p.IsAgentActive(ctx, num) {
		displayInfo[num] = &IssueDisplayInfo{
			Current:     "Agent actively running — waiting",
			Next:        "Re-check next poll",
			AgentStatus: "pending",
		}
		return nil
	}

	return p.RequestNudge(ctx, issue, num, nudgeCount, timeout, displayInfo)
}

// IsAgentActive checks if any Copilot job or repository-level run is active.
func (p *Poller) IsAgentActive(ctx context.Context, num int) bool {
	jobID, _ := p.gh.LatestCopilotJobID(ctx, num)
	if jobID != "" {
		status, err := p.gh.GetCopilotJobStatus(ctx, jobID)
		if err == nil && status != nil {
			s := status.Status
			if s == "in_progress" || s == "running" || s == "queued" || s == "requested" || s == "pending" {
				return true
			}
		}
	}
	if active, aErr := p.gh.HasActiveCopilotRun(ctx); aErr == nil && active {
		return true
	}
	return false
}

// RequestNudge re-invokes the agent via CAPI.
func (p *Poller) RequestNudge(ctx context.Context, issue *github.Issue, num, nudgeCount int, timeout time.Duration, displayInfo map[int]*IssueDisplayInfo) error {
	displayInfo[num] = &IssueDisplayInfo{
		Current: fmt.Sprintf("Invoking agent via Copilot API (attempt %d of %d)", nudgeCount+1, p.cfg.CopilotInvokeMaxRetries),
		Next:    "Waiting for response",
	}

	nudgeBody := FormatFallbackPrompt(p.cfg.FallbackIssueInvokePrompt, issue)
	jobID, invokeErr := p.gh.InvokeCopilotAgent(ctx, nudgeBody, issue.GetTitle(), num, issue.GetHTMLURL())
	if invokeErr != nil {
		logger.Load(ctx).ErrorContext(ctx, "could not invoke copilot agent", slog.Int("issue", num), slog.Any("err", invokeErr))
	}

	comment := fmt.Sprintf(
		"copilot-autocode: agent task created for issue #%d (attempt %d of %d).\n%s",
		num, nudgeCount+1, p.cfg.CopilotInvokeMaxRetries,
		ghclient.CopilotNudgeCommentMarker,
	)
	if jobID != "" {
		comment += fmt.Sprintf("\n%s%s -->", ghclient.CopilotJobIDCommentMarker, jobID)
	}
	return p.gh.PostComment(ctx, num, comment)
}

type agentTimeoutCfg struct {
	countFn      func(ctx context.Context, prNum int) (int, error)
	nudgeMarker  string
	promptKind   string
	noticeFormat string
	statusVerb   string
}

func (p *Poller) continueTimeoutCfg(promptKind, noticeFormat string) agentTimeoutCfg {
	return agentTimeoutCfg{
		countFn:      p.gh.CountAgentContinueComments,
		nudgeMarker:  ghclient.AgentContinueCommentMarker,
		statusVerb:   "retries",
		promptKind:   promptKind,
		noticeFormat: noticeFormat,
	}
}

func latestOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func runNames(runs []*github.WorkflowRun) []string {
	names := make([]string, len(runs))
	for i, r := range runs {
		names[i] = r.GetName()
	}
	return names
}

func (p *Poller) incApproveRetry(runID int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.approveRetries[runID]++
	return p.approveRetries[runID]
}

func (p *Poller) clearApproveRetry(runID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.approveRetries, runID)
}

func (p *Poller) WaitForAgentCycle(ctx context.Context, pr *github.PullRequest, num int, postedAt time.Time, cfg agentTimeoutCfg, current string, displayInfo map[int]*IssueDisplayInfo) {
	lastContinue, _ := p.gh.LastAgentContinueAt(ctx, pr.GetNumber())
	lastActivity := latestOf(postedAt, lastContinue)
	if p.HandleAgentTimeout(ctx, pr, num, lastActivity, cfg, displayInfo) {
		return
	}
	displayInfo[num] = &IssueDisplayInfo{
		Current:     current,
		Next:        "Waiting for agent to push",
		PR:          pr,
		AgentStatus: "pending",
	}
}

func (p *Poller) HandleAgentTimeout(ctx context.Context, pr *github.PullRequest, num int, lastActivity time.Time, cfg agentTimeoutCfg, displayInfo map[int]*IssueDisplayInfo) bool {
	if time.Since(lastActivity) <= time.Duration(p.cfg.CopilotInvokeTimeoutSeconds)*time.Second {
		return false
	}
	continueCount, _ := cfg.countFn(ctx, pr.GetNumber())
	if continueCount >= p.cfg.MaxAgentContinueRetries {
		_ = p.gh.PostComment(ctx, num, fmt.Sprintf(cfg.noticeFormat, continueCount))
		displayInfo[num] = &IssueDisplayInfo{
			Current:     fmt.Sprintf("Agent unresponsive after %d %s — left in review", continueCount, cfg.statusVerb),
			PR:          pr,
			AgentStatus: "failed",
		}
		return true
	}
	_ = p.gh.PostComment(ctx, pr.GetNumber(), p.cfg.AgentContinuePrompt+"\n"+cfg.nudgeMarker)
	return false
}

func (p *Poller) HandleMissingPR(ctx context.Context, issue *github.Issue, num int, displayInfo map[int]*IssueDisplayInfo) error {
	mergedPR, mErr := p.gh.MergedPRForIssue(ctx, issue)
	if mErr == nil && mergedPR != nil {
		displayInfo[num] = &IssueDisplayInfo{
			Current:     "PR merged manually - closing issue",
			PR:          mergedPR,
			AgentStatus: "success",
		}
		for _, lbl := range []string{p.cfg.LabelReview, p.cfg.LabelCoding, p.cfg.LabelQueue} {
			_ = p.gh.RemoveLabel(ctx, num, lbl)
		}
		return p.gh.CloseIssue(ctx, num)
	}
	displayInfo[num] = &IssueDisplayInfo{
		Current:     "No PR found - returning to coding",
		AgentStatus: "failed",
	}
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelReview, p.cfg.LabelCoding)
}

func (p *Poller) ProcessOne(ctx context.Context, issue *github.Issue, displayInfo map[int]*IssueDisplayInfo) error {
	pr, err := p.gh.OpenPRForIssue(ctx, issue)
	if err != nil {
		return err
	}
	if pr == nil {
		return p.HandleMissingPR(ctx, issue, issue.GetNumber(), displayInfo)
	}
	if _, ok := displayInfo[issue.GetNumber()]; !ok {
		displayInfo[issue.GetNumber()] = &IssueDisplayInfo{PR: pr}
	}
	t := &PRTask{
		P:           p,
		PR:          pr,
		Num:         issue.GetNumber(),
		Sha:         pr.GetHead().GetSHA(),
		DisplayInfo: displayInfo,
	}
	return t.Run(ctx)
}

func agentTimeoutDelay(cfg *config.Config) time.Duration {
	return time.Duration(cfg.AgentTimeoutRetryDelaySeconds) * time.Second
}

var timeNow = time.Now //nolint:gochecknoglobals

func (p *Poller) BuildRefinementCIPrompt(ctx context.Context, round, maxRounds, issueNum int, anyFail bool, sha string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "@copilot (refinement check %d of %d against issue #%d). %s Please address any review feedback.", round, maxRounds, issueNum, p.cfg.RefinementPrompt)
	if anyFail {
		workflowName, failedJobs, err := p.gh.FailedRunDetails(ctx, sha)
		if err == nil {
			sb.WriteString(BuildCIFailureSection(workflowName, failedJobs))
		}
	}
	sb.WriteString("\n" + ghclient.RefinementCommentMarker)
	sb.WriteString("\n" + ghclient.SHAMarker("refinement", sha))
	return sb.String()
}

func BuildCIFailureSection(workflowName string, failedJobs []ghclient.FailedJobInfo) string {
	var sb strings.Builder
	sb.WriteString("\n\n**Additionally, please fix the following CI failures before pushing:**")
	if workflowName != "" {
		fmt.Fprintf(&sb, "\n**Failing workflow:** %s", workflowName)
	}
	if len(failedJobs) > 0 {
		names := make([]string, len(failedJobs))
		for i, j := range failedJobs {
			names[i] = j.Name
		}
		fmt.Fprintf(&sb, "\n**Failed jobs:** %s", strings.Join(names, ", "))
		for _, job := range failedJobs {
			if job.LogURL != "" {
				fmt.Fprintf(&sb, "\n\n**%s** logs: %s", job.Name, job.LogURL)
			}
		}
	}
	return sb.String()
}

func (p *Poller) Snapshot(ctx context.Context, displayInfo map[int]*IssueDisplayInfo) ([]*State, []*State, []*State) {
	queueIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelQueue)
	codingIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelCoding)
	reviewIssues, _ := p.gh.IssuesByLabel(ctx, p.cfg.LabelReview)

	p.SortQueueByPriority(queueIssues)
	SortIssuesAsc(codingIssues)
	SortIssuesAsc(reviewIssues)

	toStates := func(issues []*github.Issue, status string) []*State {
		states := make([]*State, 0, len(issues))
		for _, i := range issues {
			s := &State{Issue: i, Status: status}
			if status == "queue" {
				s.CurrentStatus = "Waiting to be assigned"
				s.NextAction = "Assign Copilot"
			} else if info, ok := displayInfo[i.GetNumber()]; ok {
				s.CurrentStatus = info.Current
				s.NextAction = info.Next
				s.NextActionAt = info.NextActionAt
				s.PR = info.PR
				s.RefinementCount = info.RefinementCount
				s.RefinementMax = info.RefinementMax
				s.AgentStatus = info.AgentStatus
				s.MergeLogPath = info.MergeLogPath
				s.CICompleted = info.CICompleted
				s.CITotal = info.CITotal
				s.CIPassed = info.CIPassed
				s.CIFailed = info.CIFailed
			}
			states = append(states, s)
		}
		return states
	}

	return toStates(queueIssues, "queue"), toStates(codingIssues, "coding"), toStates(reviewIssues, "review")
}

func DeduplicateIssueLists(queue, coding, reviewing []*github.Issue) ([]*github.Issue, []*github.Issue) {
	seen := make(map[int]struct{}, len(reviewing)+len(queue))
	for _, i := range reviewing {
		seen[i.GetNumber()] = struct{}{}
	}
	filtered := coding[:0]
	for _, i := range coding {
		if _, dup := seen[i.GetNumber()]; !dup {
			filtered = append(filtered, i)
		}
	}
	queueSet := make(map[int]struct{}, len(queue))
	for _, i := range queue {
		queueSet[i.GetNumber()] = struct{}{}
	}
	deduped := filtered[:0]
	for _, i := range filtered {
		if _, dup := queueSet[i.GetNumber()]; !dup {
			deduped = append(deduped, i)
		}
	}
	return deduped, reviewing
}

// SortQueueByPriority sorts issues by priority (highest first), then by issue
// number ascending as a tiebreaker.
func (p *Poller) SortQueueByPriority(issues []*github.Issue) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < len(issues); i++ {
		for j := i + 1; j < len(issues); j++ {
			pi := p.priorities[issues[i].GetNumber()]
			pj := p.priorities[issues[j].GetNumber()]
			if pi < pj || (pi == pj && issues[i].GetNumber() > issues[j].GetNumber()) {
				issues[i], issues[j] = issues[j], issues[i]
			}
		}
	}
}

func SortIssuesAsc(issues []*github.Issue) {
	for i := 0; i < len(issues); i++ {
		for j := i + 1; j < len(issues); j++ {
			if issues[i].GetNumber() > issues[j].GetNumber() {
				issues[i], issues[j] = issues[j], issues[i]
			}
		}
	}
}

func FormatFallbackPrompt(tpl string, issue *github.Issue) string {
	res := strings.ReplaceAll(tpl, "{{.Title}}", issue.GetTitle())
	res = strings.ReplaceAll(res, "{{.Number}}", strconv.Itoa(issue.GetNumber()))
	res = strings.ReplaceAll(res, "{{.URL}}", issue.GetHTMLURL())
	return res
}

func (p *Poller) HandleNudgeExhaustion(ctx context.Context, num, nudgeCount int, displayInfo map[int]*IssueDisplayInfo) error {
	_ = p.gh.PostComment(ctx, num, fmt.Sprintf("copilot-autocode: nudge limit reached (%d). Returning to queue.", nudgeCount))
	displayInfo[num] = &IssueDisplayInfo{
		Current:     fmt.Sprintf("No response after %d nudge(s) — returning to queue", nudgeCount),
		AgentStatus: "failed",
	}
	return p.gh.SwapLabel(ctx, num, p.cfg.LabelCoding, p.cfg.LabelQueue)
}
