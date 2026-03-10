package ghclient_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BlackbirdWorks/copilot-autodev/config"
	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
)

type fakeRoundTripper struct {
	handler func(*http.Request) (*http.Response, error)
}

func (f *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.handler(r)
}

func setupMockGitHubAPI(t *testing.T, handler http.HandlerFunc) *ghclient.Client {
	t.Helper()
	rt := &fakeRoundTripper{
		handler: func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler(rec, r)
			resp := rec.Result()
			return resp, nil
		},
	}
	cfg := &config.Config{
		GitHubOwner: "test-owner",
		GitHubRepo:  "test-repo",
	}
	c := ghclient.NewWithTransport("test-token", cfg, rt)
	return c
}
