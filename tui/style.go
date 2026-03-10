// Package tui provides the Bubble Tea terminal UI model and lipgloss styles.
package tui

import "github.com/charmbracelet/lipgloss"

// titleHPad is the horizontal padding applied to the dashboard title bar.
const titleHPad = 2

var (
	// Column header colors.
	headerQueue  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#0075ca")).Padding(0, 1)
	headerCoding = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#e4b800")).Padding(0, 1)
	headerReview = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#d93f0b")).Padding(0, 1)

	// Column containers.
	columnStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#444444")).
			Padding(0, 1)

	selectedColumnStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#00ff87")). // Vibrant Copilot green
				Padding(0, 1)

	// Item styles.
	itemStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#dddddd"))
	dimItemStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#777777"))
	issueNumStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#aaaaaa"))
	prNumStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#5a9fe8"))
	codeSpanStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#d2a8ff")) // GitHub magenta code span color

	// Status sub-line style: muted text shown below each item's title line,
	// describing the current phase and next scheduled action.
	statusLineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#555555"))

	// Status bar.
	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888")).
			PaddingLeft(1)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ff5555")).
			Bold(true).
			PaddingLeft(1)

	warnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#e4b800")).
			PaddingLeft(1)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.Color("#1a1a2e")).
			Padding(0, titleHPad).
			Align(lipgloss.Center)

	// Badge styles (inline status chips).
	badgeQueue = lipgloss.NewStyle().
			Background(lipgloss.Color("#0075ca")).
			Foreground(lipgloss.Color("#ffffff")).
			Padding(0, 1)
	badgeCoding = lipgloss.NewStyle().
			Background(lipgloss.Color("#e4b800")).
			Foreground(lipgloss.Color("#000000")).
			Padding(0, 1)
	badgeReview = lipgloss.NewStyle().
			Background(lipgloss.Color("#d93f0b")).
			Foreground(lipgloss.Color("#ffffff")).
			Padding(0, 1)

	// App Logging area styles.
	logBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#444444")).
			Padding(0, 1)

	logLineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))

	// Log level parsing styles.
	logInfoStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#5a9fe8")) // Blue
	logWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#e4b800")) // Yellow
	logErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555")) // Red
	logDebugStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#aaaaaa")) // Gray

	// Selected item indicator prefix.
	selectedIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff87")).Bold(true)

	// Detail pane styles.
	detailValueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#dddddd"))
)
