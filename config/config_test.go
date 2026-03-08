package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes content to a temp file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config*.yaml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestConfigLoad(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		nonexistent bool   // use a path that does not exist
		wantErr     bool
		errContains string
		check       func(t *testing.T, c *Config)
	}{
		{
			name: "minimal config uses all defaults",
			yaml: "github_owner: myorg\ngithub_repo: myrepo\n",
			check: func(t *testing.T, c *Config) {
				if c.GitHubOwner != "myorg" {
					t.Errorf("GitHubOwner = %q; want myorg", c.GitHubOwner)
				}
				if c.GitHubRepo != "myrepo" {
					t.Errorf("GitHubRepo = %q; want myrepo", c.GitHubRepo)
				}
				if c.LabelQueue != "ai-queue" {
					t.Errorf("LabelQueue = %q; want ai-queue", c.LabelQueue)
				}
				if c.LabelCoding != "ai-coding" {
					t.Errorf("LabelCoding = %q; want ai-coding", c.LabelCoding)
				}
				if c.LabelReview != "ai-review" {
					t.Errorf("LabelReview = %q; want ai-review", c.LabelReview)
				}
				if c.CopilotUser != "copilot" {
					t.Errorf("CopilotUser = %q; want copilot", c.CopilotUser)
				}
				if c.MergeMethod != "squash" {
					t.Errorf("MergeMethod = %q; want squash", c.MergeMethod)
				}
				if c.MaxConcurrentIssues != 3 {
					t.Errorf("MaxConcurrentIssues = %d; want 3", c.MaxConcurrentIssues)
				}
				if c.PollIntervalSeconds != 45 {
					t.Errorf("PollIntervalSeconds = %d; want 45", c.PollIntervalSeconds)
				}
				if c.MaxRefinementRounds != 3 {
					t.Errorf("MaxRefinementRounds = %d; want 3", c.MaxRefinementRounds)
				}
				if c.MaxMergeConflictRetries != 3 {
					t.Errorf("MaxMergeConflictRetries = %d; want 3", c.MaxMergeConflictRetries)
				}
				if c.CopilotInvokeTimeoutSeconds != 600 {
					t.Errorf("CopilotInvokeTimeoutSeconds = %d; want 600", c.CopilotInvokeTimeoutSeconds)
				}
				if c.CopilotInvokeMaxRetries != 3 {
					t.Errorf("CopilotInvokeMaxRetries = %d; want 3", c.CopilotInvokeMaxRetries)
				}
				if c.FallbackIssueInvokePrompt == "" {
					t.Error("FallbackIssueInvokePrompt must not be empty")
				}
				if !strings.Contains(c.FallbackIssueInvokePrompt, "{issue_number}") {
					t.Errorf("FallbackIssueInvokePrompt = %q; want it to contain {issue_number}", c.FallbackIssueInvokePrompt)
				}
			},
		},
		{
			name: "custom values override defaults",
			yaml: `
github_owner: corp
github_repo: platform
merge_method: rebase
max_concurrent_issues: 5
poll_interval_seconds: 120
copilot_invoke_timeout_seconds: 300
copilot_invoke_max_retries: 2
copilot_user: my-copilot
label_queue: custom-queue
`,
			check: func(t *testing.T, c *Config) {
				if c.MergeMethod != "rebase" {
					t.Errorf("MergeMethod = %q; want rebase", c.MergeMethod)
				}
				if c.MaxConcurrentIssues != 5 {
					t.Errorf("MaxConcurrentIssues = %d; want 5", c.MaxConcurrentIssues)
				}
				if c.PollIntervalSeconds != 120 {
					t.Errorf("PollIntervalSeconds = %d; want 120", c.PollIntervalSeconds)
				}
				if c.CopilotInvokeTimeoutSeconds != 300 {
					t.Errorf("CopilotInvokeTimeoutSeconds = %d; want 300", c.CopilotInvokeTimeoutSeconds)
				}
				if c.CopilotInvokeMaxRetries != 2 {
					t.Errorf("CopilotInvokeMaxRetries = %d; want 2", c.CopilotInvokeMaxRetries)
				}
				if c.CopilotUser != "my-copilot" {
					t.Errorf("CopilotUser = %q; want my-copilot", c.CopilotUser)
				}
				if c.LabelQueue != "custom-queue" {
					t.Errorf("LabelQueue = %q; want custom-queue", c.LabelQueue)
				}
			},
		},
		{
			name: "merge method: all valid variants accepted",
			yaml: "github_owner: o\ngithub_repo: r\nmerge_method: merge\n",
			check: func(t *testing.T, c *Config) {
				if c.MergeMethod != "merge" {
					t.Errorf("MergeMethod = %q; want merge", c.MergeMethod)
				}
			},
		},
		{
			name:        "missing github_owner → error",
			yaml:        "github_repo: myrepo\n",
			wantErr:     true,
			errContains: "github_owner is required",
		},
		{
			name:        "missing github_repo → error",
			yaml:        "github_owner: myorg\n",
			wantErr:     true,
			errContains: "github_repo is required",
		},
		{
			name:        "invalid merge_method → error",
			yaml:        "github_owner: o\ngithub_repo: r\nmerge_method: cherry-pick\n",
			wantErr:     true,
			errContains: "merge_method must be squash",
		},
		{
			name: "below-minimum values are clamped to their floors",
			yaml: `
github_owner: o
github_repo: r
max_concurrent_issues: 0
poll_interval_seconds: 5
max_refinement_rounds: -1
max_merge_conflict_retries: 0
copilot_invoke_timeout_seconds: 10
copilot_invoke_max_retries: 0
`,
			check: func(t *testing.T, c *Config) {
				if c.MaxConcurrentIssues != 1 {
					t.Errorf("MaxConcurrentIssues = %d; want 1 (clamped from 0)", c.MaxConcurrentIssues)
				}
				if c.PollIntervalSeconds != 10 {
					t.Errorf("PollIntervalSeconds = %d; want 10 (clamped from 5)", c.PollIntervalSeconds)
				}
				if c.MaxRefinementRounds != 0 {
					t.Errorf("MaxRefinementRounds = %d; want 0 (clamped from -1)", c.MaxRefinementRounds)
				}
				if c.MaxMergeConflictRetries != 1 {
					t.Errorf("MaxMergeConflictRetries = %d; want 1 (clamped from 0)", c.MaxMergeConflictRetries)
				}
				if c.CopilotInvokeTimeoutSeconds != 30 {
					t.Errorf("CopilotInvokeTimeoutSeconds = %d; want 30 (clamped from 10)", c.CopilotInvokeTimeoutSeconds)
				}
				if c.CopilotInvokeMaxRetries != 1 {
					t.Errorf("CopilotInvokeMaxRetries = %d; want 1 (clamped from 0)", c.CopilotInvokeMaxRetries)
				}
			},
		},
		{
			name:        "file not found → error",
			nonexistent: true,
			wantErr:     true,
			errContains: "read config file",
		},
		{
			name:        "malformed YAML → error",
			yaml:        "github_owner: [not a string\n",
			wantErr:     true,
			errContains: "parse config file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.nonexistent {
				path = filepath.Join(t.TempDir(), "nonexistent.yaml")
			} else {
				path = writeConfig(t, tc.yaml)
			}

			cfg, err := Load(path)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() error = nil; want error containing %q", tc.errContains)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("Load() error = %q; want it to contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}
