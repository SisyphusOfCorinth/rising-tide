package ui

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines all keyboard bindings, organized by context. It implements
// the help.KeyMap interface so the ? overlay can display available commands.
type KeyMap struct {
	// Global (always active unless an overlay captures input)
	Quit       key.Binding
	Help       key.Binding
	Search     key.Binding
	PlayPause  key.Binding
	SkipTrack  key.Binding
	DeviceMenu key.Binding
	Queue      key.Binding
	Library    key.Binding

	// Pane navigation (shift+hjkl)
	FocusLeft  key.Binding
	FocusDown  key.Binding
	FocusUp    key.Binding
	FocusRight key.Binding

	// Within-pane navigation
	CursorUp   key.Binding
	CursorDown key.Binding
	Select     key.Binding // Enter
	Back       key.Binding // Escape
	PageUp     key.Binding
	PageDown   key.Binding
}

// DefaultKeyMap returns the neovim-inspired key bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit:       key.NewBinding(key.WithKeys("ctrl+c", "q"), key.WithHelp("q", "quit")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Search:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		PlayPause:  key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "play/pause")),
		SkipTrack:  key.NewBinding(key.WithKeys(">"), key.WithHelp(">", "skip track")),
		DeviceMenu: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "audio device")),
		Queue:      key.NewBinding(key.WithKeys(";"), key.WithHelp(";", "queue")),
		Library:    key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "library")),

		FocusLeft:  key.NewBinding(key.WithKeys("H"), key.WithHelp("H", "focus left")),
		FocusDown:  key.NewBinding(key.WithKeys("J"), key.WithHelp("J", "focus down")),
		FocusUp:    key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "focus up")),
		FocusRight: key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "focus right")),

		CursorUp:   key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k", "up")),
		CursorDown: key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j", "down")),
		Select:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),
		Back:       key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		PageUp:     key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("C-u", "page up")),
		PageDown:   key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("C-d", "page down")),
	}
}

// ShortHelp returns the bindings shown in the footer.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Search, k.Help, k.PlayPause, k.Quit}
}

// FullHelp returns the complete set of bindings for the ? overlay.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Search, k.Select, k.Back},
		{k.CursorUp, k.CursorDown, k.PageUp, k.PageDown},
		{k.FocusLeft, k.FocusRight, k.FocusUp, k.FocusDown},
		{k.PlayPause, k.SkipTrack, k.Queue, k.Library, k.DeviceMenu},
		{k.Help, k.Quit},
	}
}
