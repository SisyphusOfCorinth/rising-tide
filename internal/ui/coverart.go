// Cover art display using the Kitty terminal graphics protocol.
//
// The image is always rendered at exactly 640x640 pixels. The terminal
// layout adapts: the cover art panel has a fixed column/row size computed
// from terminal cell dimensions, and the now-playing bar fills the space
// below the sidebar+navigator to be flush with the cover art bottom edge.
//
// Supported terminals: Ghostty, Kitty, WezTerm.
// Unsupported terminals fall back to a text placeholder panel.
package ui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder for Tidal CDN images
	"image/png"
	"os"
	"strings"

	"golang.org/x/image/draw"
)

// kittyChunkSize is the maximum base64 payload length per Kitty APC chunk.
const kittyChunkSize = 4096

// Cover art is always 640x640 pixels. These constants define the terminal
// cell dimensions needed to contain that image. Ghostty/Kitty typically
// have cells around 9x20 pixels at default font sizes.
//
// We use conservative estimates so the image fits within the reserved cells.
// The Kitty protocol's c= and r= parameters reserve terminal cells, while
// the actual image renders at native pixel size within that space.
const (
	CoverPixels = 640 // image width and height in pixels

	// Terminal cell estimates. These determine how many columns/rows to
	// reserve in the TUI layout. The actual image is always 640x640px
	// regardless of these values -- the Kitty protocol handles scaling.
	CellPixelW = 9
	CellPixelH = 20

	// Fixed terminal dimensions for the cover art panel.
	CoverCols = (CoverPixels / CellPixelW) + 1 // ~72 columns
	CoverRows = (CoverPixels / CellPixelH) + 1 // ~33 rows
)

// CoverArt manages album cover art display via the Kitty terminal graphics
// protocol. The image is always rendered at exactly 640x640 pixels.
type CoverArt struct {
	Supported bool        // kitty protocol available (checked once at startup)
	Img       image.Image // cached decoded image (for re-render on resize)
	Rows      []string    // pre-rendered kitty escape sequences, one per row
	CoverURL  string      // current URL (for dedup -- skip fetch if same album)
}

// NewCoverArt creates a CoverArt panel, detecting Kitty protocol support.
func NewCoverArt() CoverArt {
	return CoverArt{
		Supported: KittySupported(),
	}
}

// Width returns the fixed terminal column width for the cover art panel.
func (c CoverArt) Width() int {
	if c.Supported {
		return CoverCols
	}
	return 30 // placeholder width for unsupported terminals
}

// Height returns the fixed terminal row height for the cover art panel.
func (c CoverArt) Height() int {
	return CoverRows
}

// Clear resets the cover art display (e.g. when playback stops).
func (c *CoverArt) Clear() {
	c.Img = nil
	c.Rows = nil
	c.CoverURL = ""
}

// View renders the cover art placeholder panel. When kitty rows are available,
// they are appended directly to output lines in app.go's View() method rather
// than going through lipgloss, to avoid APC escape sequence width issues.
func (c CoverArt) View(w, h int, focused bool) string {
	return c.fallbackView(w, h, focused)
}

// fallbackView renders the text placeholder for unsupported terminals.
func (c CoverArt) fallbackView(w, h int, focused bool) string {
	innerW := w - 2
	innerH := h - 2
	if innerW < 0 {
		innerW = 0
	}
	if innerH < 0 {
		innerH = 0
	}

	header := StyleSectionHeader.Render("Cover Art")
	var hint string
	if !c.Supported {
		hint = StyleDim.Render("(kitty protocol not available)")
	} else {
		hint = StyleDim.Render("Play a track to see cover art")
	}

	content := header + "\n\n" + hint

	lines := strings.Split(content, "\n")
	for len(lines) < innerH {
		lines = append(lines, "")
	}
	if len(lines) > innerH {
		lines = lines[:innerH]
	}
	body := strings.Join(lines, "\n")

	return panelStyle(innerW, innerH, focused).Render(body)
}

// --- Kitty Terminal Graphics Protocol ---

// KittySupported reports whether the running terminal supports the Kitty
// terminal graphics protocol. Ghostty, Kitty, and WezTerm qualify.
func KittySupported() bool {
	switch os.Getenv("TERM_PROGRAM") {
	case "ghostty", "WezTerm":
		return true
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return true
	}
	if strings.Contains(os.Getenv("TERM"), "kitty") {
		return true
	}
	return false
}

// RenderKittyRows encodes a 640x640 image as a single Kitty APC sequence
// placed on the first line, spanning cols columns and rows terminal rows.
// The terminal renders the image as one block behind the text layer.
// Remaining lines are empty (the caller fills them with spaces so the
// image shows through).
//
// Returns a slice where index 0 has the full-image kitty sequence and all
// other entries are empty strings.
func RenderKittyRows(img image.Image, cols, rows int) []string {
	if img == nil || cols <= 0 || rows <= 0 {
		return nil
	}

	// Scale source image to exactly 640x640 pixels.
	scaled := image.NewRGBA(image.Rect(0, 0, CoverPixels, CoverPixels))
	draw.BiLinear.Scale(scaled, scaled.Bounds(), img, img.Bounds(), draw.Over, nil)

	// PNG-encode the full image.
	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		return nil
	}

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Build the kitty sequence for the full image: c=cols, r=rows tells the
	// terminal to span the image over that many cells. The image renders as
	// a graphic layer behind text.
	seq := kittyChunkedFull(encoded, cols, rows)

	result := make([]string, rows)
	result[0] = seq // full image on first line
	// Lines 1..rows-1 are empty -- spaces added by the caller let the
	// image show through since spaces have no background.
	return result
}

// kittyChunkedFull wraps base64-encoded image data for a full multi-row image.
// Uses r={rows} so the terminal renders the image spanning multiple rows as
// a single block, avoiding per-row strip gaps.
func kittyChunkedFull(encoded string, cols, rows int) string {
	var sb strings.Builder
	for i := 0; i < len(encoded); i += kittyChunkSize {
		end := i + kittyChunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		chunk := encoded[i:end]
		more := 1
		if end >= len(encoded) {
			more = 0
		}
		if i == 0 {
			fmt.Fprintf(&sb, "\x1b_Ga=T,f=100,c=%d,r=%d,q=2,m=%d;%s\x1b\\", cols, rows, more, chunk)
		} else {
			fmt.Fprintf(&sb, "\x1b_Gm=%d;%s\x1b\\", more, chunk)
		}
	}
	return sb.String()
}

// kittyChunked wraps base64-encoded image data in one or more Kitty APC
// sequences for a single-row strip. Kept for potential future use.
func kittyChunked(encoded string, cols int) string {
	var sb strings.Builder
	for i := 0; i < len(encoded); i += kittyChunkSize {
		end := i + kittyChunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		chunk := encoded[i:end]
		more := 1
		if end >= len(encoded) {
			more = 0
		}
		if i == 0 {
			fmt.Fprintf(&sb, "\x1b_Ga=T,f=100,c=%d,r=1,q=2,m=%d;%s\x1b\\", cols, more, chunk)
		} else {
			fmt.Fprintf(&sb, "\x1b_Gm=%d;%s\x1b\\", more, chunk)
		}
	}
	return sb.String()
}
