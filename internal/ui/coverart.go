package ui

import "strings"

// CoverArt is a placeholder panel for the top-right section.
// In future versions this will display album artwork using the Kitty
// terminal graphics protocol (640x640).
type CoverArt struct{}

func NewCoverArt() CoverArt {
	return CoverArt{}
}

// View renders the cover art panel within the given bounds.
func (c CoverArt) View(w, h int, focused bool) string {
	innerW := w - 2
	innerH := h - 2
	if innerW < 0 {
		innerW = 0
	}
	if innerH < 0 {
		innerH = 0
	}

	header := StyleSectionHeader.Render("Cover Art")
	hint := StyleDim.Render("Coming soon...")

	content := header + "\n\n" + hint

	lines := strings.Split(content, "\n")
	for len(lines) < innerH {
		lines = append(lines, "")
	}
	body := strings.Join(lines[:innerH], "\n")

	return panelStyle(innerW, innerH, focused).Render(body)
}

// Width returns the fixed width for the cover art panel (including borders).
func (c CoverArt) Width() int {
	return 30
}
