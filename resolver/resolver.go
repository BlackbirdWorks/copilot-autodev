// Package resolver handles local merge-conflict resolution by cloning a PR
// branch into a temporary directory, merging the base branch, running a
// configurable AI CLI to resolve any conflicts, and pushing the result back.
//
// It is invoked by the poller only after a configured number of @copilot
// attempts have failed to resolve conflicts automatically.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BlackbirdWorks/copilot-autodev/config"
)

// Runner defines the interface for executing system commands.
type Runner interface {
	Run(ctx context.Context, out io.Writer, dir, token, name string, args ...string) error
	Output(ctx context.Context, dir, name string, args ...string) (string, error)
}

// LogRunner wraps a Runner and writes all command invocations and their output
// to a log writer.
type LogRunner struct {
	inner Runner
	log   io.Writer
}

func (r *LogRunner) Run(ctx context.Context, _ io.Writer, dir, token, name string, args ...string) error {
	fmt.Fprintf(r.log, "$ %s %s\n", name, strings.Join(args, " "))
	err := r.inner.Run(ctx, r.log, dir, token, name, args...)
	if err != nil {
		fmt.Fprintf(r.log, "ERROR: %s\n\n", redact(err.Error(), token))
	} else {
		fmt.Fprintf(r.log, "OK\n\n")
	}
	return err
}

func (r *LogRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	fmt.Fprintf(r.log, "$ %s %s\n", name, strings.Join(args, " "))
	out, err := r.inner.Output(ctx, dir, name, args...)
	if err != nil {
		fmt.Fprintf(r.log, "ERROR: %s\n\n", err)
	} else {
		fmt.Fprintf(r.log, "%s\n", out)
	}
	return out, err
}

// RealRunner implements Runner using [exec.Command].
type RealRunner struct{}

func (r *RealRunner) Run(ctx context.Context, out io.Writer, dir, token, name string, args ...string) error {
	return run(ctx, out, dir, token, name, args...)
}

func (r *RealRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return output(ctx, dir, name, args...)
}

// PRDetails holds the clone-related information extracted from a GitHub PR.
type PRDetails struct {
	Owner      string // repository owner
	Repo       string // repository name
	HeadBranch string // PR head branch (the branch to clone and push back to)
	BaseBranch string // base branch to merge in (e.g. "main")
}

// Resolver handles the merge-conflict resolution process.
type Resolver struct {
	runner Runner
}

// New creates a new Resolver with the default real runner.
func New() *Resolver {
	return &Resolver{runner: &RealRunner{}}
}

// NewWithRunner creates a new Resolver with a custom runner (useful for testing).
func NewWithRunner(r Runner) *Resolver {
	return &Resolver{runner: r}
}

// LogPath returns the path where the merge resolution log for a given PR will
// be written.
func LogPath(prNum int) string {
	return fmt.Sprintf("merge-resolution-pr-%d.log", prNum)
}

// RunLocalResolution clones the PR head branch into a fresh temp directory,
// merges the base branch, invokes the configured AI CLI to resolve conflicts,
// then commits and pushes the result.
//
// All command invocations and their output are written to a per-PR log file
// (merge-resolution-pr-<N>.log) so failures can be diagnosed from the TUI.
//
// The GitHub token is used only for authenticated git operations and is
// redacted from all returned error messages and log files.
func (r *Resolver) RunLocalResolution(
	ctx context.Context,
	token string,
	prd PRDetails,
	cfg *config.Config,
	prNum int,
) (string, error) {
	// Open the per-PR log file in append mode so previous attempt details
	// are preserved when the user retries via 'r'.  A separator line marks
	// each new attempt so the viewer stays readable.
	logFile, err := os.OpenFile(LogPath(prNum), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("create merge log: %w", err)
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "=== Merge resolution for PR #%d ===\n", prNum)
	fmt.Fprintf(logFile, "Time:   %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(logFile, "Repo:   %s/%s\n", prd.Owner, prd.Repo)
	fmt.Fprintf(logFile, "Head:   %s\n", prd.HeadBranch)
	fmt.Fprintf(logFile, "Base:   %s\n", prd.BaseBranch)
	fmt.Fprintf(logFile, "Cmd:    %s %s\n", cfg.AIMergeResolverCmd, strings.Join(cfg.AIMergeResolverArgs, " "))
	fmt.Fprintf(logFile, "Prompt: %s\n\n", cfg.AIMergeResolverPrompt)

	// Wrap the runner with logging.
	logged := &LogRunner{inner: r.runner, log: logFile}
	lr := &Resolver{runner: logged}

	tmpDir, err := os.MkdirTemp("", "copilot-autodev-merge-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Fprintf(logFile, "Working directory: %s\n\n", tmpDir)

	// Build an authenticated clone URL.  The token is never included in
	// returned errors — redactToken() strips it before surfacing messages.
	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		token, prd.Owner, prd.Repo)

	// Clone only the head branch (shallow enough for speed, full for commit).
	if err := lr.run(ctx, tmpDir, token, "git", "clone",
		"--branch", prd.HeadBranch,
		"--single-branch",
		cloneURL, "."); err != nil {
		return "", fmt.Errorf("git clone: %w", err)
	}

	// Configure a git identity so the merge commit is accepted.
	if err := lr.run(
		ctx, tmpDir, token,
		"git", "config", "user.email", "copilot-autodev@users.noreply.github.com",
	); err != nil {
		return "", fmt.Errorf("git config user.email: %w", err)
	}
	if err := lr.run(ctx, tmpDir, token, "git", "config", "user.name", "copilot-autodev"); err != nil {
		return "", fmt.Errorf("git config user.name: %w", err)
	}

	// Fetch the base branch so we can merge it.
	if err := lr.run(ctx, tmpDir, token, "git", "fetch", "origin", prd.BaseBranch); err != nil {
		return "", fmt.Errorf("git fetch %s: %w", prd.BaseBranch, err)
	}

	// Attempt the merge.  Use FETCH_HEAD instead of origin/main because
	// --single-branch clones don't create remote tracking refs, so
	// "origin/main" won't exist even after `git fetch origin main`.
	_ = lr.run(ctx, tmpDir, token, "git", "merge", "--no-edit", "FETCH_HEAD")

	// Run the AI CLI in the working tree.
	// Build args from cfg.AIMergeResolverArgs. If "{prompt}" appears in any arg,
	// it is replaced by cfg.AIMergeResolverPrompt. Otherwise, the prompt is appended.
	aiArgs := make([]string, 0, len(cfg.AIMergeResolverArgs)+1)
	promptInjected := false
	for _, arg := range cfg.AIMergeResolverArgs {
		if strings.Contains(arg, "{prompt}") {
			aiArgs = append(aiArgs, strings.ReplaceAll(arg, "{prompt}", cfg.AIMergeResolverPrompt))
			promptInjected = true
		} else {
			aiArgs = append(aiArgs, arg)
		}
	}
	if !promptInjected {
		aiArgs = append(aiArgs, cfg.AIMergeResolverPrompt)
	}
	if err := lr.run(ctx, tmpDir, token, cfg.AIMergeResolverCmd, aiArgs...); err != nil {
		return "", fmt.Errorf("AI resolver %q: %w", cfg.AIMergeResolverCmd, err)
	}

	// Stage everything the AI may have written.
	if err := lr.run(ctx, tmpDir, token, "git", "add", "--all"); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	// Verify the AI actually made changes — if nothing is staged the resolver
	// did not resolve the conflicts and we should not create an empty commit.
	statusOut, statusErr := lr.output(ctx, tmpDir, "git", "diff", "--cached", "--name-only")
	if statusErr != nil {
		return "", fmt.Errorf("git diff --cached: %w", statusErr)
	}
	if strings.TrimSpace(statusOut) == "" {
		return "", fmt.Errorf("AI resolver %q made no changes; conflicts may still be present",
			cfg.AIMergeResolverCmd)
	}

	// Commit.
	if err := lr.run(ctx, tmpDir, token, "git", "commit",
		"-m", "chore: resolve merge conflicts via AI"); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}

	// Get the new SHA
	newSha, err := lr.output(ctx, tmpDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	newSha = strings.TrimSpace(newSha)

	// Push the resolved branch back to origin.
	if err := lr.run(ctx, tmpDir, token, "git", "push", "origin", prd.HeadBranch); err != nil {
		return "", fmt.Errorf("git push: %w", err)
	}

	fmt.Fprintf(logFile, "=== Merge resolution completed successfully ===\n")
	return newSha, nil
}

// run executes name+args in dir, captures combined output, and returns a
// redacted error (token stripped) if the command fails.  Standard output and
// error are continuously streamed to out (if provided).
func (r *Resolver) run(ctx context.Context, dir, token, name string, args ...string) error {
	err := r.runner.Run(ctx, nil, dir, token, name, args...)
	if err != nil {
		// The error message itself might contain the token if the Runner returned it.
		errMsg := redact(err.Error(), token)
		return errors.New(errMsg)
	}
	return nil
}

// output runs name+args in dir and returns stdout as a string.
func (r *Resolver) output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.runner.Output(ctx, dir, name, args...)
}

// redactingWriter ensures tokens are stripped from continuous log streams.
type redactingWriter struct {
	w     io.Writer
	token string
}

func (rw *redactingWriter) Write(p []byte) (int, error) {
	if rw.token == "" {
		return rw.w.Write(p)
	}
	s := strings.ReplaceAll(string(p), rw.token, "<redacted>")
	_, err := rw.w.Write([]byte(s))
	// Always return len(p) so callers don't treat it as a short write due to the missing chars.
	return len(p), err
}

// run is the internal helper using [exec.Command].
// If the binary is not found in the current PATH, it retries via `sh -c` so
// that shell-expanded PATH entries (e.g. from ~/.zshrc) are honoured.  This
// is important for AI CLI tools like `copilot` or `gemini` that users install
// via shell package managers but are not on the system PATH seen by exec.
// The output of the command is continuously streamed into `out` while also buffered to
// return as an error if the command fails.
func run(ctx context.Context, out io.Writer, dir, token, name string, args ...string) error {
	var buf strings.Builder
	var outputWriter io.Writer = &buf
	if out != nil {
		outputWriter = io.MultiWriter(&redactingWriter{w: out, token: token}, &buf)
	}

	if _, err := exec.LookPath(name); err != nil {
		// Binary not found in current PATH — invoke via login shell so that
		// the user's full PATH (from .zshrc / .zprofile / .profile) is available.
		parts := make([]string, 0, 1+len(args))
		parts = append(parts, name)
		parts = append(parts, args...)
		// shell-quote each argument simply (no fancy escaping; tokens already
		// redacted before logging, and the prompt won't contain single quotes).
		quoted := make([]string, len(parts))
		for i, p := range parts {
			quoted[i] = "'" + strings.ReplaceAll(p, "'", "'\\''") + "'"
		}
		shellLine := strings.Join(quoted, " ")
		// Prefix the command with inline token assignments so they are applied
		// AFTER the login shell has sourced .zprofile/.profile (which otherwise
		// could clear env vars set via cmd.Env before our shell command runs).
		var tokenPrefix string
		if !strings.Contains(name, "copilot") || !strings.HasPrefix(token, "ghp_") {
			tokenPrefix = fmt.Sprintf(
				"GITHUB_TOKEN=%s GH_TOKEN=%s COPILOT_GITHUB_TOKEN=%s ",
				token, token, token,
			)
		}
		cmd := exec.CommandContext(ctx, "sh", "-l", "-c", tokenPrefix+shellLine)
		cmd.Dir = dir
		cmd.Stdout = outputWriter
		cmd.Stderr = outputWriter
		err := cmd.Run()
		if err != nil {
			msg := redact(buf.String(), token)
			return fmt.Errorf("%w\n%s", err, msg)
		}
		return nil
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = outputWriter
	cmd.Stderr = outputWriter
	// Always inject GitHub token vars so AI CLIs (copilot, gemini, etc.)
	// can authenticate even when invoked directly without a shell.
	// Filter out existing token vars first to prevent duplicate keys in Env.
	tokenKeys := map[string]bool{
		"GITHUB_TOKEN":         true,
		"GH_TOKEN":             true,
		"COPILOT_GITHUB_TOKEN": true,
	}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		key := kv
		if before, _, ok := strings.Cut(kv, "="); ok {
			key = before
		}
		if !tokenKeys[key] {
			env = append(env, kv)
		}
	}
	if !strings.Contains(name, "copilot") || !strings.HasPrefix(token, "ghp_") {
		env = append(env,
			"GITHUB_TOKEN="+token,
			"GH_TOKEN="+token,
			"COPILOT_GITHUB_TOKEN="+token,
		)
	}
	cmd.Env = env
	err := cmd.Run()
	if err != nil {
		msg := redact(buf.String(), token)
		return fmt.Errorf("%w\n%s", err, msg)
	}
	return nil
}

// output is the internal helper using [exec.Command].
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
