package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/poller"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PollEvent wraps a poller.Event for delivery into the Bubble Tea message bus.
type PollEvent struct{ poller.Event }

// Model is the root Bubble Tea model for the dashboard.
type Model struct {
	spinner spinner.Model
	width   int
	height  int

	queue   []*poller.State
	coding  []*poller.State
	review  []*poller.State
	lastRun time.Time
	lastErr error

	owner string
	repo  string
}

// New creates a fresh Model.
func New(owner, repo string) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff87"))
	return Model{
		spinner: sp,
		owner:   owner,
		repo:    repo,
	}
}

// Init starts the spinner.
func (m Model) Init() tea.Cmd {
	return m.spinner.Tick
}

// Update handles all messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case PollEvent:
		m.queue = msg.Queue
		m.coding = msg.Coding
		m.review = msg.Review
		m.lastRun = msg.LastRun
		m.lastErr = msg.Err

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

// View renders the full dashboard.
func (m Model) View() string {
	if m.width == 0 {
		return "Initializing…"
	}

	// Reserve space for title (3 lines) + status bar (1 line) + borders (2).
	colHeight := m.height - 6
	if colHeight < 5 {
		colHeight = 5
	}
	colWidth := (m.width - 8) / 3
	if colWidth < 20 {
		colWidth = 20
	}

	title := titleStyle.Width(m.width).Render(
		fmt.Sprintf(" 🤖  Copilot Orchestrator  ·  %s/%s ", m.owner, m.repo),
	)

	queueCol := m.renderColumn("📋  Queue", headerQueue, badgeQueue,
		m.queue, colWidth, colHeight)
	codingCol := m.renderColumn("⚙️   Active (Coding)", headerCoding, badgeCoding,
		m.coding, colWidth, colHeight)
	reviewCol := m.renderColumn("🔍  In Review (CI/Fix)", headerReview, badgeReview,
		m.review, colWidth, colHeight)

	columns := lipgloss.JoinHorizontal(lipgloss.Top,
		queueCol, "  ", codingCol, "  ", reviewCol)

	statusLine := m.renderStatus()

	return lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		columns,
		statusLine,
	)
}

func (m Model) renderColumn(
	header string,
	headerSt lipgloss.Style,
	_ lipgloss.Style,
	states []*poller.State,
	width, height int,
) string {
	var sb strings.Builder
	sb.WriteString(headerSt.Render(header))
	sb.WriteString(fmt.Sprintf("  (%d)\n", len(states)))

	linesUsed := 2
	for _, s := range states {
		line := m.renderItem(s)
		sb.WriteString(line)
		sb.WriteString("\n")
		linesUsed++
		if linesUsed >= height-2 {
			remaining := len(states) - (linesUsed - 2)
			if remaining > 0 {
				sb.WriteString(dimItemStyle.Render(fmt.Sprintf("  … %d more", remaining)))
				sb.WriteString("\n")
			}
			break
		}
	}

	// Pad to fill the column height.
	for linesUsed < height-2 {
		sb.WriteString("\n")
		linesUsed++
	}

	return columnStyle.Width(width).Height(height).Render(sb.String())
}

func (m Model) renderItem(s *poller.State) string {
	issue := s.Issue
	num := issueNumStyle.Render(fmt.Sprintf("#%d", issue.GetNumber()))
	title := issue.GetTitle()
	if len(title) > 42 {
		title = title[:39] + "…"
	}

	age := ghclient.TimeAgo(issue.GetCreatedAt().Time)
	line := fmt.Sprintf("  %s %s", num, itemStyle.Render(title))

	if s.PR != nil {
		prPart := prNumStyle.Render(fmt.Sprintf(" → PR#%d", s.PR.GetNumber()))
		line += prPart
	}
	line += dimItemStyle.Render(fmt.Sprintf("  [%s]", age))
	return line
}

func (m Model) renderStatus() string {
	spin := m.spinner.View()
	var parts []string

	if m.lastRun.IsZero() {
		parts = append(parts, spin+" Waiting for first poll…")
	} else {
		parts = append(parts, spin+fmt.Sprintf(" Last poll: %s", ghclient.TimeAgo(m.lastRun)))
	}

	total := len(m.queue) + len(m.coding) + len(m.review)
	parts = append(parts, fmt.Sprintf("Issues tracked: %d", total))
	parts = append(parts, "Press q / Ctrl-C to quit")

	status := statusBarStyle.Render(strings.Join(parts, "  ·  "))

	if m.lastErr != nil {
		errStr := m.lastErr.Error()
		if len(errStr) > 80 {
			errStr = errStr[:77] + "…"
		}
		status = lipgloss.JoinVertical(lipgloss.Left,
			status,
			errorStyle.Render("⚠  "+errStr),
		)
	}
	return status
}
