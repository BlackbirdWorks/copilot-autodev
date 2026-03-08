package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestInvokeCopilotAgent verifies that InvokeCopilotAgent sends a correctly
// formed POST request to the Copilot API and handles success/error responses.
func TestInvokeCopilotAgent(t *testing.T) {
	tests := []struct {
		name           string
		prompt         string
		serverStatus   int
		wantErr        bool
		wantPrompt     string
		wantEventType  string
	}{
		{
			name:          "success – 201 Created",
			prompt:        "Please implement issue #42",
			serverStatus:  http.StatusCreated,
			wantErr:       false,
			wantPrompt:    "Please implement issue #42",
			wantEventType: "copilot-autocode",
		},
		{
			name:          "success – 200 OK also accepted",
			prompt:        "Fix the bug in issue #7",
			serverStatus:  http.StatusOK,
			wantErr:       false,
			wantPrompt:    "Fix the bug in issue #7",
			wantEventType: "copilot-autocode",
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
			var gotReq copilotAgentJobRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected method %s; want POST", r.Method)
				}
				if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				w.WriteHeader(tc.serverStatus)
			}))
			defer srv.Close()

			c := &Client{
				owner: "test-owner",
				repo:  "test-repo",
				token: "test-token",
			}

			err := c.invokeAgentAt(context.Background(), srv.URL+"/agents/swe/v1/jobs/test-owner/test-repo", tc.prompt)

			if tc.wantErr {
				if err == nil {
					t.Error("expected error; got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotReq.ProblemStatement != tc.wantPrompt {
				t.Errorf("ProblemStatement = %q; want %q", gotReq.ProblemStatement, tc.wantPrompt)
			}
			if gotReq.EventType != tc.wantEventType {
				t.Errorf("EventType = %q; want %q", gotReq.EventType, tc.wantEventType)
			}
		})
	}
}


// TestTimeAgo verifies that TimeAgo produces the correct relative-time label
// for representative durations across all four branches of the function.
func TestTimeAgo(t *testing.T) {
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
			got := TimeAgo(tc.t)
			if got != tc.want {
				t.Errorf("TimeAgo(%v) = %q; want %q", tc.t, got, tc.want)
			}
		})
	}
}
