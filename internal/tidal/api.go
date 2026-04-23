package tidal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/SisyphusOfCorinth/rising-tide/internal/logger"
)

// ErrNotFound is returned by GetTrack when the API responds with 404.
var ErrNotFound = errors.New("not found")

// CoverURL returns the HTTPS URL for the album cover image at the given size.
// cover is the UUID string returned in Track.Album.Cover.
// Typical sizes: "80x80", "160x160", "320x320", "640x640", "1280x1280".
// Returns "" when cover is empty.
func CoverURL(cover, size string) string {
	if cover == "" {
		return ""
	}
	// Tidal stores the cover UUID with dashes; the image CDN path uses
	// forward slashes between groups.
	dashed := strings.ReplaceAll(cover, "-", "/")
	return "https://resources.tidal.com/images/" + dashed + "/" + size + ".jpg"
}

// apiErr returns a formatted error from a non-2xx response. It tries to extract
// the human-readable "userMessage" field from the Tidal JSON error body; if
// that is not present it falls back to the raw body text.
func apiErr(op string, status int, body []byte) error {
	var e struct {
		UserMessage string `json:"userMessage"`
	}
	if json.Unmarshal(body, &e) == nil && e.UserMessage != "" {
		return fmt.Errorf("%s: %s", op, e.UserMessage)
	}
	return fmt.Errorf("%s (status %d): %s", op, status, strings.TrimSpace(string(body)))
}

// --- Data Types ---
// These are the core types returned by the Tidal v1 API. The JSON tags match
// the API response format exactly.

type Track struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Artist struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		ID    int    `json:"id"`
		Title string `json:"title"`
		Cover string `json:"cover"` // UUID, e.g. "a3f1d2e4-1234-5678-abcd-ef0123456789"
	} `json:"album"`
	Duration int    `json:"duration"`
	URL      string `json:"url"`
}

type Album struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Cover  string `json:"cover"`
	Type   string `json:"type"` // ALBUM, EP, SINGLE, COMPILATION
	Artist struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
	Duration       int `json:"duration"`
	NumberOfTracks int `json:"numberOfTracks"`
}

type Artist struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

type Mix struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	SubTitle    string `json:"subTitle"`
	Description string `json:"description"`
}

// --- Response Wrappers ---

type SearchResponse struct {
	Artists struct {
		Items []Artist `json:"items"`
	} `json:"artists"`
	Albums struct {
		Items []Album `json:"items"`
	} `json:"albums"`
	Tracks struct {
		Items []Track `json:"items"`
	} `json:"tracks"`
}

// playbackInfoResponse is the envelope returned by
// /tracks/{id}/playbackinfopostpaywall. The manifest itself is base64-encoded
// and its format depends on manifestMimeType ("application/vnd.tidal.bt" =
// JSON with codec + urls; "application/dash+xml" = a DASH MPD).
type playbackInfoResponse struct {
	TrackID          int    `json:"trackId"`
	AudioQuality     string `json:"audioQuality"`
	ManifestMimeType string `json:"manifestMimeType"`
	Manifest         string `json:"manifest"`
}

// btsManifest is the decoded JSON carried inside a "application/vnd.tidal.bt"
// manifest: the playable CDN URLs plus the actual codec Tidal is delivering
// (needed because the quality tier requested doesn't guarantee FLAC -- Tidal
// silently downgrades unsupported tiers to AAC).
type btsManifest struct {
	MimeType       string   `json:"mimeType"`
	Codecs         string   `json:"codecs"`
	EncryptionType string   `json:"encryptionType"`
	KeyID          string   `json:"keyId"`
	URLs           []string `json:"urls"`
}

type UserResponse struct {
	ID           int    `json:"id"`
	CountryCode  string `json:"countryCode"`
	Email        string `json:"email"`
	FullName     string `json:"fullName"`
	ProfileImage string `json:"picture"`
}

type FavoritesResponse struct {
	Items []struct {
		Item Track `json:"item"`
	} `json:"items"`
}

type radioResponse struct {
	Items []Track `json:"items"`
}

type albumTracksResponse struct {
	Items []Track `json:"items"`
}

// v2 JSON:API types for mixes and playlist items.

type v2ResourceIdentifier struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type v2PlaylistAttributes struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type v2jsonAPIResponse struct {
	Data     []v2ResourceIdentifier `json:"data"`
	Included []json.RawMessage      `json:"included"`
}

// --- API Methods ---

func (c *Client) GetUser(ctx context.Context) (*UserResponse, error) {
	client := c.GetAuthClient(ctx)
	resp, err := client.Get(fmt.Sprintf("%s/users/%d", BaseURL, c.Session.UserID))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get user", resp.StatusCode, body)
	}

	var u UserResponse
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (c *Client) GetTrack(ctx context.Context, trackID string) (*Track, error) {
	params := url.Values{}
	params.Set("countryCode", c.Session.CountryCode)
	client := c.GetAuthClient(ctx)
	resp, err := client.Get(BaseURL + "/tracks/" + trackID + "?" + params.Encode())
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get track", resp.StatusCode, body)
	}

	var t Track
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *Client) Search(ctx context.Context, query string) ([]Track, []Album, []Artist, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("limit", "20")
	params.Set("countryCode", c.Session.CountryCode)
	params.Set("types", "TRACKS,ALBUMS,ARTISTS")

	client := c.GetAuthClient(ctx)
	u := BaseURL + "/search?" + params.Encode()
	resp, err := client.Get(u)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, nil, nil, apiErr("search", resp.StatusCode, body)
	}

	var res SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, nil, nil, err
	}
	return res.Tracks.Items, res.Albums.Items, res.Artists.Items, nil
}

// OpenStream resolves a track's playback manifest and returns a reader over
// its audio bytestream plus the codec string Tidal is delivering ("flac",
// "mp4a.40.2", etc.). It walks the quality ladder (HI_RES_LOSSLESS ->
// LOSSLESS -> HIGH -> LOW). The returned reader hides the two possible
// Tidal delivery shapes:
//
//   - "application/vnd.tidal.bt" manifests: a single CDN URL. For FLAC
//     content the reader is the raw HTTP body; for AAC content ffmpeg is
//     spawned to transcode to FLAC so the downstream decode pipeline
//     remains codec-agnostic.
//   - "application/dash+xml" manifests: an MPEG-DASH MPD describing an
//     init segment plus N media segments. The reader is a chain reader
//     that fetches each segment in order and concatenates their bodies,
//     so the MP4 demuxer sees one continuous fMP4 stream.
//
// The codec string is the raw value from the manifest (not mutated by
// transcoding) -- callers that want to show the user what Tidal is
// actually serving should use it as-is.
//
// The endpoint used is /playbackinfopostpaywall -- the legacy
// /urlpostpaywall endpoint is no longer trustworthy because Tidal routes
// its responses to AAC CDN assets even when LOSSLESS is requested.
func (c *Client) OpenStream(ctx context.Context, trackID int) (io.ReadCloser, string, error) {
	qualities := []string{"HI_RES_LOSSLESS", "LOSSLESS", "HIGH", "LOW"}
	var lastErr error

	for _, q := range qualities {
		rc, codec, err := c.openStreamAtQuality(ctx, trackID, q)
		if err != nil {
			logger.L.Debug("stream quality rejected", "quality", q, "trackID", trackID, "err", err)
			lastErr = err
			continue
		}
		return rc, codec, nil
	}
	return nil, "", lastErr
}

// openStreamAtQuality fetches and resolves the playback manifest for a
// single quality tier. It returns an io.ReadCloser positioned at the start
// of the audio stream, the codec string, or an error (which causes
// OpenStream to advance to the next tier).
func (c *Client) openStreamAtQuality(ctx context.Context, trackID int, quality string) (io.ReadCloser, string, error) {
	info, err := c.fetchPlaybackInfo(ctx, trackID, quality)
	if err != nil {
		return nil, "", err
	}

	manifestBytes, err := base64.StdEncoding.DecodeString(info.Manifest)
	if err != nil {
		return nil, "", fmt.Errorf("decode manifest base64: %w", err)
	}

	switch info.ManifestMimeType {
	case "application/vnd.tidal.bt", "application/vnd.tidal.bts":
		return c.openBTStream(ctx, manifestBytes, quality, trackID)
	case "application/dash+xml":
		return c.openDashStream(ctx, manifestBytes, quality, trackID)
	default:
		return nil, "", fmt.Errorf("unsupported manifest type %q", info.ManifestMimeType)
	}
}

// fetchPlaybackInfo calls /tracks/{id}/playbackinfopostpaywall for one
// quality tier and returns the parsed envelope. Status codes other than 200
// (typically 401 for unauthorised tiers) surface as errors so the caller
// can try the next tier.
func (c *Client) fetchPlaybackInfo(ctx context.Context, trackID int, quality string) (*playbackInfoResponse, error) {
	endpoint := fmt.Sprintf("/tracks/%d/playbackinfopostpaywall", trackID)
	params := url.Values{}
	params.Set("audioquality", quality)
	params.Set("playbackmode", "STREAM")
	params.Set("assetpresentation", "FULL")
	params.Set("countryCode", c.Session.CountryCode)

	client := c.GetAuthClient(ctx)
	u := BaseURL + endpoint + "?" + params.Encode()
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get playbackinfo ("+quality+")", resp.StatusCode, body)
	}

	var info playbackInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode playbackinfo: %w", err)
	}
	return &info, nil
}

// openBTStream handles the single-URL "vnd.tidal.bt" manifest. FLAC content
// is streamed straight through; non-FLAC content (typically 320 kbps
// AAC-LC, for tracks Tidal never mastered as lossless) is piped through
// ffmpeg which demuxes the MP4, decodes AAC, and re-emits raw FLAC so the
// player's FLAC pipeline still does the final decode and ALSA write. This
// keeps every downstream component -- demuxer, decoder, mixer, ALSA
// format selection -- on a single FLAC-only code path.
func (c *Client) openBTStream(ctx context.Context, manifestBytes []byte, quality string, trackID int) (io.ReadCloser, string, error) {
	var m btsManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, "", fmt.Errorf("decode bt manifest: %w", err)
	}
	if len(m.URLs) == 0 {
		return nil, "", fmt.Errorf("bt manifest contained no URLs")
	}
	if m.EncryptionType != "" && m.EncryptionType != "NONE" {
		return nil, "", fmt.Errorf("stream is encrypted (%s); not supported", m.EncryptionType)
	}
	if isLosslessCodec(m.Codecs) {
		logger.L.Debug("stream quality selected",
			"quality", quality, "trackID", trackID, "codec", m.Codecs, "manifest", "bt")
		body, err := httpGetBody(ctx, m.URLs[0])
		return body, m.Codecs, err
	}
	logger.L.Debug("stream quality selected (transcoded)",
		"quality", quality, "trackID", trackID, "codec", m.Codecs, "manifest", "bt")
	body, err := openAACTranscode(ctx, m.URLs[0])
	return body, m.Codecs, err
}

// openDashStream handles a DASH+XML manifest by parsing it, building the
// ordered list of (init + media) segment URLs, and returning a chain reader
// that transparently fetches and concatenates segment bodies on read.
func (c *Client) openDashStream(ctx context.Context, manifestBytes []byte, quality string, trackID int) (io.ReadCloser, string, error) {
	codec, initURL, mediaURLs, err := parseDashMPD(manifestBytes)
	if err != nil {
		return nil, "", fmt.Errorf("parse dash manifest: %w", err)
	}
	if err := ensureAcceptableCodec(quality, codec); err != nil {
		logger.L.Debug("stream quality downgraded by server, skipping",
			"quality", quality, "trackID", trackID, "codec", codec)
		return nil, "", err
	}
	logger.L.Debug("stream quality selected",
		"quality", quality, "trackID", trackID, "codec", codec, "manifest", "dash",
		"segments", len(mediaURLs))
	segURLs := append([]string{initURL}, mediaURLs...)
	return newSegmentChainReader(ctx, segURLs), codec, nil
}

// ensureAcceptableCodec refuses lossless quality tiers when the server has
// quietly dropped back to a lossy codec. For HIGH/LOW tiers any codec is
// acceptable because those tiers are expected to be lossy.
func ensureAcceptableCodec(quality, codec string) error {
	if quality != "LOSSLESS" && quality != "HI_RES_LOSSLESS" {
		return nil
	}
	if isLosslessCodec(codec) {
		return nil
	}
	return fmt.Errorf("server delivered codec %q for quality %s (not lossless)", codec, quality)
}

// httpGetBody issues a GET and returns the response body. Non-200 responses
// are treated as errors so they don't silently propagate as unparseable
// audio bytes further down the pipeline.
func httpGetBody(ctx context.Context, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: http %d", u, resp.StatusCode)
	}
	return resp.Body, nil
}

// isLosslessCodec reports whether the codec string returned by Tidal's
// playbackinfo manifest is a lossless codec. Tidal uses "flac", "mqa", or
// "flac_hires"-style strings; everything else (aac, mp4a.40.2, mp3, etc.)
// is lossy and should not be accepted when the user requested lossless.
func isLosslessCodec(codec string) bool {
	c := strings.ToLower(codec)
	if strings.Contains(c, "flac") {
		return true
	}
	if strings.Contains(c, "alac") {
		return true
	}
	// MQA is technically lossy-encoded in a lossless container but Tidal
	// historically treats it as part of the LOSSLESS/HI_RES tiers. Accept
	// it; the player will still decode the FLAC container bit-perfectly.
	if strings.Contains(c, "mqa") {
		return true
	}
	return false
}

func (c *Client) GetFavorites(ctx context.Context, limit int) ([]Track, error) {
	endpoint := fmt.Sprintf("/users/%d/favorites/tracks", c.Session.UserID)
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	params.Set("countryCode", c.Session.CountryCode)
	params.Set("order", "DATE")
	params.Set("orderDirection", "DESC")

	client := c.GetAuthClient(ctx)
	u := BaseURL + endpoint + "?" + params.Encode()
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get favorites", resp.StatusCode, body)
	}

	var res FavoritesResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	tracks := make([]Track, len(res.Items))
	for i, item := range res.Items {
		tracks[i] = item.Item
	}
	return tracks, nil
}

func (c *Client) GetTrackRadio(ctx context.Context, trackID int) ([]Track, error) {
	params := url.Values{}
	params.Set("limit", "100")
	params.Set("countryCode", c.Session.CountryCode)

	client := c.GetAuthClient(ctx)
	u := fmt.Sprintf("%s/tracks/%d/radio?%s", BaseURL, trackID, params.Encode())
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get track radio", resp.StatusCode, body)
	}

	var res radioResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

func (c *Client) GetAlbumTracks(ctx context.Context, albumID string) ([]Track, error) {
	params := url.Values{}
	params.Set("countryCode", c.Session.CountryCode)

	client := c.GetAuthClient(ctx)
	u := fmt.Sprintf("%s/albums/%s/tracks?%s", BaseURL, albumID, params.Encode())
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get album tracks", resp.StatusCode, body)
	}

	var res albumTracksResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

type artistAlbumsResponse struct {
	Items []Album `json:"items"`
}

// GetArtistAlbums returns all albums, EPs, and singles for an artist.
// It makes two API calls (one for albums, one for EPs+singles) and merges
// the results so the UI shows the full discography.
func (c *Client) GetArtistAlbums(ctx context.Context, artistID string) ([]Album, error) {
	client := c.GetAuthClient(ctx)

	fetch := func(filter string) ([]Album, error) {
		params := url.Values{}
		params.Set("countryCode", c.Session.CountryCode)
		params.Set("limit", "50")
		if filter != "" {
			params.Set("filter", filter)
		}
		u := fmt.Sprintf("%s/artists/%s/albums?%s", BaseURL, artistID, params.Encode())
		resp, err := client.Get(u)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return nil, apiErr("get artist albums", resp.StatusCode, body)
		}
		var res artistAlbumsResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return nil, err
		}
		return res.Items, nil
	}

	albums, err := fetch("ALBUMS")
	if err != nil {
		return nil, err
	}
	eps, err := fetch("EPSANDSINGLES")
	if err != nil {
		// Non-fatal: return albums without EPs if the second call fails.
		return albums, nil
	}
	return append(albums, eps...), nil
}

func (c *Client) AddFavorite(ctx context.Context, trackID int) error {
	endpoint := fmt.Sprintf("/users/%d/favorites/tracks", c.Session.UserID)
	query := url.Values{}
	query.Set("countryCode", c.Session.CountryCode)

	body := url.Values{}
	body.Set("trackId", strconv.Itoa(trackID))

	client := c.GetAuthClient(ctx)
	resp, err := client.Post(
		BaseURL+endpoint+"?"+query.Encode(),
		"application/x-www-form-urlencoded",
		strings.NewReader(body.Encode()),
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return apiErr("add favorite", resp.StatusCode, b)
	}
	return nil
}

func (c *Client) RemoveFavorite(ctx context.Context, trackID int) error {
	endpoint := fmt.Sprintf("/users/%d/favorites/tracks/%d", c.Session.UserID, trackID)
	params := url.Values{}
	params.Set("countryCode", c.Session.CountryCode)

	client := c.GetAuthClient(ctx)
	req, err := newDeleteRequest(BaseURL + endpoint + "?" + params.Encode())
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		body, _ := io.ReadAll(resp.Body)
		return apiErr("remove favorite", resp.StatusCode, body)
	}
	return nil
}

func newDeleteRequest(u string) (*http.Request, error) {
	return http.NewRequest(http.MethodDelete, u, nil)
}

func (c *Client) GetMixes(ctx context.Context) ([]Mix, error) {
	params := url.Values{}
	params.Set("countryCode", c.Session.CountryCode)
	params.Set("include", "myMixes")

	client := c.GetAuthClient(ctx)
	u := BaseURLV2 + "/userRecommendations/me/relationships/myMixes?" + params.Encode()
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get mixes", resp.StatusCode, body)
	}

	var res v2jsonAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	// Build a lookup of playlist attributes from included resources.
	playlistAttrs := make(map[string]v2PlaylistAttributes)
	for _, raw := range res.Included {
		var obj struct {
			ID         string               `json:"id"`
			Type       string               `json:"type"`
			Attributes v2PlaylistAttributes `json:"attributes"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		if obj.Type == "playlists" {
			playlistAttrs[obj.ID] = obj.Attributes
		}
	}

	mixes := make([]Mix, 0, len(res.Data))
	for _, ref := range res.Data {
		mix := Mix{ID: ref.ID}
		if attrs, ok := playlistAttrs[ref.ID]; ok {
			mix.Title = attrs.Name
			mix.SubTitle = attrs.Description
		} else {
			mix.Title = ref.ID
		}
		mixes = append(mixes, mix)
	}
	return mixes, nil
}

// GetMixTracks fetches the full track list for a mix/playlist. The v2 API only
// returns track IDs, so we concurrently fetch full metadata via individual v1
// /tracks/{id} calls, then sort by original playlist order.
func (c *Client) GetMixTracks(ctx context.Context, mixID string) ([]Track, error) {
	params := url.Values{}
	params.Set("countryCode", c.Session.CountryCode)
	params.Set("include", "items")

	client := c.GetAuthClient(ctx)
	u := BaseURLV2 + "/playlists/" + mixID + "/relationships/items?" + params.Encode()
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, apiErr("get mix tracks", resp.StatusCode, body)
	}

	var res v2jsonAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	// Collect track IDs in order, skipping non-track refs (e.g. videos).
	ids := make([]string, 0, len(res.Data))
	for _, ref := range res.Data {
		if ref.Type == "tracks" {
			ids = append(ids, ref.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Fetch full track details concurrently, one request per track.
	type result struct {
		idx   int
		track Track
		err   error
	}
	ch := make(chan result, len(ids))
	for i, id := range ids {
		go func(idx int, trackID string) {
			t, err := c.GetTrack(ctx, trackID)
			if err != nil {
				ch <- result{idx: idx, err: err}
				return
			}
			ch <- result{idx: idx, track: *t}
		}(i, id)
	}

	type indexedTrack struct {
		idx   int
		track Track
	}
	var available []indexedTrack
	for range ids {
		r := <-ch
		if errors.Is(r.err, ErrNotFound) {
			continue // Skip unavailable tracks (removed, region-blocked)
		}
		if r.err != nil {
			return nil, fmt.Errorf("failed to get mix track details: %w", r.err)
		}
		available = append(available, indexedTrack{r.idx, r.track})
	}
	// Sort by original playlist position to preserve the intended order.
	slices.SortFunc(available, func(a, b indexedTrack) int { return a.idx - b.idx })
	ordered := make([]Track, len(available))
	for i, it := range available {
		ordered[i] = it.track
	}
	return ordered, nil
}
