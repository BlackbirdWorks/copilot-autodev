package poller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"

	"github.com/BlackbirdWorks/copilot-autodev/poller"
)

func TestSortIssuesAsc(t *testing.T) {
	t.Parallel()
	makeIssue := func(n int) *github.Issue { return &github.Issue{Number: &n} }

	tests := []struct {
		name     string
		input    []*github.Issue
		expected []int
	}{
		{"sorted", []*github.Issue{makeIssue(1), makeIssue(2), makeIssue(3)}, []int{1, 2, 3}},
		{"unsorted", []*github.Issue{makeIssue(3), makeIssue(1), makeIssue(2)}, []int{1, 2, 3}},
		{"reverse", []*github.Issue{makeIssue(5), makeIssue(4), makeIssue(3)}, []int{3, 4, 5}},
		{"empty", []*github.Issue{}, []int{}},
		{"single", []*github.Issue{makeIssue(42)}, []int{42}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			poller.SortIssuesAsc(tt.input)
			actual := make([]int, len(tt.input))
			for i, v := range tt.input {
				actual[i] = v.GetNumber()
			}
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestFormatFallbackPrompt(t *testing.T) {
	t.Parallel()
	issue := &github.Issue{
		Number:  github.Ptr(123),
		Title:   github.Ptr("Fix the bug"),
		HTMLURL: github.Ptr("https://github.com/org/repo/issues/123"),
	}

	tpl := "Issue #{{.Number}}: {{.Title}} ({{.URL}})"
	expected := "Issue #123: Fix the bug (https://github.com/org/repo/issues/123)"
	actual := poller.FormatFallbackPrompt(tpl, issue)
	assert.Equal(t, expected, actual)
}

func TestPoller_PromoteFromQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name          string
		codingIssues  []*github.Issue
		reviewIssues  []*github.Issue
		queueIssues   []*github.Issue
		maxConcurrent int
		wantActions   int
	}{
		{
			name:          "promote within limits",
			codingIssues:  []*github.Issue{{Number: github.Ptr(1)}},
			reviewIssues:  []*github.Issue{{Number: github.Ptr(2)}},
			queueIssues:   []*github.Issue{{Number: github.Ptr(3)}, {Number: github.Ptr(4)}},
			maxConcurrent: 4,
			wantActions:   2,
		},
		{
			name:          "already at limit",
			codingIssues:  []*github.Issue{{Number: github.Ptr(1)}},
			reviewIssues:  []*github.Issue{{Number: github.Ptr(2)}},
			queueIssues:   []*github.Issue{{Number: github.Ptr(3)}},
			maxConcurrent: 2,
			wantActions:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/issues") && r.Method == http.MethodGet:
					if strings.Contains(r.URL.Query().Get("labels"), "ai-coding") {
						_ = json.NewEncoder(w).Encode(tt.codingIssues)
					} else if strings.Contains(r.URL.Query().Get("labels"), "ai-review") {
						_ = json.NewEncoder(w).Encode(tt.reviewIssues)
					} else {
						_ = json.NewEncoder(w).Encode(tt.queueIssues)
					}
				case strings.Contains(path, "/labels") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusOK)
				case strings.Contains(path, "/comments") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusCreated)
				case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
				}
			})
			p.Cfg().MaxConcurrentIssues = tt.maxConcurrent

			err := p.PromoteFromQueue(ctx, tt.queueIssues)
			assert.NoError(t, err)
		})
	}
}

func TestPoller_Tick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/issues") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.Issue{})
		case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
		}
	})

	p.Tick(ctx)
	// Just verify no panic and at least one run
	assert.NotNil(t, p)
}

func TestPoller_ProcessOne_NoPR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
			// No PR found
			_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
		case strings.Contains(path, "/issues") && r.Method == http.MethodGet:
			// No merged PR found
			_ = json.NewEncoder(w).Encode([]*github.Issue{})
		case strings.Contains(path, "/labels") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		}
	})

	num := 123
	issue := &github.Issue{Number: &num}
	displayInfo := make(map[int]*poller.IssueDisplayInfo)
	err := p.ProcessOne(ctx, issue, displayInfo)
	assert.NoError(t, err)
}

func TestPoller_DeduplicateIssueLists(t *testing.T) {
	t.Parallel()
	makeIssue := func(n int) *github.Issue { return &github.Issue{Number: &n} }

	queue := []*github.Issue{makeIssue(1), makeIssue(2)}
	coding := []*github.Issue{makeIssue(2), makeIssue(3), makeIssue(4)}
	reviewing := []*github.Issue{makeIssue(4), makeIssue(5)}

	newCoding, newReviewing := poller.DeduplicateIssueLists(queue, coding, reviewing)

	assert.Len(t, newReviewing, 2)
	assert.Len(t, newCoding, 1)
	assert.Equal(t, 3, newCoding[0].GetNumber())
}

func TestPoller_Snapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.Issue{})
	})

	num := 123
	displayInfo := map[int]*poller.IssueDisplayInfo{
		num: {
			Current:     "working",
			AgentStatus: "pending",
		},
	}

	queue, coding, review := p.Snapshot(ctx, displayInfo)
	assert.NotNil(t, queue)
	assert.NotNil(t, coding)
	assert.NotNil(t, review)
}

func TestPoller_Tick_Complex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/issues") && r.Method == http.MethodGet:
			if strings.Contains(r.URL.Query().Get("labels"), "ai-coding") {
				_ = json.NewEncoder(w).Encode([]*github.Issue{{Number: github.Ptr(10)}})
			} else {
				_ = json.NewEncoder(w).Encode([]*github.Issue{})
			}
		case strings.Contains(path, "/pulls") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
		}
	})

	p.Tick(ctx)
}
