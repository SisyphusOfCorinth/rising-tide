# Rising Tide

A terminal-based Tidal music streaming client with a four-panel TUI layout and neovim-style controls.

## Build Requirements

- Go 1.22+
- CGO enabled (required for ALSA)
- ALSA development headers: `alsa-lib` (Arch) or `libasound2-dev` (Debian/Ubuntu)
- D-Bus session bus (for ALSA device reservation and future MPRIS support)

## Build

```bash
CGO_ENABLED=1 go build ./cmd/rising-tide
```

## Test

```bash
go test ./internal/tidal/
```

## Run

```bash
./rising-tide
```

Set `RISING_TIDE_DEBUG=true` for debug logging to `~/.local/share/rising-tide/`.

## Architecture

```
cmd/rising-tide/main.go    Entry point: bootstrap store, client, player, run TUI
internal/tidal/             Tidal API client (OAuth2 device flow + REST)
internal/player/            Bit-perfect ALSA playback via CGO (FLAC decode, PCM write)
internal/store/             Persistent storage (keychain/age secrets + bbolt cache)
internal/logger/            Debug logging with URL redaction
internal/ui/                Bubble Tea TUI (four-panel layout)
```

The backend packages (tidal, player, store, logger) are ported from
[tidalt](https://github.com/Benehiko/tidalt) v3. The UI layer is new.
