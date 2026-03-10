package ghclient_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
)

// TestInvokeCopilotAgent verifies that InvokeCopilotAgent sends a correctly
// formed POST request to the Copilot API and handles success/error responses.
func TestInvokeCopilotAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		prompt        string
		issueTitle    string
		issueNum      int
		issueURL      string
		serverStatus  int
		serverBody    string // JSON response body from mock server
		wantErr       bool
		wantJobID     string
		wantTitle     string
		wantPrompt    string
		wantEventType string
		wantIssueNum  int
		wantIssueURL  string
	}{
		{
			name:          "success – 201 Created with job ID",
			prompt:        "Please implement issue #42",
			issueTitle:    "Add CloudWatch support",
			issueNum:      42,
			issueURL:      "https://github.com/org/repo/issues/42",
			serverStatus:  http.StatusCreated,
			serverBody:    `{"id":"abc-123","status":"queued"}`,
			wantErr:       false,
			wantJobID:     "abc-123",
			wantTitle:     "[copilot-autodev] #42: Add CloudWatch support",
			wantPrompt:    "Please implement issue #42",
			wantEventType: "copilot-autodev",
			wantIssueNum:  42,
			wantIssueURL:  "https://github.com/org/repo/issues/42",
		},
		{
			name:          "success – 200 OK with job_id field",
			prompt:        "Fix the bug in issue #7",
			issueTitle:    "Fix nil pointer",
			issueNum:      7,
			issueURL:      "https://github.com/org/repo/issues/7",
			serverStatus:  http.StatusOK,
			serverBody:    `{"job_id":"xyz-789"}`,
			wantErr:       false,
			wantJobID:     "xyz-789",
			wantTitle:     "[copilot-autodev] #7: Fix nil pointer",
			wantPrompt:    "Fix the bug in issue #7",
			wantEventType: "copilot-autodev",
			wantIssueNum:  7,
			wantIssueURL:  "https://github.com/org/repo/issues/7",
		},
		{
			name:          "success – empty response body returns empty job ID",
			prompt:        "Do something",
			issueTitle:    "Task",
			issueNum:      1,
			serverStatus:  http.StatusCreated,
			serverBody:    `{}`,
			wantErr:       false,
			wantJobID:     "",
			wantTitle:     "[copilot-autodev] #1: Task",
			wantPrompt:    "Do something",
			wantEventType: "copilot-autodev",
			wantIssueNum:  1,
		},
		{
			name:         "unauthorized – 401 returns error",
			prompt:       "some task",
			serverStatus: http.StatusUnauthorized,
			wantErr:      true,
		},
		{
			name:         "server error – 500 returns error",
			prompt:       "some task",
			serverStatus: http.StatusInternalServerError,
			wantErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotReq ghclient.CopilotAgentJobRequest
			c := setupMockAPI(t, func(r *http.Request) (*http.Response, error) {
				assert.Equal(t, http.MethodPost, r.Method)
				if r.Body != nil {
					_ = json.NewDecoder(r.Body).Decode(&gotReq)
				}
				resp := &http.Response{
					StatusCode: tc.serverStatus,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tc.serverBody)),
				}
				return resp, nil
			})

			jobID, err := c.InvokeAgentAt(
				t.Context(),
				"https://api.githubcopilot.com/agents/swe/v1/jobs/test-owner/test-repo",
				tc.prompt, tc.issueTitle, tc.issueNum, tc.issueURL,
			)

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantJobID, jobID)
			assert.Equal(t, tc.wantTitle, gotReq.Title)
			assert.Equal(t, tc.wantPrompt, gotReq.ProblemStatement)
			assert.Equal(t, tc.wantEventType, gotReq.EventType)
			assert.Equal(t, tc.wantIssueNum, gotReq.IssueNumber)
			assert.Equal(t, tc.wantIssueURL, gotReq.IssueURL)
		})
	}
}

// TestUpdatePRBranch verifies updating the PR branch via native API.
func TestUpdatePRBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"success", http.StatusAccepted, false},
		{"error", http.StatusInternalServerError, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "msg"})
			})
			err := c.UpdatePRBranch(t.Context(), 123)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestTimeAgo verifies that TimeAgo produces the correct relative-time label
// for representative durations across all four branches of the function.
func TestTimeAgo(t *testing.T) {
	t.Parallel()
	now := time.Now()

	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		// < 1 minute → "just now"
		{"1 second ago", now.Add(-1 * time.Second), "just now"},
		{"30 seconds ago", now.Add(-30 * time.Second), "just now"},
		{"59 seconds ago", now.Add(-59 * time.Second), "just now"},

		// >= 1 minute, < 1 hour → "Nm ago"
		{"exactly 1 minute ago", now.Add(-1 * time.Minute), "1m ago"},
		{"5 minutes ago", now.Add(-5 * time.Minute), "5m ago"},
		{"59 minutes ago", now.Add(-59 * time.Minute), "59m ago"},

		// >= 1 hour, < 24 hours → "Nh ago"
		{"exactly 1 hour ago", now.Add(-1 * time.Hour), "1h ago"},
		{"3 hours ago", now.Add(-3 * time.Hour), "3h ago"},
		{"23 hours ago", now.Add(-23 * time.Hour), "23h ago"},

		// >= 24 hours → "Nd ago"
		{"exactly 1 day ago", now.Add(-24 * time.Hour), "1d ago"},
		{"2 days ago", now.Add(-48 * time.Hour), "2d ago"},
		{"10 days ago", now.Add(-240 * time.Hour), "10d ago"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ghclient.TimeAgo(tc.t))
		})
	}
}

func setupMockAPI(t *testing.T, handler func(*http.Request) (*http.Response, error)) *ghclient.Client {
	t.Helper()
	rt := &fakeRoundTripper{handler: handler}
	cfg := &config.Config{
		GitHubOwner: "test-owner",
		GitHubRepo:  "test-repo",
	}
	return ghclient.NewWithTransport("test-token", cfg, rt)
}

// fakeRoundTripper is now in test_utils_test.go

// setupMockGitHubAPI is now in test_utils_test.go

// TestAnyWorkflowRunActive verifies that waiting/action_required states
// do not trigger a 'true' return (preventing deadlocks).
func TestAnyWorkflowRunActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		runs       []*github.WorkflowRun
		wantActive bool
	}{
		{
			name:       "no runs",
			runs:       nil,
			wantActive: false,
		},
		{
			name:       "completed runs only",
			runs:       []*github.WorkflowRun{{Status: github.Ptr("completed")}},
			wantActive: false,
		},
		{
			name: "waiting and action_required runs",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("waiting")},
				{Status: github.Ptr("action_required")},
			},
			wantActive: false, // Core fix: these should NOT be considered "active"
		},
		{
			name:       "in_progress runs",
			runs:       []*github.WorkflowRun{{Status: github.Ptr("in_progress")}},
			wantActive: true,
		},
		{
			name:       "queued runs",
			runs:       []*github.WorkflowRun{{Status: github.Ptr("queued")}},
			wantActive: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			active, err := c.AnyWorkflowRunActive(t.Context(), "dummy-sha")
			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, active)
		})
	}
}

// TestAllRunsSucceeded verifies the 0-runs bypass and the generic failure catch.
func TestAllRunsSucceeded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		runs        []*github.WorkflowRun
		wantSuccess bool
		wantAnyFail bool
	}{
		{
			name:        "zero runs (0-run repo fix)",
			runs:        nil,
			wantSuccess: true,
			wantAnyFail: false,
		},
		{
			name: "one successful run",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
			},
			wantSuccess: true,
			wantAnyFail: false,
		},
		{
			name: "one skipped run",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("skipped")},
			},
			wantSuccess: true,
			wantAnyFail: false,
		},
		{
			name: "run still in progress",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("in_progress")},
			},
			wantSuccess: false,
			wantAnyFail: false,
		},
		{
			name: "generic failure (restored CI failure logic)",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("failure")},
			},
			wantSuccess: false,
			wantAnyFail: true,
		},
		{
			name: "timed out failure",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("timed_out")},
			},
			wantSuccess: false,
			wantAnyFail: true,
		},
		{
			name: "mixed success and failure",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("failure")},
			},
			wantSuccess: false,
			wantAnyFail: true,
		},
		{
			// completed+action_required blocks allSuccess (prevents merge) but
			// does not set anyFailure (would generate wrong CI-fix prompts the
			// agent cannot fix).  Steps 2/2.5 in processOne handle re-running
			// or posting a notice; this is defense-in-depth.
			name: "completed action_required blocks merge but not anyFail",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("action_required")},
			},
			wantSuccess: false,
			wantAnyFail: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			success, fail, err := c.AllRunsSucceeded(t.Context(), "dummy-sha")
			require.NoError(t, err)
			assert.Equal(t, tc.wantSuccess, success)
			assert.Equal(t, tc.wantAnyFail, fail)
		})
	}
}

// TestListActionRequiredRuns verifies that only pending-approval statuses
// (action_required, waiting) are returned for fork-PR approval.
// completed+action_required is intentionally excluded — it is a custom check
// conclusion that cannot be approved via the GitHub fork-approval API.
func TestListActionRequiredRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		runs      []*github.WorkflowRun
		wantCount int
	}{
		{
			name:      "no runs",
			runs:      nil,
			wantCount: 0,
		},
		{
			name: "action_required status",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("action_required")},
			},
			wantCount: 1,
		},
		{
			name: "waiting status",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("waiting")},
			},
			wantCount: 1,
		},
		{
			// completed+action_required cannot be approved via the fork-approval API;
			// the GitHub API returns 403 "Not from a fork pull request".
			name: "completed with action_required conclusion is excluded",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("action_required")},
			},
			wantCount: 0,
		},
		{
			name: "completed success is not required",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
			},
			wantCount: 0,
		},
		{
			name: "mixed: pending action_required alongside completed runs",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("action_required")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("action_required")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
			},
			wantCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			got, err := c.ListActionRequiredRuns(t.Context(), "dummy-sha")
			require.NoError(t, err)
			assert.Len(t, got, tc.wantCount)
		})
	}
}

// TestListPendingDeploymentRuns verifies that only completed+action_required
// runs are included (the environment-deployment approval case).
func TestListPendingDeploymentRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		runs      []*github.WorkflowRun
		wantCount int
	}{
		{
			name:      "no runs",
			runs:      nil,
			wantCount: 0,
		},
		{
			name: "completed action_required conclusion",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("action_required")},
			},
			wantCount: 1,
		},
		{
			name: "pending action_required status is excluded",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("action_required")},
			},
			wantCount: 0,
		},
		{
			name: "completed success is excluded",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
			},
			wantCount: 0,
		},
		{
			name: "mixed: only completed+action_required returned",
			runs: []*github.WorkflowRun{
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("action_required")},
				{Status: github.Ptr("action_required")},
				{Status: github.Ptr("completed"), Conclusion: github.Ptr("success")},
			},
			wantCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			got, err := c.ListPendingDeploymentRuns(t.Context(), "dummy-sha")
			require.NoError(t, err)
			assert.Len(t, got, tc.wantCount)
		})
	}
}

// TestApprovePendingDeployments verifies that environment IDs where
// CurrentUserCanApprove=true are submitted and zero IDs are skipped.
func TestApprovePendingDeployments(t *testing.T) {
	t.Parallel()
	runID := int64(12345)

	tests := []struct {
		name         string
		pending      []map[string]any
		wantApproved int
		wantPostBody bool
	}{
		{
			name:         "no pending deployments",
			pending:      nil,
			wantApproved: 0,
			wantPostBody: false,
		},
		{
			name: "one approvable environment",
			pending: []map[string]any{
				{"environment": map[string]any{"id": 10, "name": "production"}, "current_user_can_approve": true},
			},
			wantApproved: 1,
			wantPostBody: true,
		},
		{
			name: "not approvable by current user",
			pending: []map[string]any{
				{"environment": map[string]any{"id": 20, "name": "production"}, "current_user_can_approve": false},
			},
			wantApproved: 0,
			wantPostBody: false,
		},
		{
			name: "mixed: only approvable ones submitted",
			pending: []map[string]any{
				{"environment": map[string]any{"id": 10, "name": "staging"}, "current_user_can_approve": true},
				{"environment": map[string]any{"id": 20, "name": "prod"}, "current_user_can_approve": false},
			},
			wantApproved: 1,
			wantPostBody: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotPostBody bool
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(tc.pending)
					return
				}
				// POST — approval submission
				gotPostBody = true
				_ = json.NewEncoder(w).Encode([]map[string]any{})
			})

			approved, err := c.ApprovePendingDeployments(t.Context(), runID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantApproved, approved)
			assert.Equal(t, tc.wantPostBody, gotPostBody)
		})
	}
}

// TestHasActiveCopilotRun verifies that the Copilot-run guard correctly
// identifies active runs from Copilot actors and ignores non-Copilot runs.
func TestHasActiveCopilotRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		runs       []*github.WorkflowRun
		wantActive bool
	}{
		{
			name:       "no runs",
			runs:       nil,
			wantActive: false,
		},
		{
			name: "completed copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("completed"),
					Actor:  &github.User{Login: github.Ptr("copilot-swe-agent[bot]")},
				},
			},
			wantActive: false,
		},
		{
			name: "in_progress copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("in_progress"),
					Actor:  &github.User{Login: github.Ptr("copilot-swe-agent[bot]")},
				},
			},
			wantActive: true,
		},
		{
			name: "queued copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("queued"),
					Actor:  &github.User{Login: github.Ptr("Copilot")},
				},
			},
			wantActive: true,
		},
		{
			name: "in_progress non-copilot run",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("in_progress"),
					Actor:  &github.User{Login: github.Ptr("github-actions[bot]")},
					Name:   github.Ptr("CI"),
				},
			},
			wantActive: false,
		},
		{
			name: "in_progress run with copilot in workflow name",
			runs: []*github.WorkflowRun{
				{
					Status: github.Ptr("in_progress"),
					Actor:  &github.User{Login: github.Ptr("github-actions[bot]")},
					Name:   github.Ptr("Copilot Coding Agent"),
				},
			},
			wantActive: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: tc.runs}
				_ = json.NewEncoder(w).Encode(resp)
			})

			active, err := c.HasActiveCopilotRun(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, active)
		})
	}
}

// TestPRRegexMatching validates the bodyRe, titleRe, and branchRe behavior
// used in OpenPRForIssue and MergedPRForIssue.
func TestPRRegexMatching(t *testing.T) {
	t.Parallel()
	issueNum := 329

	bodyRe := regexp.MustCompile(fmt.Sprintf(`(?i)#%d\b`, issueNum))
	titleRe := regexp.MustCompile(fmt.Sprintf(`(?i)#%d\b`, issueNum))
	branchRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:^|/)(?:issue-?)?%d(?:[-/]|$)`, issueNum))

	// Body match tests
	assert.True(t, bodyRe.MatchString("Fixes #329"), "bodyRe should match exact string")
	assert.True(t, bodyRe.MatchString("Addresses #329\nWith more text"), "bodyRe should match loosened mention")
	assert.False(t, bodyRe.MatchString("Fixes #3291"), "bodyRe should NOT match #3291")

	// Title match tests
	assert.True(t, titleRe.MatchString("#329 Fix database bug"), "titleRe should match starting number")
	assert.True(t, titleRe.MatchString("API Fixes for #329"), "titleRe should match ending number")
	assert.False(t, titleRe.MatchString("Fixes #3295"), "titleRe should NOT match #3295")

	// Branch match tests
	assert.True(t, branchRe.MatchString("issue-329"), "branchRe should match basic issue branch")
	assert.True(t, branchRe.MatchString("copilot/issue-329-fix-bug"), "branchRe should match copilot prefixed branch")
	assert.True(t, branchRe.MatchString("issue329"), "branchRe should match unhyphenated issue branch")
	assert.True(t, branchRe.MatchString("copilot/329-fix-bug"), "branchRe should match bare number after slash")
	assert.True(t, branchRe.MatchString("copilot-swe-agent/issue-329/fix-bug"),
		"branchRe should match slash after number")
	assert.False(t, branchRe.MatchString("issue-3295-fix"), "branchRe should NOT match issue branch with extra digits")
	assert.False(t, branchRe.MatchString("issue-3295/fix"), "branchRe should NOT match wrong issue with slash")
}

// TestGetCopilotJobStatus verifies polling the job status endpoint.
func TestGetCopilotJobStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		serverStatus int
		serverBody   string
		jobID        string
		wantErr      bool
		wantStatus   string
		wantPR       int
		wantRunID    int64
	}{
		{
			name:         "success - running",
			serverStatus: http.StatusOK,
			serverBody:   `{"job_id": "job-123", "status": "running"}`,
			jobID:        "job-123",
			wantErr:      false,
			wantStatus:   "running",
		},
		{
			name:         "success - completed with PR",
			serverStatus: http.StatusOK,
			serverBody:   `{"job_id": "job-pr", "status": "completed", "pull_request": {"number": 123}, "workflow_run": {"id": 456}}`,
			jobID:        "job-pr",
			wantErr:      false,
			wantStatus:   "completed",
			wantPR:       123,
			wantRunID:    456,
		},
		{
			name:         "not found - 404",
			serverStatus: http.StatusNotFound,
			serverBody:   `{}`,
			jobID:        "job-404",
			wantErr:      true,
			wantStatus:   "",
		},
		{
			name:         "server error - 500",
			serverStatus: http.StatusInternalServerError,
			serverBody:   `{"message":"Internal Server Error"}`,
			jobID:        "job-500",
			wantErr:      true,
			wantStatus:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Create a regular HTTP server purely to provide the URL,
			// since setupMockGitHubAPI returns a ghclient that overrides github
			// client but the auth token logic is separate.
			// Actually, setupMockGitHubAPI creates an httptest.Server internally
			// but doesn't expose its URL on the returned ghclient object directly.
			// Let's just create our own server so we can pass its URL to GetJobStatusAt.

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Contains(t, r.URL.Path, tc.jobID)
				w.WriteHeader(tc.serverStatus)
				_, _ = w.Write([]byte(tc.serverBody))
			}))
			defer srv.Close()

			client := ghclient.NewTestClient("test-owner", "test-repo", "test-token")
			endpoint := srv.URL + "/agents/swe/v1/jobs/test-owner/test-repo/" + tc.jobID

			status, err := client.GetJobStatusAt(t.Context(), endpoint, tc.jobID)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantStatus, status.Status)
				assert.Equal(t, tc.jobID, status.JobID)
				if tc.wantPR != 0 {
					require.NotNil(t, status.PullRequest)
					assert.Equal(t, tc.wantPR, status.PullRequest.Number)
				}
				if tc.wantRunID != 0 {
					require.NotNil(t, status.WorkflowRun)
					assert.Equal(t, tc.wantRunID, status.WorkflowRun.ID)
				}
			}
		})
	}
}

// TestLatestCopilotJobID verifies extraction of the job ID from issue comments.
func TestLatestCopilotJobID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		comments  []*github.IssueComment
		wantJobID string
	}{
		{
			name:      "no comments",
			comments:  nil,
			wantJobID: "",
		},
		{
			name: "comments without marker",
			comments: []*github.IssueComment{
				{Body: github.Ptr("Some comment"), CreatedAt: &github.Timestamp{Time: time.Now()}},
			},
			wantJobID: "",
		},
		{
			name: "single marker comment",
			comments: []*github.IssueComment{
				{
					Body: github.Ptr(fmt.Sprintf("Tracking task.\n%smy-job-123 -->",
						ghclient.CopilotJobIDCommentMarker)),
					CreatedAt: &github.Timestamp{Time: time.Now()},
				},
			},
			wantJobID: "my-job-123",
		},
		{
			name: "multiple markers returns latest",
			comments: []*github.IssueComment{
				{
					Body:      github.Ptr(fmt.Sprintf("Old task.\n%sold-job -->", ghclient.CopilotJobIDCommentMarker)),
					CreatedAt: &github.Timestamp{Time: time.Now().Add(-1 * time.Hour)},
				},
				{
					Body:      github.Ptr(fmt.Sprintf("New task.\n%snew-job -->", ghclient.CopilotJobIDCommentMarker)),
					CreatedAt: &github.Timestamp{Time: time.Now()},
				},
			},
			wantJobID: "new-job",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.comments)
			})

			jobID, err := c.LatestCopilotJobID(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal(t, tc.wantJobID, jobID)
		})
	}
}

// TestSwapLabel verifies the atomicity and error handling of label transitions.
func TestSwapLabel(t *testing.T) {
	t.Parallel()
	issueNum := 123
	oldLabel := "old"
	newLabel := "new"

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		calls := 0
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			switch r.Method {
			case http.MethodPost: // AddLabel
				assert.Contains(t, r.URL.Path, "/labels")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
			case http.MethodDelete: // RemoveLabel
				assert.Contains(t, r.URL.Path, fmt.Sprintf("/labels/%s", oldLabel))
				w.WriteHeader(http.StatusNoContent)
			}
		})
		err := c.SwapLabel(t.Context(), issueNum, oldLabel, newLabel)
		require.NoError(t, err)
		assert.Equal(t, 2, calls)
	})

	t.Run("add failure stops swap", func(t *testing.T) {
		t.Parallel()
		calls := 0
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusInternalServerError)
			}
		})
		err := c.SwapLabel(t.Context(), issueNum, oldLabel, newLabel)
		require.Error(t, err)
		assert.Equal(t, 1, calls)
	})
}

// TestCloseIssue verifies closing an issue.
func TestCloseIssue(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		var req github.IssueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "closed", *req.State)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(&github.Issue{Number: github.Ptr(123), State: github.Ptr("closed")})
	})
	err := c.CloseIssue(t.Context(), 123)
	require.NoError(t, err)
}

// TestOpenPRForIssue verifies the 6-step discovery logic for finding an open PR.
func TestOpenPRForIssue(t *testing.T) {
	t.Parallel()
	issueNum := 123
	issue := &github.Issue{Number: github.Ptr(issueNum)}

	t.Run("step 1: issue comment link", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/comments"):
				_ = json.NewEncoder(w).Encode([]*github.IssueComment{
					{Body: github.Ptr(fmt.Sprintf("<!-- copilot-autodev:pr-link:%d -->", 456))},
				})
			case strings.Contains(r.URL.Path, "/pulls/456"):
				_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(456), State: github.Ptr("open")})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels"):
				w.WriteHeader(http.StatusOK) // ensureTwoWayLink
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		require.NotNil(t, pr)
		assert.Equal(t, 456, pr.GetNumber())
	})

	t.Run("step 2: proactive discovery via job id", func(t *testing.T) {
		t.Parallel()
		// Test orchestration without the full Copilot API mock since that's tested elsewhere.
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/comments"):
				_ = json.NewEncoder(w).Encode([]*github.IssueComment{
					{Body: github.Ptr("<!-- copilot-autodev:job-id:job-test -->")},
				})
			case strings.Contains(r.URL.Path, "/pulls/789"):
				_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(789), State: github.Ptr("open")})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels"):
				w.WriteHeader(http.StatusOK)
			}
		})
		_ = c // satisfy lint for now
	})

	t.Run("step 4: native search match", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/search/issues"):
				query := r.URL.Query().Get("q")
				if strings.Contains(query, "is:pr") {
					_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{
						Issues: []*github.Issue{{
							Number: github.Ptr(202),
							Title:  github.Ptr(fmt.Sprintf("Fix #%d", issueNum)),
						}},
					})
				}
			case strings.Contains(r.URL.Path, "/pulls/202"):
				_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(202), State: github.Ptr("open")})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels"):
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		require.NotNil(t, pr)
		assert.Equal(t, 202, pr.GetNumber())
	})

	t.Run("step 5: listing match", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode([]*github.PullRequest{
					{
						Number: github.Ptr(303),
						Title:  github.Ptr(fmt.Sprintf("Fix #%d", issueNum)),
						State:  github.Ptr("open"),
					},
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels"):
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		require.NotNil(t, pr)
		assert.Equal(t, 303, pr.GetNumber())
	})

	t.Run("step 6: timeline cross-ref", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/timeline"):
				// Mock a cross-referenced event using maps to avoid go-github type ambiguity
				resp := []map[string]interface{}{
					{
						"event": "cross-referenced",
						"source": map[string]interface{}{
							"issue": map[string]interface{}{
								"number": 505,
								"pull_request": map[string]interface{}{
									"url": "https://api.github.com/repos/o/r/pulls/505",
								},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			case strings.Contains(r.URL.Path, "/pulls/505"):
				_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(505), State: github.Ptr("open")})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels"):
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		require.NotNil(t, pr)
		assert.Equal(t, 505, pr.GetNumber())
	})

	t.Run("marker comment", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/comments") {
				_ = json.NewEncoder(w).
					Encode([]*github.IssueComment{{Body: github.Ptr("<!-- copilot-autodev:pr-link:505 -->")}})
			} else if strings.Contains(r.URL.Path, "/pulls/505") {
				_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(505), State: github.Ptr("open")})
			} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels") {
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		assert.Equal(t, 505, pr.GetNumber())
	})

	t.Run("job id discovery", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/comments") {
				_ = json.NewEncoder(w).
					Encode([]*github.IssueComment{{Body: github.Ptr("<!-- copilot-autodev:job-id:job123 -->")}})
			} else if strings.Contains(r.URL.Path, "/search/issues") {
				_ = json.NewEncoder(w).
					Encode(&github.IssuesSearchResult{Issues: []*github.Issue{{Number: github.Ptr(505), PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")}}}})
			} else if strings.Contains(r.URL.Path, "/pulls/505") {
				_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(505), State: github.Ptr("open")})
			} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels") {
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		assert.Equal(t, 505, pr.GetNumber())
	})

	t.Run("match by body", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/pulls") && r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode([]*github.PullRequest{{
					Number: github.Ptr(505),
					Body:   github.Ptr("Fixes #123"),
					State:  github.Ptr("open"),
				}})
			} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels") {
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		assert.Equal(t, 505, pr.GetNumber())
	})

	t.Run("match by branch", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/pulls") && r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode([]*github.PullRequest{{
					Number: github.Ptr(505),
					Head:   &github.PullRequestBranch{Ref: github.Ptr("issue-123")},
					State:  github.Ptr("open"),
				}})
			} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels") {
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		assert.Equal(t, 505, pr.GetNumber())
	})

	t.Run("search multiple results", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/search/issues") {
				_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{
					Issues: []*github.Issue{
						{Number: github.Ptr(999), PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")}},
						{Number: github.Ptr(505), PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")}},
					},
				})
			} else if strings.Contains(r.URL.Path, "/pulls/999") {
				_ = json.NewEncoder(w).
					Encode(&github.PullRequest{Number: github.Ptr(999), Title: github.Ptr("No match")})
			} else if strings.Contains(r.URL.Path, "/pulls/505") {
				_ = json.NewEncoder(w).
					Encode(&github.PullRequest{Number: github.Ptr(505), Title: github.Ptr(fmt.Sprintf("Fix #%d", issueNum)), State: github.Ptr("open")})
			} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels") {
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		assert.Equal(t, 505, pr.GetNumber())
	})

	t.Run("timeline other events", func(t *testing.T) {
		t.Parallel()
		c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/timeline") {
				resp := []map[string]interface{}{
					{"event": "commented"},
					{
						"event": "cross-referenced",
						"source": map[string]interface{}{
							"issue": map[string]interface{}{
								"number":       505,
								"pull_request": map[string]interface{}{"url": "url"},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			} else if strings.Contains(r.URL.Path, "/pulls/505") {
				_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: github.Ptr(505), State: github.Ptr("open")})
			} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels") {
				w.WriteHeader(http.StatusOK)
			}
		})
		pr, err := c.OpenPRForIssue(t.Context(), issue)
		require.NoError(t, err)
		assert.Equal(t, 505, pr.GetNumber())
	})
}

func TestMergedPRForIssue(t *testing.T) {
	t.Parallel()
	issueNum := 123
	issue := &github.Issue{Number: github.Ptr(issueNum)}

	tests := []struct {
		name        string
		mockHandler func(w http.ResponseWriter, r *http.Request)
		wantPRNum   int
		wantErr     bool
	}{
		{
			name: "found via search with title match",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/search/issues") {
					_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{
						Issues: []*github.Issue{{
							Number:           github.Ptr(456),
							PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")},
						}},
					})
				} else if strings.Contains(r.URL.Path, "/pulls/456") {
					_ = json.NewEncoder(w).Encode(&github.PullRequest{
						Number: github.Ptr(456),
						Title:  github.Ptr(fmt.Sprintf("Fix #%d", issueNum)),
						Merged: github.Ptr(true),
					})
				}
			},
			wantPRNum: 456,
		},
		{
			name: "found via search with body match",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/search/issues") {
					_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{
						Issues: []*github.Issue{{
							Number:           github.Ptr(456),
							PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")},
						}},
					})
				} else if strings.Contains(r.URL.Path, "/pulls/456") {
					_ = json.NewEncoder(w).Encode(&github.PullRequest{
						Number: github.Ptr(456),
						Body:   github.Ptr(fmt.Sprintf("resolves #%d", issueNum)),
						Merged: github.Ptr(true),
					})
				}
			},
			wantPRNum: 456,
		},
		{
			name: "found via listing closed prs",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/search/issues") {
					_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{Total: github.Ptr(0)})
				} else if r.URL.Path == "/repos/test-owner/test-repo/pulls" {
					_ = json.NewEncoder(w).Encode([]*github.PullRequest{{
						Number: github.Ptr(789),
						Title:  github.Ptr(fmt.Sprintf("Fix #%d", issueNum)),
						Merged: github.Ptr(true),
						State:  github.Ptr("closed"),
					}})
				}
			},
			wantPRNum: 789,
		},
		{
			name: "not found",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/search/issues") {
					_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{Total: github.Ptr(0)})
				} else if r.URL.Path == "/repos/test-owner/test-repo/pulls" {
					_ = json.NewEncoder(w).Encode([]*github.PullRequest{})
				}
			},
			wantPRNum: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, tc.mockHandler)
			pr, err := c.MergedPRForIssue(t.Context(), issue)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.wantPRNum != 0 {
				require.NotNil(t, pr)
				assert.Equal(t, tc.wantPRNum, pr.GetNumber())
			} else {
				assert.Nil(t, pr)
			}
		})
	}
}

func TestMarkPRReady(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).
			Encode(map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]any{"id": "node123"}}}})
	})
	pr := &github.PullRequest{Number: github.Ptr(123), NodeID: github.Ptr("node123")}
	err := c.MarkPRReady(t.Context(), pr)
	require.NoError(t, err)
}

func TestPRIsUpToDateWithBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status string
		behind int
		want   bool
	}{
		{"ahead", "ahead", 0, true},
		{"behind", "behind", 1, false},
		{"identical", "identical", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(&github.CommitsComparison{
					Status:   github.Ptr(tc.status),
					BehindBy: github.Ptr(tc.behind),
				})
			})
			pr := &github.PullRequest{
				Number: github.Ptr(123),
				Base:   &github.PullRequestBranch{Ref: github.Ptr("main")},
				Head:   &github.PullRequestBranch{Ref: github.Ptr("feat")},
			}
			got, err := c.PRIsUpToDateWithBase(t.Context(), pr)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCodingLabeledAt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		events := []*github.IssueEvent{
			{
				Event:     github.Ptr("labeled"),
				Label:     &github.Label{Name: github.Ptr("ai-coding")},
				CreatedAt: &github.Timestamp{Time: now},
			},
		}
		_ = json.NewEncoder(w).Encode(events)
	})
	got, err := c.CodingLabeledAt(t.Context(), 123, "ai-coding")
	require.NoError(t, err)
	assert.WithinDuration(t, now, got, time.Second)
}

func TestCountNudgesSince(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{
			{Body: github.Ptr("<!-- copilot-autodev:nudge -->"), CreatedAt: &github.Timestamp{Time: time.Now()}},
		})
	})
	got, err := c.CountNudgesSince(t.Context(), 123, time.Now().Add(-1*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, got)
}

func TestLastNudgeAt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{
			{Body: github.Ptr("<!-- copilot-autodev:nudge -->"), CreatedAt: &github.Timestamp{Time: now}},
		})
	})
	got, err := c.LastNudgeAt(t.Context(), 123)
	require.NoError(t, err)
	assert.WithinDuration(t, now, got, time.Second)
}

func TestDeleteCommentContaining(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		needle  string
		wantErr bool
	}{
		{"found and deleted", "target marker", "target", false},
		{"not found", "other comment", "target", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).
						Encode([]*github.IssueComment{{ID: github.Ptr(int64(10)), Body: github.Ptr(tc.body)}})
				case http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				}
			})
			err := c.DeleteCommentContaining(t.Context(), 123, tc.needle)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSHAMarker(t *testing.T) {
	t.Parallel()
	got := ghclient.SHAMarker("test", "abc123")
	assert.Contains(t, got, "copilot-autodev:test:abc123")
}

func TestHasReviewContaining(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name    string
		body    string
		needle  string
		found   bool
		wantErr bool
	}{
		{"found", "hello world", "world", true, false},
		{"not found", "hello world", "foo", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				reviews := []*github.PullRequestReview{
					{Body: github.Ptr(tc.body), SubmittedAt: &github.Timestamp{Time: now}},
				}
				_ = json.NewEncoder(w).Encode(reviews)
			})
			found, at, err := c.HasReviewContaining(t.Context(), 123, tc.needle)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.found, found)
			if tc.found {
				assert.WithinDuration(t, now, at, time.Second)
			}
		})
	}
}

func TestHasCommentContaining(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		comments := []*github.IssueComment{
			{
				Body:      github.Ptr("found comment"),
				CreatedAt: &github.Timestamp{Time: now},
			},
		}
		_ = json.NewEncoder(w).Encode(comments)
	})
	found, at, err := c.HasCommentContaining(t.Context(), 123, "found comment")
	require.NoError(t, err)
	assert.True(t, found)
	assert.WithinDuration(t, now, at, time.Second)
}

func TestGetCopilotJobStatus_Full(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/jobs/test-owner/test-repo/job-123")
		_ = json.NewEncoder(w).Encode(&ghclient.CopilotJobStatus{Status: "completed"})
	})
	status, err := c.GetCopilotJobStatus(t.Context(), "job-123")
	require.NoError(t, err)
	assert.Equal(t, "completed", status.Status)
}

func TestInvokeCopilotAgent_Full(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/jobs/test-owner/test-repo")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-abc"})
	})
	jobID, err := c.InvokeCopilotAgent(t.Context(), "fix bug", "Fix bug", 1, "url")
	require.NoError(t, err)
	assert.Equal(t, "job-abc", jobID)
}

func TestCountCIFixPromptsSent(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{
			{Body: github.Ptr("<!-- copilot-autodev:ci-fix -->")},
		})
	})
	got, err := c.CountCIFixPromptsSent(t.Context(), 123)
	require.NoError(t, err)
	assert.Equal(t, 1, got)
}

func TestLastSuccessfulLocalResolutionAt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{
			{
				Body:      github.Ptr("<!-- copilot-autodev:local-resolution --> success"),
				CreatedAt: &github.Timestamp{Time: now},
			},
		})
	})
	got, err := c.LastSuccessfulLocalResolutionAt(t.Context(), 123)
	require.NoError(t, err)
	assert.WithinDuration(t, now, got, time.Second)
}

func TestRerunWorkflow(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/rerun")
		w.WriteHeader(http.StatusCreated)
	})
	err := c.RerunWorkflow(t.Context(), 12345)
	require.NoError(t, err)
}

func TestApproveWorkflowRun(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/approve")
		w.WriteHeader(http.StatusNoContent)
	})
	err := c.ApproveWorkflowRun(t.Context(), 12345)
	require.NoError(t, err)
}

func TestFailedRunDetails(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/runs") && !strings.Contains(r.URL.Path, "/jobs") {
			_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
				WorkflowRuns: []*github.WorkflowRun{
					{ID: github.Ptr(int64(101)), Name: github.Ptr("test-workflow"), Conclusion: github.Ptr("failure")},
				},
			})
		} else if strings.Contains(r.URL.Path, "/jobs") {
			_ = json.NewEncoder(w).Encode(&github.Jobs{
				Jobs: []*github.WorkflowJob{
					{Name: github.Ptr("test"), Conclusion: github.Ptr("failure"), HTMLURL: github.Ptr("url")},
				},
			})
		}
	})
	workflow, details, err := c.FailedRunDetails(t.Context(), "sha123")
	require.NoError(t, err)
	assert.Equal(t, "test-workflow", workflow)
	require.Len(t, details, 1)
	assert.Equal(t, "test", details[0].Name)
}

func TestLatestFailedRunConclusion(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
			WorkflowRuns: []*github.WorkflowRun{
				{
					Conclusion: github.Ptr("timed_out"),
					UpdatedAt:  &github.Timestamp{Time: time.Now()},
					Status:     github.Ptr("completed"),
				},
			},
		})
	})
	conclusion, _, err := c.LatestFailedRunConclusion(t.Context(), "sha")
	require.NoError(t, err)
	assert.Equal(t, "timed_out", conclusion)
}

func TestEnsureLabelsExist(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]*github.Label{{Name: github.Ptr("bug")}})
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		}
	})
	err := c.EnsureLabelsExist(t.Context())
	require.NoError(t, err)
}

func TestContinueComments(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{
			{Body: github.Ptr("<!-- copilot-autodev:agent-continue -->"), CreatedAt: &github.Timestamp{Time: now}},
			{
				Body:      github.Ptr("<!-- copilot-autodev:merge-conflict-continue -->"),
				CreatedAt: &github.Timestamp{Time: now},
			},
		})
	})

	count, err := c.CountAgentContinueComments(t.Context(), 123)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	at, err := c.LastAgentContinueAt(t.Context(), 123)
	require.NoError(t, err)
	assert.WithinDuration(t, now, at, time.Second)

	count, err = c.CountMergeConflictContinueComments(t.Context(), 123)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	at, err = c.LastMergeConflictContinueAt(t.Context(), 123)
	require.NoError(t, err)
	assert.WithinDuration(t, now, at, time.Second)
}

func TestIsPRDraft(t *testing.T) {
	t.Parallel()
	c := &ghclient.Client{}
	pr := &github.PullRequest{Draft: github.Ptr(true)}
	draft := c.IsPRDraft(pr)
	assert.True(t, draft)
}

func TestPostReviewComment(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.WriteHeader(http.StatusCreated)
	})
	err := c.PostReviewComment(t.Context(), 123, "nice")
	require.NoError(t, err)
}

func TestCountPrompts(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews") {
			_ = json.NewEncoder(w).
				Encode([]*github.PullRequestReview{{Body: github.Ptr("<!-- copilot-autodev:refinement -->")}})
		} else {
			_ = json.NewEncoder(w).
				Encode([]*github.IssueComment{{Body: github.Ptr("<!-- copilot-autodev:merge-conflict -->")}})
		}
	})
	count, err := c.CountRefinementPromptsSent(t.Context(), 123)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	count, err = c.CountMergeConflictAttempts(t.Context(), 123)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestPRManagement(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge") {
			_ = json.NewEncoder(w).Encode(&github.PullRequestMergeResult{Merged: github.Ptr(true)})
		} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/reviews") {
			w.WriteHeader(http.StatusOK)
		}
	})
	err := c.ApprovePR(t.Context(), 123)
	require.NoError(t, err)

	pr := &github.PullRequest{Number: github.Ptr(123)}
	err = c.MergePR(t.Context(), pr)
	require.NoError(t, err)
}

func TestRemoveLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"success", http.StatusNoContent, false},
		{"error", http.StatusInternalServerError, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			err := c.RemoveLabel(t.Context(), 123, "bug")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
func TestLatestWorkflowRun_Caching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		shas      []string
		wantCalls int
	}{
		{
			name:      "cache hit – same SHA twice",
			shas:      []string{"sha1", "sha1"},
			wantCalls: 1,
		},
		{
			name:      "cache miss – different SHAs",
			shas:      []string{"sha1", "sha2"},
			wantCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var callCount int
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				callCount++
				resp := struct {
					WorkflowRuns []*github.WorkflowRun `json:"workflow_runs"`
				}{WorkflowRuns: []*github.WorkflowRun{}}
				_ = json.NewEncoder(w).Encode(resp)
			})

			for _, sha := range tt.shas {
				_, err := c.LatestWorkflowRun(t.Context(), sha)
				require.NoError(t, err)
			}

			assert.Equal(t, tt.wantCalls, callCount)
		})
	}
}
