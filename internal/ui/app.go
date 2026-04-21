package ui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/SisyphusOfCorinth/rising-tide/internal/player"
	"github.com/SisyphusOfCorinth/rising-tide/internal/store"
	"github.com/SisyphusOfCorinth/rising-tide/internal/tidal"
)

// AppState represents the lifecycle stage of the application.
type AppState int

const (
	StateLoading AppState = iota // Checking for stored session
	StateLogin                   // OAuth device flow in progress
	StateReady                   // Normal operation
)

// FocusedPane identifies which panel receives key input.
type FocusedPane int

const (
	PaneNavigator FocusedPane = iota
	PaneCoverArt
)

// App is the root Bubble Tea model. It owns all state and dispatches messages
// to child panels via method calls (not nested tea.Model chains).
type App struct {
	// Lifecycle
	state  AppState
	width  int
	height int
	ready  bool // set after first WindowSizeMsg

	// Focus
	focused FocusedPane

	// Child panels (struct fields, not interfaces)
	navigator  Navigator
	nowPlaying NowPlaying
	coverArt   CoverArt

	// Search overlay
	searchInput  textinput.Model
	searchActive bool

	// Help overlay
	helpVisible bool
	help        help.Model

	// Device picker overlay
	devicePickerVisible bool
	deviceList          []player.DeviceInfo
	deviceCursor        int

	// Library menu overlay ('p' key)
	libraryVisible bool
	libraryCursor  int // 0=Favorites, 1=My Mixes

	// Queue: tracks to play after the current one finishes.
	queue       []tidal.Track
	queueIndex  int // index of the currently playing track in queue
	queueVisible bool
	queueCursor  int // cursor position in the queue overlay

	// playGeneration is incremented each time a new track play is initiated
	// (via skip, queue select, or track selection). waitForPlaybackDone
	// captures the current generation; PlaybackFinishedMsg is only acted on
	// if the generation still matches, preventing cascade-skip bugs.
	playGeneration uint64

	// Backend references (injected, not owned)
	tidal  *tidal.Client
	player *player.Player
	store  *store.SecretsStore

	// Key bindings
	keys KeyMap

	// Status messages
	statusMsg string
}

// NewApp creates the root application model with injected dependencies.
func NewApp(client *tidal.Client, p *player.Player, st *store.SecretsStore) App {
	ti := textinput.New()
	ti.Placeholder = "Search Tidal..."
	ti.CharLimit = 100
	ti.Width = 50

	h := help.New()

	return App{
		state:     StateReady, // Auth is handled before the TUI starts
		focused:   PaneNavigator,
		navigator:  NewNavigator(),
		nowPlaying: NewNowPlaying(),
		coverArt:   NewCoverArt(),
		searchInput: ti,
		help:      h,
		tidal:     client,
		player:    p,
		store:     st,
		keys:      DefaultKeyMap(),
	}
}

// Init is called when the TUI starts. Auth is already complete by this point.
func (m App) Init() tea.Cmd {
	return nil
}

// Update is the central message dispatcher. Key events follow a strict
// priority chain: quit > overlays > global > focus-routed.
func (m App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		// Re-render cover art at new dimensions. Use a new image ID so
		// SetImage() will delete the old positioned image and transmit
		// a new one at the correct position.
		if m.coverArt.Supported && m.coverArt.Img != nil {
			nextID := m.coverArt.imageID + 1
			// On resize, force-delete ALL images immediately. The old image
			// is at the wrong pixel position and must be removed before the
			// new one is placed. Use deleteAll flag to send the command on
			// the next frame.
			m.coverArt.deleteSeq = "\x1b_Ga=d,d=a,q=2\x1b\\"
			m.coverArt.placed = true // so the delete fires next frame
			m.coverArt.imageID = nextID
			return m, rerenderCoverArt(m.coverArt.Img, m.coverArt.CoverURL, CoverCols, CoverRows, nextID)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	// --- Auth messages ---
	case AuthCheckCompleteMsg:
		if msg.Authenticated {
			m.state = StateReady
			return m, nil
		}
		// No stored session -- need to login. We run the interactive auth
		// outside the alt-screen so the QR code and prompts render correctly.
		m.state = StateLogin
		return m, startLogin(m.tidal)

	case LoginCompleteMsg:
		if msg.Err != nil {
			m.statusMsg = fmt.Sprintf("Login failed: %v", msg.Err)
			return m, nil
		}
		// Persist the session for future runs.
		if err := m.store.SaveSession(msg.Session); err != nil {
			m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", err)
		}
		m.state = StateReady
		return m, nil

	// --- Search messages ---
	case SearchResultsMsg:
		if msg.Err != nil {
			m.navigator.Loading = false
			m.navigator.ErrMsg = msg.Err.Error()
			return m, nil
		}
		m.navigator.SetSearchResults(msg.Query, msg.Tracks, msg.Albums, msg.Artists)
		return m, nil

	// --- Navigation messages ---
	case ArtistAlbumsMsg:
		if msg.Err != nil {
			m.navigator.Loading = false
			m.navigator.ErrMsg = msg.Err.Error()
			return m, nil
		}
		m.navigator.SetArtistAlbums(msg.ArtistName, msg.Albums)
		return m, nil

	case AlbumTracksMsg:
		if msg.Err != nil {
			m.navigator.Loading = false
			m.navigator.ErrMsg = msg.Err.Error()
			return m, nil
		}
		m.navigator.SetAlbumTracks(msg.AlbumTitle, msg.Tracks)
		return m, nil

	// --- Library messages ---
	case FavoritesMsg:
		if msg.Err != nil {
			m.navigator.Loading = false
			m.navigator.ErrMsg = msg.Err.Error()
			return m, nil
		}
		m.navigator.SetTrackList("Favorites", ViewFavorites, msg.Tracks)
		return m, nil

	case MixListMsg:
		if msg.Err != nil {
			m.navigator.Loading = false
			m.navigator.ErrMsg = msg.Err.Error()
			return m, nil
		}
		m.navigator.SetMixList(msg.Mixes)
		return m, nil

	case MixTracksMsg:
		if msg.Err != nil {
			m.navigator.Loading = false
			m.navigator.ErrMsg = msg.Err.Error()
			return m, nil
		}
		m.navigator.SetTrackList(msg.MixTitle, ViewMix, msg.Tracks)
		return m, nil

	// --- Playback messages ---
	case StreamURLMsg:
		if msg.Err != nil {
			m.nowPlaying.Clear()
			m.statusMsg = fmt.Sprintf("Stream error: %v", msg.Err)
			return m, nil
		}
		return m, startPlayback(m.player, msg.Track, msg.URL)

	case PlaybackStartedMsg:
		m.nowPlaying.SetTrack(msg.Track.Title, msg.Track.Artist.Name, msg.Track.Album.Title)
		m.statusMsg = ""
		cmds := []tea.Cmd{tickPlaybackProgress(), waitForPlaybackDone(m.player, m.playGeneration)}
		// Fetch cover art if kitty is supported and this is a different album.
		// Don't modify coverArt state here -- let SetImage() handle the
		// delete+transmit atomically when CoverArtMsg arrives, so the old
		// image stays visible until the new one is ready.
		if m.coverArt.Supported {
			newCoverURL := tidal.CoverURL(msg.Track.Album.Cover, "640x640")
			if newCoverURL != m.coverArt.CoverURL {
				nextID := m.coverArt.imageID + 1
				// Set imageID immediately so HasImage() returns true. This
				// prevents the fallback placeholder layout from rendering
				// during the fetch, which would write text underneath the
				// kitty image layer and cause the ghost bug.
				m.coverArt.imageID = nextID
				cmds = append(cmds, fetchCoverArt(msg.Track.Album.Cover, CoverCols, CoverRows, nextID))
			}
		}
		return m, tea.Batch(cmds...)

	case PlaybackErrorMsg:
		m.nowPlaying.Clear()
		m.statusMsg = fmt.Sprintf("Playback error: %v", msg.Err)
		return m, nil

	case PlaybackFinishedMsg:
		// Ignore stale finish signals from tracks that were stopped by a
		// skip or new-play command. Only act if the generation matches.
		if msg.Generation != m.playGeneration {
			return m, nil
		}
		// Advance to the next track in the queue.
		m.queueIndex++
		if m.queueIndex < len(m.queue) {
			next := m.queue[m.queueIndex]
			m.playGeneration++
			m.nowPlaying.Resolving = true
			return m, resolveAndPlay(m.tidal, next)
		}
		// Queue exhausted.
		m.nowPlaying.Clear()
		m.coverArt.Clear()
		m.queue = nil
		m.queueIndex = 0
		return m, nil

	case TickMsg:
		if !m.nowPlaying.Playing {
			return m, nil // stop tick loop
		}
		pos, _ := m.player.GetPosition()
		dur, _ := m.player.GetDuration()
		m.nowPlaying.SetProgress(pos, dur)
		return m, tickPlaybackProgress() // continue tick loop

	// --- Device messages ---
	case DeviceListMsg:
		if msg.Err != nil {
			m.statusMsg = fmt.Sprintf("Device error: %v", msg.Err)
			return m, nil
		}
		if len(msg.Devices) == 0 {
			m.statusMsg = "No audio devices found"
			return m, nil
		}
		m.deviceList = msg.Devices
		m.deviceCursor = 0
		m.devicePickerVisible = true
		return m, nil

	// --- Cover Art messages ---
	case CoverArtMsg:
		if msg.Err != nil {
			return m, nil
		}
		albumChanged := m.coverArt.CoverURL != msg.CoverURL
		m.coverArt.SetImage(msg.Img, msg.CoverURL, msg.Rows, msg.ImageID)
		if albumChanged {
			// Clear the terminal when the album changes. This erases ALL
			// text (including stale text under the old image) and forces
			// BubbleTea to rewrite everything. The new kitty image is
			// placed as part of the rewrite.
			return m, func() tea.Msg { return tea.ClearScreen() }
		}
		return m, nil
	}

	return m, nil
}

// handleKey processes keyboard input with the priority chain:
// quit > overlays (search, help) > global > focus-routed.
func (m App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Always allow ctrl+c to quit
	if key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))) {
		return m, tea.Quit
	}

	// --- Search overlay captures all input ---
	if m.searchActive {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.searchActive = false
			m.searchInput.Blur()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			query := strings.TrimSpace(m.searchInput.Value())
			if query == "" {
				return m, nil
			}
			m.searchActive = false
			m.searchInput.Blur()
			m.navigator.Loading = true
			m.navigator.ErrMsg = ""
			return m, searchTidal(m.tidal, query)
		default:
			var cmd tea.Cmd
			m.searchInput, cmd = m.searchInput.Update(msg)
			return m, cmd
		}
	}

	// --- Help overlay: only ? or q accepted ---
	if m.helpVisible {
		if key.Matches(msg, m.keys.Help) || key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) {
			m.helpVisible = false
			return m, nil
		}
		if key.Matches(msg, key.NewBinding(key.WithKeys("q"))) {
			return m, tea.Quit
		}
		return m, nil
	}

	// --- Device picker overlay ---
	if m.devicePickerVisible {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.devicePickerVisible = false
			return m, nil
		case key.Matches(msg, m.keys.CursorDown):
			if m.deviceCursor < len(m.deviceList)-1 {
				m.deviceCursor++
			}
			return m, nil
		case key.Matches(msg, m.keys.CursorUp):
			if m.deviceCursor > 0 {
				m.deviceCursor--
			}
			return m, nil
		case key.Matches(msg, m.keys.Select):
			dev := m.deviceList[m.deviceCursor]
			m.player.SetDevice(dev.HWName)
			_ = m.store.SaveDevice(dev.HWName)
			m.statusMsg = fmt.Sprintf("Audio device: %s (%s)", dev.LongName, dev.HWName)
			m.devicePickerVisible = false
			return m, nil
		}
		return m, nil
	}

	// --- Queue overlay ---
	if m.queueVisible {
		switch {
		case key.Matches(msg, m.keys.Queue) || key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.queueVisible = false
			return m, nil
		case key.Matches(msg, m.keys.CursorDown):
			if m.queueCursor < len(m.queue)-1 {
				m.queueCursor++
			}
			return m, nil
		case key.Matches(msg, m.keys.CursorUp):
			if m.queueCursor > 0 {
				m.queueCursor--
			}
			return m, nil
		case key.Matches(msg, m.keys.Select):
			// Jump to the selected track in the queue and play it.
			if m.queueCursor >= 0 && m.queueCursor < len(m.queue) {
				m.queueIndex = m.queueCursor
				m.playGeneration++
				m.nowPlaying.Resolving = true
				m.queueVisible = false
				return m, resolveAndPlay(m.tidal, m.queue[m.queueIndex])
			}
			return m, nil
		}
		return m, nil
	}

	// --- Library overlay ---
	if m.libraryVisible {
		switch {
		case key.Matches(msg, m.keys.Library) || key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.libraryVisible = false
			return m, nil
		case key.Matches(msg, m.keys.CursorDown):
			if m.libraryCursor < 1 { // 2 options: 0=Favorites, 1=My Mixes
				m.libraryCursor++
			}
			return m, nil
		case key.Matches(msg, m.keys.CursorUp):
			if m.libraryCursor > 0 {
				m.libraryCursor--
			}
			return m, nil
		case key.Matches(msg, m.keys.Select):
			m.libraryVisible = false
			m.navigator.Loading = true
			switch m.libraryCursor {
			case 0: // Favorites
				return m, fetchFavorites(m.tidal)
			case 1: // My Mixes
				return m, fetchMixList(m.tidal)
			}
			return m, nil
		}
		return m, nil
	}

	// --- Global keys ---
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.helpVisible = true
		return m, nil

	case key.Matches(msg, m.keys.Library):
		m.libraryVisible = !m.libraryVisible
		m.libraryCursor = 0
		return m, nil

	case key.Matches(msg, m.keys.Queue):
		if len(m.queue) > 0 {
			m.queueVisible = !m.queueVisible
			m.queueCursor = m.queueIndex
		}
		return m, nil

	case key.Matches(msg, m.keys.Search):
		m.searchActive = true
		m.searchInput.SetValue("")
		m.searchInput.Focus()
		return m, m.searchInput.Cursor.SetMode(cursor.CursorBlink)

	case key.Matches(msg, m.keys.PlayPause):
		if m.nowPlaying.TrackTitle != "" {
			_ = m.player.Pause()
			m.nowPlaying.Playing = !m.nowPlaying.Playing
			// Restart the tick loop on unpause so the progress bar resumes.
			if m.nowPlaying.Playing {
				return m, tickPlaybackProgress()
			}
		}
		return m, nil

	case key.Matches(msg, m.keys.SkipTrack):
		// Skip to the next track in the queue. Increment generation so the
		// PlaybackFinishedMsg from the stopped track is ignored.
		if m.nowPlaying.TrackTitle != "" && m.queueIndex+1 < len(m.queue) {
			m.queueIndex++
			m.playGeneration++
			next := m.queue[m.queueIndex]
			m.nowPlaying.Resolving = true
			return m, resolveAndPlay(m.tidal, next)
		}
		return m, nil

	case key.Matches(msg, m.keys.DeviceMenu):
		return m, listDevices()

	// Pane focus navigation (shift+HJKL)
	case key.Matches(msg, m.keys.FocusLeft), key.Matches(msg, m.keys.FocusUp),
		key.Matches(msg, m.keys.FocusDown):
		m.focused = PaneNavigator
		return m, nil
	case key.Matches(msg, m.keys.FocusRight):
		// Reserved for future use (cover art panel focus)
		m.focused = PaneNavigator
		return m, nil
	}

	// --- Focus-routed keys ---
	if m.focused == PaneNavigator {
		return m.handleNavigatorKey(msg)
	}

	return m, nil
}

// handleNavigatorKey processes keys when the navigator panel has focus.
func (m App) handleNavigatorKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.CursorDown):
		m.navigator.CursorDown()
		return m, nil

	case key.Matches(msg, m.keys.CursorUp):
		m.navigator.CursorUp()
		return m, nil

	case key.Matches(msg, m.keys.PageDown):
		m.navigator.PageDown(m.height - 8) // approximate visible lines
		return m, nil

	case key.Matches(msg, m.keys.PageUp):
		m.navigator.PageUp(m.height - 8)
		return m, nil

	case key.Matches(msg, m.keys.Back):
		m.navigator.Pop()
		return m, nil

	case key.Matches(msg, m.keys.Select):
		item := m.navigator.SelectedItem()
		if item == nil {
			return m, nil
		}

		switch item.Kind {
		case NavItemArtist:
			m.navigator.Loading = true
			return m, fetchArtistAlbums(m.tidal, item.Artist.ID, item.Artist.Name)

		case NavItemAlbum:
			m.navigator.Loading = true
			return m, fetchAlbumTracks(m.tidal, item.Album.ID, item.Album.Title)

		case NavItemMix:
			m.navigator.Loading = true
			return m, fetchMixTracks(m.tidal, item.Mix.ID, item.Mix.Title)

		case NavItemTrack:
			m.nowPlaying.Resolving = true
			// Build a queue from all tracks in the current view, starting
			// from the selected track. This mimics standard Tidal behavior:
			// selecting track 4 of 10 queues tracks 4-10.
			m.queue = nil
			m.queueIndex = 0
			cur := m.navigator.Current()
			if cur != nil {
				foundSelected := false
				for _, navItem := range cur.Items {
					if navItem.Kind == NavItemTrack && navItem.Track != nil {
						if navItem.Track.ID == item.Track.ID {
							foundSelected = true
						}
						if foundSelected {
							m.queue = append(m.queue, *navItem.Track)
						}
					}
				}
			}
			// If we didn't build a queue (e.g. from search results with mixed types),
			// just queue the single track.
			if len(m.queue) == 0 {
				m.queue = []tidal.Track{*item.Track}
			}
			m.playGeneration++
			return m, resolveAndPlay(m.tidal, m.queue[0])
		}
	}

	return m, nil
}

// View composes the four panels into the terminal output.
func (m App) View() string {
	if !m.ready {
		return "Loading..."
	}

	if m.state == StateLoading {
		return "Checking authentication..."
	}

	if m.state == StateLogin {
		return "Authenticating with Tidal...\n\nCheck your browser or terminal for the login prompt."
	}

	// Calculate panel dimensions.
	coverW := m.coverArt.Width()

	var full string

	if m.coverArt.Supported {
		// Kitty layout: cover art is a fixed 640x640px box in the top-right.
		// The navigator fills the full left area. Now-playing bar below.
		//
		// +-------------------------------+--------------------+
		// | Navigator                     |    Cover Art       |
		// |  (topH rows)                  |  (fixed 640x640)   |
		// +-------------------------------+--------------------+
		// | Now Playing (full width)                            |
		// +-----------------------------------------------------+
		topH := m.coverArt.Height()
		if topH > m.height-1 {
			topH = m.height - 1
		}
		bottomH := m.height - topH
		_ = bottomH // used below for bottom bar padding

		navW := m.width - coverW
		if navW < 10 {
			navW = 10
		}

		// Render the navigator (full left area).
		navView := m.navigator.View(navW, topH, m.focused == PaneNavigator)

		// Build the top section: left panel lines with kitty sequence on line 0.
		// Get the kitty sequence for this frame (transmit + optional delete).
		// Returns "" if the image is already placed and nothing changed.
		kittySeq := m.coverArt.KittySequenceForFrame()

		navLines := strings.Split(navView, "\n")
		topLines := make([]string, topH)
		for i := 0; i < topH; i++ {
			if i < len(navLines) {
				topLines[i] = navLines[i]
			} else {
				topLines[i] = ""
			}
		}

		if kittySeq != "" {
			// Append the kitty sequence to line 0. It renders the image as
			// a graphic overlay spanning CoverRows x CoverCols cells.
			topLines[0] += kittySeq
		} else if !m.coverArt.HasImage() {
			// No image loaded yet -- show the placeholder panel.
			coverView := m.coverArt.View(coverW, topH, m.focused == PaneCoverArt)
			joined := lipgloss.JoinHorizontal(lipgloss.Top, navView, coverView)
			topLines = strings.Split(joined, "\n")
			for len(topLines) < topH {
				topLines = append(topLines, "")
			}
			topLines = topLines[:topH]
		}
		// When HasImage() is true and kittySeq is "", the image persists in
		// the terminal's memory from a previous frame. No kitty commands needed.

		topSection := strings.Join(topLines, "\n")

		// Now-playing bar spans full terminal width.
		bottomBar := m.nowPlaying.View(m.width)
		if m.statusMsg != "" {
			bottomBar = StyleError.Render(" "+m.statusMsg+" ") + "\n" + bottomBar
		}
		// Pad/truncate bottom bar to exactly bottomH lines.
		bottomLines := strings.Split(bottomBar, "\n")
		for len(bottomLines) < bottomH {
			bottomLines = append(bottomLines, "")
		}
		bottomLines = bottomLines[:bottomH]

		bottomBar = strings.Join(bottomLines, "\n")

		full = topSection + "\n" + bottomBar
	} else {
		// Fallback layout: navigator + cover art placeholder, full-width bottom bar.
		bottomH := 3
		navW := m.width - coverW
		if navW < 10 {
			navW = 10
		}
		topH := m.height - bottomH
		if topH < 3 {
			topH = 3
		}

		navView := m.navigator.View(navW, topH, m.focused == PaneNavigator)
		coverView := m.coverArt.View(coverW, topH, m.focused == PaneCoverArt)

		topRow := lipgloss.JoinHorizontal(lipgloss.Top, navView, coverView)

		bottomBar := m.nowPlaying.View(m.width)
		if m.statusMsg != "" {
			bottomBar = StyleError.Render(" "+m.statusMsg+" ") + "\n" + bottomBar
		}

		full = lipgloss.JoinVertical(lipgloss.Left, topRow, bottomBar)
	}

	// Overlay search bar if active
	if m.searchActive {
		full = m.overlaySearchBar(full)
	}

	// Overlay device picker if visible
	if m.devicePickerVisible {
		full = m.overlayDevicePicker(full)
	}

	// Overlay queue if visible
	if m.queueVisible {
		full = m.overlayQueue(full)
	}

	// Overlay library menu if visible
	if m.libraryVisible {
		full = m.overlayLibrary(full)
	}

	// Overlay help if visible
	if m.helpVisible {
		full = m.overlayHelp(full)
	}

	// DEBUG: dump ALL lines to file, check for now-playing text in top section
	if m.coverArt.Supported && m.nowPlaying.TrackTitle != "" {
		debugLines := strings.Split(full, "\n")
		f, err := os.OpenFile("/tmp/rising-tide-view-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			topH := m.coverArt.Height()
			fmt.Fprintf(f, "\n=== FRAME === total=%d height=%d topH=%d imageID=%d placed=%v track=%q\n",
				len(debugLines), m.height, topH, m.coverArt.imageID, m.coverArt.placed, m.nowPlaying.TrackTitle)
			// Check every line for now-playing content
			for i, line := range debugLines {
				hasNP := strings.Contains(line, m.nowPlaying.TrackTitle)
				hasKitty := strings.Contains(line, "\x1b_G")
				if hasNP || hasKitty || i == 0 || i == topH || i == topH-1 {
					truncLine := line
					if hasKitty {
						if idx := strings.Index(truncLine, "\x1b_G"); idx >= 0 {
							truncLine = truncLine[:idx] + "[KITTY@" + fmt.Sprint(idx) + "]"
						}
					}
					if len(truncLine) > 200 {
						truncLine = truncLine[:200]
					}
					tag := ""
					if hasNP { tag += " **HAS_TRACK_TITLE**" }
					if hasKitty { tag += " [has_kitty]" }
					fmt.Fprintf(f, "  line[%d]%s: %q\n", i, tag, truncLine)
				}
			}
			f.Close()
		}
	}

	return full
}

// overlaySearchBar renders the search input at the top of the screen.
// When kitty cover art is active, the search bar is centered within the
// left panel area so it doesn't overlap the image.
func (m App) overlaySearchBar(base string) string {
	searchBar := StyleSearchBar.Width(50).Render(
		"Search: " + m.searchInput.View(),
	)

	// Center within the left panel area (exclude cover art column).
	availW := m.width
	if m.coverArt.Supported {
		availW = m.width - m.coverArt.Width()
	}
	xOffset := (availW - 54) / 2
	if xOffset < 0 {
		xOffset = 0
	}

	lines := strings.Split(base, "\n")
	searchLines := strings.Split(searchBar, "\n")

	for i, sl := range searchLines {
		row := i + 1
		if row < len(lines) {
			padded := strings.Repeat(" ", xOffset) + sl
			lines[row] = padded
		}
	}

	return strings.Join(lines, "\n")
}

// overlayHelp renders the help panel in the center of the screen.
func (m App) overlayHelp(base string) string {
	helpContent := "COMMANDS\n\n"

	groups := m.keys.FullHelp()
	groupNames := []string{"Navigation", "Movement", "Pane Focus", "Playback", "Application"}

	for i, group := range groups {
		if i < len(groupNames) {
			helpContent += StyleSectionHeader.Render(groupNames[i]) + "\n"
		}
		for _, binding := range group {
			helpKeys := binding.Help().Key
			helpDesc := binding.Help().Desc
			helpContent += fmt.Sprintf("  %-12s %s\n", helpKeys, helpDesc)
		}
		helpContent += "\n"
	}

	helpContent += StyleDim.Render("Press ? or Esc to close")

	helpBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorPrimary).
		Padding(1, 2).
		Width(40).
		Render(helpContent)

	// Center the help overlay
	lines := strings.Split(base, "\n")
	helpLines := strings.Split(helpBox, "\n")

	yOffset := (m.height - len(helpLines)) / 2
	availW := m.width
	if m.coverArt.Supported {
		availW = m.width - m.coverArt.Width()
	}
	xOffset := (availW - 44) / 2
	if yOffset < 0 {
		yOffset = 0
	}
	if xOffset < 0 {
		xOffset = 0
	}

	for i, hl := range helpLines {
		row := yOffset + i
		if row < len(lines) {
			padded := strings.Repeat(" ", xOffset) + hl
			lines[row] = padded
		}
	}

	return strings.Join(lines, "\n")
}

// overlayDevicePicker renders the audio device selection list.
func (m App) overlayDevicePicker(base string) string {
	content := StyleSectionHeader.Render("SELECT AUDIO DEVICE") + "\n\n"

	for i, dev := range m.deviceList {
		label := fmt.Sprintf("%s  %s", dev.LongName, StyleDim.Render(dev.HWName))
		if i == m.deviceCursor {
			content += StyleItemSelected.Render(" > "+label) + "\n"
		} else {
			content += StyleItemNormal.Render("   "+label) + "\n"
		}
	}

	content += "\n" + StyleDim.Render("Enter to select, Esc to cancel")

	pickerBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorPrimary).
		Padding(1, 2).
		Width(60).
		Render(content)

	lines := strings.Split(base, "\n")
	pickerLines := strings.Split(pickerBox, "\n")

	availW := m.width
	if m.coverArt.Supported {
		availW = m.width - m.coverArt.Width()
	}
	yOffset := (m.height - len(pickerLines)) / 2
	xOffset := (availW - 64) / 2
	if yOffset < 0 {
		yOffset = 0
	}
	if xOffset < 0 {
		xOffset = 0
	}

	for i, pl := range pickerLines {
		row := yOffset + i
		if row < len(lines) {
			lines[row] = strings.Repeat(" ", xOffset) + pl
		}
	}

	return strings.Join(lines, "\n")
}

// overlayQueue renders the playback queue as a scrollable overlay.
func (m App) overlayQueue(base string) string {
	content := StyleSectionHeader.Render("QUEUE") + "\n\n"

	// Show tracks with the currently playing one highlighted.
	maxVisible := 20
	startIdx := m.queueCursor - maxVisible/2
	if startIdx < 0 {
		startIdx = 0
	}
	endIdx := startIdx + maxVisible
	if endIdx > len(m.queue) {
		endIdx = len(m.queue)
	}

	for i := startIdx; i < endIdx; i++ {
		track := m.queue[i]
		dur := formatTime(float64(track.Duration))
		label := fmt.Sprintf("%s - %s  %s", track.Artist.Name, track.Title, StyleDim.Render(dur))

		prefix := "   "
		if i == m.queueIndex {
			prefix = " > " // currently playing
		}
		if i == m.queueCursor {
			content += StyleItemSelected.Render(prefix+label) + "\n"
		} else if i == m.queueIndex {
			content += StyleTitle.Render(prefix+label) + "\n"
		} else if i < m.queueIndex {
			content += StyleItemMuted.Render(prefix+label) + "\n"
		} else {
			content += StyleItemNormal.Render(prefix+label) + "\n"
		}
	}

	content += "\n" + StyleDim.Render("; or Esc to close")

	boxW := 60
	availW := m.width
	if m.coverArt.Supported {
		availW = m.width - m.coverArt.Width()
	}

	queueBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorPrimary).
		Padding(1, 2).
		Width(boxW).
		Render(content)

	lines := strings.Split(base, "\n")
	queueLines := strings.Split(queueBox, "\n")

	yOffset := (m.height - len(queueLines)) / 2
	xOffset := (availW - boxW - 4) / 2
	if yOffset < 0 {
		yOffset = 0
	}
	if xOffset < 0 {
		xOffset = 0
	}

	for i, ql := range queueLines {
		row := yOffset + i
		if row < len(lines) {
			lines[row] = strings.Repeat(" ", xOffset) + ql
		}
	}

	return strings.Join(lines, "\n")
}

// overlayLibrary renders the library menu popup.
func (m App) overlayLibrary(base string) string {
	options := []string{"Favorites", "My Mixes"}

	content := StyleSectionHeader.Render("LIBRARY") + "\n\n"

	for i, opt := range options {
		if i == m.libraryCursor {
			content += StyleItemSelected.Render(" > "+opt) + "\n"
		} else {
			content += StyleItemNormal.Render("   "+opt) + "\n"
		}
	}

	content += "\n" + StyleDim.Render("Enter to select, p or Esc to close")

	boxW := 40
	availW := m.width
	if m.coverArt.Supported {
		availW = m.width - m.coverArt.Width()
	}

	libraryBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorPrimary).
		Padding(1, 2).
		Width(boxW).
		Render(content)

	lines := strings.Split(base, "\n")
	boxLines := strings.Split(libraryBox, "\n")

	yOffset := (m.height - len(boxLines)) / 2
	xOffset := (availW - boxW - 4) / 2
	if yOffset < 0 {
		yOffset = 0
	}
	if xOffset < 0 {
		xOffset = 0
	}

	for i, bl := range boxLines {
		row := yOffset + i
		if row < len(lines) {
			lines[row] = strings.Repeat(" ", xOffset) + bl
		}
	}

	return strings.Join(lines, "\n")
}
