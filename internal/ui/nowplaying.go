package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// NowPlaying renders the bottom bar showing the currently playing track,
// progress bar, elapsed/total time, and an audio-quality label (bit depth,
// sample rate, and codec) on the right. The codec is supplied by the
// Tidal manifest so it reflects what Tidal actually served even when
// ffmpeg transparently transcoded AAC to FLAC in the decode pipeline.
// Rate and bit depth are pulled from the player's live FLAC stream info.
type NowPlaying struct {
	TrackTitle string
	ArtistName string
	AlbumTitle string
	Position   float64 // seconds
	Duration   float64 // seconds
	Playing    bool
	Resolving  bool // "Resolving stream..." state

	// Audio quality (populated after playback begins).
	Codec      string // raw Tidal codec string, e.g. "flac", "mp4a.40.2"
	SampleRate uint32 // Hz
	BitDepth   uint8
}

func NewNowPlaying() NowPlaying {
	return NowPlaying{}
}

// SetTrack updates the now-playing display with a new track. Rate and bit
// depth are not known until the FLAC decoder has parsed a header, so they
// are cleared here and the tick handler fills them in after the first
// decode.
func (n *NowPlaying) SetTrack(title, artist, album, codec string) {
	n.TrackTitle = title
	n.ArtistName = artist
	n.AlbumTitle = album
	n.Codec = codec
	n.Playing = true
	n.Resolving = false
	n.Position = 0
	n.Duration = 0
	n.SampleRate = 0
	n.BitDepth = 0
}

// SetProgress updates the playback position and duration.
func (n *NowPlaying) SetProgress(pos, dur float64) {
	n.Position = pos
	n.Duration = dur
}

// SetFormat updates the sample rate and bit depth once the decoder has
// parsed the FLAC stream header.
func (n *NowPlaying) SetFormat(rate uint32, bits uint8) {
	n.SampleRate = rate
	n.BitDepth = bits
}

// Clear resets the now-playing bar.
func (n *NowPlaying) Clear() {
	n.TrackTitle = ""
	n.ArtistName = ""
	n.AlbumTitle = ""
	n.Codec = ""
	n.Position = 0
	n.Duration = 0
	n.SampleRate = 0
	n.BitDepth = 0
	n.Playing = false
	n.Resolving = false
}

// View renders the now-playing bar within the given width.
// Layout: "Artist - Title    [====>        ] 2:34 / 5:12  24/96 FLAC"
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

	// Quality label (right edge). Hidden at narrow widths so the progress
	// bar still has room to show shape.
	quality := formatQualityLabel(n.BitDepth, n.SampleRate, n.Codec)
	qualityStr := ""
	if quality != "" {
		qualityStr = " " + quality + " "
	}

	// Progress bar fills the remaining space after info, time, and quality.
	barWidth := w - lipgloss.Width(info) - lipgloss.Width(timeStr) - lipgloss.Width(qualityStr) - 4
	if barWidth < 5 {
		// Under pressure, drop the quality label before shrinking the bar
		// further so the label doesn't get mid-truncated.
		qualityStr = ""
		barWidth = w - lipgloss.Width(info) - lipgloss.Width(timeStr) - 4
		if barWidth < 5 {
			barWidth = 5
		}
	}

	bar := renderProgressBar(barWidth, n.Position, n.Duration)

	line := info + "  " + bar + timeStr + StyleDim.Render(qualityStr)

	// Truncate if too wide
	if lipgloss.Width(line) > w {
		line = line[:w]
	}

	return style.Render(line)
}

// formatQualityLabel builds the short quality tag shown on the right of
// the now-playing bar, e.g. "24/96 FLAC" or "16/44.1 AAC-LC". Returns ""
// if any of the inputs are unset, so the bar doesn't flicker with a
// half-populated label while the decoder is still parsing the header.
func formatQualityLabel(bits uint8, rate uint32, codec string) string {
	if bits == 0 || rate == 0 || codec == "" {
		return ""
	}
	return fmt.Sprintf("%d/%s %s", bits, formatRateKHz(rate), prettyCodec(codec))
}

// formatRateKHz converts a sample rate in Hz to a compact kHz label:
// whole multiples of 1000 get no decimal ("48", "96", "192"); others
// get one decimal place ("44.1", "88.2").
func formatRateKHz(hz uint32) string {
	if hz%1000 == 0 {
		return fmt.Sprintf("%d", hz/1000)
	}
	return fmt.Sprintf("%.1f", float64(hz)/1000)
}

// prettyCodec maps Tidal's manifest codec strings into short labels for
// the status bar. Unknown values pass through verbatim (uppercased).
func prettyCodec(c string) string {
	switch c {
	case "flac":
		return "FLAC"
	case "alac":
		return "ALAC"
	case "mqa":
		return "MQA"
	case "mp4a.40.2":
		return "AAC-LC"
	case "mp4a.40.5":
		return "HE-AAC"
	case "mp4a.40.29":
		return "HE-AAC v2"
	case "mp3":
		return "MP3"
	}
	return strings.ToUpper(c)
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
