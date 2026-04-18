package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// NowPlaying renders the bottom bar showing the currently playing track,
// progress bar, and elapsed/total time.
type NowPlaying struct {
	TrackTitle  string
	ArtistName  string
	AlbumTitle  string
	Position    float64 // seconds
	Duration    float64 // seconds
	Playing     bool
	Resolving   bool // "Resolving stream..." state
}

func NewNowPlaying() NowPlaying {
	return NowPlaying{}
}

// SetTrack updates the now-playing display with a new track.
func (n *NowPlaying) SetTrack(title, artist, album string) {
	n.TrackTitle = title
	n.ArtistName = artist
	n.AlbumTitle = album
	n.Playing = true
	n.Resolving = false
	n.Position = 0
	n.Duration = 0
}

// SetProgress updates the playback position and duration.
func (n *NowPlaying) SetProgress(pos, dur float64) {
	n.Position = pos
	n.Duration = dur
}

// Clear resets the now-playing bar.
func (n *NowPlaying) Clear() {
	n.TrackTitle = ""
	n.ArtistName = ""
	n.AlbumTitle = ""
	n.Position = 0
	n.Duration = 0
	n.Playing = false
	n.Resolving = false
}

// View renders the now-playing bar within the given width.
// Layout: "Artist - Title    [====>        ] 2:34 / 5:12"
func (n NowPlaying) View(w int) string {
	if w < 10 {
		return ""
	}

	style := StyleNowPlaying.Width(w)

	if n.Resolving {
		return style.Render(StyleDim.Render("  Resolving stream..."))
	}

	if n.TrackTitle == "" {
		return style.Render(StyleDim.Render("  Nothing playing  --  Press / to search"))
	}

	// Track info
	status := ">"
	if !n.Playing {
		status = "||"
	}
	info := fmt.Sprintf("  %s %s - %s", status, n.ArtistName, n.TrackTitle)

	// Time display
	posStr := formatTime(n.Position)
	durStr := formatTime(n.Duration)
	timeStr := fmt.Sprintf(" %s / %s ", posStr, durStr)

	// Progress bar fills the remaining space
	barWidth := w - lipgloss.Width(info) - lipgloss.Width(timeStr) - 4
	if barWidth < 5 {
		barWidth = 5
	}

	bar := renderProgressBar(barWidth, n.Position, n.Duration)

	line := info + "  " + bar + timeStr

	// Truncate if too wide
	if lipgloss.Width(line) > w {
		line = line[:w]
	}

	return style.Render(line)
}

// renderProgressBar draws an ASCII progress bar of the given width.
func renderProgressBar(width int, position, duration float64) string {
	if width <= 2 {
		return "[]"
	}

	innerW := width - 2 // account for [ and ]
	filled := 0
	if duration > 0 {
		ratio := position / duration
		if ratio > 1 {
			ratio = 1
		}
		filled = int(ratio * float64(innerW))
	}

	bar := strings.Repeat("=", filled)
	if filled < innerW {
		bar += ">"
		bar += strings.Repeat(" ", innerW-filled-1)
	}

	return lipgloss.NewStyle().Foreground(ColorProgress).Render("["+bar+"]")
}

// formatTime converts seconds to "M:SS" format.
func formatTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(seconds)
	m := total / 60
	s := total % 60
	return fmt.Sprintf("%d:%02d", m, s)
}
