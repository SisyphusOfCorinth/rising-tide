// Package ui implements the four-panel Bubble Tea TUI for Rising Tide.
//
// Layout:
//
//	+----------+-------------------+----------+
//	| Sidebar  |    Navigator      | Cover Art|
//	| (queue)  | (search/browse)   | (album)  |
//	+----------+-------------------+----------+
//	| Now Playing (track info + progress bar)  |
//	+------------------------------------------+
package ui

import "github.com/charmbracelet/lipgloss"

// --- Color Palette ---

var (
	ColorPrimary   = lipgloss.Color("#00BFFF") // Tidal blue accent
	ColorSecondary = lipgloss.Color("#535353")
	ColorBorder    = lipgloss.Color("#3a3a3a")
	ColorFocused   = lipgloss.Color("#00BFFF")
	ColorMuted     = lipgloss.Color("#6a6a6a")
	ColorError     = lipgloss.Color("#ff5555")
	ColorWhite     = lipgloss.Color("#ffffff")
	ColorDim       = lipgloss.Color("#888888")
	ColorProgress  = lipgloss.Color("#00BFFF")
)

// --- Panel Borders ---

func borderFocused(w, h int) lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorFocused).
		Width(w).
		Height(h)
}

func borderUnfocused(w, h int) lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Width(w).
		Height(h)
}

// --- Text Styles ---

var (
	StyleSectionHeader = lipgloss.NewStyle().
				Foreground(ColorPrimary).
				Bold(true)

	StyleItemNormal = lipgloss.NewStyle().
			Foreground(ColorWhite)

	StyleItemSelected = lipgloss.NewStyle().
				Foreground(ColorPrimary).
				Bold(true)

	StyleItemMuted = lipgloss.NewStyle().
			Foreground(ColorMuted)

	StyleError = lipgloss.NewStyle().
			Foreground(ColorError)

	StyleDim = lipgloss.NewStyle().
			Foreground(ColorDim)

	StyleNowPlaying = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), true, false, false, false).
			BorderForeground(ColorBorder)

	StyleSearchBar = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorPrimary).
			Padding(0, 1)

	StyleHelp = lipgloss.NewStyle().
			Foreground(ColorDim)

	StyleTitle = lipgloss.NewStyle().
			Foreground(ColorPrimary).
			Bold(true)
)
