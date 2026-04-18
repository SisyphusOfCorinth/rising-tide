package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/SisyphusOfCorinth/rising-tide/internal/tidal"
)

// ViewKind identifies what type of content a navigation stack frame displays.
type ViewKind int

const (
	ViewEmpty  ViewKind = iota // No content (initial state)
	ViewSearch                 // Search results (tracks, albums, artists)
	ViewArtist                 // Artist's album list
	ViewAlbum                  // Album's track list
)

// StackFrame captures the complete state of a navigation view so it can be
// restored when the user presses Escape (back).
type StackFrame struct {
	Kind  ViewKind
	Title string // Breadcrumb display: "Search: radiohead", "Artist: Radiohead"

	// Content -- only the relevant fields are populated per frame.
	Tracks  []tidal.Track
	Albums  []tidal.Album
	Artists []tidal.Artist

	// Flat item list for unified cursor navigation across mixed-type results.
	// Each entry has a label for display and a reference to the original item.
	Items []NavItem

	// Cursor state -- preserved when navigating back.
	Cursor       int
	ScrollOffset int
}

// NavItemKind tags what a NavItem points to.
type NavItemKind int

const (
	NavItemArtist NavItemKind = iota
	NavItemAlbum
	NavItemTrack
	NavItemHeader // Non-selectable section header
)

// NavItem is a single row in the navigator list. It wraps a track, album, or
// artist with a display label and selection metadata.
type NavItem struct {
	Kind       NavItemKind
	Label      string
	Selectable bool

	// Exactly one of these is set, depending on Kind.
	Track  *tidal.Track
	Album  *tidal.Album
	Artist *tidal.Artist
}

// Navigator is the central content panel implementing stack-based navigation
// through search results, artist discographies, and album track lists.
type Navigator struct {
	Stack   []StackFrame
	Loading bool
	ErrMsg  string
}

func NewNavigator() Navigator {
	return Navigator{
		Stack: []StackFrame{{Kind: ViewEmpty}},
	}
}

// Current returns the top of the navigation stack.
func (n *Navigator) Current() *StackFrame {
	if len(n.Stack) == 0 {
		return nil
	}
	return &n.Stack[len(n.Stack)-1]
}

// Push adds a new frame to the stack.
func (n *Navigator) Push(frame StackFrame) {
	n.Stack = append(n.Stack, frame)
	n.Loading = false
	n.ErrMsg = ""
}

// Pop removes the top frame and restores the previous view.
// Returns false if the stack has only one frame (nothing to pop to).
func (n *Navigator) Pop() bool {
	if len(n.Stack) <= 1 {
		return false
	}
	n.Stack = n.Stack[:len(n.Stack)-1]
	n.Loading = false
	n.ErrMsg = ""
	return true
}

// CursorDown moves the cursor to the next selectable item.
func (n *Navigator) CursorDown() {
	cur := n.Current()
	if cur == nil || len(cur.Items) == 0 {
		return
	}
	for i := cur.Cursor + 1; i < len(cur.Items); i++ {
		if cur.Items[i].Selectable {
			cur.Cursor = i
			return
		}
	}
}

// CursorUp moves the cursor to the previous selectable item.
func (n *Navigator) CursorUp() {
	cur := n.Current()
	if cur == nil || len(cur.Items) == 0 {
		return
	}
	for i := cur.Cursor - 1; i >= 0; i-- {
		if cur.Items[i].Selectable {
			cur.Cursor = i
			return
		}
	}
}

// PageDown moves the cursor down by half the visible height.
func (n *Navigator) PageDown(visibleLines int) {
	half := visibleLines / 2
	if half < 1 {
		half = 1
	}
	for range half {
		n.CursorDown()
	}
}

// PageUp moves the cursor up by half the visible height.
func (n *Navigator) PageUp(visibleLines int) {
	half := visibleLines / 2
	if half < 1 {
		half = 1
	}
	for range half {
		n.CursorUp()
	}
}

// SelectedItem returns the currently highlighted item, or nil if nothing is
// selected.
func (n *Navigator) SelectedItem() *NavItem {
	cur := n.Current()
	if cur == nil || cur.Cursor < 0 || cur.Cursor >= len(cur.Items) {
		return nil
	}
	item := &cur.Items[cur.Cursor]
	if !item.Selectable {
		return nil
	}
	return item
}

// SetSearchResults builds a unified item list from search results and pushes
// it as a new stack frame.
func (n *Navigator) SetSearchResults(query string, tracks []tidal.Track, albums []tidal.Album, artists []tidal.Artist) {
	var items []NavItem

	if len(artists) > 0 {
		items = append(items, NavItem{Kind: NavItemHeader, Label: "ARTISTS", Selectable: false})
		for i := range artists {
			items = append(items, NavItem{
				Kind:       NavItemArtist,
				Label:      artists[i].Name,
				Selectable: true,
				Artist:     &artists[i],
			})
		}
	}

	if len(albums) > 0 {
		if len(items) > 0 {
			items = append(items, NavItem{Kind: NavItemHeader, Label: "", Selectable: false})
		}
		items = append(items, NavItem{Kind: NavItemHeader, Label: "ALBUMS", Selectable: false})
		for i := range albums {
			label := fmt.Sprintf("%s - %s", albums[i].Artist.Name, albums[i].Title)
			if albums[i].Type != "" && albums[i].Type != "ALBUM" {
				label += fmt.Sprintf(" [%s]", albums[i].Type)
			}
			items = append(items, NavItem{
				Kind:       NavItemAlbum,
				Label:      label,
				Selectable: true,
				Album:      &albums[i],
			})
		}
	}

	if len(tracks) > 0 {
		if len(items) > 0 {
			items = append(items, NavItem{Kind: NavItemHeader, Label: "", Selectable: false})
		}
		items = append(items, NavItem{Kind: NavItemHeader, Label: "TRACKS", Selectable: false})
		for i := range tracks {
			dur := formatTime(float64(tracks[i].Duration))
			label := fmt.Sprintf("%s - %s  %s", tracks[i].Artist.Name, tracks[i].Title, StyleDim.Render(dur))
			items = append(items, NavItem{
				Kind:       NavItemTrack,
				Label:      label,
				Selectable: true,
				Track:      &tracks[i],
			})
		}
	}

	frame := StackFrame{
		Kind:    ViewSearch,
		Title:   "Search: " + query,
		Tracks:  tracks,
		Albums:  albums,
		Artists: artists,
		Items:   items,
		Cursor:  0,
	}

	// Set cursor to first selectable item
	for i, item := range items {
		if item.Selectable {
			frame.Cursor = i
			break
		}
	}

	n.Push(frame)
}

// SetArtistAlbums builds an album list and pushes it.
func (n *Navigator) SetArtistAlbums(artistName string, albums []tidal.Album) {
	var items []NavItem

	for i := range albums {
		typeTag := ""
		if albums[i].Type != "" && albums[i].Type != "ALBUM" {
			typeTag = fmt.Sprintf(" [%s]", albums[i].Type)
		}
		trackCount := fmt.Sprintf("%d tracks", albums[i].NumberOfTracks)
		label := fmt.Sprintf("%s%s  %s", albums[i].Title, typeTag, StyleDim.Render(trackCount))
		items = append(items, NavItem{
			Kind:       NavItemAlbum,
			Label:      label,
			Selectable: true,
			Album:      &albums[i],
		})
	}

	frame := StackFrame{
		Kind:   ViewArtist,
		Title:  artistName,
		Albums: albums,
		Items:  items,
		Cursor: 0,
	}

	n.Push(frame)
}

// SetAlbumTracks builds a track list and pushes it.
func (n *Navigator) SetAlbumTracks(albumTitle string, tracks []tidal.Track) {
	var items []NavItem

	for i := range tracks {
		dur := formatTime(float64(tracks[i].Duration))
		label := fmt.Sprintf("%2d. %s  %s", i+1, tracks[i].Title, StyleDim.Render(dur))
		items = append(items, NavItem{
			Kind:       NavItemTrack,
			Label:      label,
			Selectable: true,
			Track:      &tracks[i],
		})
	}

	frame := StackFrame{
		Kind:   ViewAlbum,
		Title:  albumTitle,
		Tracks: tracks,
		Items:  items,
		Cursor: 0,
	}

	n.Push(frame)
}

// View renders the navigator panel within the given bounds (no border).
func (n Navigator) View(w, h int, focused bool) string {
	var content string

	if n.Loading {
		content = StyleDim.Render("  Loading...")
	} else if n.ErrMsg != "" {
		content = StyleError.Render("  Error: " + n.ErrMsg)
	} else {
		cur := n.Current()
		if cur == nil || cur.Kind == ViewEmpty {
			content = StyleDim.Render("  Press / to search")
		} else {
			content = n.renderFrame(cur, w, h)
		}
	}

	// Ensure content fills the panel
	lines := strings.Split(content, "\n")
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	body := strings.Join(lines, "\n")

	return lipgloss.NewStyle().Width(w).Height(h).Render(body)
}

// renderFrame renders the items in a stack frame with cursor highlighting
// and scrolling.
func (n Navigator) renderFrame(frame *StackFrame, w, h int) string {
	var lines []string

	// Breadcrumb header
	breadcrumb := n.breadcrumb()
	if breadcrumb != "" {
		lines = append(lines, StyleTitle.Render(breadcrumb))
		lines = append(lines, "")
	}

	headerLines := len(lines)
	visibleItems := h - headerLines
	if visibleItems < 1 {
		visibleItems = 1
	}

	// Adjust scroll offset to keep cursor visible
	if frame.Cursor < frame.ScrollOffset {
		frame.ScrollOffset = frame.Cursor
	}
	if frame.Cursor >= frame.ScrollOffset+visibleItems {
		frame.ScrollOffset = frame.Cursor - visibleItems + 1
	}

	// Render visible items
	end := frame.ScrollOffset + visibleItems
	if end > len(frame.Items) {
		end = len(frame.Items)
	}

	for i := frame.ScrollOffset; i < end; i++ {
		item := frame.Items[i]

		if item.Kind == NavItemHeader {
			if item.Label == "" {
				lines = append(lines, "")
			} else {
				lines = append(lines, StyleSectionHeader.Render(item.Label))
			}
			continue
		}

		label := item.Label
		if w > 4 && lipgloss.Width(label) > w-4 {
			// Truncate long labels
			label = label[:w-7] + "..."
		}

		if i == frame.Cursor {
			lines = append(lines, StyleItemSelected.Render(" > "+label))
		} else {
			lines = append(lines, StyleItemNormal.Render("   "+label))
		}
	}

	return strings.Join(lines, "\n")
}

// breadcrumb builds the navigation path string from the stack.
func (n Navigator) breadcrumb() string {
	if len(n.Stack) <= 1 {
		return ""
	}

	var parts []string
	for _, frame := range n.Stack[1:] { // skip the empty root frame
		if frame.Title != "" {
			parts = append(parts, frame.Title)
		}
	}

	return strings.Join(parts, " > ")
}
