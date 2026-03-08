// Package tui provides the Bubble Tea terminal UI model and lipgloss styles.
package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Column header colors.
	headerQueue  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#0075ca")).Padding(0, 1)
	headerCoding = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#e4b800")).Padding(0, 1)
	headerReview = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#d93f0b")).Padding(0, 1)

	// Column container.
	columnStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#444444")).
			Padding(0, 1)

	// Item styles.
	itemStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#dddddd"))
	dimItemStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#777777"))
	issueNumStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#aaaaaa"))
	prNumStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#5a9fe8"))

	// Status bar.
	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888")).
			PaddingLeft(1)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ff5555")).
			Bold(true).
			PaddingLeft(1)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.Color("#1a1a2e")).
			Padding(0, 2).
			Align(lipgloss.Center)

	// Badge styles (inline status chips).
	badgeQueue  = lipgloss.NewStyle().Background(lipgloss.Color("#0075ca")).Foreground(lipgloss.Color("#ffffff")).Padding(0, 1)
	badgeCoding = lipgloss.NewStyle().Background(lipgloss.Color("#e4b800")).Foreground(lipgloss.Color("#000000")).Padding(0, 1)
	badgeReview = lipgloss.NewStyle().Background(lipgloss.Color("#d93f0b")).Foreground(lipgloss.Color("#ffffff")).Padding(0, 1)
)
