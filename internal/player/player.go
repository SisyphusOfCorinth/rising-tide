// Package player implements bit-perfect FLAC playback via direct ALSA hw:
// device access using CGO. It streams FLAC data from Tidal's CDN HTTP
// response, decodes frames in-flight, and writes PCM samples directly to an
// ALSA buffer -- no intermediate files, no PulseAudio/PipeWire mixing, no
// resampling.
//
// Key design decisions (from real-world DAC testing):
//   - S32_LE is tried before S16_LE for 16-bit sources because some USB DACs
//     (e.g. CS43198-based Hidizs S9 Pro Plus) have a broken S16_LE endpoint.
//   - Period size is set before buffer size to avoid absurdly small period
//     values on some USB DACs.
//   - ALSA device is released on pause so other applications can use the DAC.
//   - D-Bus device reservation prevents PipeWire/PulseAudio from fighting
//     over the hw: device.
package player

/*
#cgo LDFLAGS: -lasound
#include <alsa/asoundlib.h>
#include <stdlib.h>
#include <errno.h>

// alsa_open_result carries the negotiated format back to Go.
typedef struct {
    snd_pcm_format_t  format;
    int               bytes_per_sample;
    int               significant_bits; // actual DAC bit depth (e.g. 24 for Scarlett, 32 for S9 Pro Plus)
    unsigned int      rate;             // negotiated sample rate (may differ from requested)
    snd_pcm_uframes_t period_size;
    snd_pcm_uframes_t buffer_size;
    snd_pcm_uframes_t avail_min;
    snd_pcm_uframes_t start_threshold;
    snd_pcm_uframes_t stop_threshold;
} alsa_open_result_t;

// open_hw_pcm opens an ALSA hw device and negotiates the best available
// format for the given bit depth, without enabling soft resampling.
// Format preference order:
//   16-bit source : S32_LE -> S16_LE -> S24_3LE -> S24_LE
//   24-bit source : S24_3LE -> S24_LE -> S32_LE
//
// S32_LE is tried first for 16-bit sources because CS43198-based USB DACs
// (e.g. Hidizs S9 Pro Plus) have a buggy S16_LE USB endpoint but work
// correctly via their native 32-bit endpoint. The 16-bit samples are
// left-shifted to fill the MSB (standard MSB-aligned convention).
//
// Returns 0 on success, a negative ALSA error code on failure.
static int open_hw_pcm(const char *device,
                       unsigned int channels, unsigned int rate, int bits,
                       snd_pcm_t **handle_out,
                       alsa_open_result_t *result) {
    int rc;

    rc = snd_pcm_open(handle_out, device, SND_PCM_STREAM_PLAYBACK, 0);
    if (rc < 0) return rc;

    snd_pcm_hw_params_t *params;
    snd_pcm_hw_params_alloca(&params);

    rc = snd_pcm_hw_params_any(*handle_out, params);
    if (rc < 0) goto fail;

    rc = snd_pcm_hw_params_set_access(*handle_out, params,
                                       SND_PCM_ACCESS_RW_INTERLEAVED);
    if (rc < 0) goto fail;

    // Negotiate format -- try preferred formats for the source bit depth.
    {
        snd_pcm_format_t fmt16[] = {SND_PCM_FORMAT_S32_LE,
                                    SND_PCM_FORMAT_S16_LE,
                                    SND_PCM_FORMAT_S24_3LE,
                                    SND_PCM_FORMAT_S24_LE};
        snd_pcm_format_t fmt24[] = {SND_PCM_FORMAT_S24_3LE,
                                    SND_PCM_FORMAT_S24_LE,
                                    SND_PCM_FORMAT_S32_LE};
        snd_pcm_format_t *fmts   = (bits == 16) ? fmt16 : fmt24;
        int               nfmts  = (bits == 16) ? 4      : 3;
        snd_pcm_format_t  chosen = SND_PCM_FORMAT_UNKNOWN;

        for (int i = 0; i < nfmts; i++) {
            if (snd_pcm_hw_params_set_format(*handle_out, params, fmts[i]) == 0) {
                chosen = fmts[i];
                break;
            }
        }
        if (chosen == SND_PCM_FORMAT_UNKNOWN) { rc = -EINVAL; goto fail; }

        result->format = chosen;
        switch (chosen) {
            case SND_PCM_FORMAT_S16_LE:  result->bytes_per_sample = 2; break;
            case SND_PCM_FORMAT_S24_3LE: result->bytes_per_sample = 3; break;
            default:                     result->bytes_per_sample = 4; break;
        }
    }

    rc = snd_pcm_hw_params_set_channels(*handle_out, params, channels);
    if (rc < 0) goto fail;

    rc = snd_pcm_hw_params_set_rate_near(*handle_out, params, &rate, 0);
    if (rc < 0) goto fail;
    result->rate = rate;

    // Set period size first (~23ms at 44100 Hz) so the DAC gets a sane
    // interrupt rate, then set buffer to 4x the negotiated period.
    //
    // WARNING: Setting buffer first then querying period_size_min returns
    // absurdly small values on some USB DACs (e.g. 87 frames on Hidizs S9
    // Pro Plus "Martha"), causing ~1000 interrupts/s and severe distortion.
    {
        snd_pcm_uframes_t period_size = 1024;
        rc = snd_pcm_hw_params_set_period_size_near(*handle_out, params, &period_size, NULL);
        if (rc < 0) goto fail;

        snd_pcm_uframes_t buffer_size = period_size * 4;
        rc = snd_pcm_hw_params_set_buffer_size_near(*handle_out, params, &buffer_size);
        if (rc < 0) goto fail;
    }

    rc = snd_pcm_hw_params(*handle_out, params);
    if (rc < 0) goto fail;

    // Query the hardware's actual significant bit depth (e.g. 24 for a DAC
    // that uses S32_LE as a 24-bit MSB-aligned container).
    result->significant_bits = snd_pcm_hw_params_get_sbits(params);

    // Read back period/buffer for logging.
    {
        snd_pcm_uframes_t period_size, buffer_size;
        snd_pcm_hw_params_get_period_size(params, &period_size, NULL);
        snd_pcm_hw_params_get_buffer_size(params, &buffer_size);
        result->period_size = period_size;
        result->buffer_size = buffer_size;

        snd_pcm_sw_params_t *sw;
        snd_pcm_sw_params_alloca(&sw);
        snd_pcm_sw_params_current(*handle_out, sw);
        snd_pcm_sw_params_get_avail_min(sw, &result->avail_min);
        snd_pcm_sw_params_get_start_threshold(sw, &result->start_threshold);
        snd_pcm_sw_params_get_stop_threshold(sw, &result->stop_threshold);
    }

    return 0;

fail:
    snd_pcm_close(*handle_out);
    *handle_out = NULL;
    return rc;
}
*/
import "C"

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/SisyphusOfCorinth/rising-tide/internal/logger"

	"github.com/godbus/dbus/v5"
	"github.com/mewkiz/flac"
)

// knownDACs lists substrings to search for in /proc/asound/cards output.
// First match wins, so order determines priority.
var knownDACs = []string{"hidizs", "s9pro", "focusrite", "scarlett", "ifi", "go link"}

// DeviceInfo describes an ALSA playback device.
type DeviceInfo struct {
	HWName   string // ALSA device string, e.g. "hw:1,0"
	CardName string // short name from brackets, e.g. "S9Pro"
	LongName string // description after " - ", e.g. "HiDizs S9 Pro"
}

// ListDevices returns all ALSA cards that have at least one playback PCM.
// The first entry is always the "default" device (PipeWire/PulseAudio),
// followed by any hardware devices.
func ListDevices() ([]DeviceInfo, error) {
	cardData, err := os.ReadFile("/proc/asound/cards")
	if err != nil {
		return nil, fmt.Errorf("cannot read /proc/asound/cards: %w", err)
	}
	pcmData, err := os.ReadFile("/proc/asound/pcm")
	if err != nil {
		return nil, fmt.Errorf("cannot read /proc/asound/pcm: %w", err)
	}

	// Collect card numbers that have at least one playback PCM.
	playback := make(map[int]bool)
	for _, line := range strings.Split(string(pcmData), "\n") {
		if !strings.Contains(line, "playback") {
			continue
		}
		var card, dev int
		if _, err := fmt.Sscanf(line, "%d-%d:", &card, &dev); err == nil {
			playback[card] = true
		}
	}

	// Always include the default device first -- routes through
	// PipeWire/PulseAudio and works reliably with built-in sound cards.
	devices := []DeviceInfo{{
		HWName:   "default",
		CardName: "Default",
		LongName: "Default (PipeWire/PulseAudio)",
	}}
	lines := strings.Split(string(cardData), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var cardNum int
		if _, err := fmt.Sscanf(trimmed, "%d", &cardNum); err != nil {
			continue // continuation line, not a card header
		}
		if !playback[cardNum] {
			continue
		}
		cardName := ""
		if s := strings.Index(line, "["); s != -1 {
			if e := strings.Index(line, "]"); e > s {
				cardName = strings.TrimSpace(line[s+1 : e])
			}
		}
		longName := ""
		if idx := strings.Index(line, " - "); idx != -1 {
			longName = strings.TrimSpace(line[idx+3:])
		}
		if longName == "" {
			longName = cardName
		}
		devices = append(devices, DeviceInfo{
			HWName:   fmt.Sprintf("hw:%d,0", cardNum),
			CardName: cardName,
			LongName: longName,
		})
	}
	return devices, nil
}

// StreamOpener opens a fresh audio byte stream for a single track. It may be
// called more than once per track -- once for initial playback, and again if
// the user seeks (the loop reopens the stream from the start and skips
// forward to the target). Implementations are responsible for any auth,
// manifest resolution, and HTTP fetching; the player just reads bytes and
// closes the returned ReadCloser when done.
type StreamOpener func(ctx context.Context) (io.ReadCloser, error)

// Player manages ALSA playback of FLAC streams from Tidal's CDN.
//
// Concurrency model:
//   - sync.Mutex protects mutable state (cancel, doneCh, skipCh, etc.)
//   - sync.RWMutex protects track info (sampleRate, channels, etc.)
//   - atomic operations for hot-path data (samplesPlayed, paused, volume)
//   - Channels for cross-goroutine signaling (seekCh, nextOpenerCh, skipCh)
type Player struct {
	mu             sync.Mutex
	cancel         context.CancelFunc
	doneCh         chan struct{}
	deviceOverride string // set via SetDevice; empty = auto-detect

	// seekCh carries seek targets (in samples) to the running playback loop.
	// Buffered 1 so Seek never blocks; the loop drains it before checking again.
	seekCh chan uint64

	// nextOpenerCh carries the next track's stream opener into the running
	// playbackLoop so it can transition without closing the ALSA device.
	nextOpenerCh chan StreamOpener
	// transitionDoneCh is set by PlayNext() before sending on nextOpenerCh.
	// The playbackLoop installs it as the new doneCh once the new stream starts.
	transitionDoneCh chan struct{}
	// skipCh is closed by PlayNext to interrupt the current streamLoop
	// immediately, so the outer loop can pick up the next opener without
	// waiting for the current track to finish.
	skipCh chan struct{}
	// loopDone is closed when the playbackLoop goroutine returns.
	// Used by stop() to wait for the goroutine independently of doneCh.
	loopDone chan struct{}

	// Track info -- written by playbackLoop, read by UI tick.
	muInfo        sync.RWMutex
	sampleRate    uint32
	channels      uint8
	bitsPerSample uint8
	totalSamples  uint64

	// Atomics: safe for concurrent access without a mutex.
	samplesPlayed uint64
	paused        uint32 // 0 = playing, 1 = paused
	volumeBits    uint64 // float64 stored via math.Float64bits; range 0.0-1.0
}

// SetDevice sets the ALSA hw device to use for playback. Pass "" to revert to
// auto-detection from the known-DAC list.
func (p *Player) SetDevice(hwName string) {
	p.mu.Lock()
	p.deviceOverride = hwName
	p.mu.Unlock()
}

// getDevice returns the configured device override or falls back to auto-detection.
func (p *Player) getDevice() (string, error) {
	p.mu.Lock()
	override := p.deviceOverride
	p.mu.Unlock()
	if override != "" {
		return override, nil
	}
	return detectDevice()
}

// NewPlayer creates a new Player ready for playback. The ALSA device is not
// opened until Play is called.
func NewPlayer() *Player {
	p := &Player{
		seekCh:       make(chan uint64, 1),
		nextOpenerCh: make(chan StreamOpener, 1),
		skipCh:       make(chan struct{}),
	}
	atomic.StoreUint64(&p.volumeBits, math.Float64bits(1.0))
	return p
}

// Start is a no-op; the ALSA handle is opened per-track in Play.
func (p *Player) Start(_ context.Context) error { return nil }

// detectDevice scans /proc/asound/cards for a known external DAC and returns
// the hw device string, e.g. "hw:1,0". If no known DAC is found, falls back
// to "default" which routes through PipeWire/PulseAudio -- not bit-perfect,
// but reliable for built-in sound cards.
func detectDevice() (string, error) {
	data, err := os.ReadFile("/proc/asound/cards")
	if err != nil {
		return "default", nil
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		lower := strings.ToLower(line)
		for _, name := range knownDACs {
			if !strings.Contains(lower, name) {
				continue
			}
			// The card number is the leading integer on the card's first line.
			for j := i; j >= 0 && j >= i-1; j-- {
				var num int
				if _, err := fmt.Sscanf(strings.TrimSpace(lines[j]), "%d", &num); err == nil {
					return fmt.Sprintf("hw:%d,0", num), nil
				}
			}
		}
	}
	// No external DAC found -- use the default ALSA device which routes
	// through PipeWire/PulseAudio. Direct hw: access to built-in cards is
	// unreliable because PipeWire reclaims the device.
	logger.L.Debug("no known DAC detected, using default ALSA device")
	return "default", nil
}

// parseCardNum extracts the card number from an ALSA hw device string like "hw:1,0".
func parseCardNum(hwDevice string) (int, error) {
	var card, dev int
	for _, prefix := range []string{"plughw:%d,%d", "hw:%d,%d"} {
		if _, err := fmt.Sscanf(hwDevice, prefix, &card, &dev); err == nil {
			return card, nil
		}
	}
	if _, err := fmt.Sscanf(hwDevice, "front:%d", &card); err == nil {
		return card, nil
	}
	return 0, fmt.Errorf("cannot parse card number from %q", hwDevice)
}

// reserveALSADevice acquires the org.freedesktop.ReserveDevice1.Audio{N} D-Bus
// name so that PipeWire/PulseAudio releases the hw: device before we open it.
// If D-Bus is unavailable the function returns a no-op release func and nil
// error so callers can proceed unconditionally.
func reserveALSADevice(cardNum int) (release func(), err error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		// No session bus -- skip reservation and try to open ALSA directly.
		return func() {}, nil
	}

	name := fmt.Sprintf("org.freedesktop.ReserveDevice1.Audio%d", cardNum)
	objPath := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/ReserveDevice1/Audio%d", cardNum))

	releaseFunc := func() {
		_, _ = conn.ReleaseName(name)
		_ = conn.Close()
	}

	// Ask the current owner (typically WirePlumber) to release the device,
	// then claim the name ourselves with ReplaceExisting.
	obj := conn.Object(name, objPath)
	var released bool
	if callErr := obj.Call("org.freedesktop.ReserveDevice1.RequestRelease", 0, int32(math.MaxInt32)).Store(&released); callErr != nil || !released {
		_ = conn.Close()
		return nil, fmt.Errorf("audio device Audio%d is held by another process and refused to release", cardNum)
	}

	// Give WirePlumber a moment to close its ALSA handle before we claim the
	// name and open the device. Without this delay, snd_pcm_open fails EBUSY.
	time.Sleep(200 * time.Millisecond)

	reply, err := conn.RequestName(name,
		dbus.NameFlagReplaceExisting|dbus.NameFlagAllowReplacement)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to claim Audio%d reservation", cardNum)
	}

	return releaseFunc, nil
}

type alsaHandle struct {
	pcm             *C.snd_pcm_t
	format          C.snd_pcm_format_t
	bytesPerSample  int
	significantBits int // actual DAC bit depth
	rate            uint32
	periodSize      uint64
	bufferSize      uint64
	availMin        uint64
	startThreshold  uint64
	stopThreshold   uint64
}

// openALSA opens an ALSA hw device, negotiating the best available format for
// the source bit depth without enabling soft resampling (bit-perfect).
func openALSA(device string, channels uint8, rate uint32, bits uint8) (*alsaHandle, error) {
	cdev := C.CString(device)
	defer C.free(unsafe.Pointer(cdev))

	var handle *C.snd_pcm_t
	var result C.alsa_open_result_t

	if rc := C.open_hw_pcm(cdev,
		C.uint(channels), C.uint(rate), C.int(bits),
		&handle, &result,
	); rc < 0 {
		return nil, fmt.Errorf("open_hw_pcm(%s, ch=%d, rate=%d, bits=%d): %s",
			device, channels, rate, bits, C.GoString(C.snd_strerror(rc)))
	}

	return &alsaHandle{
		pcm:             handle,
		format:          result.format,
		bytesPerSample:  int(result.bytes_per_sample),
		significantBits: int(result.significant_bits),
		rate:            uint32(result.rate),
		periodSize:      uint64(result.period_size),
		bufferSize:      uint64(result.buffer_size),
		availMin:        uint64(result.avail_min),
		startThreshold:  uint64(result.start_threshold),
		stopThreshold:   uint64(result.stop_threshold),
	}, nil
}

// isHWDevice reports whether the device string refers to a direct ALSA hw:
// device that requires D-Bus reservation and exclusive access.
func isHWDevice(device string) bool {
	return strings.HasPrefix(device, "hw:") || strings.HasPrefix(device, "plughw:")
}

// Play starts playback of the stream produced by opener and returns the done
// channel for this track. The channel is closed when playback finishes
// naturally. The opener may be invoked more than once: initial read, and on
// every seek (the loop restarts the stream and skips forward to the target
// sample offset, since the HTTP side doesn't expose byte-range seeking over
// an arbitrary audio bytestream).
func (p *Player) Play(opener StreamOpener) (<-chan struct{}, error) {
	p.stop()

	device, err := p.getDevice()
	if err != nil {
		return nil, err
	}

	// Only acquire D-Bus reservation for direct hw: devices (external DACs).
	// The "default" device routes through PipeWire and doesn't need it.
	var releaseReservation func()
	if isHWDevice(device) {
		cardNum, err := parseCardNum(device)
		if err != nil {
			return nil, err
		}
		releaseReservation, err = reserveALSADevice(cardNum)
		if err != nil {
			return nil, err
		}
	} else {
		releaseReservation = func() {} // no-op for non-hw devices
	}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	loopDone := make(chan struct{})

	p.mu.Lock()
	p.cancel = cancel
	p.doneCh = doneCh
	p.loopDone = loopDone
	p.skipCh = make(chan struct{})
	p.mu.Unlock()

	atomic.StoreUint64(&p.samplesPlayed, 0)
	atomic.StoreUint32(&p.paused, 0)
	// Drain any pending seek/next-opener so the new track starts cleanly.
	select {
	case <-p.seekCh:
	default:
	}
	select {
	case <-p.nextOpenerCh:
	default:
	}

	go func() {
		defer close(loopDone)
		// Cancelling ctx on return (normal or otherwise) ensures the
		// body-close watcher in openStream tears down cleanly instead of
		// leaking a goroutine that waits on ctx.Done forever.
		defer cancel()
		p.playbackLoop(ctx, opener, device, releaseReservation)
		p.mu.Lock()
		ch := p.doneCh
		p.doneCh = nil
		p.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	}()
	return doneCh, nil
}

func (p *Player) stop() {
	p.mu.Lock()
	cancel := p.cancel
	loopDone := p.loopDone
	p.cancel = nil
	p.loopDone = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if loopDone != nil {
		select {
		case <-loopDone:
		case <-time.After(3 * time.Second):
			// Should be unreachable now that openStream's ctx watcher
			// closes the body when ctx cancels. A hit here means a
			// goroutine is still blocked somewhere downstream and the
			// next Play may start while the old ALSA handle is still
			// draining -- worth knowing about.
			logger.L.Warn("stop timeout: playback loop did not exit within 3s")
		}
	}
}

// PlayNext signals the running playbackLoop to transition to the next track
// by swapping in a new stream opener, without closing the ALSA device. If
// the new track has a different format (sample rate, channels, bits), the
// loop will close and reopen the device internally. If no playback loop is
// running, it falls back to Play().
func (p *Player) PlayNext(opener StreamOpener) (<-chan struct{}, error) {
	p.mu.Lock()
	loopDone := p.loopDone
	p.mu.Unlock()

	if loopDone == nil {
		return p.Play(opener)
	}
	select {
	case <-loopDone:
		return p.Play(opener)
	default:
	}

	newDone := make(chan struct{})

	p.mu.Lock()
	p.transitionDoneCh = newDone
	close(p.skipCh)
	p.skipCh = make(chan struct{})
	p.mu.Unlock()

	select {
	case <-p.nextOpenerCh:
	default:
	}
	p.nextOpenerCh <- opener

	atomic.StoreUint32(&p.paused, 0)

	return newDone, nil
}

// openStream invokes the supplied opener and returns the underlying body
// alongside a FLAC decoder positioned at the first audio frame. Tidal
// delivers either raw FLAC or FLAC-in-MP4 depending on quality tier and
// delivery rollout; we sniff the first four bytes and, if the body is
// MP4/ISOBMFF, demux it on the fly so the decoder always sees a raw FLAC
// bytestream. The caller owns the returned ReadCloser and must close it
// when playback for this stream ends.
//
// A watcher goroutine is started alongside the stream: when ctx is
// cancelled (typically because stop() is tearing playback down), it
// closes the body. Without this, a stop() call could block while the
// decode goroutine was stuck in stream.ParseNext -> pipe read -> body
// read -- ctx cancellation alone doesn't unblock an in-flight HTTP read,
// and the 3-second stop() timeout would then let a new ALSA handle open
// against the same hw: device while the old one was still in flight,
// producing static from the overlap. The watcher exits when ctx cancels
// (either a real stop or the defer cancel() at the end of playbackLoop's
// goroutine, which covers the natural end-of-track case).
func openStream(ctx context.Context, opener StreamOpener) (io.ReadCloser, *flac.Stream, error) {
	body, err := opener(ctx)
	if err != nil {
		return nil, nil, err
	}

	go func() {
		<-ctx.Done()
		_ = body.Close()
	}()

	audio, err := unwrapFlacStream(body)
	if err != nil {
		_ = body.Close()
		return nil, nil, err
	}

	stream, err := flac.New(audio)
	if err != nil {
		_ = body.Close()
		return nil, nil, err
	}
	return body, stream, nil
}

// unwrapFlacStream peeks the first four bytes of the HTTP body and returns
// either the body unchanged (raw FLAC, "fLaC" magic) or a reader that
// streams the demuxed FLAC out of an MP4 container. Closing the HTTP body
// cascades: for the MP4 case, the demux goroutine's reads on the body error
// out, propagating through the io.Pipe to the FLAC decoder -- so the usual
// cleanup path (resp.Body.Close in playbackLoop) still tears everything down.
func unwrapFlacStream(body io.Reader) (io.Reader, error) {
	br := bufio.NewReader(body)
	magic, err := br.Peek(4)
	if err != nil {
		return nil, fmt.Errorf("sniff stream header: %w", err)
	}
	if string(magic) == "fLaC" {
		return br, nil
	}
	logger.L.Debug("wrapping stream with MP4 demuxer", "magic", fmt.Sprintf("%x", magic))
	return FlacFromMp4Reader(br), nil
}

// closeALSA drains and closes an ALSA handle.
func closeALSA(ah *alsaHandle) {
	C.snd_pcm_drain(ah.pcm)
	C.snd_pcm_close(ah.pcm)
}

// playbackLoop is the main playback goroutine. It manages the ALSA device
// lifetime, FLAC decoding, PCM writing, pause/resume, seek, and gapless
// track transitions. It owns the stream body: every call to openStream
// yields a fresh body that must be closed exactly once when that stream
// phase (initial play, post-seek, or post-transition) ends.
func (p *Player) playbackLoop(ctx context.Context, opener StreamOpener, device string, releaseReservation func()) {
	logger.L.Debug("playbackLoop start", "device", device)

	// For hw: devices we need the card number for D-Bus reservation on
	// pause/resume. For non-hw devices (default, pulse, etc.) we skip it.
	hwDevice := isHWDevice(device)
	var cardNum int
	if hwDevice {
		var err error
		cardNum, err = parseCardNum(device)
		if err != nil {
			logger.L.Error("playbackLoop: cannot parse card number", "device", device, "err", err)
			releaseReservation()
			return
		}
	}

	body, stream, err := openStream(ctx, opener)
	if err != nil {
		logger.L.Error("failed to open stream", "err", err)
		releaseReservation()
		return
	}

	info := stream.Info
	sampleRate := info.SampleRate
	channels := info.NChannels
	bits := info.BitsPerSample

	logger.L.Debug("FLAC stream",
		"rate", sampleRate,
		"channels", channels,
		"bits", bits,
		"samples", info.NSamples,
	)

	p.muInfo.Lock()
	p.sampleRate = sampleRate
	p.channels = channels
	p.bitsPerSample = bits
	p.totalSamples = info.NSamples
	p.muInfo.Unlock()

	// reacquireALSA reopens the ALSA device (and D-Bus reservation for hw:
	// devices). Used after releasing on pause.
	reacquireALSA := func() (*alsaHandle, func(), error) {
		var rel func()
		if hwDevice {
			var rerr error
			rel, rerr = reserveALSADevice(cardNum)
			if rerr != nil {
				return nil, nil, rerr
			}
		} else {
			rel = func() {}
		}
		a, aerr := openALSA(device, channels, sampleRate, bits)
		if aerr != nil {
			rel()
			return nil, nil, aerr
		}
		return a, rel, nil
	}

	ah, err := openALSA(device, channels, sampleRate, bits)
	if err != nil {
		logger.L.Error("openALSA failed", "device", device, "err", err)
		releaseReservation()
		return
	}
	logger.L.Debug("ALSA opened",
		"device", device,
		"format", ah.format,
		"bps", ah.bytesPerSample,
		"significantBits", ah.significantBits,
		"srcBits", bits,
		"rate_requested", sampleRate,
		"rate_negotiated", ah.rate,
		"period_size", ah.periodSize,
		"buffer_size", ah.bufferSize,
	)
	defer func() {
		// ah is nil-ed by the pause path once ALSA is released so other apps
		// can use the DAC. If the loop then exits via ctx cancellation (e.g.
		// the user skipped while paused), we must not double-close -- that
		// segfaults in snd_pcm_drain against a freed handle. Same reasoning
		// for releaseReservation: the pause path replaces it with a no-op
		// after releasing.
		if ah != nil {
			closeALSA(ah)
		}
		releaseReservation()
	}()

	bps := ah.bytesPerSample

	// streamLoop runs the decode->ALSA pipeline for the current HTTP stream.
	// Returns (seekTarget, true) if a seek was requested, or (0, false) when
	// the stream ends naturally or the context is cancelled.
	type pcmBuf struct {
		data    []byte
		nFrames int
	}

	streamLoop := func(skipSamples uint64) (seekTarget uint64, doSeek bool) {
		p.mu.Lock()
		skipCh := p.skipCh
		p.mu.Unlock()

		stopDecode := make(chan struct{})
		pcmCh := make(chan pcmBuf, 2)

		// Decode goroutine: reads FLAC frames, converts to PCM, sends to pcmCh.
		go func() {
			defer close(pcmCh)
			var skipped uint64
			for skipped < skipSamples {
				select {
				case <-ctx.Done():
					return
				case <-stopDecode:
					return
				default:
				}
				frame, ferr := stream.ParseNext()
				if ferr != nil {
					return
				}
				skipped += uint64(frame.BlockSize)
			}
			atomic.StoreUint64(&p.samplesPlayed, skipped)

			for {
				select {
				case <-ctx.Done():
					return
				case <-stopDecode:
					return
				default:
				}
				frame, ferr := stream.ParseNext()
				if ferr != nil {
					logger.L.Debug("FLAC decode done", "err", ferr)
					return
				}
				n := int(frame.BlockSize)
				buf := make([]byte, n*int(channels)*bps)
				vol := math.Float64frombits(atomic.LoadUint64(&p.volumeBits))
				for i := 0; i < n; i++ {
					for ch := 0; ch < int(channels); ch++ {
						s := frame.Subframes[ch].Samples[i]
						if vol != 1.0 {
							s = int32(float64(s) * vol)
						}
						off := (i*int(channels) + ch) * bps
						switch ah.format {
						case C.SND_PCM_FORMAT_S16_LE:
							binary.LittleEndian.PutUint16(buf[off:], uint16(int16(s)))
						case C.SND_PCM_FORMAT_S24_3LE:
							buf[off] = byte(s)
							buf[off+1] = byte(s >> 8)
							buf[off+2] = byte(s >> 16)
						case C.SND_PCM_FORMAT_S24_LE:
							shift := uint(ah.significantBits - int(bits))
							binary.LittleEndian.PutUint32(buf[off:], uint32(int32(s)<<shift))
						case C.SND_PCM_FORMAT_S32_LE:
							shift := uint(ah.significantBits - int(bits))
							binary.LittleEndian.PutUint32(buf[off:], uint32(int32(s)<<shift))
						}
					}
				}
				select {
				case pcmCh <- pcmBuf{data: buf, nFrames: n}:
				case <-ctx.Done():
					return
				case <-stopDecode:
					return
				}
			}
		}()

		returnSeek := func(target uint64) (uint64, bool) {
			close(stopDecode)
			for range pcmCh {
			}
			C.snd_pcm_drop(ah.pcm)
			C.snd_pcm_prepare(ah.pcm)
			return target, true
		}

		// Write loop: reads PCM buffers and writes to ALSA.
		for pcm := range pcmCh {
			framesDone := 0
			for framesDone < pcm.nFrames {
				select {
				case target := <-p.seekCh:
					return returnSeek(target)
				default:
				}

				select {
				case <-skipCh:
					C.snd_pcm_drop(ah.pcm)
					C.snd_pcm_prepare(ah.pcm)
					close(stopDecode)
					for range pcmCh {
					}
					return 0, false
				default:
				}

				// Pause: release the ALSA device so other apps can use the DAC.
				// After the release, ah and releaseReservation are cleared so
				// the deferred cleanup (and any mid-pause exit paths) don't
				// double-close/double-release -- both are reassigned when the
				// device is reacquired on resume or pause+seek.
				if atomic.LoadUint32(&p.paused) == 1 {
					C.snd_pcm_drop(ah.pcm)
					closeALSA(ah)
					releaseReservation()
					ah = nil
					releaseReservation = func() {}
					logger.L.Debug("paused: ALSA device released")

					for atomic.LoadUint32(&p.paused) == 1 {
						select {
						case target := <-p.seekCh:
							newAH, newRel, raErr := reacquireALSA()
							if raErr != nil {
								logger.L.Error("reacquire ALSA after pause+seek failed", "err", raErr)
								close(stopDecode)
								for range pcmCh {
								}
								return 0, false
							}
							ah = newAH
							releaseReservation = newRel
							return returnSeek(target)
						case <-skipCh:
							close(stopDecode)
							for range pcmCh {
							}
							return 0, false
						case <-ctx.Done():
							close(stopDecode)
							for range pcmCh {
							}
							return 0, false
						case <-time.After(20 * time.Millisecond):
						}
					}

					// Resume: reacquire the device.
					newAH, newRel, raErr := reacquireALSA()
					if raErr != nil {
						logger.L.Error("reacquire ALSA on resume failed", "err", raErr)
						close(stopDecode)
						for range pcmCh {
						}
						return 0, false
					}
					ah = newAH
					releaseReservation = newRel
					logger.L.Debug("resumed: ALSA device reacquired")
					break
				}

				select {
				case <-ctx.Done():
					C.snd_pcm_drop(ah.pcm)
					close(stopDecode)
					for range pcmCh {
					}
					return 0, false
				default:
				}

				off := framesDone * int(channels) * bps
				written := C.snd_pcm_writei(ah.pcm, unsafe.Pointer(&pcm.data[off]), C.snd_pcm_uframes_t(pcm.nFrames-framesDone))
				if written < 0 {
					errStr := C.GoString(C.snd_strerror(C.int(written)))
					logger.L.Warn("snd_pcm_writei error, recovering", "err", errStr)
					if rc := C.snd_pcm_recover(ah.pcm, C.int(written), C.int(1)); rc < 0 {
						logger.L.Error("snd_pcm_recover failed, stopping playback",
							"err", C.GoString(C.snd_strerror(rc)))
						close(stopDecode)
						for range pcmCh {
						}
						return 0, false
					}
					continue
				}
				framesDone += int(written)
			}
			atomic.AddUint64(&p.samplesPlayed, uint64(pcm.nFrames))
		}
		return 0, false
	}

	// Outer loop: play the current stream, then wait for a next-track opener
	// or exit. This keeps the ALSA device open between consecutive tracks
	// for gapless playback.
	for {
		seekTarget, doSeek := streamLoop(0)
		for doSeek {
			_ = body.Close()
			body, stream, err = openStream(ctx, opener)
			if err != nil {
				logger.L.Error("failed to reopen stream for seek", "err", err)
				return
			}
			seekTarget, doSeek = streamLoop(seekTarget)
		}

		_ = body.Close()

		p.mu.Lock()
		oldDone := p.doneCh
		p.doneCh = nil
		p.mu.Unlock()
		if oldDone != nil {
			close(oldDone)
		}

		// Wait for next-track opener or exit.
		select {
		case nextOpener := <-p.nextOpenerCh:
			logger.L.Debug("transitioning to next track")
			opener = nextOpener

			body, stream, err = openStream(ctx, opener)
			if err != nil {
				logger.L.Error("failed to open next stream", "err", err)
				return
			}

			newInfo := stream.Info

			// If the audio format changed, reopen the ALSA device.
			if newInfo.SampleRate != sampleRate || newInfo.NChannels != channels || newInfo.BitsPerSample != bits {
				logger.L.Debug("format change, reopening ALSA",
					"oldRate", sampleRate, "newRate", newInfo.SampleRate,
					"oldCh", channels, "newCh", newInfo.NChannels,
					"oldBits", bits, "newBits", newInfo.BitsPerSample)
				closeALSA(ah)
				sampleRate = newInfo.SampleRate
				channels = newInfo.NChannels
				bits = newInfo.BitsPerSample
				ah, err = openALSA(device, channels, sampleRate, bits)
				if err != nil {
					logger.L.Error("openALSA failed for next track", "err", err)
					_ = body.Close()
					return
				}
				bps = ah.bytesPerSample
				reacquireALSA = func() (*alsaHandle, func(), error) {
					var rel func()
					if hwDevice {
						var rerr error
						rel, rerr = reserveALSADevice(cardNum)
						if rerr != nil {
							return nil, nil, rerr
						}
					} else {
						rel = func() {}
					}
					a, aerr := openALSA(device, channels, sampleRate, bits)
					if aerr != nil {
						rel()
						return nil, nil, aerr
					}
					return a, rel, nil
				}
			} else {
				sampleRate = newInfo.SampleRate
				channels = newInfo.NChannels
				bits = newInfo.BitsPerSample
			}

			p.muInfo.Lock()
			p.sampleRate = sampleRate
			p.channels = channels
			p.bitsPerSample = bits
			p.totalSamples = newInfo.NSamples
			p.muInfo.Unlock()

			atomic.StoreUint64(&p.samplesPlayed, 0)
			select {
			case <-p.seekCh:
			default:
			}

			p.mu.Lock()
			p.doneCh = p.transitionDoneCh
			p.transitionDoneCh = nil
			p.mu.Unlock()

			logger.L.Debug("FLAC stream (next track)",
				"rate", sampleRate,
				"channels", channels,
				"bits", bits,
				"samples", newInfo.NSamples,
			)
			continue

		case <-ctx.Done():
			return

		case <-time.After(5 * time.Second):
			logger.L.Debug("no next track within timeout, closing ALSA")
			return
		}
	}
}

// --- Public API ---

// Pause toggles the paused state.
func (p *Player) Pause() error {
	if atomic.LoadUint32(&p.paused) == 0 {
		atomic.StoreUint32(&p.paused, 1)
	} else {
		atomic.StoreUint32(&p.paused, 0)
	}
	return nil
}

// SetVolume sets the playback volume (0-100).
func (p *Player) SetVolume(vol float64) error {
	atomic.StoreUint64(&p.volumeBits, math.Float64bits(vol/100.0))
	return nil
}

// GetVolume returns the current volume (0-100).
func (p *Player) GetVolume() (float64, error) {
	return math.Float64frombits(atomic.LoadUint64(&p.volumeBits)) * 100.0, nil
}

// GetPosition returns the current playback position in seconds.
func (p *Player) GetPosition() (float64, error) {
	p.muInfo.RLock()
	sr := p.sampleRate
	p.muInfo.RUnlock()
	if sr == 0 {
		return 0, nil
	}
	return float64(atomic.LoadUint64(&p.samplesPlayed)) / float64(sr), nil
}

// GetDuration returns the total duration of the current track in seconds.
func (p *Player) GetDuration() (float64, error) {
	p.muInfo.RLock()
	sr := p.sampleRate
	ts := p.totalSamples
	p.muInfo.RUnlock()
	if sr == 0 {
		return 0, nil
	}
	return float64(ts) / float64(sr), nil
}

// GetFormat returns the current track's audio format: sample rate in Hz,
// bit depth, and channel count. All fields are zero before the first
// track's FLAC header has been parsed.
func (p *Player) GetFormat() (rate uint32, bits uint8, channels uint8) {
	p.muInfo.RLock()
	defer p.muInfo.RUnlock()
	return p.sampleRate, p.bitsPerSample, p.channels
}

// Seek jumps to the given absolute position in seconds without interrupting
// the ALSA device or D-Bus reservation. The playback loop re-fetches the HTTP
// stream and skips to the target in-place.
func (p *Player) Seek(seconds float64) error {
	p.muInfo.RLock()
	sr := p.sampleRate
	ts := p.totalSamples
	p.muInfo.RUnlock()
	if sr == 0 {
		return nil
	}

	if seconds < 0 {
		seconds = 0
	}
	maxSeconds := float64(ts) / float64(sr)
	if seconds > maxSeconds {
		seconds = maxSeconds
	}

	target := uint64(seconds * float64(sr))

	select {
	case <-p.seekCh:
	default:
	}
	p.seekCh <- target
	return nil
}

// Done returns a channel that is closed when the current track finishes
// playing naturally (not when stopped or cancelled).
func (p *Player) Done() <-chan struct{} {
	p.mu.Lock()
	ch := p.doneCh
	p.mu.Unlock()
	return ch
}

// Close stops playback and releases all resources.
func (p *Player) Close() {
	p.stop()
}
