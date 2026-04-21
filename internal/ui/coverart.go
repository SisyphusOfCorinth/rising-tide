// Cover art display using the Kitty terminal graphics protocol.
//
// Images are assigned persistent IDs so the terminal retains them between
// frames. This avoids the ghost rendering bug caused by deleting and
// re-transmitting images every frame (which exposes stale text underneath).
// See GHOST_RENDERING_ANALYSIS.md for the full analysis.
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

// Cover art is always 640x640 pixels.
const (
	CoverPixels = 640
	CellPixelW  = 9
	CellPixelH  = 20
	CoverCols   = (CoverPixels / CellPixelW) + 1 // ~72 columns
	CoverRows   = (CoverPixels / CellPixelH) + 1 // ~33 rows
)

// CoverArt manages album cover art display via the Kitty terminal graphics
// protocol. Images are assigned persistent IDs so the terminal retains them
// between frames -- no delete-and-re-render cycle needed.
type CoverArt struct {
	Supported bool        // kitty protocol available (checked once at startup)
	Img       image.Image // cached decoded image (for re-render on resize)
	CoverURL  string      // current URL (for dedup -- skip fetch if same album)

	// Kitty image state. The transmitSeq is sent exactly once (the frame
	// after CoverArtMsg arrives). On subsequent frames the image persists
	// in the terminal's memory and no kitty commands are needed.
	imageID     uint32 // current kitty image ID (incremented per album change)
	prevImageID uint32 // previous image ID that needs to be deleted
	transmitSeq string // kitty APC sequence to transmit the current image (sent once)
	deleteSeq   string // kitty APC sequence to delete the PREVIOUS image (sent once)
	placed      bool   // true after the image has been sent to the terminal
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
	return 30
}

// Height returns the fixed terminal row height for the cover art panel.
func (c CoverArt) Height() int {
	return CoverRows
}

// Clear resets the cover art display (e.g. when queue is exhausted).
// The old image is deleted from the terminal on the next frame via deleteSeq.
func (c *CoverArt) Clear() {
	if c.prevImageID > 0 {
		c.deleteSeq = kittyDeleteImage(c.prevImageID)
	}
	c.Img = nil
	c.transmitSeq = ""
	c.CoverURL = ""
	c.imageID = 0
	c.prevImageID = 0
	c.placed = false
}

// SetImage installs a new cover art image. If a previous image exists, a
// targeted delete sequence is prepared. The new image transmit sequence is
// built and will be sent on the next View() render.
func (c *CoverArt) SetImage(img image.Image, coverURL string, rows []string, newImageID uint32) {
	// Delete the previous image (if any) on the next frame.
	if c.prevImageID > 0 && c.prevImageID != newImageID {
		c.deleteSeq = kittyDeleteImage(c.prevImageID)
	}

	c.Img = img
	c.CoverURL = coverURL
	c.imageID = newImageID
	c.placed = false

	if len(rows) > 0 {
		c.transmitSeq = rows[0]
	}
}

// KittySequenceForFrame returns the kitty escape sequence(s) to include in
// the current frame's output, or "" if the image is already placed and no
// action is needed. After returning a non-empty string, subsequent calls
// return "" until the image changes.
//
// Returns the kitty escape sequence(s) for this frame.
//
// On the transmit frame: sends the new image ONLY (no delete). Both old
// and new images coexist -- the new one covers the old one visually.
// On the NEXT frame: sends the delete for the old image. By this point
// BubbleTea has fully written all text rows, so the terminal repaint
// triggered by the delete uses fresh text content (no ghost).
func (c *CoverArt) KittySequenceForFrame() string {
	// If we have a pending delete AND the new image is already placed,
	// it's safe to delete the old image now (text layer is fresh from
	// the previous full-rewrite frame).
	if c.deleteSeq != "" && c.placed {
		seq := c.deleteSeq
		c.deleteSeq = ""
		return seq
	}

	// Transmit the new image. The delete of the old image will happen
	// on the NEXT frame after the text layer has been fully rewritten.
	if !c.placed && c.transmitSeq != "" {
		seq := c.transmitSeq
		c.placed = true
		c.prevImageID = c.imageID
		return seq
	}

	return ""
}

// NeedsRender returns true if the cover art has a pending transmit or delete.
func (c CoverArt) NeedsRender() bool {
	return c.deleteSeq != "" || (!c.placed && c.transmitSeq != "")
}

// HasImage returns true if a cover art image is currently displayed in the
// terminal (or was previously displayed and a new one is being fetched).
// This prevents falling back to the placeholder layout during album
// transitions, which would write text underneath the kitty image layer.
func (c CoverArt) HasImage() bool {
	return c.imageID > 0
}

// View renders the cover art placeholder panel (used when no image is loaded
// or on unsupported terminals).
func (c CoverArt) View(w, h int, focused bool) string {
	return c.fallbackView(w, h, focused)
}

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
// terminal graphics protocol.
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
// with the given image ID. Returns a slice where index 0 has the sequence
// and the rest are empty strings.
func RenderKittyRows(img image.Image, cols, rows int, imageID uint32) []string {
	if img == nil || cols <= 0 || rows <= 0 {
		return nil
	}

	scaled := image.NewRGBA(image.Rect(0, 0, CoverPixels, CoverPixels))
	draw.BiLinear.Scale(scaled, scaled.Bounds(), img, img.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		return nil
	}

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	seq := kittyChunkedFull(encoded, cols, rows, imageID)

	result := make([]string, rows)
	result[0] = seq
	return result
}

// kittyChunkedFull wraps base64-encoded image data for a full multi-row image
// with a persistent image ID. The terminal retains the image in memory by ID.
func kittyChunkedFull(encoded string, cols, rows int, imageID uint32) string {
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
			// i={id} assigns a persistent image ID so the terminal retains
			// the image between frames without re-transmission.
			fmt.Fprintf(&sb, "\x1b_Ga=T,i=%d,f=100,c=%d,r=%d,q=2,m=%d;%s\x1b\\",
				imageID, cols, rows, more, chunk)
		} else {
			fmt.Fprintf(&sb, "\x1b_Gm=%d;%s\x1b\\", more, chunk)
		}
	}
	return sb.String()
}

// kittyDeleteImage returns a kitty APC sequence that deletes a specific image
// by its ID. This is more precise than deleting all images (a=d,d=a).
func kittyDeleteImage(imageID uint32) string {
	return fmt.Sprintf("\x1b_Ga=d,d=i,i=%d,q=2\x1b\\", imageID)
}
