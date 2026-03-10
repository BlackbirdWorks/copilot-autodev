package poller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/poller"
)

func TestPRTask_SyncBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pr          *github.PullRequest
		mockHandler func(w http.ResponseWriter, r *http.Request)
		wantStop    bool
		wantCurrent string
		wantErr     bool
	}{
		{
			name: "already up to date",
			pr: &github.PullRequest{
				Number: github.Ptr(123),
				Head:   &github.PullRequestBranch{SHA: github.Ptr("head-sha")},
			},
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/pulls/123") && r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(&github.PullRequest{
						Base: &github.PullRequestBranch{SHA: github.Ptr("base-sha")},
						Head: &github.PullRequestBranch{SHA: github.Ptr("head-sha")},
					})
					return
				}
				if strings.Contains(r.URL.Path, "/compare/") {
					_ = json.NewEncoder(w).Encode(&github.CommitsComparison{
						Status:   github.Ptr("ahead"),
						BehindBy: github.Ptr(0),
					})
					return
				}
			},
			wantStop: false,
		},
		{
			name: "behind but mergeable - native update",
			pr: &github.PullRequest{
				Number:         github.Ptr(123),
				Head:           &github.PullRequestBranch{SHA: github.Ptr("head-sha")},
				MergeableState: github.Ptr("clean"),
			},
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/compare/") {
					_ = json.NewEncoder(w).Encode(&github.CommitsComparison{
						Status:   github.Ptr("behind"),
						BehindBy: github.Ptr(5),
					})
					return
				}
				if strings.Contains(r.URL.Path, "/pulls/123/update-branch") && r.Method == http.MethodPut {
					w.WriteHeader(http.StatusAccepted)
					return
				}
			},
			wantStop:    true,
			wantCurrent: "Updating branch from base",
		},
		{
			name: "dirty branch - request merge conflict fix",
			pr: &github.PullRequest{
				Number:         github.Ptr(123),
				Head:           &github.PullRequestBranch{SHA: github.Ptr("head-sha")},
				MergeableState: github.Ptr("dirty"),
			},
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/compare/") {
					_ = json.NewEncoder(w).Encode(&github.CommitsComparison{
						Status:   github.Ptr("behind"),
						BehindBy: github.Ptr(5),
					})
					return
				}
				if strings.Contains(r.URL.Path, "/issues/123/comments") {
					if r.Method == http.MethodGet {
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
						return
					}
					w.WriteHeader(http.StatusCreated)
				}
			},
			wantStop: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, tt.mockHandler)
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P:           p,
				PR:          tt.pr,
				Num:         tt.pr.GetNumber(),
				Sha:         tt.pr.GetHead().GetSHA(),
				DisplayInfo: displayInfo,
			}

			stop, err := task.SyncBranch(ctx)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStop, stop)
			if tt.wantCurrent != "" {
				assert.Contains(t, displayInfo[tt.pr.GetNumber()].Current, tt.wantCurrent)
			}
		})
	}
}

func TestPRTask_ApproveRuns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mockHandler func(w http.ResponseWriter, r *http.Request)
		wantStop    bool
		wantErr     bool
	}{
		{
			name: "approve action required and pending deployment",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/actions/runs") && !strings.Contains(path, "/jobs") && !strings.Contains(path, "/approve") && !strings.Contains(path, "/pending_deployments") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
						WorkflowRuns: []*github.WorkflowRun{{
							ID:     github.Ptr(int64(101)),
							Name:   github.Ptr("CI"),
							Status: github.Ptr("action_required"),
						}},
					})
				case strings.Contains(path, "/actions/runs/101/approve") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusNoContent)
				case strings.Contains(path, "/actions/runs") && strings.Contains(path, "/pending_deployments") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode([]*github.PendingDeployment{
						{Environment: &github.PendingDeploymentEnvironment{Name: github.Ptr("prod")}},
					})
				case strings.Contains(path, "/actions/runs") && strings.Contains(path, "/pending_deployments") && r.Method == http.MethodPost:
					_ = json.NewEncoder(w).Encode(map[string]int{"count": 1})
				}
			},
			wantStop: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, tt.mockHandler)
			pr := &github.PullRequest{Number: github.Ptr(123), Head: &github.PullRequestBranch{SHA: github.Ptr("sha")}}
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P:           p,
				PR:          pr,
				Num:         123,
				Sha:         "sha",
				DisplayInfo: displayInfo,
			}

			stop, err := task.ApproveRuns(ctx)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStop, stop)
		})
	}
}

func TestPRTask_CheckGates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mockHandler func(w http.ResponseWriter, _ *http.Request)
		wantStop    bool
		wantCurrent string
		wantErr     bool
	}{
		{
			name: "no pending deployments",
			mockHandler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{}})
			},
			wantStop: false,
		},
		{
			name: "pending deployments - blocked",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.Contains(path, "/actions/runs") && !strings.Contains(path, "/comments") {
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
						WorkflowRuns: []*github.WorkflowRun{{
							ID:         github.Ptr(int64(202)),
							Name:       github.Ptr("Deploy"),
							Status:     github.Ptr("completed"),
							Conclusion: github.Ptr("action_required"),
						}},
					})
					return
				}
				if strings.Contains(path, "/issues/123/comments") {
					if r.Method == http.MethodGet {
						_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
						return
					}
					w.WriteHeader(http.StatusCreated)
				}
			},
			wantStop:    true,
			wantCurrent: "Waiting for manual deployment approval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, tt.mockHandler)
			pr := &github.PullRequest{Number: github.Ptr(123), Head: &github.PullRequestBranch{SHA: github.Ptr("sha")}}
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P:           p,
				PR:          pr,
				Num:         123,
				Sha:         "sha",
				DisplayInfo: displayInfo,
			}

			stop, err := task.CheckGates(ctx)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStop, stop)
			if tt.wantCurrent != "" {
				assert.Contains(t, displayInfo[123].Current, tt.wantCurrent)
			}
		})
	}
}

func TestPRTask_HandleTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mockHandler func(w http.ResponseWriter, _ *http.Request)
		wantStop    bool
		wantCurrent string
		wantErr     bool
	}{
		{
			name: "no failed runs",
			mockHandler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{}})
			},
			wantStop: false,
		},
		{
			name: "failed run - timeout delay not reached",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.Contains(path, "/actions/runs") {
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
						WorkflowRuns: []*github.WorkflowRun{{
							Status:     github.Ptr("completed"),
							Conclusion: github.Ptr("timed_out"),
							UpdatedAt:  &github.Timestamp{Time: time.Now()},
						}},
					})
					return
				}
				if strings.Contains(path, "/comments") {
					_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
					return
				}
			},
			wantStop:    true,
			wantCurrent: "Agent timed out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, tt.mockHandler)
			p.Cfg().AgentTimeoutRetryDelaySeconds = 600
			pr := &github.PullRequest{Number: github.Ptr(123), Head: &github.PullRequestBranch{SHA: github.Ptr("sha")}}
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P:           p,
				PR:          pr,
				Num:         123,
				Sha:         "sha",
				DisplayInfo: displayInfo,
			}

			stop, err := task.HandleTimeout(ctx)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStop, stop)
			if tt.wantCurrent != "" {
				assert.Contains(t, displayInfo[123].Current, tt.wantCurrent)
			}
		})
	}
}

func TestPRTask_Refine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mockHandler func(w http.ResponseWriter, r *http.Request)
		wantStop    bool
		wantSent    int
		wantErr     bool
	}{
		{
			name: "first refinement",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/actions/runs") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{}})
				case strings.Contains(path, "/pulls/123/reviews") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode([]*github.PullRequestReview{})
				case strings.Contains(path, "/pulls/123/reviews") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusCreated)
				}
			},
			wantStop: true,
			wantSent: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, tt.mockHandler)
			pr := &github.PullRequest{
				Number: github.Ptr(123),
				Head:   &github.PullRequestBranch{SHA: github.Ptr("head-sha")},
			}
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P:           p,
				PR:          pr,
				Num:         123,
				Sha:         "head-sha",
				DisplayInfo: displayInfo,
			}

			stop, err := task.Refine(ctx)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStop, stop)
			assert.Equal(t, tt.wantSent, task.Sent)
		})
	}
}

func TestPRTask_Merge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		allOK       bool
		mockHandler func(w http.ResponseWriter, r *http.Request)
		wantErr     bool
	}{
		{
			name:  "merge success",
			allOK: true,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/pulls/123/reviews") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusCreated)
				case strings.Contains(path, "/pulls/123/merge") && r.Method == http.MethodPut:
					w.WriteHeader(http.StatusOK)
				case strings.Contains(path, "/issues/123/labels") && r.Method == http.MethodDelete:
					w.WriteHeader(http.StatusOK)
				case strings.Contains(path, "/issues/123") && r.Method == http.MethodPatch:
					w.WriteHeader(http.StatusOK)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := setupMockPoller(t, tt.mockHandler)
			pr := &github.PullRequest{Number: github.Ptr(123), Head: &github.PullRequestBranch{SHA: github.Ptr("sha")}}
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P:           p,
				PR:          pr,
				Num:         123,
				Sha:         "head-sha",
				DisplayInfo: displayInfo,
				AllOK:       tt.allOK,
			}

			err := task.Merge(ctx)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestPRTask_WaitForCI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name        string
		mockHandler func(w http.ResponseWriter, r *http.Request)
		wantStop    bool
		wantErr     bool
	}{
		{
			name: "all runs successful",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.Contains(path, "/actions/runs") && r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
						WorkflowRuns: []*github.WorkflowRun{{
							Status:     github.Ptr("completed"),
							Conclusion: github.Ptr("success"),
						}},
					})
				}
			},
			wantStop: false,
		},
		{
			name: "run in progress",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.Contains(path, "/actions/runs") && r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
						WorkflowRuns: []*github.WorkflowRun{{
							Status: github.Ptr("in_progress"),
						}},
					})
				}
			},
			wantStop: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, tt.mockHandler)
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P: p,
				PR: &github.PullRequest{
					Number: github.Ptr(123),
					Head:   &github.PullRequestBranch{SHA: github.Ptr("sha")},
				},
				Num:         123,
				Sha:         "sha",
				DisplayInfo: displayInfo,
			}
			stop, err := task.WaitForCI(ctx)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantStop, stop)
			}
		})
	}
}

func TestPRTask_FixCI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name        string
		mockHandler func(w http.ResponseWriter, r *http.Request)
		wantStop    bool
		wantErr     bool
	}{
		{
			name: "CI fix posted",
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.Contains(path, "/actions/runs") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
						WorkflowRuns: []*github.WorkflowRun{{
							ID:         github.Ptr(int64(101)),
							Name:       github.Ptr("CI"),
							Status:     github.Ptr("completed"),
							Conclusion: github.Ptr("failure"),
							UpdatedAt:  &github.Timestamp{Time: time.Now().Add(-1 * time.Hour)},
						}},
					})
				case strings.Contains(path, "/actions/runs/101/jobs") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(&github.Jobs{
						Jobs: []*github.WorkflowJob{{
							Name:       github.Ptr("test"),
							Conclusion: github.Ptr("failure"),
						}},
					})
				case strings.Contains(path, "/issues/123/comments") && r.Method == http.MethodPost:
					w.WriteHeader(http.StatusCreated)
				case strings.Contains(path, "/issues/123/comments") && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
				}
			},
			wantStop: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := setupMockPoller(t, tt.mockHandler)
			p.Cfg().MaxCIFixRounds = 3
			displayInfo := make(map[int]*poller.IssueDisplayInfo)
			task := &poller.PRTask{
				P: p,
				PR: &github.PullRequest{
					Number: github.Ptr(123),
					Head:   &github.PullRequestBranch{SHA: github.Ptr("sha")},
				},
				Num:         123,
				Sha:         "sha",
				DisplayInfo: displayInfo,
				AnyFail:     true,
			}
			stop, err := task.FixCI(ctx)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantStop, stop)
			}
		})
	}
}

func TestPRTask_Run(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := setupMockPoller(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/compare/") {
			_ = json.NewEncoder(w).Encode(&github.CommitsComparison{
				Status:   github.Ptr("ahead"),
				BehindBy: github.Ptr(0),
			})
		}
	})
	displayInfo := make(map[int]*poller.IssueDisplayInfo)
	task := &poller.PRTask{
		P: p,
		PR: &github.PullRequest{
			Number: github.Ptr(123),
			Head:   &github.PullRequestBranch{SHA: github.Ptr("sha")},
		},
		Num:         123,
		Sha:         "sha",
		DisplayInfo: displayInfo,
	}

	err := task.Run(ctx)
	assert.NoError(t, err)
}
