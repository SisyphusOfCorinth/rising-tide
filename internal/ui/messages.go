package ui

import (
	"time"

	"github.com/SisyphusOfCorinth/rising-tide/internal/player"
	"github.com/SisyphusOfCorinth/rising-tide/internal/tidal"
)

// This file is a single manifest of every custom tea.Msg the application
// handles. Keeping them in one place makes the event vocabulary auditable.

// --- Authentication ---

// AuthCheckCompleteMsg is sent after checking for a stored session on startup.
type AuthCheckCompleteMsg struct {
	Authenticated bool
	Err           error
}

// LoginDeviceCodeMsg carries the device code info for the login UI.
type LoginDeviceCodeMsg struct {
	UserCode        string
	VerificationURI string
	Err             error
}

// LoginCompleteMsg signals that authentication succeeded.
type LoginCompleteMsg struct {
	Session *tidal.Session
	Err     error
}

// --- Search ---

// SearchResultsMsg carries the results of a Tidal search.
type SearchResultsMsg struct {
	Query   string
	Tracks  []tidal.Track
	Albums  []tidal.Album
	Artists []tidal.Artist
	Err     error
}

// --- Navigation (drill-down) ---

// ArtistAlbumsMsg carries the albums for a selected artist.
type ArtistAlbumsMsg struct {
	ArtistID   int
	ArtistName string
	Albums     []tidal.Album
	Err        error
}

// AlbumTracksMsg carries the tracks for a selected album.
type AlbumTracksMsg struct {
	AlbumID    int
	AlbumTitle string
	Tracks     []tidal.Track
	Err        error
}

// --- Playback ---

// StreamURLMsg carries the resolved CDN URL for a track.
type StreamURLMsg struct {
	Track tidal.Track
	URL   string
	Err   error
}

// PlaybackStartedMsg signals that audio playback has begun.
type PlaybackStartedMsg struct {
	Track tidal.Track
}

// PlaybackErrorMsg signals a playback failure.
type PlaybackErrorMsg struct {
	Err error
}

// PlaybackFinishedMsg signals that the current track ended naturally.
type PlaybackFinishedMsg struct{}

// TickMsg drives the progress bar update loop (fires every second).
type TickMsg time.Time

// --- Device ---

// DeviceListMsg carries the available ALSA playback devices.
type DeviceListMsg struct {
	Devices []player.DeviceInfo
	Err     error
}
