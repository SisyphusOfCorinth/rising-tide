package tidal_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/SisyphusOfCorinth/rising-tide/internal/tidal"
)

// roundTripFunc is a convenience type that lets a plain function satisfy
// http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTestClient returns a *tidal.Client pre-loaded with a dummy session and a
// transport that delegates to the supplied httptest.Server.  The token is set
// far in the future so the oauth2 layer never attempts a refresh.
func newTestClient(srv *httptest.Server) *tidal.Client {
	c := tidal.NewClient()
	c.Session = &tidal.Session{
		AccessToken: "test-token",
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(time.Hour),
		UserID:      42,
		CountryCode: "US",
	}
	// Rewrite every request to point at the test server instead of the real
	// Tidal API endpoints.
	c.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		req.URL.Scheme = "http"
		return srv.Client().Transport.RoundTrip(req)
	})
	// Give the oauth2 config a token endpoint on the test server so that any
	// token refresh attempt would also stay local (won't be triggered in
	// practice because the expiry is in the future).
	c.Oauth.Endpoint = oauth2.Endpoint{
		TokenURL: srv.URL + "/token",
	}
	return c
}

// respond writes JSON to the ResponseRecorder / hijacked response.
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- GetUser ---

func TestGetUser_OK(t *testing.T) {
	want := tidal.UserResponse{
		ID:          42,
		CountryCode: "US",
		Email:       "user@example.com",
		FullName:    "Test User",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/users/42") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		respond(w, 200, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).GetUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Email != want.Email || got.FullName != want.FullName {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestGetUser_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, 401, map[string]string{"error": "unauthorized"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).GetUser(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

// --- GetTrack ---

func TestGetTrack_OK(t *testing.T) {
	want := tidal.Track{ID: 123, Title: "Song Title"}
	want.Artist.Name = "Artist Name"
	want.Album.Title = "Album Title"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/tracks/123") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		respond(w, 200, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).GetTrack(context.Background(), "123")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Title != want.Title || got.Artist.Name != want.Artist.Name {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestGetTrack_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, 404, map[string]string{"error": "not found"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).GetTrack(context.Background(), "999")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

// --- Search ---

func TestSearch_OK(t *testing.T) {
	payload := tidal.SearchResponse{}
	payload.Tracks.Items = []tidal.Track{
		{ID: 1, Title: "Alpha"},
		{ID: 2, Title: "Beta"},
	}
	payload.Albums.Items = []tidal.Album{
		{ID: 100, Title: "Test Album", NumberOfTracks: 10},
	}
	payload.Artists.Items = []tidal.Artist{
		{ID: 200, Name: "Test Artist"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/search") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if q := r.URL.Query().Get("query"); q != "test query" {
			t.Errorf("unexpected query param: %q", q)
		}
		if r.URL.Query().Get("types") != "TRACKS,ALBUMS,ARTISTS" {
			t.Errorf("types param missing or wrong: %q", r.URL.Query().Get("types"))
		}
		respond(w, 200, payload)
	}))
	defer srv.Close()

	tracks, albums, artists, err := newTestClient(srv).Search(context.Background(), "test query")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}
	if tracks[0].Title != "Alpha" || tracks[1].Title != "Beta" {
		t.Errorf("unexpected tracks: %+v", tracks)
	}
	if len(albums) != 1 {
		t.Fatalf("expected 1 album, got %d", len(albums))
	}
	if albums[0].Title != "Test Album" {
		t.Errorf("unexpected album: %+v", albums[0])
	}
	if len(artists) != 1 {
		t.Fatalf("expected 1 artist, got %d", len(artists))
	}
	if artists[0].Name != "Test Artist" {
		t.Errorf("unexpected artist: %+v", artists[0])
	}
}

func TestSearch_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, 200, tidal.SearchResponse{})
	}))
	defer srv.Close()

	tracks, albums, artists, err := newTestClient(srv).Search(context.Background(), "nothing")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 0 {
		t.Errorf("expected empty tracks slice, got %d tracks", len(tracks))
	}
	if len(albums) != 0 {
		t.Errorf("expected empty albums slice, got %d albums", len(albums))
	}
	if len(artists) != 0 {
		t.Errorf("expected empty artists slice, got %d artists", len(artists))
	}
}

// --- OpenStream ---
//
// These tests model the /playbackinfopostpaywall endpoint: the server returns
// a JSON envelope with a base64-encoded manifest, and the manifest carries an
// explicit codec plus the playable URL(s). OpenStream walks the quality
// ladder (HI_RES_LOSSLESS -> LOSSLESS -> HIGH -> LOW), rejecting tiers where
// the server silently returned a lossy codec for a lossless request, and
// returns an io.ReadCloser over the chosen stream's body.
//
// The tests distinguish which tier won by giving each tier's stream path a
// unique body and reading the returned body.

// playbackInfoBody builds a fake /playbackinfopostpaywall JSON response with
// a base64-encoded vnd.tidal.bt manifest carrying the given codec and URL.
func playbackInfoBody(codec, streamURL string) map[string]any {
	manifest := map[string]any{
		"mimeType":       "audio/mp4",
		"codecs":         codec,
		"encryptionType": "NONE",
		"urls":           []string{streamURL},
	}
	raw, _ := json.Marshal(manifest)
	return map[string]any{
		"trackId":          123,
		"audioQuality":     "LOSSLESS",
		"manifestMimeType": "application/vnd.tidal.bt",
		"manifest":         base64.StdEncoding.EncodeToString(raw),
	}
}

// readStreamString opens a stream, reads the entire body to a string, and
// closes. Useful for asserting which CDN URL the client followed.
func readStreamString(t *testing.T, c *tidal.Client) string {
	t.Helper()
	rc, err := c.OpenStream(context.Background(), 123)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestOpenStream_FirstQualitySucceeds(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/playbackinfopostpaywall") {
			if r.URL.Query().Get("audioquality") == "HI_RES_LOSSLESS" {
				respond(w, 200, playbackInfoBody("flac", srv.URL+"/stream/hires"))
				return
			}
			respond(w, 404, map[string]string{"error": "not found"})
			return
		}
		if r.URL.Path == "/stream/hires" {
			_, _ = w.Write([]byte("hires-flac-bytes"))
			return
		}
	}))
	defer srv.Close()

	if got := readStreamString(t, newTestClient(srv)); got != "hires-flac-bytes" {
		t.Errorf("unexpected stream body: %q", got)
	}
}

func TestOpenStream_FallsBackThroughQualities(t *testing.T) {
	var seen []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/playbackinfopostpaywall") {
			q := r.URL.Query().Get("audioquality")
			seen = append(seen, q)
			if q == "LOSSLESS" {
				respond(w, 200, playbackInfoBody("flac", srv.URL+"/stream/lossless"))
				return
			}
			respond(w, 404, map[string]string{"error": "not found"})
			return
		}
		if r.URL.Path == "/stream/lossless" {
			_, _ = w.Write([]byte("lossless-flac-bytes"))
			return
		}
	}))
	defer srv.Close()

	if got := readStreamString(t, newTestClient(srv)); got != "lossless-flac-bytes" {
		t.Errorf("unexpected stream body: %q", got)
	}
	if len(seen) < 2 || seen[0] != "HI_RES_LOSSLESS" || seen[1] != "LOSSLESS" {
		t.Errorf("unexpected quality ladder: %v", seen)
	}
}

func TestOpenStream_AllQualitiesFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, 404, map[string]string{"error": "not found"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).OpenStream(context.Background(), 123)
	if err == nil {
		t.Fatal("expected error when all qualities fail")
	}
}

// TestOpenStream_RejectsSilentAACDowngrade exercises the behaviour that broke
// playback against real Tidal servers: when a LOSSLESS request is answered
// 200 OK with an AAC manifest, the client must treat that as a rejection
// and try the next tier rather than play back degraded audio.
func TestOpenStream_RejectsSilentAACDowngrade(t *testing.T) {
	var seen []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/playbackinfopostpaywall") {
			q := r.URL.Query().Get("audioquality")
			seen = append(seen, q)
			switch q {
			case "HI_RES_LOSSLESS", "LOSSLESS":
				// Simulate Tidal's downgrade bug: 200 OK, but codec is AAC.
				respond(w, 200, playbackInfoBody("mp4a.40.2", srv.URL+"/stream/aac-downgraded"))
			case "HIGH":
				// HIGH is legitimately AAC, so accept it.
				respond(w, 200, playbackInfoBody("mp4a.40.2", srv.URL+"/stream/high"))
			default:
				respond(w, 404, map[string]string{"error": "not found"})
			}
			return
		}
		if r.URL.Path == "/stream/high" {
			_, _ = w.Write([]byte("high-aac-bytes"))
			return
		}
		// Any hit on /stream/aac-downgraded would indicate a test failure,
		// because the lossless tiers should never have followed the URL.
		if r.URL.Path == "/stream/aac-downgraded" {
			t.Errorf("downgraded AAC stream body was fetched -- ladder should have skipped it")
		}
	}))
	defer srv.Close()

	if got := readStreamString(t, newTestClient(srv)); got != "high-aac-bytes" {
		t.Errorf("expected HIGH-tier body, got %q", got)
	}
	if len(seen) < 3 {
		t.Errorf("expected at least three quality attempts, saw %v", seen)
	}
}

// --- GetFavorites ---

func TestGetFavorites_OK(t *testing.T) {
	payload := tidal.FavoritesResponse{
		Items: []struct {
			Item tidal.Track `json:"item"`
		}{
			{Item: tidal.Track{ID: 10, Title: "Fav One"}},
			{Item: tidal.Track{ID: 20, Title: "Fav Two"}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/favorites/tracks") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("order") != "DATE" {
			t.Errorf("order param missing or wrong")
		}
		respond(w, 200, payload)
	}))
	defer srv.Close()

	tracks, err := newTestClient(srv).GetFavorites(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}
	if tracks[0].ID != 10 || tracks[1].ID != 20 {
		t.Errorf("unexpected tracks: %+v", tracks)
	}
}

// --- GetArtistAlbums ---

func TestGetArtistAlbums_OK(t *testing.T) {
	albumsPayload := struct {
		Items []tidal.Album `json:"items"`
	}{
		Items: []tidal.Album{
			{ID: 10, Title: "First Album", Type: "ALBUM", NumberOfTracks: 12},
			{ID: 20, Title: "Second Album", Type: "ALBUM", NumberOfTracks: 8},
		},
	}
	epsPayload := struct {
		Items []tidal.Album `json:"items"`
	}{
		Items: []tidal.Album{
			{ID: 30, Title: "My EP", Type: "EP", NumberOfTracks: 4},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/artists/42/albums") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		switch r.URL.Query().Get("filter") {
		case "EPSANDSINGLES":
			respond(w, 200, epsPayload)
		default:
			respond(w, 200, albumsPayload)
		}
	}))
	defer srv.Close()

	albums, err := newTestClient(srv).GetArtistAlbums(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 3 {
		t.Fatalf("expected 3 albums, got %d", len(albums))
	}
	if albums[0].Title != "First Album" || albums[2].Title != "My EP" {
		t.Errorf("unexpected albums: %+v", albums)
	}
	if albums[2].Type != "EP" {
		t.Errorf("expected EP type, got %q", albums[2].Type)
	}
}

// --- GetMixes ---

func TestGetMixes_OK(t *testing.T) {
	type playlistObj struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Attributes struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"attributes"`
	}

	pl1 := playlistObj{ID: "mix1", Type: "playlists"}
	pl1.Attributes.Name = "Daily Mix"
	pl1.Attributes.Description = "Your daily picks"

	pl2 := playlistObj{ID: "mix2", Type: "playlists"}
	pl2.Attributes.Name = "Chill Mix"
	pl2.Attributes.Description = "Relaxing vibes"

	inc1, _ := json.Marshal(pl1)
	inc2, _ := json.Marshal(pl2)

	payload := map[string]any{
		"data": []map[string]string{
			{"id": "mix1", "type": "playlists"},
			{"id": "mix2", "type": "playlists"},
		},
		"included": []json.RawMessage{inc1, inc2},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/userRecommendations/me/relationships/myMixes") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		respond(w, 200, payload)
	}))
	defer srv.Close()

	mixes, err := newTestClient(srv).GetMixes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(mixes) != 2 {
		t.Fatalf("expected 2 mixes, got %d", len(mixes))
	}
	if mixes[0].Title != "Daily Mix" || mixes[1].Title != "Chill Mix" {
		t.Errorf("unexpected mix titles: %+v", mixes)
	}
	if mixes[0].SubTitle != "Your daily picks" {
		t.Errorf("unexpected subtitle: %q", mixes[0].SubTitle)
	}
}

func TestGetMixes_MissingIncluded(t *testing.T) {
	payload := map[string]any{
		"data": []map[string]string{
			{"id": "orphan-mix", "type": "playlists"},
		},
		"included": []json.RawMessage{},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, 200, payload)
	}))
	defer srv.Close()

	mixes, err := newTestClient(srv).GetMixes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(mixes) != 1 {
		t.Fatalf("expected 1 mix, got %d", len(mixes))
	}
	if mixes[0].Title != "orphan-mix" {
		t.Errorf("expected fallback to ID, got %q", mixes[0].Title)
	}
}

// --- GetMixTracks ---

func TestGetMixTracks_OK(t *testing.T) {
	v2Payload := map[string]any{
		"data": []map[string]string{
			{"id": "101", "type": "tracks"},
			{"id": "202", "type": "tracks"},
		},
		"included": []json.RawMessage{},
	}

	track1 := tidal.Track{ID: 101, Title: "Big Song"}
	track1.Artist.Name = "The Band"
	track1.Album.Title = "The Album"

	track2 := tidal.Track{ID: 202, Title: "Small Song"}
	track2.Artist.Name = "Other Artist"
	track2.Album.Title = "Other Album"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/playlists/mix1/relationships/items"):
			respond(w, 200, v2Payload)
		case strings.HasSuffix(r.URL.Path, "/tracks/101"):
			respond(w, 200, track1)
		case strings.HasSuffix(r.URL.Path, "/tracks/202"):
			respond(w, 200, track2)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	tracks, err := newTestClient(srv).GetMixTracks(context.Background(), "mix1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}
	if tracks[0].ID != 101 || tracks[0].Title != "Big Song" {
		t.Errorf("unexpected track[0]: %+v", tracks[0])
	}
	if tracks[0].Artist.Name != "The Band" {
		t.Errorf("unexpected artist: %q", tracks[0].Artist.Name)
	}
	if tracks[0].Album.Title != "The Album" {
		t.Errorf("unexpected album: %q", tracks[0].Album.Title)
	}
	if tracks[1].ID != 202 || tracks[1].Title != "Small Song" {
		t.Errorf("unexpected track[1]: %+v", tracks[1])
	}
}

func TestGetMixTracks_PreservesPlaylistOrder(t *testing.T) {
	v2Payload := map[string]any{
		"data": []map[string]string{
			{"id": "202", "type": "tracks"},
			{"id": "101", "type": "tracks"},
		},
		"included": []json.RawMessage{},
	}

	track1 := tidal.Track{ID: 101, Title: "Alpha"}
	track2 := tidal.Track{ID: 202, Title: "Beta"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/playlists/"):
			respond(w, 200, v2Payload)
		case strings.HasSuffix(r.URL.Path, "/tracks/101"):
			respond(w, 200, track1)
		case strings.HasSuffix(r.URL.Path, "/tracks/202"):
			respond(w, 200, track2)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	tracks, err := newTestClient(srv).GetMixTracks(context.Background(), "mix1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}
	if tracks[0].ID != 202 || tracks[1].ID != 101 {
		t.Errorf("order not preserved: got [%d, %d], want [202, 101]", tracks[0].ID, tracks[1].ID)
	}
}

func TestGetMixTracks_SkipsUnavailableTracks(t *testing.T) {
	v2Payload := map[string]any{
		"data": []map[string]string{
			{"id": "101", "type": "tracks"},
			{"id": "999", "type": "tracks"},
		},
		"included": []json.RawMessage{},
	}

	track1 := tidal.Track{ID: 101, Title: "Available"}
	track1.Artist.Name = "Artist"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/playlists/"):
			respond(w, 200, v2Payload)
		case strings.HasSuffix(r.URL.Path, "/tracks/101"):
			respond(w, 200, track1)
		case strings.HasSuffix(r.URL.Path, "/tracks/999"):
			respond(w, 404, map[string]string{"error": "not found"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	tracks, err := newTestClient(srv).GetMixTracks(context.Background(), "mix1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track (404 skipped), got %d", len(tracks))
	}
	if tracks[0].ID != 101 {
		t.Errorf("unexpected track: %+v", tracks[0])
	}
}

func TestGetMixTracks_SkipsNonTrackRefs(t *testing.T) {
	v2Payload := map[string]any{
		"data": []map[string]string{
			{"id": "v1", "type": "videos"},
		},
		"included": []json.RawMessage{},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/playlists/") {
			respond(w, 200, v2Payload)
			return
		}
		t.Errorf("unexpected request to %s -- should not fetch tracks when IDs list is empty", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	tracks, err := newTestClient(srv).GetMixTracks(context.Background(), "mix1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 0 {
		t.Errorf("expected 0 tracks, got %d", len(tracks))
	}
}
