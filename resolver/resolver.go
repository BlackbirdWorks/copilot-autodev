// Package resolver handles local merge-conflict resolution by cloning a PR
// branch into a temporary directory, merging the base branch, running a
// configurable AI CLI to resolve any conflicts, and pushing the result back.
//
// It is invoked by the poller only after a configured number of @copilot
// attempts have failed to resolve conflicts automatically.
package resolver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/BlackbirdWorks/copilot-autocode/config"
)

// PRDetails holds the clone-related information extracted from a GitHub PR.
type PRDetails struct {
	Owner      string // repository owner
	Repo       string // repository name
	HeadBranch string // PR head branch (the branch to clone and push back to)
	BaseBranch string // base branch to merge in (e.g. "main")
}

// RunLocalResolution clones the PR head branch into a fresh temp directory,
// merges the base branch, invokes the configured AI CLI to resolve conflicts,
// then commits and pushes the result.
//
// The GitHub token is used only for authenticated git operations and is
// redacted from all returned error messages.
func RunLocalResolution(ctx context.Context, token string, prd PRDetails, cfg *config.Config) error {
	tmpDir, err := os.MkdirTemp("", "copilot-autocode-merge-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Build an authenticated clone URL.  The token is never included in
	// returned errors — redactToken() strips it before surfacing messages.
	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		token, prd.Owner, prd.Repo)

	// Clone only the head branch (shallow enough for speed, full for commit).
	if err := run(ctx, tmpDir, token, "git", "clone",
		"--branch", prd.HeadBranch,
		"--single-branch",
		cloneURL, "."); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	// Configure a git identity so the merge commit is accepted.
	if err := run(ctx, tmpDir, token, "git", "config", "user.email", "copilot-autocode@users.noreply.github.com"); err != nil {
		return fmt.Errorf("git config user.email: %w", err)
	}
	if err := run(ctx, tmpDir, token, "git", "config", "user.name", "copilot-autocode"); err != nil {
		return fmt.Errorf("git config user.name: %w", err)
	}

	// Fetch the base branch so we can merge it.
	if err := run(ctx, tmpDir, token, "git", "fetch", "origin", prd.BaseBranch); err != nil {
		return fmt.Errorf("git fetch %s: %w", prd.BaseBranch, err)
	}

	// Attempt the merge.  A non-zero exit code means conflicts (expected);
	// any other class of failure (invalid ref, network error) is distinguishable
	// because git prints a specific message — the AI CLI step will fail in that
	// case and bubble up a meaningful error.
	_ = run(ctx, tmpDir, token, "git", "merge", "--no-edit", "origin/"+prd.BaseBranch)

	// Run the AI CLI in the working tree.
	if err := run(ctx, tmpDir, token, cfg.AIMergeResolverCmd, cfg.AIMergeResolverPrompt); err != nil {
		return fmt.Errorf("AI resolver %q: %w", cfg.AIMergeResolverCmd, err)
	}

	// Stage everything the AI may have written.
	if err := run(ctx, tmpDir, token, "git", "add", "--all"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Verify the AI actually made changes — if nothing is staged the resolver
	// did not resolve the conflicts and we should not create an empty commit.
	statusOut, statusErr := output(ctx, tmpDir, "git", "diff", "--cached", "--name-only")
	if statusErr != nil {
		return fmt.Errorf("git diff --cached: %w", statusErr)
	}
	if strings.TrimSpace(statusOut) == "" {
		return fmt.Errorf("AI resolver %q made no changes; conflicts may still be present",
			cfg.AIMergeResolverCmd)
	}

	// Commit.
	if err := run(ctx, tmpDir, token, "git", "commit",
		"-m", "chore: resolve merge conflicts via AI"); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// Push the resolved branch back to origin.
	if err := run(ctx, tmpDir, token, "git", "push", "origin", prd.HeadBranch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	return nil
}

// run executes name+args in dir, captures combined output, and returns a
// redacted error (token stripped) if the command fails.
func run(ctx context.Context, dir, token, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := redact(string(out), token)
		return fmt.Errorf("%w\n%s", err, msg)
	}
	return nil
}

// output runs name+args in dir and returns stdout as a string.  It does not
// take a token parameter because it is only used for innocuous status queries.
func output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// redact replaces all occurrences of token in s with "<redacted>" so that
// authentication tokens are never surfaced in error messages or logs.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<redacted>")
}
