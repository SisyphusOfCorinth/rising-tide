package tidal

import (
	"context"
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

type StreamResponse struct {
	URLs []string `json:"urls"`
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

// GetStreamURL resolves the streaming URL for a track by trying audio qualities
// in descending order: HI_RES_LOSSLESS -> LOSSLESS -> HIGH -> LOW.
// The first quality the API accepts is returned. This handles subscription tier
// differences (e.g. free accounts can only stream LOW/HIGH).
func (c *Client) GetStreamURL(ctx context.Context, trackID int) (string, error) {
	qualities := []string{"HI_RES_LOSSLESS", "LOSSLESS", "HIGH", "LOW"}
	var lastErr error

	for _, q := range qualities {
		endpoint := fmt.Sprintf("/tracks/%d/urlpostpaywall", trackID)
		params := url.Values{}
		params.Set("urlusagemode", "STREAM")
		params.Set("audioquality", q)
		params.Set("assetpresentation", "FULL")
		params.Set("countryCode", c.Session.CountryCode)

		client := c.GetAuthClient(ctx)
		u := BaseURL + endpoint + "?" + params.Encode()
		resp, err := client.Get(u)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == 200 {
			var s StreamResponse
			if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
				return "", err
			}
			if len(s.URLs) == 0 {
				return "", fmt.Errorf("stream response contained no URLs")
			}
			logger.L.Debug("stream quality selected", "quality", q, "trackID", trackID)
			return s.URLs[0], nil
		}

		logger.L.Debug("stream quality rejected", "quality", q, "trackID", trackID, "status", resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		lastErr = apiErr("get stream ("+q+")", resp.StatusCode, body)
	}

	return "", lastErr
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
