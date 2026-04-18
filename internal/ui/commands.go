package ui

import (
	"context"
	"fmt"
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

// resolveAndPlay gets the stream URL for a track via the quality ladder.
func resolveAndPlay(client *tidal.Client, track tidal.Track) tea.Cmd {
	return func() tea.Msg {
		url, err := client.GetStreamURL(context.Background(), track.ID)
		if err != nil {
			return StreamURLMsg{Track: track, Err: err}
		}
		return StreamURLMsg{Track: track, URL: url}
	}
}

// startPlayback sends the URL to the player.
func startPlayback(p *player.Player, track tidal.Track, url string) tea.Cmd {
	return func() tea.Msg {
		_, err := p.Play(url)
		if err != nil {
			return PlaybackErrorMsg{Err: err}
		}
		return PlaybackStartedMsg{Track: track}
	}
}

// tickPlaybackProgress returns a command that fires a TickMsg after 1 second.
func tickPlaybackProgress() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

// waitForPlaybackDone blocks until the player's done channel closes.
func waitForPlaybackDone(p *player.Player) tea.Cmd {
	return func() tea.Msg {
		ch := p.Done()
		if ch == nil {
			return PlaybackFinishedMsg{}
		}
		<-ch
		return PlaybackFinishedMsg{}
	}
}

// listDevices returns available ALSA playback devices.
func listDevices() tea.Cmd {
	return func() tea.Msg {
		devices, err := player.ListDevices()
		return DeviceListMsg{Devices: devices, Err: err}
	}
}

// formatInt converts an int to a string (used for API call parameters).
func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
}
