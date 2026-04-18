package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Sidebar is a placeholder panel for the top-left section.
// In future versions this will display playlists and the current queue.
type Sidebar struct{}

func NewSidebar() Sidebar {
	return Sidebar{}
}

// View renders the sidebar within the given bounds.
func (s Sidebar) View(w, h int, focused bool) string {
	// Account for border (2 chars each side)
	innerW := w - 2
	innerH := h - 2
	if innerW < 0 {
		innerW = 0
	}
	if innerH < 0 {
		innerH = 0
	}

	header := StyleSectionHeader.Render("Playlists")
	hint := StyleDim.Render("Coming soon...")

	content := header + "\n\n" + hint

	// Pad to fill the inner area
	lines := strings.Split(content, "\n")
	for len(lines) < innerH {
		lines = append(lines, "")
	}
	body := strings.Join(lines[:innerH], "\n")

	style := borderUnfocused(innerW, innerH)
	if focused {
		style = borderFocused(innerW, innerH)
	}

	return style.Render(body)
}

// Width returns the fixed width for the sidebar panel (including borders).
func (s Sidebar) Width() int {
	return 25
}

// panelStyle returns the appropriate border style based on focus.
func panelStyle(w, h int, focused bool) lipgloss.Style {
	if focused {
		return borderFocused(w, h)
	}
	return borderUnfocused(w, h)
}
