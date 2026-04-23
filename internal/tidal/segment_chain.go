package tidal

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// segmentChainReader is an io.ReadCloser that yields the concatenated bodies
// of a sequence of HTTP URLs. It's used to stream DASH playback: one init
// segment followed by N media segments, produced back-to-back as if the
// whole track were a single fMP4 file. The downstream MP4 demuxer can't tell
// the difference because fMP4 is defined to be exactly a concatenation of an
// init segment (ftyp+moov) and any number of media segments (moof+mdat).
//
// Segments are fetched lazily -- only when the current segment's body has
// been fully consumed does the next GET start -- so memory stays bounded
// regardless of track length. A single HTTP connection is kept in flight at
// a time; we don't pre-fetch, which is acceptable for bounded audio
// bitrates (FLAC 24/96 at about 4 Mb/s easily rides a warm keep-alive
// connection without underrun on the ALSA side).
type segmentChainReader struct {
	ctx    context.Context
	client *http.Client
	urls   []string

	next int            // index of the next URL to fetch
	body io.ReadCloser  // current in-flight body, or nil
	err  error          // terminal error, returned to all subsequent reads
}

func newSegmentChainReader(ctx context.Context, urls []string) *segmentChainReader {
	return &segmentChainReader{
		ctx:    ctx,
		client: http.DefaultClient,
		urls:   urls,
	}
}

// Read proxies to the current segment body, advancing to the next segment
// when the current one hits EOF. Returning (0, nil) is avoided -- if there
// are more segments to try, we loop internally and issue the next GET
// before returning so callers don't mistake a segment boundary for a
// pathological zero-length read.
func (r *segmentChainReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for {
		if r.body == nil {
			if r.next >= len(r.urls) {
				r.err = io.EOF
				return 0, io.EOF
			}
			if err := r.openNext(); err != nil {
				r.err = err
				return 0, err
			}
		}
		n, err := r.body.Read(p)
		if n > 0 {
			if err == io.EOF {
				// Return the bytes now; the next Read will notice body is
				// drained and advance to the next segment. This avoids
				// interleaving data + EOF in the same return, which some
				// readers misinterpret as "no more data ever".
				_ = r.body.Close()
				r.body = nil
			}
			return n, nil
		}
		if err == nil {
			// 0, nil from the underlying body -- treat as transient and
			// retry within the same segment.
			continue
		}
		_ = r.body.Close()
		r.body = nil
		if err == io.EOF {
			// Move on to the next segment on the next loop iteration.
			continue
		}
		r.err = err
		return 0, err
	}
}

// openNext issues the GET for r.urls[r.next] and advances r.next. Non-200
// responses are treated as hard errors -- retrying within a single stream
// isn't worth the complexity since the whole pipeline will be restarted
// with a fresh manifest if the user reseeks or skips.
func (r *segmentChainReader) openNext() error {
	u := r.urls[r.next]
	r.next++
	req, err := http.NewRequestWithContext(r.ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return fmt.Errorf("segment GET %s: http %d", u, resp.StatusCode)
	}
	r.body = resp.Body
	return nil
}

// Close releases the in-flight body (if any). It's idempotent. Remaining
// URLs that were never opened don't need explicit cleanup.
func (r *segmentChainReader) Close() error {
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}
