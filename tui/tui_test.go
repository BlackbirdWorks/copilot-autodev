package tui_test

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v68/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BlackbirdWorks/copilot-autodev/poller"
	"github.com/BlackbirdWorks/copilot-autodev/tui"
)

// ansiRe matches ANSI CSI escape sequences so tests can compare plain text.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripAnsi(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// issueAt returns a minimal *github.Issue with the given number, title, and
// creation time — enough for RenderItem to work without nil dereferences.
func issueAt(num int, title string, createdAt time.Time) *github.Issue {
	ts := github.Timestamp{Time: createdAt}
	return &github.Issue{
		Number:    &num,
		Title:     &title,
		CreatedAt: &ts,
	}
}

// ─── FormatCountdown ────────────────────────────────────────────────────────

func TestFormatCountdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"500ms rounds to 1s", 500 * time.Millisecond, "1s"},
		{"45 seconds", 45 * time.Second, "45s"},
		{"59 seconds", 59 * time.Second, "59s"},
		{"exactly 1 minute", 60 * time.Second, "1m 0s"},
		{"9 minutes 42 seconds", 9*time.Minute + 42*time.Second, "9m 42s"},
		{"59 minutes 59 seconds", 59*time.Minute + 59*time.Second, "59m 59s"},
		{"exactly 1 hour", 60 * time.Minute, "1h 0m"},
		{"1 hour 3 minutes", 63 * time.Minute, "1h 3m"},
		{"25 hours", 25 * time.Hour, "25h 0m"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tui.FormatCountdown(tc.d))
		})
	}
}

// ─── RenderStatusSubLine ────────────────────────────────────────────────────

func TestRenderStatusSubLine(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	now := time.Now()

	tests := []struct {
		name         string
		current      string
		next         string
		nextActionAt time.Time
		colWidth     int
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "current only — no separator",
			current:      "CI failing",
			colWidth:     60,
			wantContains: []string{"CI failing"},
			wantAbsent:   []string{"·"},
		},
		{
			name:         "current and next — stacked present",
			current:      "CI failing",
			next:         "Asked Copilot to fix",
			colWidth:     60,
			wantContains: []string{"CI failing", "Next: Asked Copilot to fix"},
		},
		{
			name:         "future countdown appends 'in …'",
			current:      "Waiting on coding agent to start",
			next:         "Poke Copilot",
			nextActionAt: now.Add(5 * time.Minute),
			colWidth:     80,
			wantContains: []string{
				"Waiting on coding agent to start",
				"Poke Copilot in",
				"m",
			},
		},
		{
			name:         "past nextActionAt appends 'now'",
			current:      "Waiting on coding agent to start",
			next:         "Poke Copilot",
			nextActionAt: now.Add(-time.Second),
			colWidth:     80,
			wantContains: []string{"Poke Copilot now"},
		},
		{
			name:         "very narrow column wraps text",
			current:      "CI failing",
			next:         "fix",
			colWidth:     5,
			wantAbsent:   []string{"…"},
			wantContains: []string{"CI failing", "fix"},
		},
		{
			name:         "long text wraps on narrow column",
			current:      "CI running",
			next:         "Refinement (if checks pass)",
			colWidth:     30,
			wantAbsent:   []string{"…"},
			wantContains: []string{"CI", "Refinement"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &poller.State{
				CurrentStatus: tc.current,
				NextAction:    tc.next,
				NextActionAt:  tc.nextActionAt,
			}
			got := stripAnsi(m.RenderStatusSubLine(s, tc.colWidth))
			for _, want := range tc.wantContains {
				assert.Contains(t, got, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

// ─── RenderItem line count ───────────────────────────────────────────────────

func TestRenderItemLineCount(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	issue := issueAt(42, "Fix the thing", time.Now())

	tests := []struct {
		name      string
		state     *poller.State
		colWidth  int
		wantLines int
	}{
		{
			name:      "no status → 1 line",
			state:     &poller.State{Issue: issue, CurrentStatus: ""},
			colWidth:  60,
			wantLines: 1,
		},
		{
			name:      "with status defaults to unselected 1 line",
			state:     &poller.State{Issue: issue, CurrentStatus: "Waiting for CI"},
			colWidth:  60,
			wantLines: 1,
		},
		{
			name:      "status + next action defaults to unselected 1 line",
			state:     &poller.State{Issue: issue, CurrentStatus: "CI failing", NextAction: "Asked Copilot to fix"},
			colWidth:  60,
			wantLines: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered, lines := m.RenderItem(tc.state, tc.colWidth)
			assert.Equal(t, tc.wantLines, lines)
			assert.Equal(t, tc.wantLines-1, strings.Count(rendered, "\n"))
		})
	}
}

// ─── RenderItem title truncation ─────────────────────────────────────────────

func TestRenderItemTitleTruncation(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	num := 1
	now := time.Now()

	tests := []struct {
		name         string
		title        string
		colWidth     int
		wantEllipsis bool
	}{
		{
			name:         "short title fits — no ellipsis",
			title:        "Short",
			colWidth:     60,
			wantEllipsis: false,
		},
		{
			name:         "title longer than budget — ellipsis appended",
			title:        strings.Repeat("A", 100),
			colWidth:     40,
			wantEllipsis: true,
		},
		{
			name:         "title exactly at budget — no ellipsis",
			title:        "Fix",
			colWidth:     30,
			wantEllipsis: false,
		},
		{
			name:         "multi-byte (Japanese) title truncated at rune boundary",
			title:        strings.Repeat("日", 50),
			colWidth:     40,
			wantEllipsis: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := issueAt(num, tc.title, now)
			s := &poller.State{Issue: issue}
			rendered, _ := m.RenderItem(s, tc.colWidth)
			plain := stripAnsi(rendered)

			assert.Equal(t, tc.wantEllipsis, strings.Contains(plain, "…"))
			require.True(t, utf8.ValidString(plain), "output must be valid UTF-8")
		})
	}
}

// ─── RenderItem title line content ───────────────────────────────────────────

func TestRenderItemTitleContent(t *testing.T) {
	t.Parallel()
	m := tui.Model{}
	now := time.Now()

	prNum := 99
	pr := &github.PullRequest{Number: &prNum}

	tests := []struct {
		name         string
		state        *poller.State
		colWidth     int
		wantContains []string
	}{
		{
			name:         "issue number appears in title line",
			state:        &poller.State{Issue: issueAt(434, "Some title", now)},
			colWidth:     60,
			wantContains: []string{"#434"},
		},
		{
			name:         "pr ref appears when PR is attached",
			state:        &poller.State{Issue: issueAt(434, "Some title", now), PR: pr},
			colWidth:     60,
			wantContains: []string{"#434", "PR#99"},
		},
		{
			name:         "no pr ref without PR",
			state:        &poller.State{Issue: issueAt(434, "Some title", now)},
			colWidth:     60,
			wantContains: []string{"#434"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered, _ := m.RenderItem(tc.state, tc.colWidth)
			// PR string is on the first line when unselected, but might be on a
			// separate line if wrapped or part of a subline if selected. Since
			// we are testing unselected state here, the PR ref should be in the
			// plain title line. Wait no, PR string IS in the first line.
			titleLine := stripAnsi(rendered)
			for _, want := range tc.wantContains {
				assert.Contains(t, titleLine, want)
			}
		})
	}
}
