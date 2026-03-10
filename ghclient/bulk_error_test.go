package ghclient_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
)

func TestBulkErrors(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx := t.Context()
	pr := &github.PullRequest{Number: github.Ptr(456)}

	t.Run("IssuesByLabel", func(t *testing.T) {
		t.Parallel()
		_, err := c.IssuesByLabel(ctx, "bug")
		require.Error(t, err)
	})
	t.Run("AddLabel", func(t *testing.T) {
		t.Parallel()
		err := c.AddLabel(ctx, 123, "bug")
		require.Error(t, err)
	})
	t.Run("RemoveLabel", func(t *testing.T) {
		t.Parallel()
		err := c.RemoveLabel(ctx, 123, "bug")
		require.Error(t, err)
	})
	t.Run("CloseIssue", func(t *testing.T) {
		t.Parallel()
		err := c.CloseIssue(ctx, 123)
		require.Error(t, err)
	})
	t.Run("PostComment", func(t *testing.T) {
		t.Parallel()
		err := c.PostComment(ctx, 123, "hi")
		require.Error(t, err)
	})
	t.Run("PostReviewComment", func(t *testing.T) {
		t.Parallel()
		err := c.PostReviewComment(ctx, 123, "hi")
		require.Error(t, err)
	})
	t.Run("ApprovePR", func(t *testing.T) {
		t.Parallel()
		err := c.ApprovePR(ctx, 456)
		require.Error(t, err)
	})
	t.Run("MergePR", func(t *testing.T) {
		t.Parallel()
		err := c.MergePR(ctx, pr)
		require.Error(t, err)
	})
	t.Run("UpdatePRBranch", func(t *testing.T) {
		t.Parallel()
		err := c.UpdatePRBranch(ctx, 456)
		require.Error(t, err)
	})
	t.Run("LatestWorkflowRun", func(t *testing.T) {
		t.Parallel()
		_, err := c.LatestWorkflowRun(ctx, "feat")
		require.Error(t, err)
	})
	t.Run("LatestFailedRunConclusion", func(t *testing.T) {
		t.Parallel()
		_, _, err := c.LatestFailedRunConclusion(ctx, "feat")
		require.Error(t, err)
	})
	t.Run("IsPRDraft", func(t *testing.T) {
		t.Parallel()
		_ = c.IsPRDraft(pr)
	})
	t.Run("IsBranchBehindBase", func(t *testing.T) {
		t.Parallel()
		_, err := c.IsBranchBehindBase(ctx, pr)
		require.Error(t, err)
	})
	t.Run("PRIsUpToDateWithBase", func(t *testing.T) {
		t.Parallel()
		_, err := c.PRIsUpToDateWithBase(ctx, pr)
		require.Error(t, err)
	})
	t.Run("FindPRFromTimeline", func(t *testing.T) {
		t.Parallel()
		_, err := c.FindPRFromTimeline(ctx, 123)
		require.Error(t, err)
	})
	t.Run("FindPRBySearch", func(t *testing.T) {
		t.Parallel()
		_ = c.FindPRBySearch(ctx, 123)
	})
	t.Run("FindPRByListing", func(t *testing.T) {
		t.Parallel()
		_ = c.FindPRByListing(ctx, 123)
	})
	t.Run("RerunWorkflow", func(t *testing.T) {
		t.Parallel()
		err := c.RerunWorkflow(ctx, 123)
		require.Error(t, err)
	})
	t.Run("ApproveWorkflowRun", func(t *testing.T) {
		t.Parallel()
		err := c.ApproveWorkflowRun(ctx, 123)
		require.Error(t, err)
	})
	t.Run("FailedRunDetails", func(t *testing.T) {
		t.Parallel()
		_, _, err := c.FailedRunDetails(ctx, "sha123")
		require.Error(t, err)
	})
	t.Run("EnsureLabelsExist", func(t *testing.T) {
		t.Parallel()
		err := c.EnsureLabelsExist(ctx)
		require.Error(t, err)
	})
}

type errorRoundTripper struct{}

func (e *errorRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("network error")
}

func TestNetworkErrors(t *testing.T) {
	t.Parallel()
	rt := &errorRoundTripper{}
	cfg := &config.Config{GitHubOwner: "test-owner", GitHubRepo: "test-repo"}
	c := ghclient.NewWithTransport("test-token", cfg, rt)
	ctx := t.Context()

	t.Run("InvokeCopilotAgent_NetworkError", func(t *testing.T) {
		t.Parallel()
		_, err := c.InvokeCopilotAgent(ctx, "prompt", "title", 123, "url")
		require.Error(t, err)
	})
	t.Run("GetCopilotJobStatus_NetworkError", func(t *testing.T) {
		t.Parallel()
		_, err := c.GetCopilotJobStatus(ctx, "job123")
		require.Error(t, err)
	})
}

func TestSscanfError(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.IssueComment{{
			Body: github.Ptr("<!-- copilot-autodev:issue-link:notanumber -->"),
		}})
	})
	_, _ = c.OpenPRForIssue(t.Context(), &github.Issue{Number: github.Ptr(123)})
}

func TestFindPRByMarkerComment_Error(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	pr := c.FindPRByMarkerComment(t.Context(), 123)
	assert.Nil(t, pr)
}

func TestCountCIFixPromptsSent_Error(t *testing.T) {
	t.Parallel()
	c := setupMockGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, _ = c.CountCIFixPromptsSent(t.Context(), 123)
}
