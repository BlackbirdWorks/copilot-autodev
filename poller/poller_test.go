package poller

import (
	"strings"
	"testing"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/google/go-github/v68/github"
)

// ─── formatFallbackPrompt ─────────────────────────────────────────────────────

func TestFormatFallbackPrompt(t *testing.T) {
	num := 42
	title := "Fix the login bug"
	url := "https://github.com/org/repo/issues/42"
	issue := &github.Issue{
		Number:  &num,
		Title:   &title,
		HTMLURL: &url,
	}

	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name:     "default template expands all placeholders",
			template: "Please start working on issue #{issue_number}: {issue_title}.\n{issue_url}",
			want:     "Please start working on issue #42: Fix the login bug.\nhttps://github.com/org/repo/issues/42",
		},
		{
			name:     "only issue_number placeholder",
			template: "Work on #{issue_number}",
			want:     "Work on #42",
		},
		{
			name:     "only issue_title placeholder",
			template: "Task: {issue_title}",
			want:     "Task: Fix the login bug",
		},
		{
			name:     "only issue_url placeholder",
			template: "See {issue_url}",
			want:     "See https://github.com/org/repo/issues/42",
		},
		{
			name:     "no placeholders — template returned as-is",
			template: "Please start working on this issue.",
			want:     "Please start working on this issue.",
		},
		{
			name:     "all placeholders appear multiple times",
			template: "{issue_number} {issue_number} {issue_title} {issue_url}",
			want:     "42 42 Fix the login bug https://github.com/org/repo/issues/42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatFallbackPrompt(tc.template, issue)
			if got != tc.want {
				t.Errorf("formatFallbackPrompt() = %q; want %q", got, tc.want)
			}
		})
	}
}


func TestSortIssuesAsc(t *testing.T) {
	makeIssue := func(n int) *github.Issue { return &github.Issue{Number: &n} }

	tests := []struct {
		name  string
		input []int
		want  []int
	}{
		{"empty slice", nil, nil},
		{"single element", []int{5}, []int{5}},
		{"already sorted", []int{1, 2, 3, 10}, []int{1, 2, 3, 10}},
		{"reverse sorted", []int{10, 5, 3, 1}, []int{1, 3, 5, 10}},
		{"mixed order", []int{4, 1, 7, 2, 9, 3}, []int{1, 2, 3, 4, 7, 9}},
		{"duplicates preserved", []int{3, 1, 2, 1, 3}, []int{1, 1, 2, 3, 3}},
		{"two elements swapped", []int{2, 1}, []int{1, 2}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issues := make([]*github.Issue, len(tc.input))
			for i, n := range tc.input {
				issues[i] = makeIssue(n)
			}

			sortIssuesAsc(issues)

			if len(issues) != len(tc.want) {
				t.Fatalf("sortIssuesAsc() len = %d; want %d", len(issues), len(tc.want))
			}
			for i, issue := range issues {
				if issue.GetNumber() != tc.want[i] {
					t.Errorf("sortIssuesAsc()[%d] = %d; want %d",
						i, issue.GetNumber(), tc.want[i])
				}
			}
		})
	}
}

// ─── buildCIFixMessage ───────────────────────────────────────────────────────

func TestBuildCIFixMessage(t *testing.T) {
	p := &Poller{cfg: &config.Config{CIFixPrompt: "@copilot Please fix CI."}}

	tests := []struct {
		name         string
		workflowName string
		failedJobs   []ghclient.FailedJobInfo
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "prompt only — no workflow or jobs",
			workflowName: "",
			failedJobs:   nil,
			wantContains: []string{"@copilot Please fix CI."},
			wantAbsent:   []string{"Failing workflow", "Failed jobs"},
		},
		{
			name:         "workflow name included",
			workflowName: "CI / Build",
			failedJobs:   nil,
			wantContains: []string{
				"@copilot Please fix CI.",
				"**Failing workflow:** CI / Build",
			},
			wantAbsent: []string{"Failed jobs"},
		},
		{
			name:         "single failed job with log URL",
			workflowName: "CI",
			failedJobs: []ghclient.FailedJobInfo{
				{Name: "test", LogURL: "https://logs.example.com/1"},
			},
			wantContains: []string{
				"@copilot Please fix CI.",
				"**Failing workflow:** CI",
				"**Failed jobs:** test",
				"**test** logs: https://logs.example.com/1",
			},
		},
		{
			name:         "multiple failed jobs, second has no log URL",
			workflowName: "CI",
			failedJobs: []ghclient.FailedJobInfo{
				{Name: "build", LogURL: "https://logs.example.com/build"},
				{Name: "lint", LogURL: ""},
			},
			wantContains: []string{
				"**Failed jobs:** build, lint",
				"**build** logs: https://logs.example.com/build",
			},
			// lint has no LogURL so no log line for it
			wantAbsent: []string{"**lint** logs"},
		},
		{
			name:         "empty workflow name skips that section",
			workflowName: "",
			failedJobs: []ghclient.FailedJobInfo{
				{Name: "unit-tests", LogURL: ""},
			},
			wantContains: []string{"**Failed jobs:** unit-tests"},
			wantAbsent:   []string{"Failing workflow"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.buildCIFixMessage(tc.workflowName, tc.failedJobs)

			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildCIFixMessage() = %q; want it to contain %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("buildCIFixMessage() = %q; want it NOT to contain %q", got, absent)
				}
			}
		})
	}
}
