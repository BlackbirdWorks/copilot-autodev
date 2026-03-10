package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"context"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/BlackbirdWorks/copilot-autodev/ghclient"
	"github.com/BlackbirdWorks/copilot-autodev/poller"
)

// PollEvent wraps a poller.Event for delivery into the Bubble Tea message bus.
type PollEvent struct{ poller.Event }

// LogEvent is emitted by main.go's logWriter to pass standard log messages to the UI.
type LogEvent struct {
	Message string
}

// Layout and animation constants for the dashboard.
const (
	tuiNumCols       = 3  // number of kanban columns
	tuiSpinnerFPS    = 10 // spinner frames per second
	tuiChromeRows    = 9  // rows reserved for title, status bar, borders, margins, and copy button
	tuiLogBoxHeight  = 5  // fixed rows for logs
	logHistorySize   = 50 // simple ring buffer of recent logs
	tuiColMinHeight  = 5  // minimum column height in rows
	tuiColSidePad    = 6  // overhead: 3 cols × 2 border chars (no spacers)
	tuiColMinWidth   = 20 // minimum column width in characters
	tuiItemLPad      = 2  // left-indent for item content inside a column
	tuiItemLineCount = 2  // lines used per item when status sub-line is present
	tuiSubLinePad    = 4  // sub-line indent: Padding(0,1)(=2) + 2-space indent
	tuiSubLineMinW   = 5  // minimum useful sub-line content width
	tuiSecsPerMin    = 60 // seconds per minute for % modulo in formatCountdown
	tuiStatusMaxLen  = 80 // max characters shown for error/warning messages

	tuiDoublePadding = 2 // multiplier for symmetric horizontal padding
	tuiColorSuccess  = "10"
	tuiColorFailure  = "9"
	tuiColorSpinner  = "#00ff87"

	tuiCopyFeedbackDuration = time.Second * 2

	// Layout padding and border constants.
	tuiBorderRows       = 2  // rows for top/bottom borders
	tuiLogBoxPadding    = 2  // horizontal padding for log box
	tuiCopyWidgetMargin = 4  // horizontal margin for copy button
	tuiMinInnerWidth    = 10 // minimum width for truncated content

	// Selection indicator prefix (plain char, styled at render time).
	tuiSelectPrefix    = "▸ "
	tuiNonSelectPrefix = "  "
)

// secondTickMsg is fired every second so live countdown timers in item
// status sub-lines stay up-to-date between poll events.
type secondTickMsg time.Time

func secondTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return secondTickMsg(t)
	})
}

// clearCopyMsg is fired after logs are copied to reset the "[Copied!]" widget.
type clearCopyMsg struct{}

// clearActionMsg is fired after a brief delay to clear the action feedback.
type clearActionMsg struct{}

// mergeLogReloadMsg is fired every 500 ms to re-read the merge log file.
type mergeLogReloadMsg struct{}

// startupReadyMsg fires 2 seconds after the first poll, dismissing the loading screen.
type startupReadyMsg struct{}

// loadingPunTickMsg rotates the loading pun every 1.4 s during startup.
type loadingPunTickMsg struct{}

func loadingPunTick() tea.Cmd {
	return tea.Tick(1400*time.Millisecond, func(_ time.Time) tea.Msg {
		return loadingPunTickMsg{}
	})
}

// loadingPuns is the list of punny messages cycled on the startup screen.
var loadingPuns = []string{
	"Pulling PRs by their bootstraps...",
	"Teaching Copilot to parallel park...",
	"Resolving conflicts the civilised way...",
	"git blame --nobody",
	"Rate-limiting our enthusiasm...",
	"Compiling good vibes...",
	"Untangling the merge spaghetti...",
	"Making tests pass (or at least fail consistently)...",
	"Consulting the rubber duck...",
	"Counting workflow runs: one... two... many...",
	"Waiting for CI: the noblest of pursuits...",
	"Nudging Copilot awake...",
	"Dispatching agents into the void...",
	"SHA-ing my head in disbelief...",
	"Squashing bugs and commits alike...",
	"Rebasing reality on top of main...",
}

// activityFeedMsg delivers fetched timeline entries to the TUI.
type activityFeedMsg struct {
	entries []ghclient.TimelineEntry
	err     error
}

func mergeLogReloadTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(_ time.Time) tea.Msg {
		return mergeLogReloadMsg{}
	})
}

// Model is the root Bubble Tea model for the dashboard.
type Model struct {
	spinner spinner.Model
	width   int
	height  int

	queue    []*poller.State
	coding   []*poller.State
	review   []*poller.State
	lastRun  time.Time
	lastErr  error
	lastWarn string // most recent non-fatal warning (e.g. Copilot assignment failure)

	logs    []string // simple ring buffer of recent logs: size logHistorySize
	logHead int      // index for next log insertion

	logsCopied bool // true if logs were recently copied to clipboard

	// Cursor selection state.
	selectedCol int // 0=queue, 1=coding, 2=review
	selectedRow int // 0-indexed within the selected column

	// Command channel for sending actions back to the poller.
	commandCh chan<- poller.Command

	// Action feedback shown briefly in the status bar.
	actionFeedback string

	// Log viewer overlay state.
	logViewerOpen    bool
	logViewerLines   []string // lines loaded from the log file
	logViewerScroll  int      // scroll offset (0 = bottom/most recent)
	logViewerTitle   string   // title shown in the viewer header
	logFilePath      string   // path to the main poller log file
	mergeLogTailing  bool     // true when the merge log viewer is live-tailing
	mergeLogFilePath string   // path being tailed (so reload can re-read it)

	// Detail pane overlay state.
	detailPaneOpen   bool
	detailPaneLines  []string
	detailPaneScroll int

	// Activity feed overlay state.
	activityFeedOpen   bool
	activityFeedLines  []string
	activityFeedScroll int

	// GitHub client for fetching timeline data.
	gh *ghclient.Client

	owner    string
	repo     string
	interval int

	// Startup loading screen state.
	startupDone   bool // true once the first poll + grace period is done
	loadingPunIdx int  // index into loadingPuns
}

// New creates a fresh Model.  commandCh is used to send actions back to the
// poller (e.g. retry merge resolution).  It may be nil if no commands are
// needed.  gh is used for fetching timeline data (activity feed).
func New(owner, repo string, interval int, commandCh chan<- poller.Command, gh *ghclient.Client) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		Frames: []string{"-", "\\", "|", "/"},
		FPS:    time.Second / tuiSpinnerFPS,
	}
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSpinner))
	return Model{
		spinner:     sp,
		logs:        make([]string, logHistorySize), // keep last N logs
		logFilePath: "copilot-autodev.log",
		owner:       owner,
		repo:        repo,
		interval:    interval,
		commandCh:   commandCh,
		gh:          gh,
	}
}

// Init starts the spinner, per-second countdown tick, and loading pun rotator.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, secondTick(), loadingPunTick())
}

// columnItems returns the items slice for the given column index.
func (m Model) columnItems(col int) []*poller.State {
	switch col {
	case 0:
		return m.queue
	case 1:
		return m.coding
	case 2:
		return m.review
	default:
		return nil
	}
}

// clampSelection ensures selectedRow is within bounds for the current column.
func (m Model) clampSelection() Model {
	items := m.columnItems(m.selectedCol)
	if len(items) == 0 {
		m.selectedRow = 0
		return m
	}
	if m.selectedRow >= len(items) {
		m.selectedRow = len(items) - 1
	}
	if m.selectedRow < 0 {
		m.selectedRow = 0
	}
	return m
}

// selectedState returns the currently selected State, or nil if the column is empty.
func (m Model) selectedState() *poller.State {
	items := m.columnItems(m.selectedCol)
	if len(items) == 0 || m.selectedRow >= len(items) {
		return nil
	}
	return items[m.selectedRow]
}

// needsManualMergeFix returns true if the item's current status indicates a
// failed local merge resolution that can be retried.
const mergeFixStatus = "Merge conflicts unresolved \u2014 needs manual fix"

// Update handles all messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// When any overlay is open, it consumes all key input.
	if m.logViewerOpen || m.detailPaneOpen || m.activityFeedOpen {
		if keyMsg, ok := msg.(tea.KeyMsg); ok {
			if m.logViewerOpen {
				return m.updateLogViewer(keyMsg)
			}
			if m.detailPaneOpen {
				return m.updateOverlayScroll(keyMsg, "detail")
			}
			if m.activityFeedOpen {
				return m.updateOverlayScroll(keyMsg, "activity")
			}
		}
		// Still handle window resize, spinner ticks, log events, and activity feed results.
		switch msg := msg.(type) {
		case tea.WindowSizeMsg:
			m.width = msg.Width
			m.height = msg.Height
			return m, nil
		case spinner.TickMsg:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		case LogEvent:
			m.logs[m.logHead] = msg.Message
			m.logHead = (m.logHead + 1) % len(m.logs)
			return m, nil
		case activityFeedMsg:
			return m.handleActivityFeedMsg(msg)
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit

		case "c":
			var sb strings.Builder
			size := len(m.logs)
			for i := range size {
				idx := (m.logHead + i) % size
				if m.logs[idx] != "" {
					sb.WriteString(m.logs[idx])
					sb.WriteString("\n")
				}
			}
			_ = clipboard.WriteAll(sb.String())
			m.logsCopied = true
			return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearCopyMsg{} })

		// Cursor navigation.
		case "up", "k":
			if m.selectedRow > 0 {
				m.selectedRow--
			}
			return m, nil
		case "down", "j":
			items := m.columnItems(m.selectedCol)
			if m.selectedRow < len(items)-1 {
				m.selectedRow++
			}
			return m, nil
		case "left", "h":
			if m.selectedCol > 0 {
				m.selectedCol--
				m = m.clampSelection()
			}
			return m, nil
		case "right", "l":
			if m.selectedCol < tuiNumCols-1 {
				m.selectedCol++
				m = m.clampSelection()
			}
			return m, nil

		// Actions.
		case "r":
			var cmd tea.Cmd
			m, cmd = m.handleRetryMerge()
			return m, cmd
		case "t":
			var cmd tea.Cmd
			m, cmd = m.handleTakeover()
			return m, cmd
		case "f":
			var cmd tea.Cmd
			m, cmd = m.handleForceRerunCI()
			return m, cmd
		case "+", "=":
			var cmd tea.Cmd
			m, cmd = m.handlePriority(1)
			return m, cmd
		case "-":
			var cmd tea.Cmd
			m, cmd = m.handlePriority(-1)
			return m, cmd

		// Overlays.
		case "enter":
			m = m.openDetailPane()
			return m, nil
		case "a":
			return m.openActivityFeed()

		// Log viewers.
		case "L":
			m.mergeLogFilePath = m.logFilePath
			m.mergeLogTailing = true
			m = m.openLogFile(m.logFilePath)
			return m, mergeLogReloadTick()
		case "v":
			return m.openMergeLogCmd()
		}

	case clearCopyMsg:
		m.logsCopied = false
		return m, nil

	case clearActionMsg:
		m.actionFeedback = ""
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case PollEvent:
		m.queue = msg.Queue
		m.coding = msg.Coding
		m.review = msg.Review
		m.lastRun = msg.LastRun
		m.lastErr = msg.Err
		if len(msg.Warnings) > 0 {
			// Keep only the most recent warning for display.
			m.lastWarn = msg.Warnings[len(msg.Warnings)-1]
		}
		m = m.clampSelection()
		// Schedule loading-screen dismissal 2 s after the first successful poll.
		if !m.startupDone && msg.Err == nil {
			return m, tea.Tick(2*time.Second, func(_ time.Time) tea.Msg { return startupReadyMsg{} })
		}

	case LogEvent:
		m.logs[m.logHead] = msg.Message
		m.logHead = (m.logHead + 1) % len(m.logs)
		return m, nil

	case activityFeedMsg:
		return m.handleActivityFeedMsg(msg)

	case mergeLogReloadMsg:
		if m.logViewerOpen && m.mergeLogTailing {
			// Re-read and only auto-scroll if the user is already at the bottom.
			atBottom := m.logViewerScroll == 0
			m = m.reloadMergeLog()
			if atBottom {
				m.logViewerScroll = 0
			}
			return m, mergeLogReloadTick()
		}
		return m, nil

	case startupReadyMsg:
		m.startupDone = true
		return m, nil

	case loadingPunTickMsg:
		if !m.startupDone {
			m.loadingPunIdx = (m.loadingPunIdx + 1) % len(loadingPuns)
			return m, loadingPunTick()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case secondTickMsg:
		// Re-render every second so countdown timers stay accurate.
		return m, secondTick()
	}

	return m, nil
}

// handleRetryMerge sends a retry-merge command for the selected item if applicable.
func (m Model) handleRetryMerge() (Model, tea.Cmd) {
	s := m.selectedState()
	if s == nil || s.CurrentStatus != mergeFixStatus || s.PR == nil {
		return m, nil
	}
	if m.commandCh == nil {
		return m, nil
	}
	m.commandCh <- poller.Command{
		Action: "retry-merge",
		PRNum:  s.PR.GetNumber(),
	}
	m.actionFeedback = fmt.Sprintf("Retry merge resolution queued for PR#%d", s.PR.GetNumber())
	return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearActionMsg{} })
}

// handleTakeover sends a takeover command for the selected item.
func (m Model) handleTakeover() (Model, tea.Cmd) {
	s := m.selectedState()
	if s == nil || m.commandCh == nil {
		return m, nil
	}
	m.commandCh <- poller.Command{
		Action:   "takeover",
		IssueNum: s.Issue.GetNumber(),
	}
	m.actionFeedback = fmt.Sprintf("Manual takeover requested for #%d", s.Issue.GetNumber())
	return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearActionMsg{} })
}

// handleForceRerunCI sends a rerun-ci command for the selected item's PR.
func (m Model) handleForceRerunCI() (Model, tea.Cmd) {
	s := m.selectedState()
	if s == nil || s.PR == nil || m.commandCh == nil {
		return m, nil
	}
	m.commandCh <- poller.Command{
		Action: "rerun-ci",
		PRNum:  s.PR.GetNumber(),
	}
	m.actionFeedback = fmt.Sprintf("CI re-run requested for PR#%d", s.PR.GetNumber())
	return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearActionMsg{} })
}

// handlePriority sends a priority adjustment for the selected queue item.
func (m Model) handlePriority(delta int) (Model, tea.Cmd) {
	if m.selectedCol != 0 { // only works on queue column
		return m, nil
	}
	s := m.selectedState()
	if s == nil || m.commandCh == nil {
		return m, nil
	}
	action := "priority-up"
	label := "increased"
	if delta < 0 {
		action = "priority-down"
		label = "decreased"
	}
	m.commandCh <- poller.Command{
		Action:   action,
		IssueNum: s.Issue.GetNumber(),
	}
	m.actionFeedback = fmt.Sprintf("Priority %s for #%d", label, s.Issue.GetNumber())
	return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearActionMsg{} })
}

// openDetailPane builds and opens the detail pane overlay for the selected item.
func (m Model) openDetailPane() Model {
	s := m.selectedState()
	if s == nil {
		return m
	}

	var lines []string
	sep := strings.Repeat("─", 40)

	lines = append(lines, fmt.Sprintf("  Issue #%d: %s", s.Issue.GetNumber(), s.Issue.GetTitle()))
	lines = append(lines, sep)
	lines = append(lines, fmt.Sprintf("  URL:      %s", s.Issue.GetHTMLURL()))
	lines = append(lines, fmt.Sprintf("  Status:   %s", s.Status))
	lines = append(lines, fmt.Sprintf("  Created:  %s", ghclient.TimeAgo(s.Issue.GetCreatedAt().Time)))

	// Pipeline progress bar.
	lines = append(lines, "")
	lines = append(lines, "  Pipeline Progress")
	lines = append(lines, sep)
	lines = append(lines, RenderPipelineBar(s))

	if s.PR != nil {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("  PR #%d", s.PR.GetNumber()))
		lines = append(lines, sep)
		lines = append(lines, fmt.Sprintf("  URL:       %s", s.PR.GetHTMLURL()))
		lines = append(lines, fmt.Sprintf("  Head SHA:  %s", s.PR.GetHead().GetSHA()))
		lines = append(lines, fmt.Sprintf("  Mergeable: %s", s.PR.GetMergeableState()))
		lines = append(lines, fmt.Sprintf("  Draft:     %v", s.PR.GetDraft()))
	}

	lines = append(lines, "")
	lines = append(lines, "  Orchestrator Status")
	lines = append(lines, sep)
	if s.CurrentStatus != "" {
		lines = append(lines, fmt.Sprintf("  Phase:       %s", s.CurrentStatus))
	}
	if s.NextAction != "" {
		next := s.NextAction
		if !s.NextActionAt.IsZero() {
			until := time.Until(s.NextActionAt)
			if until <= 0 {
				next += " now"
			} else {
				next += " in " + FormatCountdown(until)
			}
		}
		lines = append(lines, fmt.Sprintf("  Next:        %s", next))
	}
	if s.RefinementMax > 0 {
		lines = append(lines, fmt.Sprintf("  Refinement:  %d / %d", s.RefinementCount, s.RefinementMax))
	}
	if s.AgentStatus != "" {
		lines = append(lines, fmt.Sprintf("  Agent:       %s", s.AgentStatus))
	}
	if s.MergeLogPath != "" {
		lines = append(lines, fmt.Sprintf("  Merge log:   %s", s.MergeLogPath))
	}

	// Show issue body if available.
	body := strings.TrimSpace(s.Issue.GetBody())
	if body != "" {
		lines = append(lines, "")
		lines = append(lines, "  Issue Body")
		lines = append(lines, sep)
		for bl := range strings.SplitSeq(body, "\n") {
			lines = append(lines, "  "+bl)
		}
	}

	m.detailPaneLines = lines
	m.detailPaneScroll = 0 // start at top

	m.detailPaneOpen = true
	return m
}

// RenderPipelineBar renders a single line showing all pipeline stages with
// ✓ done  ● active  ○ pending  ✗ failed indicators based on CurrentStatus.
func RenderPipelineBar(s *poller.State) string {
	type stage struct {
		name     string
		keywords []string // any match → this stage is active
	}

	doneStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSuccess))
	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00cfff")).Bold(true)
	failStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorFailure))
	pendingStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	arrowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	switch s.Status {
	case "queue":
		// Single-step — just show where it sits.
		bar := activeStyle.Render("● Queued") + arrowStyle.Render(" → ") +
			pendingStyle.Render("○ Coding") + arrowStyle.Render(" → ") +
			pendingStyle.Render("○ Review") + arrowStyle.Render(" → ") +
			pendingStyle.Render("○ Merge")
		return "  " + bar

	case "coding":
		bar := doneStyle.Render("✓ Queued") + arrowStyle.Render(" → ") +
			activeStyle.Render("● Coding") + arrowStyle.Render(" → ") +
			pendingStyle.Render("○ Review") + arrowStyle.Render(" → ") +
			pendingStyle.Render("○ Merge")
		return "  " + bar
	}

	// Review pipeline stages in order.
	stages := []stage{
		{"Branch Sync", []string{"conflict", "branch", "resolv", "merge conflict", "Merge resolved"}},
		{"CI Approval", []string{"approval", "Approving", "action_required", "Waiting for manual"}},
		{"Deploy Gates", []string{"deployment", "deploy"}},
		{"CI Checks", []string{"CI running", "waiting for all checks", "failed", "checks"}},
		{"Refinement", []string{"Refinement", "refinement"}},
		{"CI Fix", []string{"CI-fix", "ci-fix"}},
		{"Merge", []string{"merging", "merged", "All checks passed"}},
	}

	cs := strings.ToLower(s.CurrentStatus)

	// Find the latest stage that matches (search backwards).
	current := -1
	for i := len(stages) - 1; i >= 0; i-- {
		st := stages[i]
		for _, kw := range st.keywords {
			if strings.Contains(cs, strings.ToLower(kw)) {
				current = i
				break
			}
		}
		if current >= 0 {
			break
		}
	}

	// If status is failed/success and we couldn't match, infer from AgentStatus.
	if current < 0 {
		switch s.AgentStatus {
		case "success":
			current = len(stages) // all done
		default:
			current = 0 // default to first review stage
		}
	}

	isFailed := s.AgentStatus == "failed"

	var parts []string
	for i, st := range stages {
		var token string
		switch {
		case i < current:
			token = doneStyle.Render("✓ " + st.name)
		case i == current && isFailed:
			token = failStyle.Render("✗ " + st.name)
		case i == current:
			token = activeStyle.Render("● " + st.name)
		default:
			token = pendingStyle.Render("○ " + st.name)
		}
		parts = append(parts, token)
	}

	arrow := arrowStyle.Render(" → ")
	return "  " + strings.Join(parts, arrow)
}

// openActivityFeed starts fetching the timeline for the selected item.
func (m Model) openActivityFeed() (tea.Model, tea.Cmd) {
	s := m.selectedState()
	if s == nil || m.gh == nil {
		return m, nil
	}
	issueNum := s.Issue.GetNumber()
	gh := m.gh
	m.activityFeedOpen = true
	m.activityFeedLines = []string{"  Loading timeline..."}
	m.activityFeedScroll = 0
	return m, func() tea.Msg {
		entries, err := gh.FetchTimeline(context.Background(), issueNum)
		return activityFeedMsg{entries: entries, err: err}
	}
}

// handleActivityFeedMsg processes the timeline data and populates the overlay.
func (m Model) handleActivityFeedMsg(msg activityFeedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.activityFeedLines = []string{fmt.Sprintf("  Error: %v", msg.err)}
		return m, nil
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("  %d events", len(msg.entries)))
	lines = append(lines, strings.Repeat("─", 60))
	for _, e := range msg.entries {
		ts := e.Time.Format("2006-01-02 15:04:05")
		actor := e.Actor
		if actor == "" {
			actor = "system"
		}
		line := fmt.Sprintf("  %s  %-15s  %s", ts, actor, e.Event)
		if e.Detail != "" {
			line += "  " + e.Detail
		}
		lines = append(lines, line)
	}
	m.activityFeedLines = lines
	m.activityFeedScroll = 0 // show from top
	return m, nil
}

// overlayLines returns the lines slice for the given overlay type.
func (m Model) overlayLines(kind string) []string {
	if kind == "detail" {
		return m.detailPaneLines
	}
	return m.activityFeedLines
}

// overlayScroll returns the scroll offset for the given overlay.
func (m Model) overlayScroll(kind string) int {
	if kind == "detail" {
		return m.detailPaneScroll
	}
	return m.activityFeedScroll
}

// updateOverlayScroll handles key input for scrollable overlay panes (detail, activity).
func (m Model) updateOverlayScroll(msg tea.KeyMsg, kind string) (tea.Model, tea.Cmd) {
	lines := m.overlayLines(kind)
	scroll := m.overlayScroll(kind)
	viewHeight := m.logViewerHeight()
	maxScroll := max(len(lines)-viewHeight, 0)

	switch msg.String() {
	case "esc", "q", "enter":
		if kind == "detail" {
			m.detailPaneOpen = false
			m.detailPaneLines = nil
			m.detailPaneScroll = 0
		} else {
			m.activityFeedOpen = false
			m.activityFeedLines = nil
			m.activityFeedScroll = 0
		}
		return m, nil
	case "up", "k":
		if scroll > 0 {
			scroll--
		}
	case "down", "j":
		if scroll < maxScroll {
			scroll++
		}
	case "pgup", "ctrl+u":
		scroll = max(scroll-viewHeight/2, 0)
	case "pgdown", "ctrl+d":
		scroll = min(scroll+viewHeight/2, maxScroll)
	case "home", "g":
		scroll = 0
	case "end", "G":
		scroll = maxScroll
	case "c":
		var sb strings.Builder
		for i, line := range lines {
			sb.WriteString(line)
			if i < len(lines)-1 {
				sb.WriteString("\n")
			}
		}
		_ = clipboard.WriteAll(sb.String())
		m.logsCopied = true
	}
	if kind == "detail" {
		m.detailPaneScroll = scroll
	} else {
		m.activityFeedScroll = scroll
	}
	return m, nil
}

// View renders the full dashboard.
func (m Model) View() string {
	if m.width == 0 {
		return "Initializing..."
	}

	if !m.startupDone {
		return m.renderLoadingScreen()
	}

	if m.logViewerOpen {
		return m.renderLogViewer()
	}
	if m.detailPaneOpen {
		return m.renderOverlay("DETAIL", m.detailPaneLines, m.detailPaneScroll)
	}
	if m.activityFeedOpen {
		return m.renderOverlay("ACTIVITY TIMELINE", m.activityFeedLines, m.activityFeedScroll)
	}

	// Roughly tuiChromeRows + (tuiLogBoxHeight + tuiBorderRows) total reserved lines.
	reservedRows := tuiChromeRows + tuiLogBoxHeight + tuiBorderRows
	colHeight := max(m.height-reservedRows, tuiColMinHeight)

	// Ensure colWidth takes internal padding/borders into account.
	// Each col's outer rendered width = colWidth + 2 (left+right borders).
	// 3 cols with no spacers → total = 3*(colWidth+2) = 3*colWidth+6.
	colWidth := max((m.width-tuiColSidePad)/tuiNumCols, tuiColMinWidth)
	// Give leftover chars to the last column so columns fill the full terminal.
	lastColWidth := max(m.width-tuiColSidePad-colWidth*(tuiNumCols-1), colWidth)

	title := titleStyle.Width(m.width).Render(
		fmt.Sprintf(" [BOT] Copilot Orchestrator - %s/%s ", m.owner, m.repo),
	)

	queueCol := m.renderColumn("LIST Queue", headerQueue, badgeQueue,
		m.queue, colWidth, colHeight, 0)
	codingCol := m.renderColumn("RUN  Active (Coding)", headerCoding, badgeCoding,
		m.coding, colWidth, colHeight, 1)
	reviewCol := m.renderColumn("TEST In Review (CI/Fix)", headerReview, badgeReview,
		m.review, lastColWidth, colHeight, 2)

	columns := lipgloss.JoinHorizontal(lipgloss.Top, queueCol, codingCol, reviewCol)

	logBoxWidth := m.width - tuiLogBoxPadding // horizontal border padding
	logContent := m.renderLogs(tuiLogBoxHeight)
	logBox := logBoxStyle.Width(logBoxWidth).Height(tuiLogBoxHeight).Render(logContent)

	copyText := dimItemStyle.Render(
		"[enter] detail  [a] timeline  [t] takeover  [f] rerun CI  [r] retry merge  [+/-] priority  [v/L] logs",
	)
	if m.logsCopied {
		copyText = lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSuccess)).Render("[Copied!]")
	} else if m.actionFeedback != "" {
		copyText = lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSuccess)).Render(m.actionFeedback)
	}
	copyWidget := lipgloss.NewStyle().Width(m.width - tuiCopyWidgetMargin).Align(lipgloss.Right).Render(copyText)

	statusLine := m.renderStatus()
	if m.lastErr != nil {
		statusLine = errorStyle.Render(statusLine)
	} else if m.lastWarn != "" {
		statusLine = warnStyle.Render(statusLine)
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		columns,
		"",
		copyWidget,
		logBox,
		"",
		statusLine,
	)
}

// renderLoadingScreen draws the full-screen startup splash shown before the
// first poll completes.  It has a big title, animated progress bar, and
// rotating punny loading message.
func (m Model) renderLoadingScreen() string {
	w := max(m.width, tuiColMinWidth)
	h := max(m.height, tuiColMinHeight)

	titleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00cfff")).
		Bold(true).
		Width(w).
		Align(lipgloss.Center)

	subStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Width(w).
		Align(lipgloss.Center)

	punStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ffcc00")).
		Bold(true).
		Width(w).
		Align(lipgloss.Center)

	spinStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(tuiColorSpinner)).
		Width(w).
		Align(lipgloss.Center)

	ascii := []string{
		`  ██████╗ ██████╗ ██████╗ ██╗██╗      ██████╗ ████████╗`,
		`██╔════╝██╔═══██╗██╔══██╗██║██║     ██╔═══██╗╚══██╔══╝`,
		`██║     ██║   ██║██████╔╝██║██║     ██║   ██║   ██║   `,
		`██║     ██║   ██║██╔═══╝ ██║██║     ██║   ██║   ██║   `,
		`╚██████╗╚██████╔╝██║     ██║███████╗╚██████╔╝   ██║   `,
		` ╚═════╝ ╚═════╝ ╚═╝     ╚═╝╚══════╝ ╚═════╝    ╚═╝   `,
		` █████╗ ██╗   ██╗████████╗ ██████╗ ██████╗ ███████╗██╗   ██╗`,
		`██╔══██╗██║   ██║╚══██╔══╝██╔═══██╗██╔══██╗██╔════╝██║   ██║`,
		`███████║██║   ██║   ██║   ██║   ██║██║  ██║█████╗  ██║   ██║`,
		`██╔══██║██║   ██║   ██║   ██║   ██║██║  ██║██╔══╝  ╚██╗ ██╔╝`,
		`██║  ██║╚██████╔╝   ██║   ╚██████╔╝██████╔╝███████╗ ╚████╔╝ `,
		`╚═╝  ╚═╝ ╚═════╝    ╚═╝    ╚═════╝ ╚═════╝ ╚══════╝  ╚═══╝  `,
	}

	// Animated bar using the spinner frame.
	barWidth := min(50, w-4)
	frame := m.spinner.View()
	barChars := strings.Repeat("━", barWidth)
	bar := spinStyle.Render(frame + " " + barChars + " " + frame)

	pun := loadingPuns[m.loadingPunIdx%len(loadingPuns)]

	// Build vertical layout centred in the terminal.
	topPad := max((h-len(ascii)-8)/2, 0)
	var sb strings.Builder
	for range topPad {
		sb.WriteString("\n")
	}
	for _, line := range ascii {
		sb.WriteString(titleStyle.Render(line))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(bar)
	sb.WriteString("\n\n")
	sb.WriteString(punStyle.Render(pun))
	sb.WriteString("\n\n")
	sb.WriteString(subStyle.Render(fmt.Sprintf("Connecting to %s/%s  •  Press q to quit", m.owner, m.repo)))
	return sb.String()
}

func colorLogLine(line string) string {
	styled := line
	switch {
	case strings.Contains(line, "INFO"):
		styled = strings.Replace(line, "INFO", logInfoStyle.Render("INFO"), 1)
	case strings.Contains(line, "WARN"):
		styled = strings.Replace(line, "WARN", logWarnStyle.Render("WARN"), 1)
	case strings.Contains(line, "ERROR"):
		styled = strings.Replace(line, "ERROR", logErrorStyle.Render("ERROR"), 1)
	case strings.Contains(line, "DEBUG"):
		styled = strings.Replace(line, "DEBUG", logDebugStyle.Render("DEBUG"), 1)
	}
	return logLineStyle.Render(styled)
}

func (m Model) renderLogs(height int) string {
	// Calculate an effective inner width to truncate logs
	// Box padding is left/right 1 + border left/right 1 = 4 total subtracted
	innerW := m.width - (tuiLogBoxPadding * tuiDoublePadding) // padding + borders
	innerW = max(innerW, tuiMinInnerWidth)

	// Read out logs from the ring buffer in chronological order
	var ordered []string
	size := len(m.logs)
	for i := range size {
		idx := (m.logHead + i) % size
		if m.logs[idx] != "" {
			// truncate log to prevent terminal wrapping from ruining box
			line := m.logs[idx]
			if lipgloss.Width(line) > innerW {
				// use runes for safe truncation
				runes := []rune(line)
				if len(runes) > innerW {
					line = string(runes[:innerW-1]) + "…"
				}
			}
			ordered = append(ordered, line)
		}
	}

	// Keep only the last 'height' logs
	if len(ordered) > height {
		ordered = ordered[len(ordered)-height:]
	}

	var sb strings.Builder
	for i, l := range ordered {
		sb.WriteString(colorLogLine(l))
		if i < len(ordered)-1 {
			sb.WriteString("\n")
		}
	}

	// Pad with newlines if fewer logs than height
	for i := len(ordered); i < height; i++ {
		sb.WriteString("\n")
	}
	return sb.String()
}

func (m Model) renderColumn(
	header string,
	headerSt lipgloss.Style,
	_ lipgloss.Style,
	states []*poller.State,
	width, height int,
	colIndex int,
) string {
	var sb strings.Builder
	sb.WriteString(headerSt.Render(header))
	fmt.Fprintf(&sb, "  (%d)\n", len(states))

	linesUsed := 2
	itemsRendered := 0
	for i, s := range states {
		selected := colIndex == m.selectedCol && i == m.selectedRow
		item, itemLines := m.renderItem(s, width, selected)
		sb.WriteString(item)
		sb.WriteString("\n")
		linesUsed += itemLines
		itemsRendered++
		if linesUsed >= height-2 {
			remaining := len(states) - itemsRendered
			if remaining > 0 {
				sb.WriteString(dimItemStyle.Render(fmt.Sprintf("  ... %d more", remaining)))
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

	cStyle := columnStyle
	if colIndex == m.selectedCol {
		cStyle = selectedColumnStyle
	}
	return cStyle.Width(width).Height(height).Render(sb.String())
}

// renderItem renders a single issue as a title line plus an optional status
// sub-line.  It returns the rendered string and the number of lines it occupies
// (1 if no status is known, 2 when a status sub-line is present).
func (m Model) renderItem(s *poller.State, colWidth int, selected bool) (string, int) {
	issue := s.Issue
	numStr := fmt.Sprintf("#%d", issue.GetNumber())
	title := issue.GetTitle()

	age := ghclient.TimeAgo(issue.GetCreatedAt().Time)
	agePart := ""
	if s.CurrentStatus == "" {
		agePart = fmt.Sprintf("  [%s]", age)
	}

	prStr := ""
	if s.PR != nil {
		prStr = fmt.Sprintf(" -> PR#%d", s.PR.GetNumber())
	}

	// Determine the line prefix based on selection state.
	prefix := tuiNonSelectPrefix
	if selected {
		prefix = selectedIndicator.Render(tuiSelectPrefix)
	}

	// Line format: "<prefix><numStr> <title><prStr><agePart><spinner>"
	// colWidth is passed to lipgloss .Width() which includes padding (0,1).
	// Item content fits in colWidth - 2 (1 padding each side), minus 1 safety buffer.
	effectiveWidth := colWidth - 3
	overhead := tuiItemLPad + lipgloss.Width(numStr) + 1 + lipgloss.Width(prStr) + lipgloss.Width(agePart)

	spinnerStr := ""
	if s.AgentStatus == "pending" {
		spinnerStr = "  " + m.spinner.View()
		overhead += lipgloss.Width(spinnerStr)
	}

	available := max(effectiveWidth-overhead, 1)

	// Truncate title using physical display width.
	if runewidth.StringWidth(title) > available {
		if available > 1 {
			title = runewidth.Truncate(title, available, "…")
		} else {
			title = "…"
		}
	}

	issueHTML := issue.GetHTMLURL()

	// Colorize text in backticks
	parts := strings.Split(title, "`")
	var styledTitleBuilder strings.Builder
	for i, part := range parts {
		if i%2 == 1 && i < len(parts)-1 {
			styledTitleBuilder.WriteString(codeSpanStyle.Render(part))
		} else {
			styledTitleBuilder.WriteString(itemStyle.Render(part))
		}
	}
	renderedTitle := styledTitleBuilder.String()

	// Make Issue Number and Title clickable links (OSC-8) and visually underline them.
	if issueHTML != "" {
		numStr = issueNumStyle.Underline(true).Render(numStr)
		numStr = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", issueHTML, numStr)

		var underlineBuilder strings.Builder
		for i, part := range parts {
			if i%2 == 1 && i < len(parts)-1 {
				underlineBuilder.WriteString(codeSpanStyle.Underline(true).Render(part))
			} else {
				underlineBuilder.WriteString(itemStyle.Underline(true).Render(part))
			}
		}
		renderedTitle = underlineBuilder.String()
		renderedTitle = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", issueHTML, renderedTitle)
	} else {
		numStr = issueNumStyle.Render(numStr)
	}

	if s.PR != nil {
		prNum := fmt.Sprintf("PR#%d", s.PR.GetNumber())
		renderedPR := prNumStyle.Render(prNum)
		if prHTML := s.PR.GetHTMLURL(); prHTML != "" {
			renderedPR = prNumStyle.Underline(true).Render(prNum)
			renderedPR = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", prHTML, renderedPR)
		}
		prStr = " -> " + renderedPR
	}

	line := fmt.Sprintf("%s%s %s", prefix, numStr, renderedTitle)
	line += prStr
	line += dimItemStyle.Render(agePart)
	line += spinnerStr

	if s.CurrentStatus == "" || !selected {
		return line, lipgloss.Height(line)
	}

	// Calculate and render the full expanded sub-line for selected items without truncating it.
	subLineText := m.RenderStatusSubLine(s, colWidth)
	fullText := line + "\n" + subLineText
	return fullText, lipgloss.Height(fullText)
}

// RenderItem is the exported version for tests (delegates to renderItem without selection).
func (m Model) RenderItem(s *poller.State, colWidth int) (string, int) {
	return m.renderItem(s, colWidth, false)
}

// RenderStatusSubLine builds the dim secondary line shown beneath a title line,
// containing the current phase and (if applicable) the next action with a live
// countdown derived from State.NextActionAt.
func (m Model) RenderStatusSubLine(s *poller.State, colWidth int) string {
	current := s.CurrentStatus
	next := s.NextAction

	if !s.NextActionAt.IsZero() {
		until := time.Until(s.NextActionAt)
		if until <= 0 {
			next += " now"
		} else {
			next += " in " + FormatCountdown(until)
		}
	}

	var text string
	agentIcon := ""
	switch s.AgentStatus {
	case "success":
		agentIcon = " " + lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorSuccess)).Render("[OK]")
	case "failed":
		agentIcon = " " + lipgloss.NewStyle().Foreground(lipgloss.Color(tuiColorFailure)).Render("[X]")
	}

	parts := []string{}
	// Prefix with refinement if applicable.
	if s.RefinementMax > 0 {
		parts = append(parts, fmt.Sprintf("refinement[%d/%d]", s.RefinementCount, s.RefinementMax))
	}

	// Add current phase status (CI failing, Waiting for CI, etc).
	if current != "" {
		parts = append(parts, current)
	}

	// CI progress bar: only shown when we have run data.
	if s.CITotal > 0 {
		const barWidth = 10
		filled := 0
		if s.CITotal > 0 {
			filled = (s.CICompleted * barWidth) / s.CITotal
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		barColor := tuiColorSuccess
		if s.CIFailed > 0 {
			barColor = tuiColorFailure
		}
		barStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(barColor)).Render(bar)
		parts = append(parts, fmt.Sprintf("[%s] %d/%d checks", barStyled, s.CICompleted, s.CITotal))
	}

	if agentIcon != "" {
		parts = append(parts, "copilot"+agentIcon)
	}

	// Append next action if applicable.
	if next != "" {
		parts = append(parts, fmt.Sprintf("Next: %s", next))
	}

	text = strings.Join(parts, "\n")

	// Allow the sub-line to wrap freely. Apply left padding of 7 to align neatly under title.
	// We use the full available column width and do not truncate.
	contentStyle := statusLineStyle.Width(colWidth - 7).PaddingLeft(7)
	return contentStyle.Render(text)
}

// FormatCountdown formats a duration as a short human-readable string,
// e.g. "9m 42s", "1h 3m", "58s".
func FormatCountdown(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	mins := int(d.Minutes()) % tuiSecsPerMin
	secs := int(d.Seconds()) % tuiSecsPerMin
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// ── Log viewer overlay ──────────────────────────────────────────────────

const logViewerMaxLines = 500 // max lines to load from the log file

// openLogFile reads the given file and opens the fullscreen viewer.
func (m Model) openLogFile(path string) Model {
	data, err := os.ReadFile(path)
	if err != nil {
		m.actionFeedback = fmt.Sprintf("Cannot open log: %v", err)
		return m
	}
	lines := strings.Split(string(data), "\n")
	// Keep only the last logViewerMaxLines lines.
	if len(lines) > logViewerMaxLines {
		lines = lines[len(lines)-logViewerMaxLines:]
	}
	m.logViewerLines = lines
	m.logViewerScroll = 0 // 0 = viewing the bottom (most recent)
	m.logViewerOpen = true
	m.logViewerTitle = path
	return m
}

// openMergeLogCmd starts the live-tail merge log viewer for the selected item.
func (m Model) openMergeLogCmd() (tea.Model, tea.Cmd) {
	s := m.selectedState()
	if s == nil || s.MergeLogPath == "" {
		m.actionFeedback = "No merge log available for selected item"
		return m, nil
	}
	m.mergeLogFilePath = s.MergeLogPath
	m.mergeLogTailing = true
	m = m.openLogFile(s.MergeLogPath)
	return m, mergeLogReloadTick()
}

// reloadMergeLog re-reads the merge log file in place (called by the tail ticker).
func (m Model) reloadMergeLog() Model {
	if m.mergeLogFilePath == "" {
		return m
	}
	data, err := os.ReadFile(m.mergeLogFilePath)
	if err != nil {
		return m // file may not exist yet — silently skip
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > logViewerMaxLines {
		lines = lines[len(lines)-logViewerMaxLines:]
	}
	m.logViewerLines = lines
	return m
}

// updateLogViewer handles key input while the log viewer overlay is open.
func (m Model) updateLogViewer(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	viewHeight := m.logViewerHeight()
	maxScroll := max(len(m.logViewerLines)-viewHeight, 0)

	switch msg.String() {
	case "L", "v", "esc", "q":
		m.logViewerOpen = false
		m.logViewerLines = nil
		m.mergeLogTailing = false
		m.mergeLogFilePath = ""
		return m, nil
	case "up", "k":
		if m.logViewerScroll < maxScroll {
			m.logViewerScroll++
		}
		return m, nil
	case "down", "j":
		if m.logViewerScroll > 0 {
			m.logViewerScroll--
		}
		return m, nil
	case "pgup", "ctrl+u":
		m.logViewerScroll = min(m.logViewerScroll+viewHeight/2, maxScroll)
		return m, nil
	case "pgdown", "ctrl+d":
		m.logViewerScroll = max(m.logViewerScroll-viewHeight/2, 0)
		return m, nil
	case "c":
		var sb strings.Builder
		for i, line := range m.logViewerLines {
			sb.WriteString(line)
			if i < len(m.logViewerLines)-1 {
				sb.WriteString("\n")
			}
		}
		_ = clipboard.WriteAll(sb.String())
		m.logsCopied = true
		return m, tea.Tick(tuiCopyFeedbackDuration, func(_ time.Time) tea.Msg { return clearCopyMsg{} })
	case "home", "g":
		m.logViewerScroll = maxScroll
		return m, nil
	case "end", "G":
		m.logViewerScroll = 0
		return m, nil
	}
	return m, nil
}

// logViewerHeight returns the number of visible log lines (screen minus chrome).
func (m Model) logViewerHeight() int {
	// 3 rows: title bar, blank, status bar
	return max(m.height-3, 1)
}

// renderLogViewer draws the fullscreen log viewer overlay.
func (m Model) renderLogViewer() string {
	viewH := m.logViewerHeight()
	total := len(m.logViewerLines)

	// Calculate the window: scroll=0 means bottom, scroll=N means N lines up.
	endIdx := total - m.logViewerScroll
	startIdx := max(endIdx-viewH, 0)
	if endIdx < 0 {
		endIdx = 0
	}

	visible := m.logViewerLines[startIdx:endIdx]

	var sb strings.Builder

	// Display scroll position as [ %d/%d ]
	scrollPos := ""
	if total > viewH {
		scrollPos = fmt.Sprintf("  [ %d/%d ]", startIdx+1, total)
	}

	headerText := fmt.Sprintf(
		" LOG VIEWER%s — %s%s  (scroll: up/down/pgup/pgdn  home/end: g/G  copy: c  close: L/v/esc/q) ",
		map[bool]string{true: " [LIVE]", false: ""}[m.mergeLogTailing],
		m.logViewerTitle,
		scrollPos,
	)
	if m.logsCopied {
		headerText = fmt.Sprintf(" LOG VIEWER%s — %s%s  [Copied!] ",
			map[bool]string{true: " [LIVE]", false: ""}[m.mergeLogTailing],
			m.logViewerTitle,
			scrollPos,
		)
	}
	sb.WriteString(titleStyle.Width(m.width).Render(headerText))
	sb.WriteString("\n")

	innerW := max(m.width-2, tuiMinInnerWidth)
	for _, line := range visible {
		// Truncate long lines to avoid terminal wrapping.
		runes := []rune(line)
		if len(runes) > innerW {
			line = string(runes[:innerW-1]) + "…"
		}
		sb.WriteString(colorLogLine(line))
		sb.WriteString("\n")
	}
	// Pad remaining lines.
	for i := len(visible); i < viewH; i++ {
		sb.WriteString("\n")
	}

	posInfo := fmt.Sprintf("Line %d-%d of %d", startIdx+1, endIdx, total)
	sb.WriteString(statusBarStyle.Render(posInfo))

	return sb.String()
}

// renderOverlay draws a generic fullscreen overlay with a title, scrollable lines, and controls.
func (m Model) renderOverlay(title string, lines []string, scroll int) string {
	viewH := m.logViewerHeight()
	total := len(lines)

	endIdx := min(scroll+viewH, total)
	startIdx := max(scroll, 0)

	visible := lines[startIdx:endIdx]

	var sb strings.Builder

	scrollPos := ""
	if total > viewH {
		scrollPos = fmt.Sprintf("  [ %d/%d ]", startIdx+1, total)
	}
	headerText := fmt.Sprintf(
		" %s%s  (scroll: up/down/pgup/pgdn  home/end: g/G  copy: c  close: esc/q) ",
		title,
		scrollPos,
	)
	if m.logsCopied {
		headerText = fmt.Sprintf(" %s%s  [Copied!] ", title, scrollPos)
	}
	sb.WriteString(titleStyle.Width(m.width).Render(headerText))
	sb.WriteString("\n")

	innerW := max(m.width-2, tuiMinInnerWidth)
	for _, line := range visible {
		runes := []rune(line)
		if len(runes) > innerW {
			line = string(runes[:innerW-1]) + "…"
		}
		sb.WriteString(detailValueStyle.Render(line))
		sb.WriteString("\n")
	}
	for i := len(visible); i < viewH; i++ {
		sb.WriteString("\n")
	}

	posInfo := fmt.Sprintf("Line %d-%d of %d", startIdx+1, endIdx, total)
	sb.WriteString(statusBarStyle.Render(posInfo))

	return sb.String()
}

func (m Model) renderStatus() string {
	spin := m.spinner.View()
	var parts []string

	if m.lastRun.IsZero() {
		parts = append(parts, spin+" Waiting for first poll...")
	} else {
		nextPollRun := m.lastRun.Add(time.Duration(m.interval) * time.Second)
		until := time.Until(nextPollRun)
		if until <= 0 {
			parts = append(parts, spin+" Polling now...")
		} else {
			parts = append(parts, spin+fmt.Sprintf(" Next poll in %s", FormatCountdown(until)))
		}
	}

	total := len(m.queue) + len(m.coding) + len(m.review)
	parts = append(parts, fmt.Sprintf("Issues tracked: %d", total))
	parts = append(parts, "Press q / Ctrl-C to quit")

	status := statusBarStyle.Render(strings.Join(parts, "  -  "))

	if m.lastErr != nil {
		errStr := m.lastErr.Error()
		if len(errStr) > tuiStatusMaxLen {
			errStr = errStr[:tuiStatusMaxLen-3] + "..."
		}
		status = lipgloss.JoinVertical(lipgloss.Left,
			status,
			errorStyle.Render("!  "+errStr),
		)
	} else if m.lastWarn != "" {
		warnStr := m.lastWarn
		if len(warnStr) > tuiStatusMaxLen {
			warnStr = warnStr[:tuiStatusMaxLen-3] + "..."
		}
		status = lipgloss.JoinVertical(lipgloss.Left,
			status,
			warnStyle.Render("!  "+warnStr),
		)
	}
	return status
}
