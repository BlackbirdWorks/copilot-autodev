package poller

import (
	"context"
	"fmt"
	"strings"

	"log/slog"

	"github.com/google/go-github/v68/github"

	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/pkgs/logger"
	"github.com/BlackbirdWorks/copilot-autocode/resolver"
)

// PRTask is a single-tick execution context for one PR/issue pair.
type PRTask struct {
	P   *Poller
	PR  *github.PullRequest
	Num int
	Sha string

	DisplayInfo map[int]*IssueDisplayInfo

	Sent    int
	AllOK   bool
	AnyFail bool

	ApprovedRuns []*github.WorkflowRun
}

type StepFn func(context.Context) (bool, error)

func (t *PRTask) Run(ctx context.Context) error {
	for _, step := range []StepFn{
		t.SyncBranch,
		t.ApproveRuns,
		t.CheckGates,
		t.WaitForCI,
		t.HandleTimeout,
		t.Refine,
		t.FixCI,
	} {
		if stop, err := step(ctx); err != nil || stop {
			return err
		}
	}
	return t.Merge(ctx)
}

func (t *PRTask) Display(info IssueDisplayInfo) {
	t.DisplayInfo[t.Num] = &info
}

func (t *PRTask) SyncBranch(ctx context.Context) (bool, error) {
	upToDate, err := t.P.gh.PRIsUpToDateWithBase(ctx, t.PR)
	if err != nil {
		return false, err
	}
	if upToDate {
		return false, nil
	}

	if t.PR.GetMergeableState() != "dirty" {
		t.Display(IssueDisplayInfo{
			Current:     "Updating branch from base",
			Next:        "Waiting for update",
			PR:          t.PR,
			AgentStatus: "pending",
		})
		if err := t.P.gh.UpdatePRBranch(ctx, t.PR.GetNumber()); err != nil {
			logger.Load(ctx).WarnContext(ctx, "failed to update PR branch", slog.Int("pr", t.PR.GetNumber()), slog.Any("err", err))
		} else {
			return true, nil
		}
	}

	if running, _ := t.P.gh.AnyWorkflowRunActive(ctx, t.Sha); running {
		t.Display(IssueDisplayInfo{
			Current:     "Agent resolving merge conflicts",
			Next:        "Waiting for agent",
			PR:          t.PR,
			AgentStatus: "pending",
		})
		return true, nil
	}

	attempts, err := t.P.gh.CountMergeConflictAttempts(ctx, t.PR.GetNumber())
	if err != nil {
		return false, err
	}
	if attempts >= t.P.cfg.MaxMergeConflictRetries {
		return t.resolveConflictsLocally(ctx, attempts)
	}
	return t.requestMergeConflictFix(ctx)
}

func (t *PRTask) resolveConflictsLocally(ctx context.Context, attempts int) (bool, error) {
	alreadyTried, _, _ := t.P.gh.HasCommentContaining(ctx, t.PR.GetNumber(), ghclient.LocalResolutionFailedMarker)
	if alreadyTried {
		t.Display(IssueDisplayInfo{
			Current:      "Merge conflicts unresolved — needs manual fix",
			PR:           t.PR,
			AgentStatus:  "failed",
			MergeLogPath: resolver.LogPath(t.PR.GetNumber()),
		})
		return true, nil
	}

	// Check if this specific SHA was already resolved successfully.
	// If it was, GitHub's `mergeable_state` cache is just lagging behind.
	successMarker := ghclient.SHAMarker("local-resolution-success", t.Sha)
	recentlySucceeded, _, _ := t.P.gh.HasCommentContaining(ctx, t.PR.GetNumber(), successMarker)
	if recentlySucceeded {
		t.Display(IssueDisplayInfo{
			Current:     "Merge resolved locally — waiting for PR state update",
			Next:        "Checking next poll",
			PR:          t.PR,
			AgentStatus: "success",
		})
		return true, nil
	}

	t.Display(IssueDisplayInfo{
		Current:     "Running local AI merge resolution",
		Next:        "Pushing resolved changes",
		PR:          t.PR,
		AgentStatus: "pending",
	})

	prd := resolver.PRDetails{
		Owner:      t.P.cfg.GitHubOwner,
		Repo:       t.P.cfg.GitHubRepo,
		HeadBranch: t.PR.GetHead().GetRef(),
		BaseBranch: t.PR.GetBase().GetRef(),
	}
	prNum := t.PR.GetNumber()
	newSha, err := resolver.New().RunLocalResolution(ctx, t.P.token, prd, t.P.cfg, prNum)
	if err != nil {
		logPath := resolver.LogPath(prNum)
		logger.Load(ctx).ErrorContext(ctx, "local AI merge resolution failed",
			slog.String("cmd", t.P.cfg.AIMergeResolverCmd),
			slog.Int("pr", prNum),
			slog.String("log", logPath),
			slog.Any("err", err),
		)
		notice := fmt.Sprintf("copilot-autocode: local AI merge resolution via `%s` failed. Manual conflict resolution is required.\nSee `%s` for details.\n%s\n%s", t.P.cfg.AIMergeResolverCmd, logPath, ghclient.LocalResolutionCommentMarker, ghclient.LocalResolutionFailedMarker)
		_ = t.P.gh.PostComment(ctx, t.PR.GetNumber(), notice)
		// Set MergeLogPath in the display immediately so the user can press
		// 'v' on this tick without waiting for the next poll.
		t.Display(IssueDisplayInfo{
			Current:      "Merge conflicts unresolved — needs manual fix",
			PR:           t.PR,
			AgentStatus:  "failed",
			MergeLogPath: logPath,
		})
		return true, nil
	}
	notice := fmt.Sprintf("copilot-autocode: Merge conflicts were resolved locally by copilot-autocode using `%s`.\n%s\n%s", t.P.cfg.AIMergeResolverCmd, ghclient.LocalResolutionCommentMarker, ghclient.SHAMarker("local-resolution-success", newSha))
	_ = t.P.gh.PostComment(ctx, t.PR.GetNumber(), notice)
	return true, nil
}

func (t *PRTask) requestMergeConflictFix(ctx context.Context) (bool, error) {
	shaTag := ghclient.SHAMarker("merge-conflict", t.Sha)
	alreadyPosted, postedAt, _ := t.P.gh.HasCommentContaining(ctx, t.PR.GetNumber(), shaTag)
	if alreadyPosted {
		lastContinue, _ := t.P.gh.LastMergeConflictContinueAt(ctx, t.PR.GetNumber())
		mergeTimeoutCfg := agentTimeoutCfg{
			countFn:      t.P.gh.CountMergeConflictContinueComments,
			nudgeMarker:  ghclient.MergeConflictContinueCommentMarker,
			promptKind:   "merge-conflict prompt",
			noticeFormat: "copilot-autocode: the Copilot coding agent became unresponsive while resolving merge conflicts and %d nudge(s) were exhausted. The PR has been left open in review for manual inspection.",
			statusVerb:   "nudges",
		}
		t.P.HandleAgentTimeout(ctx, t.PR, t.Num, latestOf(postedAt, lastContinue), mergeTimeoutCfg, t.DisplayInfo)
		t.Display(IssueDisplayInfo{
			Current:     "Merge conflicts - waiting for Copilot",
			Next:        "Re-checking next poll",
			PR:          t.PR,
			AgentStatus: "pending",
		})
		return true, nil
	}

	mergePrompt := t.P.cfg.MergeConflictPrompt
	if !strings.Contains(mergePrompt, "@copilot") {
		mergePrompt = "@copilot " + mergePrompt
	}
	comment := mergePrompt + "\n" + ghclient.MergeConflictCommentMarker + "\n" + shaTag
	t.Display(IssueDisplayInfo{
		Current:     "Merge conflicts detected",
		Next:        "Asked Copilot to fix",
		PR:          t.PR,
		AgentStatus: "pending",
	})
	return true, t.P.gh.PostComment(ctx, t.PR.GetNumber(), comment)
}

func (t *PRTask) ApproveRuns(ctx context.Context) (bool, error) {
	runs, err := t.P.gh.ListActionRequiredRuns(ctx, t.Sha)
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		runID := r.GetID()
		if err := t.P.gh.ApproveWorkflowRun(ctx, runID); err != nil {
			if count := t.P.incApproveRetry(runID); count >= t.P.cfg.MaxAgentContinueRetries {
				notice := fmt.Sprintf("copilot-autocode: failed to automatically approve the GitHub Actions workflow run '%s' after %d retries. The PR has been left open in review for manual inspection.", r.GetName(), count)
				_ = t.P.gh.PostComment(ctx, t.PR.GetNumber(), notice)
				return true, nil
			}
		} else {
			t.P.clearApproveRetry(runID)
		}
	}

	depRuns, err := t.P.gh.ListPendingDeploymentRuns(ctx, t.Sha)
	if err != nil {
		return false, err
	}
	for _, r := range depRuns {
		runID := r.GetID()
		approved, err := t.P.gh.ApprovePendingDeployments(ctx, runID)
		switch {
		case err != nil:
		case approved == 0:
			if rerr := t.P.gh.RerunWorkflow(ctx, runID); rerr == nil {
				runs = append(runs, r)
			}
		default:
			runs = append(runs, r)
		}
	}

	t.ApprovedRuns = runs
	return false, nil
}

func (t *PRTask) CheckGates(ctx context.Context) (bool, error) {
	depRuns, err := t.P.gh.ListPendingDeploymentRuns(ctx, t.Sha)
	if err != nil {
		return false, err
	}
	if len(depRuns) == 0 {
		return false, nil
	}

	names := runNames(depRuns)
	t.Display(IssueDisplayInfo{
		Current:     "Waiting for manual deployment approval",
		Next:        fmt.Sprintf("Blocked by: %s", strings.Join(names, ", ")),
		PR:          t.PR,
		AgentStatus: "pending",
	})

	shaTag := ghclient.SHAMarker("deployment-pending", t.Sha)
	if posted, _, _ := t.P.gh.HasCommentContaining(ctx, t.PR.GetNumber(), shaTag); posted {
		return true, nil
	}
	notice := fmt.Sprintf("copilot-autocode: PR requires manual deployment approval for workflow(s): %s. Waiting for a reviewer to approve the environment deployment before proceeding.\n%s\n%s", strings.Join(names, ", "), ghclient.DeploymentPendingCommentMarker, shaTag)
	_ = t.P.gh.PostComment(ctx, t.PR.GetNumber(), notice)
	return true, nil
}

func (t *PRTask) WaitForCI(ctx context.Context) (bool, error) {
	// Fetch runs once — reuse for both the active-check and progress counts.
	runs, err := t.P.gh.LatestWorkflowRun(ctx, t.Sha)
	if err != nil {
		return false, err
	}

	anyActive := false
	ciCompleted, ciTotal, ciPassed, ciFailed := 0, 0, 0, 0
	for _, r := range runs {
		ciTotal++
		status := r.GetStatus()
		if status == "in_progress" || status == "queued" || status == "requested" {
			anyActive = true
		}
		if status == "completed" {
			ciCompleted++
			c := r.GetConclusion()
			if c == "success" || c == "skipped" || c == "neutral" {
				ciPassed++
			} else if c != "" {
				ciFailed++
			}
		}
	}

	if !anyActive && len(t.ApprovedRuns) == 0 {
		return false, nil
	}

	sent, err := t.P.gh.CountRefinementPromptsSent(ctx, t.PR.GetNumber())
	if err != nil {
		return false, err
	}
	nextAction := "Merge (if checks pass)"
	if sent < t.P.cfg.MaxRefinementRounds {
		nextAction = "Refinement (if checks pass)"
	}

	t.Display(IssueDisplayInfo{
		Current:     "CI running — waiting for all checks",
		Next:        nextAction,
		PR:          t.PR,
		AgentStatus: "pending",
		CICompleted: ciCompleted,
		CITotal:     ciTotal,
		CIPassed:    ciPassed,
		CIFailed:    ciFailed,
	})
	return true, nil
}

func (t *PRTask) HandleTimeout(ctx context.Context) (bool, error) {
	conclusion, completedAt, err := t.P.gh.LatestFailedRunConclusion(ctx, t.Sha)
	if err != nil {
		return false, err
	}
	if conclusion == "" {
		return false, nil
	}

	continueCount, err := t.P.gh.CountAgentContinueComments(ctx, t.PR.GetNumber())
	if err != nil {
		return false, err
	}
	if continueCount >= t.P.cfg.MaxAgentContinueRetries {
		// Before declaring failure, verify the agent didn't eventually recover.
		// AllRunsSucceeded returns true when every completed run passed (or was
		// skipped/neutral).  If CI is green now we should fall through to Refine
		// rather than wrongly leaving the PR marked as unresponsive.
		if !t.P.cfg.SkipCIChecks {
			allOK, _, checkErr := t.P.gh.AllRunsSucceeded(ctx, t.Sha)
			if checkErr == nil && allOK {
				// Agent eventually recovered — continue the normal pipeline.
				return false, nil
			}
		}
		notice := fmt.Sprintf("copilot-autocode: the Copilot coding agent timed out and %d continue attempt(s) were exhausted. The PR has been left open in review for manual inspection.", continueCount)
		_ = t.P.gh.PostComment(ctx, t.Num, notice)
		t.Display(IssueDisplayInfo{
			Current:     fmt.Sprintf("Agent timed out after %d retries — left in review", continueCount),
			PR:          t.PR,
			AgentStatus: "failed",
		})
		return true, nil
	}

	lastContinue, err := t.P.gh.LastAgentContinueAt(ctx, t.PR.GetNumber())
	if err != nil {
		return false, err
	}
	lastActivity := latestOf(completedAt, lastContinue)
	if lastActivity.IsZero() {
		lastActivity = timeNow()
	}

	delay := agentTimeoutDelay(t.P.cfg)
	deadline := lastActivity.Add(delay)
	if timeNow().Before(deadline) {
		t.Display(IssueDisplayInfo{
			Current:      fmt.Sprintf("Agent timed out (attempt %d of %d)", continueCount+1, t.P.cfg.MaxAgentContinueRetries),
			Next:         "Post continue",
			NextActionAt: deadline,
			PR:           t.PR,
			AgentStatus:  "pending",
		})
		return true, nil
	}

	_ = t.P.gh.PostComment(ctx, t.PR.GetNumber(), t.P.cfg.AgentContinuePrompt+"\n"+ghclient.AgentContinueCommentMarker)
	t.Display(IssueDisplayInfo{
		Current:     fmt.Sprintf("Agent timed out — continue posted (attempt %d of %d)", continueCount+1, t.P.cfg.MaxAgentContinueRetries),
		PR:          t.PR,
		AgentStatus: "pending",
	})
	return true, nil
}

func (t *PRTask) Refine(ctx context.Context) (bool, error) {
	t.AllOK = t.P.cfg.SkipCIChecks
	if !t.AllOK {
		var err error
		t.AllOK, t.AnyFail, err = t.P.gh.AllRunsSucceeded(ctx, t.Sha)
		if err != nil {
			return false, err
		}
	}

	sent, err := t.P.gh.CountRefinementPromptsSent(ctx, t.PR.GetNumber())
	if err != nil {
		return false, err
	}
	t.Sent = sent

	if sent >= t.P.cfg.MaxRefinementRounds {
		return false, nil
	}

	refSHATag := ghclient.SHAMarker("refinement", t.Sha)
	alreadyPosted, postedAt, _ := t.P.gh.HasReviewContaining(ctx, t.PR.GetNumber(), refSHATag)
	if alreadyPosted {
		lastContinue, _ := t.P.gh.LastAgentContinueAt(ctx, t.PR.GetNumber())
		if t.P.HandleAgentTimeout(ctx, t.PR, t.Num, latestOf(postedAt, lastContinue), t.P.continueTimeoutCfg("refinement prompt", "copilot-autocode: the Copilot coding agent became unresponsive and %d continue attempt(s) were exhausted. The PR has been left open in review for manual inspection."), t.DisplayInfo) {
			return true, nil
		}
		t.DisplayInfo[t.Num] = &IssueDisplayInfo{
			Current:         fmt.Sprintf("Refinement %d/%d — waiting for agent", sent, t.P.cfg.MaxRefinementRounds),
			Next:            "Waiting for agent to push",
			PR:              t.PR,
			RefinementCount: sent,
			RefinementMax:   t.P.cfg.MaxRefinementRounds,
			AgentStatus:     "pending",
		}
		return true, nil
	}

	body := t.P.BuildRefinementCIPrompt(ctx, sent+1, t.P.cfg.MaxRefinementRounds, t.Num, t.AnyFail, t.Sha)
	if err := t.P.gh.PostReviewComment(ctx, t.PR.GetNumber(), body); err != nil {
		return false, err
	}
	sent++
	t.Sent = sent
	t.DisplayInfo[t.Num] = &IssueDisplayInfo{
		Current:         fmt.Sprintf("Refinement %d/%d posted — waiting for agent", sent, t.P.cfg.MaxRefinementRounds),
		Next:            "Waiting for agent to push",
		PR:              t.PR,
		RefinementCount: sent,
		RefinementMax:   t.P.cfg.MaxRefinementRounds,
		AgentStatus:     "pending",
	}
	return true, nil
}

func (t *PRTask) FixCI(ctx context.Context) (bool, error) {
	if !t.AnyFail {
		return false, nil
	}

	ciFixSent, err := t.P.gh.CountCIFixPromptsSent(ctx, t.PR.GetNumber())
	if err != nil {
		return false, err
	}
	if ciFixSent >= t.P.cfg.MaxCIFixRounds {
		return false, nil
	}

	ciSHATag := ghclient.SHAMarker("ci-fix", t.Sha)
	alreadyPosted, postedAt, _ := t.P.gh.HasCommentContaining(ctx, t.PR.GetNumber(), ciSHATag)
	if alreadyPosted {
		t.P.WaitForAgentCycle(ctx, t.PR, t.Num, postedAt, t.P.continueTimeoutCfg("CI-fix prompt", "copilot-autocode: the Copilot coding agent became unresponsive during CI fixing and %d continue attempt(s) were exhausted. The PR has been left open in review for manual inspection."), fmt.Sprintf("CI-fix %d/%d — waiting for agent", ciFixSent, t.P.cfg.MaxCIFixRounds), t.DisplayInfo)
		return true, nil
	}

	workflowName, failedJobs, err := t.P.gh.FailedRunDetails(ctx, t.Sha)
	if err != nil {
		return false, err
	}
	body := fmt.Sprintf("@copilot (CI-fix %d of %d). The tests are still failing — please fix them.%s\n%s\n%s", ciFixSent+1, t.P.cfg.MaxCIFixRounds, BuildCIFailureSection(workflowName, failedJobs), ghclient.CIFixCommentMarker, ciSHATag)
	if err := t.P.gh.PostComment(ctx, t.PR.GetNumber(), body); err != nil {
		return false, err
	}
	ciFixSent++
	t.Display(IssueDisplayInfo{
		Current:     fmt.Sprintf("CI-fix %d/%d posted — waiting for agent", ciFixSent, t.P.cfg.MaxCIFixRounds),
		Next:        "Waiting for agent to push",
		PR:          t.PR,
		AgentStatus: "pending",
	})
	return true, nil
}

func (t *PRTask) Merge(ctx context.Context) error {
	if !t.AllOK {
		t.DisplayInfo[t.Num] = &IssueDisplayInfo{
			Current:         fmt.Sprintf("Refinements (%d/%d) and CI-fix rounds exhausted — CI still failing", t.Sent, t.P.cfg.MaxRefinementRounds),
			PR:              t.PR,
			RefinementCount: t.Sent,
			RefinementMax:   t.P.cfg.MaxRefinementRounds,
			AgentStatus:     "failed",
		}
		return nil
	}

	t.DisplayInfo[t.Num] = &IssueDisplayInfo{
		Current:         "All checks passed — merging",
		PR:              t.PR,
		RefinementCount: t.Sent,
		RefinementMax:   t.P.cfg.MaxRefinementRounds,
		AgentStatus:     "success",
	}

	if err := t.P.gh.ApprovePR(ctx, t.PR.GetNumber()); err != nil {
		if !strings.Contains(err.Error(), "already approved") && !strings.Contains(err.Error(), "Can not approve your own pull request") {
			return err
		}
	}
	if err := t.P.gh.MergePR(ctx, t.PR); err != nil {
		return err
	}
	for _, lbl := range []string{t.P.cfg.LabelReview, t.P.cfg.LabelCoding, t.P.cfg.LabelQueue} {
		_ = t.P.gh.RemoveLabel(ctx, t.Num, lbl)
	}
	return t.P.gh.CloseIssue(ctx, t.Num)
}
