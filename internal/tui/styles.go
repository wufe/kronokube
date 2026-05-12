package tui

import "github.com/charmbracelet/lipgloss"

// Centralized lipgloss styles. Keep palette small so themes are easy to tweak.
var (
	colTitle     = lipgloss.Color("#5DADE2")
	colMuted     = lipgloss.Color("#808080")
	colWarn      = lipgloss.Color("#E67E22")
	colError     = lipgloss.Color("#E74C3C")
	colOK        = lipgloss.Color("#27AE60")
	colTimeline  = lipgloss.Color("#9B59B6")
	colBorder    = lipgloss.Color("#404040")
	colSelectBG  = lipgloss.Color("#1F3A5F")
	colSelectFG  = lipgloss.Color("#FFFFFF")

	StyleTitle     = lipgloss.NewStyle().Foreground(colTitle).Bold(true)
	StyleMuted     = lipgloss.NewStyle().Foreground(colMuted)
	StyleWarn      = lipgloss.NewStyle().Foreground(colWarn)
	StyleError     = lipgloss.NewStyle().Foreground(colError)
	StyleOK        = lipgloss.NewStyle().Foreground(colOK)
	StyleTimeline  = lipgloss.NewStyle().Foreground(colTimeline)
	StyleSelected  = lipgloss.NewStyle().Background(colSelectBG).Foreground(colSelectFG).Bold(true)
	StyleHeader    = lipgloss.NewStyle().Bold(true).Foreground(colTitle)
	StyleBorder    = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(colBorder)
	StyleStatusBar = lipgloss.NewStyle().Foreground(colMuted)
)
