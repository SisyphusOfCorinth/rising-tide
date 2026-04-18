# Tidalt Core Infrastructure Analysis

This document is a detailed technical analysis of the battle-tested infrastructure from [tidalt](https://github.com/Benehiko/tidalt) (v3) — specifically the parts that work well and should be preserved or closely replicated in a new project: Tidal OAuth2 authentication, API client, FLAC streaming to DAC, session/secrets management, MPRIS integration, and desktop integration. The UI layer is intentionally excluded.

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Tidal OAuth2 Authentication](#tidal-oauth2-authentication)
3. [Tidal API Client](#tidal-api-client)
4. [FLAC Streaming & ALSA Playback Engine](#flac-streaming--alsa-playback-engine)
5. [Session & Secrets Storage](#session--secrets-storage)
6. [MPRIS2 D-Bus Integration](#mpris2-d-bus-integration)
7. [Desktop Integration](#desktop-integration)
8. [Logging](#logging)
9. [Dependencies](#dependencies)
10. [Gotchas & Hard-Won Knowledge](#gotchas--hard-won-knowledge)
11. [Testing Patterns](#testing-patterns)

---

## Architecture Overview

```
cmd/tidalt/main.go          Entry point: arg dispatch, session bootstrap, signal handling
   |
   +-- internal/tidal/       Tidal API client (auth + REST)
   |     client.go           OAuth2 device-flow, token refresh, authenticated HTTP client
   |     api.go              All API calls: search, favorites, streams, mixes, albums, artists
   |     loginprint.go       QR code + big-font user code for device-flow login prompt
   |
   +-- internal/player/      Bit-perfect ALSA playback via CGO
   |     mpv.go              D-Bus device reservation, ALSA hw: open/negotiate, FLAC decode, PCM write loop
   |
   +-- internal/store/       Persistent storage
   |     store.go            Secrets (keychain/age), settings + cache (bbolt)
   |
   +-- internal/mpris/       MPRIS2 D-Bus server + client
   |     server.go           Media key handling, multi-instance coordination, playerctl support
   |
   +-- internal/logger/      Debug logging with URL redaction
        logger.go
```

The key architectural insight: **the player streams FLAC directly from Tidal's CDN HTTP response into an in-memory FLAC decoder, and writes decoded PCM frames directly to an ALSA `hw:` device — no intermediate files, no PulseAudio/PipeWire mixing, no resampling.** This is what makes the audio quality genuinely bit-perfect.

---

## Tidal OAuth2 Authentication

**File:** `internal/tidal/client.go` (203 lines)

### How It Works

Tidal uses OAuth2 Device Authorization Grant (RFC 8628) but with **non-standard camelCase JSON keys** (`deviceCode` instead of `device_code`). This means Go's `oauth2.Config.DeviceAuth()` can't parse the initial response, so the device code request is done manually:

```go
// Step 1: Manual device code request (Tidal uses camelCase JSON)
data := url.Values{}
data.Set("client_id", ClientID)
data.Set("scope", "r_usr w_usr w_sub")
resp, err := httpClient.PostForm(c.Oauth.Endpoint.AuthURL, data)

var da struct {
    DeviceCode      string `json:"deviceCode"`      // NOT device_code
    UserCode        string `json:"userCode"`         // NOT user_code
    VerificationURI string `json:"verificationUri"`  // NOT verification_uri
    Interval        int    `json:"interval"`
}
```

After parsing the non-standard response, it creates a standard `oauth2.DeviceAuthResponse` and uses `oauth2.Config.DeviceAccessToken()` for the polling phase (which works because the token endpoint uses standard keys).

### Session Bootstrap Flow

1. Request device code from `https://auth.tidal.com/v1/oauth2/device_authorization`
2. Display QR code + big-font user code in terminal (auto-opens browser)
3. Poll for token via standard oauth2 library
4. Fetch `/sessions` endpoint to get `UserID` and `CountryCode` (with fallback to token extras)
5. Persist session to secrets store

### Key Constants

```go
ClientID     = "fX2JxdmntZWK0ixT"
ClientSecret = "1Nn9AfDAjxrgJFJbKNWLeAyKGVGmINuXPPLHVXAvxAg="
AuthURL      = "https://auth.tidal.com/v1/oauth2"
BaseURL      = "https://api.tidal.com/v1"
BaseURLV2    = "https://openapi.tidal.com/v2"
```

### Token Management

- `TokenSource()` wraps the session into an `oauth2.TokenSource` that handles automatic refresh
- `GetAuthClient()` returns an `*http.Client` with the oauth2 transport injected
- Supports a `Transport` field override for testing (wraps custom transport in `oauth2.Transport` so the auth header is still added)
- `RevokeToken()` calls the revocation endpoint with basic auth

### Login Display (`loginprint.go`)

- Renders a terminal-sized QR code using `mdp/qrterminal/v3` with half-blocks
- Below it, renders the user code in 7-row-tall ASCII block art (custom glyph map for 0-9, A-Z, `-`, space)
- Measures terminal size and omits QR if it won't fit
- Attempts to auto-open the verification URL in the default browser via `pkg/browser`

---

## Tidal API Client

**File:** `internal/tidal/api.go` (570 lines)

### Data Types

```go
type Track struct {
    ID     int
    Title  string
    Artist struct { ID int; Name string }
    Album  struct { ID int; Title string; Cover string }  // Cover = UUID
    Duration int
    URL      string
}

type Album struct {
    ID, Duration, NumberOfTracks int
    Title, Cover, Type string  // Type: ALBUM, EP, SINGLE, COMPILATION
    Artist struct { ID int; Name string }
}

type Artist struct { ID int; Name string; Picture string }
type Mix    struct { ID, Title, SubTitle, Description string }
```

### Stream URL Resolution (Quality Ladder)

This is one of the most critical functions. It tries stream qualities in descending order and takes the first one the API accepts:

```
HI_RES_LOSSLESS -> LOSSLESS -> HIGH -> LOW
```

Uses the undocumented `/tracks/{id}/urlpostpaywall` endpoint:

```go
params.Set("urlusagemode", "STREAM")
params.Set("audioquality", q)
params.Set("assetpresentation", "FULL")
params.Set("countryCode", c.Session.CountryCode)
```

Returns the first URL from the `StreamResponse.URLs` array. The URL is a direct HTTPS link to a FLAC file on Tidal's CDN.

### API Endpoints Used

| Method | Endpoint | API Version | Notes |
|--------|----------|-------------|-------|
| `GetUser` | `/users/{id}` | v1 | User profile |
| `GetTrack` | `/tracks/{id}` | v1 | Single track metadata |
| `Search` | `/search?types=TRACKS,ALBUMS,ARTISTS` | v1 | Returns tracks, albums, artists |
| `GetStreamURL` | `/tracks/{id}/urlpostpaywall` | v1 | Quality ladder fallback |
| `GetFavorites` | `/users/{id}/favorites/tracks` | v1 | Ordered by date DESC |
| `GetTrackRadio` | `/tracks/{id}/radio` | v1 | 100 similar tracks |
| `GetAlbumTracks` | `/albums/{id}/tracks` | v1 | All tracks in album |
| `GetArtistAlbums` | `/artists/{id}/albums` | v1 | Two calls: ALBUMS + EPSANDSINGLES, merged |
| `AddFavorite` | `POST /users/{id}/favorites/tracks` | v1 | Form-encoded trackId |
| `RemoveFavorite` | `DELETE /users/{id}/favorites/tracks/{trackId}` | v1 | |
| `GetMixes` | `/userRecommendations/me/relationships/myMixes` | **v2** | JSON:API format |
| `GetMixTracks` | `/playlists/{id}/relationships/items` | **v2** | IDs only, then concurrent v1 fetches |

### v2 API (JSON:API) Handling

The v2 endpoints return JSON:API format with `data` (resource identifiers) and `included` (sideloaded full objects). The code builds a lookup map from `included`, then resolves each `data` ref:

```go
type v2jsonAPIResponse struct {
    Data     []v2ResourceIdentifier `json:"data"`
    Included []json.RawMessage      `json:"included"`
}
```

### GetMixTracks Concurrency Pattern

Mix track IDs come from v2, but full metadata requires individual v1 `/tracks/{id}` calls. These are fetched concurrently with order preservation:

1. Fetch track IDs from v2 playlist endpoint
2. Launch one goroutine per track ID, each calling `GetTrack()`
3. Collect results via a channel, skip 404s (unavailable tracks)
4. Sort by original playlist index using `slices.SortFunc`

### Cover Art URLs

```go
func CoverURL(cover, size string) string {
    // cover UUID "a3f1d2e4-..." -> path "a3/f1/d2/e4/.../size.jpg"
    dashed := strings.ReplaceAll(cover, "-", "/")
    return "https://resources.tidal.com/images/" + dashed + "/" + size + ".jpg"
}
// Sizes: "80x80", "160x160", "320x320", "640x640", "1280x1280"
```

### Error Handling Pattern

All API methods follow the same pattern:
1. Build URL with query params (always includes `countryCode`)
2. Use `c.GetAuthClient(ctx)` for authenticated requests
3. Check status code, read body on non-200, call `apiErr()` for formatted error
4. Decode JSON response into typed struct

`apiErr()` tries to extract `userMessage` from Tidal's error JSON, falling back to raw body.

---

## FLAC Streaming & ALSA Playback Engine

**File:** `internal/player/mpv.go` (1071 lines)

This is the most complex and most valuable component. It achieves bit-perfect FLAC playback by:
1. Opening ALSA `hw:` devices directly (bypassing PipeWire/PulseAudio)
2. Negotiating the best PCM format the DAC supports (no soft resampling)
3. Decoding FLAC frames in-flight from the HTTP stream
4. Writing PCM samples directly to the ALSA buffer

### Device Detection & Selection

**Auto-detection** scans `/proc/asound/cards` for known DAC substrings:

```go
var knownDACs = []string{"hidizs", "s9pro", "focusrite", "scarlett"}
```

**Full device listing** (`ListDevices()`) cross-references `/proc/asound/cards` with `/proc/asound/pcm` to find cards with playback capability, extracting card name and long name.

**Manual override** via `SetDevice("hw:N,0")` takes priority over auto-detection.

### D-Bus Device Reservation

Before opening an ALSA `hw:` device, the player acquires the `org.freedesktop.ReserveDevice1.Audio{N}` D-Bus name. This tells PipeWire/PulseAudio to release the device:

```go
func reserveALSADevice(cardNum int) (release func(), err error) {
    // 1. Connect to session bus
    // 2. Call RequestRelease on current owner (WirePlumber)
    // 3. Sleep 200ms for WirePlumber to close its ALSA handle
    // 4. Claim the name with ReplaceExisting | AllowReplacement
    // 5. Return release func that releases name + closes connection
}
```

If D-Bus is unavailable, reservation is skipped (returns no-op release func) — ALSA open is attempted directly.

### ALSA Device Opening (C/CGO)

The C function `open_hw_pcm()` handles format negotiation. **This is the most hardware-specific code and encodes hard-won DAC compatibility knowledge:**

**Format preference for 16-bit sources:**
```
S32_LE -> S16_LE -> S24_3LE -> S24_LE
```
S32_LE is tried first because some USB DACs (e.g., CS43198-based Hidizs S9 Pro Plus) have a **broken S16_LE USB endpoint** but work correctly via their native 32-bit endpoint. The 16-bit samples are left-shifted to fill the MSB.

**Format preference for 24-bit sources:**
```
S24_3LE -> S24_LE -> S32_LE
```

**Buffer configuration is critical:**
- Period size is set to 1024 frames first (~23ms at 44.1kHz)
- Buffer is set to 4x the negotiated period
- Setting buffer first and then querying period_size_min can return absurdly small values on some USB DACs (87 frames on Hidizs S9 Pro Plus "Martha"), causing ~1000 interrupts/s and severe distortion

The function returns:
- Negotiated format, bytes per sample, significant bits (actual DAC bit depth)
- Negotiated sample rate (may differ from requested)
- Period size, buffer size, and software parameters

### Playback Architecture

```
Play(url)
  |
  +-- stop()  (cancel previous, wait for loopDone)
  +-- getDevice() / parseCardNum()
  +-- reserveALSADevice()
  +-- spawn goroutine: playbackLoop()
       |
       +-- openStream(url) -> HTTP GET + flac.New(resp.Body)
       +-- openALSA(device, channels, rate, bits)
       +-- OUTER LOOP (keeps ALSA open between tracks):
            |
            +-- streamLoop(skipSamples):
            |     |
            |     +-- DECODE GOROUTINE:
            |     |     stream.ParseNext() -> PCM conversion -> pcmCh
            |     |
            |     +-- WRITE LOOP:
            |           Read from pcmCh -> check seek/skip/pause -> snd_pcm_writei()
            |           On pause: close ALSA, release D-Bus, spin-wait, reacquire on resume
            |
            +-- If seek: reopen HTTP stream, call streamLoop(seekTarget)
            +-- If track done: close HTTP, signal doneCh, wait for nextURLCh
            +-- If next track: open new stream, compare format, reopen ALSA if changed
```

### PCM Sample Conversion

The decode goroutine converts FLAC samples to the negotiated ALSA format with volume scaling:

```go
s := frame.Subframes[ch].Samples[i]
if vol != 1.0 {
    s = int32(float64(s) * vol)
}

switch ah.format {
case C.SND_PCM_FORMAT_S16_LE:
    binary.LittleEndian.PutUint16(buf[off:], uint16(int16(s)))
case C.SND_PCM_FORMAT_S24_3LE:
    buf[off] = byte(s)
    buf[off+1] = byte(s >> 8)
    buf[off+2] = byte(s >> 16)
case C.SND_PCM_FORMAT_S24_LE, C.SND_PCM_FORMAT_S32_LE:
    shift := uint(ah.significantBits - int(bits))
    binary.LittleEndian.PutUint32(buf[off:], uint32(int32(s)<<shift))
}
```

The `significantBits` field from the ALSA hardware params is used to left-shift samples into the correct MSB position (e.g., 24-bit audio in a 32-bit container).

### Gapless Track Transitions

`PlayNext()` signals the running playback loop to transition without closing ALSA:

1. Creates a new `doneCh` for the next track
2. Closes `skipCh` to interrupt the current `streamLoop`
3. Sends the new URL on `nextURLCh`
4. The outer loop picks up the URL, opens the new HTTP stream
5. If format (rate/channels/bits) changed, ALSA is closed and reopened
6. If format is the same, continues writing to the same ALSA handle (gapless)

### Pause/Resume with Device Release

When paused, the player **releases the ALSA device and D-Bus reservation** so other applications (PipeWire, etc.) can use the DAC:

```go
if atomic.LoadUint32(&p.paused) == 1 {
    C.snd_pcm_drop(ah.pcm)
    closeALSA(ah)
    releaseReservation()
    // spin-wait checking for resume, seek, skip, or cancel
    // on resume: reacquireALSA() (new reservation + new handle)
}
```

### Seek Implementation

Seek re-fetches the HTTP stream from the beginning and skips decoded frames up to the target sample position. There's no HTTP range request — the FLAC decoder just discards frames until reaching the target:

```go
func (p *Player) Seek(seconds float64) error {
    target := uint64(seconds * float64(sr))
    // non-blocking send on seekCh (drop stale pending seek)
    p.seekCh <- target
}
```

The `streamLoop` checks `seekCh` between writes. On seek, it closes the current HTTP response, reopens the stream, and calls itself with `skipSamples` set to the target.

### Concurrency Model

- `sync.Mutex` protects mutable state (cancel, doneCh, skipCh, currentURL, deviceOverride)
- `sync.RWMutex` protects track info (sampleRate, channels, bitsPerSample, totalSamples)
- `atomic` operations for hot-path data (samplesPlayed, paused, volumeBits)
- Channels for cross-goroutine signaling (seekCh, nextURLCh, skipCh — all buffered 1)
- Volume stored as `uint64` via `math.Float64bits` / `math.Float64frombits` for lock-free atomic access

### Player API Surface

```go
func NewPlayer() *Player
func (p *Player) SetDevice(hwName string)
func (p *Player) Play(url string) (<-chan struct{}, error)      // returns done channel
func (p *Player) PlayNext(url string) (<-chan struct{}, error)   // gapless transition
func (p *Player) Pause() error                                   // toggle
func (p *Player) SetVolume(vol float64) error                    // 0-100
func (p *Player) GetVolume() (float64, error)
func (p *Player) GetPosition() (float64, error)                  // seconds
func (p *Player) GetDuration() (float64, error)                  // seconds
func (p *Player) Seek(seconds float64) error
func (p *Player) Done() <-chan struct{}
func (p *Player) Close()
func ListDevices() ([]DeviceInfo, error)
```

---

## Session & Secrets Storage

**File:** `internal/store/store.go` (413 lines)

### Secret Storage (OAuth2 Tokens)

Two-tier approach using `docker/secrets-engine`:

1. **Primary: System keychain** (`keychain.New`) — uses the OS keychain (libsecret on Linux, Keychain on macOS)
2. **Fallback: age-encrypted file** (`posixage.New`) at `~/.config/tidalt/secrets` — prompted passphrase, encrypted with [age](https://age-encryption.org/)

The `PassphraseFunc` type allows the caller to inject how passphrases are read (the entry point reads from stdin with echo disabled via `unix.IoctlSetTermios`).

### Cache Storage (bbolt)

Location: `~/.local/share/tidalt/tidal-cache.db`

Three buckets:
- **`Tracks`**: keyed by track ID, stores JSON-encoded track metadata
- **`Settings`**: keyed by name, stores:
  - `device` — selected ALSA hw: string
  - `volume` — float64
  - `lastPosition` — playback position in seconds
  - `lastTrackID` — for resume-on-restart
  - `playlist` — JSON-encoded track list for session restore
- **`Cache`**: keyed by `search:{query}`, stores JSON-encoded search results

### Two Store Modes

- `NewSecretsStore()` — full store: secrets backend + bbolt (exclusive lock). Used by the primary instance.
- `NewClientStore()` — secrets only, no bbolt. Used by client-mode instances to avoid lock contention with the running daemon.

### Store API

```go
func NewSecretsStore(passphrase PassphraseFunc) *SecretsStore
func NewClientStore(passphrase PassphraseFunc) *SecretsStore

// Secrets
func (s *SecretsStore) SaveSession(data any) error
func (s *SecretsStore) LoadSession(target any) error
func (s *SecretsStore) DeleteSession() error

// Settings
func (s *SecretsStore) SaveDevice(hwName string) error
func (s *SecretsStore) LoadDevice() (string, error)
func (s *SecretsStore) SaveVolume(vol float64) error
func (s *SecretsStore) LoadVolume() (float64, error)
func (s *SecretsStore) SaveLastPosition(seconds float64) error
func (s *SecretsStore) LoadLastPosition() (float64, error)
func (s *SecretsStore) SaveLastTrackID(trackID int) error
func (s *SecretsStore) LoadLastTrackID() (int, error)
func (s *SecretsStore) SavePlaylist(tracks any) error
func (s *SecretsStore) LoadPlaylist(target any) error

// Cache
func (s *SecretsStore) CacheTrack(trackID int, data any) error
func (s *SecretsStore) CacheSearchResults(query string, tracks any) error
func (s *SecretsStore) LoadSearchResults(query string, target any) (bool, error)

func (s *SecretsStore) Close()
```

---

## MPRIS2 D-Bus Integration

**File:** `internal/mpris/server.go` (581 lines)

### Purpose

Exposes a standard MPRIS2 interface on the D-Bus session bus so:
- Media keys (play/pause/next/prev) work without the TUI focused
- `playerctl` can control playback
- Multiple tidalt instances coordinate (server/client architecture)

### Interfaces Implemented

| Interface | Purpose |
|-----------|---------|
| `org.mpris.MediaPlayer2` | Identity, capabilities |
| `org.mpris.MediaPlayer2.Player` | PlayPause, Next, Previous, Play, Pause, Stop |
| `org.freedesktop.DBus.Properties` | Get/GetAll — required by playerctl |
| `org.freedesktop.DBus.Introspectable` | Full XML introspection |
| `io.tidalt.App` | Custom: OpenURL, PlayTrackID, PlayPlaylist, GetState |

### Multi-Instance Architecture

```
Instance 1 (daemon/first launch):
  - Claims "org.mpris.MediaPlayer2.tidalt" bus name
  - Runs MPRIS server, exports all interfaces
  - Receives commands via Commands channel

Instance 2+ (client mode):
  - Detects ErrAlreadyRunning when trying to claim bus name
  - Connects as Client, forwards commands over D-Bus
  - Reads playback state via GetState for display
```

### Command Flow

D-Bus method calls -> `Event` structs on `Commands` channel -> UI/model consumes them

```go
type Event struct {
    Cmd                Cmd        // CmdPlayPause, CmdNext, CmdPrevious, CmdOpenURL, etc.
    URL                string     // for CmdOpenURL
    TrackID            int        // for CmdPlayTrackID
    PlaylistJSON       string     // for CmdPlayPlaylist
    PlaylistStartIndex int
}
```

### State Broadcasting

The parent pushes state via `Server.SetState()`, stored in a `sharedState` (RWMutex-protected). Clients read it via `GetState()` D-Bus call. State includes: current track JSON, playlist JSON, playback status, position, duration, volume, device, shuffle mode.

### MPRIS2 Metadata

Builds standard MPRIS2 metadata from `tidal.Track`:
- `mpris:trackid` — D-Bus object path `/org/mpris/MediaPlayer2/Track/{id}`
- `xesam:title`, `xesam:artist` (as `[]string`), `xesam:album`
- `mpris:length` — in microseconds (spec requirement)

---

## Desktop Integration

**Files:** `cmd/tidalt/setup.go`, `cmd/tidalt/play.go`, `cmd/tidalt/daemon.go`

### URL Handler (`setup.go`)

Installs a `.desktop` file that registers `tidal://` URL scheme:

```ini
[Desktop Entry]
Name=tidalt
Exec=tidalt play %u
Terminal=false
Type=Application
MimeType=x-scheme-handler/tidal;
```

Uses `xdg-mime` to register and `update-desktop-database` to refresh.

### Play Command (`play.go`)

`tidalt play <url>` handles URLs from the desktop handler:

1. If a tidalt instance is already running: forward URL via D-Bus (`mpris.NewClient().SendURL()`) and exit
2. Otherwise: find a terminal emulator and launch `tidalt <url>` in it

Terminal detection order: `$TERMINAL`, kitty, ghostty, alacritty, foot, wezterm, konsole, xfce4-terminal, gnome-terminal, xterm. Each has its correct flag convention (`-e`, `start --`, bare args for kitty/foot).

### Daemon Mode (`daemon.go`)

Headless mode: runs the full playback engine + MPRIS server without a TUI. Can be installed as a systemd user service (`tidalt setup --daemon`):

```ini
[Service]
Type=simple
ExecStart=/path/to/tidalt daemon
Restart=on-failure
```

---

## Logging

**File:** `internal/logger/logger.go` (88 lines)

- Global `logger.L` (`*slog.Logger`)
- Enabled by `TIDALT_DEBUG=true` environment variable; otherwise discards all output
- Writes to `~/.local/share/tidalt/debug-YYYYMMDD-HHMMSS.log`
- **URL redaction**: wraps `slog.Handler` to strip query strings from any string attribute that looks like a URL — prevents OAuth tokens and CDN auth params from appearing in logs

---

## Dependencies

### Core (direct)

| Dependency | Purpose |
|------------|---------|
| `golang.org/x/oauth2` | OAuth2 device flow, token refresh, authenticated HTTP client |
| `github.com/mewkiz/flac` | Pure-Go FLAC decoder (streaming from `io.Reader`) |
| `github.com/godbus/dbus/v5` | D-Bus: device reservation, MPRIS2 server/client |
| `github.com/docker/secrets-engine` | Keychain + age-encrypted file storage for OAuth tokens |
| `go.etcd.io/bbolt` | Embedded key-value DB for settings/cache |
| `github.com/mdp/qrterminal/v3` | Terminal QR code rendering |
| `github.com/pkg/browser` | Open URL in default browser |
| `github.com/atotto/clipboard` | Clipboard access (copy track link) |
| `golang.org/x/sys` | Unix terminal control (echo disable for passphrase) |
| `golang.org/x/term` | Terminal size detection |
| `golang.org/x/image` | Image processing (cover art) |

### Build Requirements

- **Go 1.26+**
- **CGO enabled** — links against `libasound` (`-lasound`) for direct ALSA access
- ALSA development headers (`alsa-lib` / `libasound2-dev`)
- D-Bus session bus (for device reservation and MPRIS)

---

## Gotchas & Hard-Won Knowledge

These are implementation details that were discovered through real-world testing with actual hardware and should be preserved in any reimplementation:

### Tidal API

1. **Device auth uses camelCase JSON** — `deviceCode`, `userCode`, `verificationUri` instead of RFC 8628 snake_case. The manual POST + custom struct decode is required.

2. **Stream URL endpoint is undocumented** — `/tracks/{id}/urlpostpaywall` with `urlusagemode=STREAM`, `audioquality=...`, `assetpresentation=FULL`. Quality ladder (HI_RES_LOSSLESS down to LOW) handles subscription tier differences.

3. **v2 API playlist items don't include full track data** — despite `include=items` in the spec, the v2 endpoint only returns track IDs. Full metadata requires individual v1 `/tracks/{id}` calls (done concurrently with order preservation).

4. **Country code is required everywhere** — every API call includes `countryCode` from the session. Missing it causes 400 errors or empty results.

5. **Cover art UUID format** — stored with dashes in JSON, CDN path uses forward slashes between groups. The `CoverURL()` helper does `strings.ReplaceAll(cover, "-", "/")`.

6. **Artist albums require two API calls** — one with `filter=ALBUMS`, one with `filter=EPSANDSINGLES`. They're merged in the response. The EP call failure is non-fatal.

### ALSA / DAC

7. **S32_LE before S16_LE for 16-bit sources** — CS43198-based USB DACs (Hidizs S9 Pro Plus) have a broken S16_LE USB endpoint. S32_LE with MSB-aligned left-shift works correctly.

8. **Set period size before buffer size** — Setting buffer first then querying `period_size_min` returns absurdly small values on some USB DACs (87 frames = ~2ms on Hidizs S9 Pro Plus "Martha"), causing ~1000 interrupts/s and audible distortion. Set period to 1024 first, then buffer to 4x period.

9. **200ms sleep after D-Bus device release** — WirePlumber needs time to close its ALSA handle after releasing the reservation. Without this delay, `snd_pcm_open` fails with EBUSY.

10. **Release ALSA on pause** — If the player holds the device while paused, no other application can use the DAC. The player closes ALSA and releases the D-Bus reservation on pause, reacquires both on resume.

11. **Significant bits vs container size** — A DAC using S32_LE may only have 24 significant bits. `snd_pcm_hw_params_get_sbits()` returns the actual bit depth, used to calculate the correct left-shift for sample alignment.

12. **FLAC seek = re-fetch + skip** — There's no HTTP range request. Seeking reopens the HTTP stream from the start and discards frames until reaching the target sample position. Simple and reliable, though not bandwidth-efficient for seeks near the end of long tracks.

### Multi-Instance

13. **bbolt has exclusive locking** — Only one process can open the database file. Client instances use `NewClientStore()` which skips bbolt entirely, accessing only the secrets backend.

14. **MPRIS bus name as mutex** — `dbus.NameFlagDoNotQueue` means the second instance immediately gets `ErrAlreadyRunning` instead of waiting. This is the multi-instance detection mechanism.

---

## Testing Patterns

**File:** `internal/tidal/api_test.go` (607 lines)

The test infrastructure is clean and reusable:

### HTTP Transport Injection

```go
type roundTripFunc func(*http.Request) (*http.Response, error)
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestClient(srv *httptest.Server) *tidal.Client {
    c := tidal.NewClient()
    c.Session = &tidal.Session{
        AccessToken: "test-token",
        Expiry:      time.Now().Add(time.Hour),  // far future = no refresh
        UserID:      42,
        CountryCode: "US",
    }
    // Rewrite requests to hit test server
    c.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
        req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
        req.URL.Scheme = "http"
        return srv.Client().Transport.RoundTrip(req)
    })
    // Token endpoint on test server too
    c.Oauth.Endpoint = oauth2.Endpoint{TokenURL: srv.URL + "/token"}
    return c
}
```

### Test Coverage

- **Happy paths**: all API methods return correct data
- **Error responses**: non-200 status codes produce errors
- **Quality ladder**: stream URL falls back through qualities correctly
- **Concurrent ordering**: `GetMixTracks` preserves playlist order despite concurrent fetches
- **Graceful degradation**: 404 tracks are skipped in mix track lists, non-track refs are filtered
- **Empty results**: searches with no matches return empty slices (not nil)

---

## Recommendations for a New Project

1. **Copy `internal/tidal/` nearly verbatim** — the API client, auth flow, and type definitions are solid and well-tested. The only UI coupling is in `loginprint.go` which can be adapted to any display method.

2. **Copy `internal/player/mpv.go` verbatim** — this is the crown jewel. The CGO/ALSA code, format negotiation, D-Bus reservation, and gapless playback are extremely well-tuned. The `Player` API surface is already cleanly separated from any UI.

3. **Copy `internal/store/store.go` verbatim** — the two-tier secrets storage and bbolt cache are UI-independent. You may want to extend the bbolt schema for new features.

4. **Copy `internal/mpris/server.go` with modifications** — the MPRIS2 implementation and multi-instance coordination are valuable. The `io.tidalt.App` custom interface may need changes for new features.

5. **Copy `internal/logger/logger.go` verbatim** — small, useful, and the URL redaction is a security feature worth keeping.

6. **The player package requires CGO and libasound** — this is a hard requirement for bit-perfect playback. Any reimplementation that goes through PipeWire/PulseAudio will lose the bit-perfect guarantee.

7. **The entry point (`cmd/tidalt/main.go`)** and subcommands (`daemon.go`, `play.go`, `setup.go`, `logout.go`) provide a good template for CLI structure with URL handling, daemon mode, and desktop integration.
