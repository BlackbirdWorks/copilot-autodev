// Package config provides configuration loading from config.yaml.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the application configuration.
// All fields have built-in defaults; only github_owner and github_repo are required.
type Config struct {
	// Required: the target repository.
	GitHubOwner string `yaml:"github_owner"`
	GitHubRepo  string `yaml:"github_repo"`

	// Labels applied to issues to track their state through the workflow.
	// Defaults: ai-queue, ai-coding, ai-review.
	LabelQueue  string `yaml:"label_queue"`
	LabelCoding string `yaml:"label_coding"`
	LabelReview string `yaml:"label_review"`

	// CopilotUser is the GitHub username of the Copilot coding agent that is
	// assigned to issues when they enter the coding state.
	// Default: "copilot".
	CopilotUser string `yaml:"copilot_user"`

	// MaxConcurrentIssues is the maximum number of issues that may be in the
	// ai-coding state simultaneously.  Default: 3.
	MaxConcurrentIssues int `yaml:"max_concurrent_issues"`

	// PollIntervalSeconds controls how often the orchestrator polls GitHub.
	// Minimum: 10.  Default: 45.
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`

	// MaxRefinementRounds is the number of times @copilot is asked to review
	// its implementation against the original issue requirements after CI
	// passes.  The PR is approved and merged only after all rounds are done.
	// Default: 3.
	MaxRefinementRounds int `yaml:"max_refinement_rounds"`

	// RefinementPrompt is the instructional body sent to @copilot for each
	// refinement round.  The orchestrator prefixes it with the round counter
	// and appends the machine-readable marker automatically.
	// Default: a sensible built-in prompt.
	RefinementPrompt string `yaml:"refinement_prompt"`

	// MergeMethod is the strategy used when auto-merging a PR.
	// Accepted values: squash, merge, rebase.  Default: "squash".
	MergeMethod string `yaml:"merge_method"`

	// MergeCommitMessage is the commit message written when the PR is merged.
	// Default: "Auto-merged by copilot-autocode".
	MergeCommitMessage string `yaml:"merge_commit_message"`

	// MergeConflictPrompt is the comment posted on a PR that is behind or has
	// merge conflicts, asking @copilot to rebase/resolve.
	MergeConflictPrompt string `yaml:"merge_conflict_prompt"`

	// CIFixPrompt is the opening instruction sent to @copilot when CI fails.
	// The orchestrator always appends the failing workflow name, job names,
	// and per-job log URLs after this text.
	CIFixPrompt string `yaml:"ci_fix_prompt"`

	// MaxMergeConflictRetries is the number of times the orchestrator asks
	// @copilot to fix merge conflicts before falling back to local AI-powered
	// resolution.  Default: 3.
	MaxMergeConflictRetries int `yaml:"max_merge_conflict_retries"`

	// AIMergeResolverCmd is the executable invoked for local merge-conflict
	// resolution after @copilot retries are exhausted.  The prompt is passed
	// as a single argument.  Default: "gemini".
	AIMergeResolverCmd string `yaml:"ai_merge_resolver_cmd"`

	// AIMergeResolverPrompt is the text passed to AIMergeResolverCmd.
	AIMergeResolverPrompt string `yaml:"ai_merge_resolver_prompt"`
}

// Load reads a YAML config file from path and returns a populated Config with
// defaults applied.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	cfg := &Config{
		LabelQueue:          "ai-queue",
		LabelCoding:         "ai-coding",
		LabelReview:         "ai-review",
		CopilotUser:         "copilot",
		MaxConcurrentIssues: 3,
		PollIntervalSeconds: 45,
		MaxRefinementRounds: 3,
		RefinementPrompt:    "Please review your implementation against all requirements in the original issue and refine anything that is missing or incomplete.",
		MergeMethod:         "squash",
		MergeCommitMessage:  "Auto-merged by copilot-autocode",
		MergeConflictPrompt: "@copilot Please merge from main and address any merge conflicts.",
		CIFixPrompt:         "@copilot Please fix the failing CI checks.",
		MaxMergeConflictRetries: 3,
		AIMergeResolverCmd:      "gemini",
		AIMergeResolverPrompt:   "Please resolve all git merge conflicts in this repository. Make minimal changes to resolve the conflicts while preserving the intent of both sides.",
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}

	if cfg.GitHubOwner == "" {
		return nil, fmt.Errorf("config: github_owner is required")
	}
	if cfg.GitHubRepo == "" {
		return nil, fmt.Errorf("config: github_repo is required")
	}
	if cfg.MaxConcurrentIssues < 1 {
		cfg.MaxConcurrentIssues = 1
	}
	if cfg.PollIntervalSeconds < 10 {
		cfg.PollIntervalSeconds = 10
	}
	if cfg.MaxRefinementRounds < 0 {
		cfg.MaxRefinementRounds = 0
	}
	switch cfg.MergeMethod {
	case "squash", "merge", "rebase":
		// valid
	default:
		return nil, fmt.Errorf("config: merge_method must be squash, merge, or rebase (got %q)", cfg.MergeMethod)
	}

	if cfg.MaxMergeConflictRetries < 1 {
		cfg.MaxMergeConflictRetries = 1
	}

	return cfg, nil
}
