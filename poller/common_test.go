package poller_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
	"github.com/BlackbirdWorks/copilot-autodev/poller"
)

func setupMockPoller(t *testing.T, handler http.HandlerFunc) *poller.Poller {
	t.Helper()
	rt := &fakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler(rec, r)
			return rec.Result(), nil
		},
	}
	cfg := config.DefaultConfig()
	cfg.GitHubOwner = "test-owner"
	cfg.GitHubRepo = "test-repo"
	cfg.LabelQueue = "ai-todo"
	cfg.LabelCoding = "ai-coding"
	cfg.LabelReview = "ai-review"
	cfg.CopilotInvokeTimeoutSeconds = 60 // faster for tests

	client := ghclient.NewWithTransport("test-token", cfg, rt)
	return poller.New(cfg, client, "test-token")
}

type fakeRoundTripper struct {
	handler func(*http.Request) (*http.Response, error)
}

func (f *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.handler(r)
}
