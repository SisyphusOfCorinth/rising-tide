package ui

import (
	"fmt"
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
	PaneSidebar   FocusedPane = iota
	PaneNavigator
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
	sidebar    Sidebar
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
		navigator: NewNavigator(),
		nowPlaying: NewNowPlaying(),
		sidebar:   NewSidebar(),
		coverArt:  NewCoverArt(),
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
		// Start tick loop and wait for track completion concurrently.
		return m, tea.Batch(tickPlaybackProgress(), waitForPlaybackDone(m.player))

	case PlaybackErrorMsg:
		m.nowPlaying.Clear()
		m.statusMsg = fmt.Sprintf("Playback error: %v", msg.Err)
		return m, nil

	case PlaybackFinishedMsg:
		m.nowPlaying.Clear()
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

	// --- Global keys ---
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.helpVisible = true
		return m, nil

	case key.Matches(msg, m.keys.Search):
		m.searchActive = true
		m.searchInput.SetValue("")
		m.searchInput.Focus()
		return m, m.searchInput.Cursor.SetMode(cursor.CursorBlink)

	case key.Matches(msg, m.keys.PlayPause):
		if m.nowPlaying.Playing || m.nowPlaying.Resolving {
			_ = m.player.Pause()
			m.nowPlaying.Playing = !m.nowPlaying.Playing
		}
		return m, nil

	case key.Matches(msg, m.keys.SkipTrack):
		// TODO: implement queue-based skip
		return m, nil

	case key.Matches(msg, m.keys.DeviceMenu):
		return m, listDevices()

	// Pane focus navigation (shift+HJKL)
	case key.Matches(msg, m.keys.FocusLeft):
		m.focused = PaneSidebar
		return m, nil
	case key.Matches(msg, m.keys.FocusRight):
		m.focused = PaneCoverArt
		return m, nil
	case key.Matches(msg, m.keys.FocusUp), key.Matches(msg, m.keys.FocusDown):
		// Cycle through panes
		if m.focused == PaneNavigator {
			m.focused = PaneSidebar
		} else {
			m.focused = PaneNavigator
		}
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

		case NavItemTrack:
			m.nowPlaying.Resolving = true
			return m, resolveAndPlay(m.tidal, *item.Track)
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

	// Calculate panel dimensions
	bottomH := 3
	topH := m.height - bottomH
	if topH < 3 {
		topH = 3
	}

	sidebarW := m.sidebar.Width()
	coverW := m.coverArt.Width()
	navW := m.width - sidebarW - coverW
	if navW < 10 {
		navW = 10
	}

	// Render each panel
	sidebarView := m.sidebar.View(sidebarW, topH, m.focused == PaneSidebar)
	navView := m.navigator.View(navW, topH, m.focused == PaneNavigator)
	coverView := m.coverArt.View(coverW, topH, m.focused == PaneCoverArt)

	// Compose the top row
	topRow := lipgloss.JoinHorizontal(lipgloss.Top,
		sidebarView,
		navView,
		coverView,
	)

	// Bottom bar
	bottomBar := m.nowPlaying.View(m.width)

	// Status message (errors, device info)
	if m.statusMsg != "" {
		bottomBar = StyleError.Render(" "+m.statusMsg+" ") + "\n" + bottomBar
	}

	full := lipgloss.JoinVertical(lipgloss.Left, topRow, bottomBar)

	// Overlay search bar if active
	if m.searchActive {
		full = m.overlaySearchBar(full)
	}

	// Overlay device picker if visible
	if m.devicePickerVisible {
		full = m.overlayDevicePicker(full)
	}

	// Overlay help if visible
	if m.helpVisible {
		full = m.overlayHelp(full)
	}

	return full
}

// overlaySearchBar renders the search input at the top of the screen.
func (m App) overlaySearchBar(base string) string {
	searchBar := StyleSearchBar.Width(50).Render(
		"Search: " + m.searchInput.View(),
	)

	// Place at top center
	lines := strings.Split(base, "\n")
	searchLines := strings.Split(searchBar, "\n")

	xOffset := (m.width - 54) / 2
	if xOffset < 0 {
		xOffset = 0
	}

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
	xOffset := (m.width - 44) / 2
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

	yOffset := (m.height - len(pickerLines)) / 2
	xOffset := (m.width - 64) / 2
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
