package tidal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/SisyphusOfCorinth/rising-tide/internal/logger"
)

// This file contains the AAC fallback for the subset of Tidal tracks that
// don't have a FLAC master on the server and are only available as
// 320 kbps AAC-LC at every quality tier. We spawn ffmpeg as a child
// process, let it fetch the CDN URL directly, decode AAC, and re-encode
// the output as raw FLAC which the player's existing FLAC decoder can
// consume unchanged.
//
// Re-encoding AAC to FLAC is CPU-cheap (FLAC is a simple predictive codec)
// and lossless, so no further quality is lost beyond what Tidal's AAC
// already compressed away. Routing via ffmpeg also keeps the rest of the
// player pipeline bit-perfect for the real FLAC catalog: nothing else in
// the playback path needs a second codec implementation.

// openAACTranscode spawns "ffmpeg -i <url> -c:a flac -f flac pipe:1" and
// returns a reader over its stdout. The process is killed when the returned
// ReadCloser is closed or when ctx is cancelled.
//
// ffmpeg is handed the URL directly (rather than us piping the HTTP body
// via stdin) because MP4 demuxing may need range reads; ffmpeg's HTTP
// layer handles them transparently. Tidal CDN URLs carry their auth as
// query-string tokens, so no extra headers are required.
func openAACTranscode(ctx context.Context, url string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		// warning level so connection retries and recoverable decode
		// glitches make it into our debug log; error-only hides them.
		"-loglevel", "warning",
		"-hide_banner",
		"-nostdin",
		// HTTP resilience: if the CDN closes or stalls mid-stream, let
		// ffmpeg reconnect and resume. Without these, a single brief
		// network hiccup during the FLAC encoder's startup can yield a
		// truncated opening frame that renders as static even though
		// later frames decode fine. -reconnect_at_eof handles servers
		// that send an early EOF; -reconnect_delay_max bounds the retry
		// backoff at 5 seconds so we don't silently stall forever.
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_at_eof", "1",
		"-reconnect_delay_max", "5",
		"-i", url,
		"-c:a", "flac",
		// Match the AAC-LC source depth. FFmpeg's default sample_fmt
		// varies by input, and a 32-bit float intermediate can produce
		// a higher bit depth in STREAMINFO than the source actually
		// carries -- forcing s16 keeps the output predictable.
		"-sample_fmt", "s16",
		"-f", "flac",
		"pipe:1",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		// Most likely cause: ffmpeg is not installed on $PATH. Surface a
		// clear message so the user knows what to install.
		return nil, fmt.Errorf("ffmpeg unavailable (install ffmpeg to play AAC tracks): %w", err)
	}
	logger.L.Debug("ffmpeg transcode started", "url", redactTidalURL(url))

	go drainFFmpegStderr(stderr)

	return &ffmpegReader{cmd: cmd, stdout: stdout}, nil
}

// ffmpegReader is an io.ReadCloser over an ffmpeg child process's stdout.
// Closing it kills the process and reaps it, so no zombies are left behind
// when playback stops mid-stream.
type ffmpegReader struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	closed bool
}

func (f *ffmpegReader) Read(p []byte) (int, error) { return f.stdout.Read(p) }

func (f *ffmpegReader) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	// Close stdout first so ffmpeg's next write triggers SIGPIPE. Then Kill
	// as a safety net in case ffmpeg is blocked elsewhere (e.g. waiting on
	// an HTTP read). Wait() reaps the process.
	_ = f.stdout.Close()
	if f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
	}
	_ = f.cmd.Wait()
	return nil
}

// drainFFmpegStderr forwards ffmpeg's stderr lines to the debug logger so
// decode failures (404s, invalid URL, bad container) are visible without
// hanging the caller on an unread pipe.
func drainFFmpegStderr(r io.ReadCloser) {
	defer func() { _ = r.Close() }()
	s := bufio.NewScanner(r)
	for s.Scan() {
		logger.L.Debug("ffmpeg", "msg", s.Text())
	}
}

// redactTidalURL strips the query string from a Tidal CDN URL so signed
// tokens don't land in logs. Tidal tokens aren't long-lived, but they're
// single-use credentials and worth keeping out of debug files.
func redactTidalURL(u string) string {
	for i := 0; i < len(u); i++ {
		if u[i] == '?' {
			return u[:i] + "?<redacted>"
		}
	}
	return u
}
