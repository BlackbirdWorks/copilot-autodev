package tui_test

import (
	"testing"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"

	"github.com/BlackbirdWorks/copilot-autodev/poller"
	"github.com/BlackbirdWorks/copilot-autodev/tui"
)

func TestRenderPipelineBar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		state    *poller.State
		contains []string
	}{
		{
			name: "queue state",
			state: &poller.State{
				Status: "queue",
			},
			contains: []string{"● Queued", "○ Coding", "○ Review", "○ Merge"},
		},
		{
			name: "coding state",
			state: &poller.State{
				Status: "coding",
			},
			contains: []string{"✓ Queued", "● Coding", "○ Review", "○ Merge"},
		},
		{
			name: "review - branch sync",
			state: &poller.State{
				Status:        "review",
				CurrentStatus: "Branch Sync: resolving conflicts",
			},
			contains: []string{"● Branch Sync", "○ CI Approval", "○ Merge"},
		},
		{
			name: "review - ci running",
			state: &poller.State{
				Status:        "review",
				CurrentStatus: "CI running",
			},
			contains: []string{"✓ Branch Sync", "✓ CI Approval", "✓ Deploy Gates", "● CI Checks", "○ Refinement"},
		},
		{
			name: "review - refinement",
			state: &poller.State{
				Status:        "review",
				CurrentStatus: "Refinement in progress",
			},
			contains: []string{"✓ CI Checks", "● Refinement", "○ CI Fix"},
		},
		{
			name: "review - merge",
			state: &poller.State{
				Status:        "review",
				CurrentStatus: "All checks passed - merging",
			},
			contains: []string{"✓ CI Fix", "● Merge"},
		},
		{
			name: "terminal success",
			state: &poller.State{
				Status:      "review",
				AgentStatus: "success",
			},
			contains: []string{"✓ Branch Sync", "✓ Merge"},
		},
		{
			name: "terminal failure",
			state: &poller.State{
				Status:        "review",
				CurrentStatus: "CI Checks failed",
				AgentStatus:   "failed",
			},
			contains: []string{"✗ CI Checks"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Add a dummy issue to avoid nil panics if renderPipelineBar uses it
			if tt.state.Issue == nil {
				tt.state.Issue = &github.Issue{Number: github.Ptr(1)}
			}

			got := tui.RenderPipelineBar(tt.state)
			for _, c := range tt.contains {
				assert.Contains(t, got, c)
			}
		})
	}
}
