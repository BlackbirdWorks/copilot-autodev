package ghclient_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
)

func TestIsMatchForIssue_Units(t *testing.T) {
	t.Parallel()
	c := &ghclient.Client{}
	issueNum := 123
	tests := []struct {
		name string
		pr   *github.PullRequest
		want bool
	}{
		{"title match", &github.PullRequest{Title: github.Ptr("Fix #123")}, true},
		{"body match", &github.PullRequest{Body: github.Ptr("resolves #123")}, true},
		{"branch match", &github.PullRequest{Head: &github.PullRequestBranch{Ref: github.Ptr("issue-123")}}, true},
		{
			"no match",
			&github.PullRequest{
				Title: github.Ptr("Fix #456"),
				Body:  github.Ptr("no"),
				Head:  &github.PullRequestBranch{Ref: github.Ptr("main")},
			},
			false,
		},
		{"nil fields", &github.PullRequest{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := c.IsMatchForIssue(tc.pr, issueNum)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDeleteCommentContaining_Extra(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
	})
	err := c.DeleteCommentContaining(t.Context(), 123, "marker")
	require.NoError(t, err)
}

func TestPRIsUpToDateWithBase_Extra(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	pr := &github.PullRequest{
		Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
		Head: &github.PullRequestBranch{Ref: github.Ptr("feat")},
	}
	_, err := c.PRIsUpToDateWithBase(t.Context(), pr)
	require.Error(t, err)
}

func TestInvokeCopilotAgent_Error(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := c.InvokeCopilotAgent(t.Context(), "prompt", "title", 123, "url")
	require.Error(t, err)
}

func TestGetCopilotJobStatus_Error(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.GetCopilotJobStatus(t.Context(), "job123")
	require.Error(t, err)
}

func TestMergedPRForIssue_Error(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := c.MergedPRForIssue(t.Context(), &github.Issue{Number: github.Ptr(123)})
	require.Error(t, err)
}

func TestFindMatchInSearchResults_Exhaustive(t *testing.T) {
	t.Parallel()
	issueNum := 123
	tests := []struct {
		name      string
		issues    []*github.Issue
		mock      func(w http.ResponseWriter, r *http.Request)
		wantPRNum int
	}{
		{
			name: "skip no pr link",
			issues: []*github.Issue{
				{Number: github.Ptr(456)},
			},
			wantPRNum: 0,
		},
		{
			name: "skip same number",
			issues: []*github.Issue{
				{Number: github.Ptr(123), PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")}},
			},
			wantPRNum: 0,
		},
		{
			name: "skip get error",
			issues: []*github.Issue{
				{Number: github.Ptr(456), PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")}},
			},
			mock: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantPRNum: 0,
		},
		{
			name: "skip closed or no match",
			issues: []*github.Issue{
				{Number: github.Ptr(456), PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")}},
			},
			mock: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(&github.PullRequest{
					Number: github.Ptr(456),
					State:  github.Ptr("closed"),
				})
			},
			wantPRNum: 0,
		},
		{
			name: "success match",
			issues: []*github.Issue{
				{Number: github.Ptr(456), PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("url")}},
			},
			mock: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(&github.PullRequest{
					Number: github.Ptr(456),
					State:  github.Ptr("open"),
					Title:  github.Ptr("Fix #123"),
				})
			},
			wantPRNum: 456,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.mock != nil {
					tc.mock(w, r)
				}
			})
			pr := c.FindMatchInSearchResults(t.Context(), issueNum, tc.issues)
			if tc.wantPRNum == 0 {
				assert.Nil(t, pr)
			} else {
				require.NotNil(t, pr)
				assert.Equal(t, tc.wantPRNum, pr.GetNumber())
			}
		})
	}
}

func TestDiscoverPRViaJobID_Gaps(t *testing.T) {
	t.Parallel()
	issueNum := 123
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/123/comments") {
			// Return a comment with a job ID
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{{
				Body: github.Ptr("<!-- copilot-autodev:job-id:abc-123 -->"),
			}})
		} else if strings.Contains(r.URL.Path, "/search/issues") {
			// Mock search failure
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	pr := c.DiscoverPRViaJobID(t.Context(), issueNum)
	assert.Nil(t, pr)
}

func TestUpdatePRBranch_Error_Extra(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	err := c.UpdatePRBranch(t.Context(), 456)
	require.Error(t, err)
}

func TestFindPRFromTimeline_Error_Extra(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.FindPRFromTimeline(t.Context(), 123)
	require.Error(t, err)
}

func TestAllRunsSucceeded_Extra(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
				WorkflowRuns: []*github.WorkflowRun{
					{Conclusion: github.Ptr("failure")},
				},
			})
		}
	})
	ok, _, err := c.AllRunsSucceeded(t.Context(), "feat")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestLastSuccessfulLocalResolutionAt_Extra(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{})
	})
	_, err := c.LastSuccessfulLocalResolutionAt(t.Context(), 123)
	require.NoError(t, err)
}

func TestUpdatePRBranch_Success(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(&github.PullRequestBranchUpdateResponse{
			Message: github.Ptr("updated"),
		})
	})
	err := c.UpdatePRBranch(t.Context(), 456)
	require.NoError(t, err)
}

func TestInvokeCopilotAgent_Success(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id": "abc-123",
		})
	})
	jobID, err := c.InvokeCopilotAgent(t.Context(), "prompt", "title", 123, "url")
	require.NoError(t, err)
	assert.Equal(t, "abc-123", jobID)
}

func TestDiscoverPRViaJobID_Success(t *testing.T) {
	t.Parallel()
	issueNum := 123
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "comments"):
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{{
				Body:      github.Ptr("<!-- copilot-autodev:job-id:abc-123 -->"),
				CreatedAt: &github.Timestamp{Time: time.Now()},
			}})
		case strings.Contains(path, "abc-123"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"pull_request": map[string]interface{}{"number": 456},
			})
		case strings.Contains(path, "pulls/456"):
			_ = json.NewEncoder(w).Encode(&github.PullRequest{
				Number: github.Ptr(456),
				State:  github.Ptr("open"),
			})
		default:
			_ = json.NewEncoder(w).Encode(&github.PullRequest{})
		}
	})
	pr := c.DiscoverPRViaJobID(t.Context(), issueNum)
	require.NotNil(t, pr)
	assert.Equal(t, 456, pr.GetNumber())
}

func TestIsPRDraft_Extra(t *testing.T) {
	t.Parallel()
	c := &ghclient.Client{}
	assert.True(t, c.IsPRDraft(&github.PullRequest{Draft: github.Ptr(true)}))
	assert.False(t, c.IsPRDraft(&github.PullRequest{Draft: github.Ptr(false)}))
}

func TestIsBranchBehindBase_Extra(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(&github.CommitsComparison{
			Status:   github.Ptr("behind"),
			BehindBy: github.Ptr(5),
		})
	})
	behind, err := c.IsBranchBehindBase(t.Context(), &github.PullRequest{
		Base: &github.PullRequestBranch{Ref: github.Ptr("main")},
		Head: &github.PullRequestBranch{Ref: github.Ptr("feat")},
	})
	require.NoError(t, err)
	assert.True(t, behind)
}

func TestAddLabel_Success(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	err := c.AddLabel(t.Context(), 123, "bug")
	require.NoError(t, err)
}

func TestRemoveLabel_Success(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	err := c.RemoveLabel(t.Context(), 123, "bug")
	require.NoError(t, err)
}

func TestLatestCopilotJobID_Success(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "comments") {
			_ = json.NewEncoder(w).Encode([]*github.IssueComment{{
				Body:      github.Ptr("<!-- copilot-autodev:job-id:abc-123 -->"),
				CreatedAt: &github.Timestamp{Time: time.Now()},
			}})
		}
	})
	id, err := c.LatestCopilotJobID(t.Context(), 123)
	require.NoError(t, err)
	assert.Equal(t, "abc-123", id)
}

func TestLatestWorkflowRun_Success(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "runs") {
			_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
				WorkflowRuns: []*github.WorkflowRun{{
					ID: github.Ptr(int64(123)),
				}},
			})
		}
	})
	run, err := c.LatestWorkflowRun(t.Context(), "dummy-sha")
	require.NoError(t, err)
	require.NotEmpty(t, run)
	assert.Equal(t, int64(123), run[0].GetID())
}

func TestTimeAgo_More(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assert.Equal(t, "just now", ghclient.TimeAgo(now))
	assert.Equal(t, "1m ago", ghclient.TimeAgo(now.Add(-65*time.Second)))
	assert.Equal(t, "1h ago", ghclient.TimeAgo(now.Add(-65*time.Minute)))
	assert.Equal(t, "1d ago", ghclient.TimeAgo(now.Add(-25*time.Hour)))
}
