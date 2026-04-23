package ui

import (
	"context"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder for cover art
	"io"
	"net/http"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/SisyphusOfCorinth/rising-tide/internal/player"
	"github.com/SisyphusOfCorinth/rising-tide/internal/store"
	"github.com/SisyphusOfCorinth/rising-tide/internal/tidal"
)

// This file contains all tea.Cmd factories. Each function captures backend
// references by closure, runs in a goroutine, and returns a typed message.
// None of these functions mutate model state directly.

// checkAuth verifies whether a valid session exists in the store.
func checkAuth(st *store.SecretsStore, client *tidal.Client) tea.Cmd {
	return func() tea.Msg {
		var session tidal.Session
		if err := st.LoadSession(&session); err != nil {
			return AuthCheckCompleteMsg{Authenticated: false, Err: err}
		}
		client.Session = &session
		return AuthCheckCompleteMsg{Authenticated: true}
	}
}

// startLogin runs the interactive OAuth2 device flow.
func startLogin(client *tidal.Client) tea.Cmd {
	return func() tea.Msg {
		session, err := client.AuthenticateInteractive(context.Background())
		return LoginCompleteMsg{Session: session, Err: err}
	}
}

// searchTidal calls the Tidal search API.
func searchTidal(client *tidal.Client, query string) tea.Cmd {
	return func() tea.Msg {
		tracks, albums, artists, err := client.Search(context.Background(), query)
		return SearchResultsMsg{
			Query:   query,
			Tracks:  tracks,
			Albums:  albums,
			Artists: artists,
			Err:     err,
		}
	}
}

// fetchArtistAlbums retrieves all albums for an artist.
func fetchArtistAlbums(client *tidal.Client, artistID int, artistName string) tea.Cmd {
	return func() tea.Msg {
		albums, err := client.GetArtistAlbums(context.Background(), formatInt(artistID))
		return ArtistAlbumsMsg{
			ArtistID:   artistID,
			ArtistName: artistName,
			Albums:     albums,
			Err:        err,
		}
	}
}

// fetchAlbumTracks retrieves all tracks in an album.
func fetchAlbumTracks(client *tidal.Client, albumID int, albumTitle string) tea.Cmd {
	return func() tea.Msg {
		tracks, err := client.GetAlbumTracks(context.Background(), formatInt(albumID))
		return AlbumTracksMsg{
			AlbumID:    albumID,
			AlbumTitle: albumTitle,
			Tracks:     tracks,
			Err:        err,
		}
	}
}

// resolveAndPlay resolves the Tidal manifest for a track and opens the first
// byte of its audio stream. Doing the open synchronously (rather than inside
// the player's goroutine) gives the UI a chance to show a concrete error
// for resolution failures instead of silently cascading-skip when a track
// turns out to be unavailable.
//
// On success, it hands back an opener closure that serves the already-open
// body on its first call and re-resolves via the Tidal client on any
// subsequent call; the player needs that re-open capability to implement
// seek (the audio stream itself isn't random-access, so seek is
// restart-from-zero + skip-forward-to-target).
func resolveAndPlay(client *tidal.Client, track tidal.Track) tea.Cmd {
	return func() tea.Msg {
		body, codec, err := client.OpenStream(context.Background(), track.ID)
		if err != nil {
			return StreamReadyMsg{Track: track, Err: err}
		}
		return StreamReadyMsg{
			Track:  track,
			Codec:  codec,
			Opener: cachedOpener(client, track.ID, body),
		}
	}
}

// cachedOpener returns a StreamOpener that yields the supplied already-open
// body exactly once, then delegates to Tidal for any further opens. It is
// safe for concurrent invocation -- the handoff happens under a mutex --
// though in practice the player calls it sequentially. The codec reported
// by subsequent re-opens is discarded; the caller has already recorded it
// from the initial resolution.
func cachedOpener(client *tidal.Client, trackID int, first io.ReadCloser) player.StreamOpener {
	var mu sync.Mutex
	return func(ctx context.Context) (io.ReadCloser, error) {
		mu.Lock()
		cached := first
		first = nil
		mu.Unlock()
		if cached != nil {
			return cached, nil
		}
		body, _, err := client.OpenStream(ctx, trackID)
		return body, err
	}
}

// startPlayback hands the opener to the player and reports the outcome.
func startPlayback(p *player.Player, track tidal.Track, codec string, opener player.StreamOpener) tea.Cmd {
	return func() tea.Msg {
		_, err := p.Play(opener)
		if err != nil {
			return PlaybackErrorMsg{Err: err}
		}
		return PlaybackStartedMsg{Track: track, Codec: codec}
	}
}

// tickPlaybackProgress returns a command that fires a TickMsg after 1 second.
func tickPlaybackProgress() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

// waitForPlaybackDone blocks until the player's done channel closes.
// The generation parameter is passed through so the handler can ignore
// stale finish signals from tracks that were stopped by a skip command.
func waitForPlaybackDone(p *player.Player, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ch := p.Done()
		if ch == nil {
			return PlaybackFinishedMsg{Generation: generation}
		}
		<-ch
		return PlaybackFinishedMsg{Generation: generation}
	}
}

// listDevices returns available ALSA playback devices.
func listDevices() tea.Cmd {
	return func() tea.Msg {
		devices, err := player.ListDevices()
		return DeviceListMsg{Devices: devices, Err: err}
	}
}

// fetchCoverArt downloads a cover image from Tidal's CDN and encodes it as
// Kitty terminal graphics escape sequences. Cover art URLs are public (no
// auth needed). The image is scaled and sliced into horizontal strips, one
// per terminal row.
func fetchCoverArt(coverUUID string, cols, rows int, imageID uint32) tea.Cmd {
	return func() tea.Msg {
		if coverUUID == "" {
			return CoverArtMsg{}
		}

		coverURL := tidal.CoverURL(coverUUID, "640x640")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(coverURL)
		if err != nil {
			return CoverArtMsg{CoverURL: coverURL, Err: err}
		}
		defer resp.Body.Close()

		img, _, err := image.Decode(resp.Body)
		if err != nil {
			return CoverArtMsg{CoverURL: coverURL, Err: err}
		}

		kittyRows := RenderKittyRows(img, cols, rows, imageID)

		return CoverArtMsg{
			CoverURL: coverURL,
			Rows:     kittyRows,
			Img:      img,
			ImageID:  imageID,
		}
	}
}

// rerenderCoverArt re-encodes a cached image at new dimensions (e.g. after
// terminal resize) without re-fetching from the network.
func rerenderCoverArt(img image.Image, coverURL string, cols, rows int, imageID uint32) tea.Cmd {
	return func() tea.Msg {
		kittyRows := RenderKittyRows(img, cols, rows, imageID)
		return CoverArtMsg{
			CoverURL: coverURL,
			Rows:     kittyRows,
			Img:      img,
			ImageID:  imageID,
		}
	}
}

// fetchFavorites retrieves the user's favorited tracks.
func fetchFavorites(client *tidal.Client) tea.Cmd {
	return func() tea.Msg {
		tracks, err := client.GetFavorites(context.Background(), 100)
		return FavoritesMsg{Tracks: tracks, Err: err}
	}
}

// fetchMixList retrieves the user's daily mixes.
func fetchMixList(client *tidal.Client) tea.Cmd {
	return func() tea.Msg {
		mixes, err := client.GetMixes(context.Background())
		return MixListMsg{Mixes: mixes, Err: err}
	}
}

// fetchMixTracks retrieves all tracks for a specific mix.
func fetchMixTracks(client *tidal.Client, mixID, mixTitle string) tea.Cmd {
	return func() tea.Msg {
		tracks, err := client.GetMixTracks(context.Background(), mixID)
		return MixTracksMsg{MixID: mixID, MixTitle: mixTitle, Tracks: tracks, Err: err}
	}
}

// formatInt converts an int to a string (used for API call parameters).
func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
}
