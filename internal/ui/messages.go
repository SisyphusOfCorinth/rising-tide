package ui

import (
	"image"
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

// PlaybackFinishedMsg signals that the current track ended. The Generation
// field is checked against the app's playGeneration to distinguish natural
// track endings from stops caused by skip/new-play commands.
type PlaybackFinishedMsg struct {
	Generation uint64
}

// TickMsg drives the progress bar update loop (fires every second).
type TickMsg time.Time

// --- Library (Favorites / Mixes) ---

// FavoritesMsg carries the user's favorited tracks.
type FavoritesMsg struct {
	Tracks []tidal.Track
	Err    error
}

// MixListMsg carries the user's daily mixes.
type MixListMsg struct {
	Mixes []tidal.Mix
	Err   error
}

// MixTracksMsg carries the tracks for a selected mix.
type MixTracksMsg struct {
	MixID    string
	MixTitle string
	Tracks   []tidal.Track
	Err      error
}

// --- Cover Art ---

// CoverArtMsg carries the pre-rendered cover art kitty escape sequences.
type CoverArtMsg struct {
	CoverURL string      // for dedup
	Rows     []string    // one kitty APC sequence per terminal row
	Img      image.Image // cached decoded image (for re-render on resize)
	ImageID  uint32      // persistent kitty image ID
	Err      error
}

// --- Device ---

// DeviceListMsg carries the available ALSA playback devices.
type DeviceListMsg struct {
	Devices []player.DeviceInfo
	Err     error
}
