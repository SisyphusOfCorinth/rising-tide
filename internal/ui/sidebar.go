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

// View renders the sidebar within the given bounds (no border).
func (s Sidebar) View(w, h int, focused bool) string {
	header := StyleSectionHeader.Render("Playlists")
	hint := StyleDim.Render("Coming soon...")

	content := header + "\n\n" + hint

	lines := strings.Split(content, "\n")
	for len(lines) < h {
		lines = append(lines, "")
	}
	body := strings.Join(lines[:h], "\n")

	return lipgloss.NewStyle().Width(w).Height(h).Render(body)
}

// Width returns the fixed width for the sidebar panel.
func (s Sidebar) Width() int {
	return 25
}
