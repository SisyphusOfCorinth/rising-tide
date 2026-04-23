// Package player -- MP4-to-FLAC demuxer.
//
// In 2026, Tidal's streaming CDN switched from delivering raw FLAC bytestreams
// to delivering FLAC audio wrapped in an MP4/ISOBMFF container (both LOSSLESS
// and HI_RES_LOSSLESS qualities). The mewkiz/flac decoder used by the
// playback loop only understands raw FLAC -- so without demuxing, every
// stream fails with "invalid FLAC signature" and the UI cascades through
// the queue skipping every track.
//
// This file wraps an MP4 HTTP body and returns a reader that yields a
// reconstructed raw FLAC bytestream: the fLaC magic + metadata blocks lifted
// from the dfLa box + FLAC frames concatenated from each mdat box. No audio
// data is touched; the bytes-per-frame flowing through this demuxer are
// identical to what mewkiz/flac would have received before the Tidal change,
// so playback remains bit-perfect.

package player

import (
	"fmt"
	"io"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/SisyphusOfCorinth/rising-tide/internal/logger"
)

// FlacFromMp4Reader wraps r (an MP4/ISOBMFF body such as Tidal's _37.mp4
// stream) and returns a reader that emits an equivalent raw FLAC bytestream
// suitable for mewkiz/flac.
//
// The demux runs in a goroutine and pipes output to the returned reader so
// playback can begin as soon as the moov box arrives. Closing the returned
// ReadCloser stops the goroutine (the pipe write will error and the demux
// loop exits on the next box).
//
// Layout requirement: moov must precede mdat. Tidal streams moov-first to
// support progressive playback, so this is satisfied in practice. If a
// moov-last file is ever encountered, an error is surfaced through the pipe.
func FlacFromMp4Reader(r io.Reader) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := demuxMp4ToFlac(r, pw)
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// demuxMp4ToFlac reads top-level MP4 boxes from r sequentially and writes a
// raw FLAC bytestream to w. It handles both fragmented MP4 (multiple
// moof+mdat pairs, as used by CMAF/DASH) and unfragmented MP4 (single mdat
// after moov), since in both cases FLAC frames are stored as a concatenation
// of self-synchronising frame bytes inside mdat boxes.
func demuxMp4ToFlac(r io.Reader, w io.Writer) error {
	var startPos uint64
	headerWritten := false

	for {
		box, err := mp4.DecodeBox(startPos, r)
		if err == io.EOF {
			if !headerWritten {
				return fmt.Errorf("mp4 stream ended before moov box was seen")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("mp4 decode: %w", err)
		}
		startPos += box.Size()

		switch b := box.(type) {
		case *mp4.MoovBox:
			if headerWritten {
				return fmt.Errorf("unexpected second moov box")
			}
			dfla, err := findDfLa(b)
			if err != nil {
				return err
			}
			if err := writeFlacHeader(w, dfla); err != nil {
				return err
			}
			headerWritten = true

		case *mp4.MdatBox:
			if !headerWritten {
				return fmt.Errorf("mp4 has mdat before moov (moov-last layout not supported for streaming)")
			}
			// mdat payload is a concatenation of FLAC frames. Each MP4 sample
			// is one FLAC frame, and FLAC frames are self-synchronising, so
			// we can pass the payload through verbatim without splitting on
			// sample boundaries from trun/stsz. This keeps the demuxer
			// allocation-light and lets mewkiz/flac handle frame parsing.
			if _, err := w.Write(b.Data); err != nil {
				return err
			}

		default:
			// ftyp, moof, sidx, free, skip, etc. -- not relevant to FLAC output.
		}
	}
}

// findDfLa walks moov -> trak[] -> mdia -> minf -> stbl -> stsd and returns
// the dfLa box carrying the FLAC STREAMINFO (plus any other FLAC metadata
// blocks the encoder embedded). It tolerates a few codec-tag variations:
// plain "fLaC" (per the FLAC-in-ISOBMFF spec), and "enca" (encrypted audio
// -- Tidal wraps some tiers in common encryption with no actual key work
// required for playback-time demux). The underlying sample entry carries
// the dfLa child either way.
func findDfLa(moov *mp4.MoovBox) (*mp4.DfLaBox, error) {
	var seenEntries []string
	for _, trak := range moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
			continue
		}
		stsd := trak.Mdia.Minf.Stbl.Stsd
		if stsd == nil {
			continue
		}
		for _, child := range stsd.Children {
			tag := child.Type()
			ase, ok := child.(*mp4.AudioSampleEntryBox)
			if !ok {
				seenEntries = append(seenEntries, tag)
				continue
			}
			if ase.DfLa != nil {
				logger.L.Debug("found FLAC track in mp4", "sampleEntry", tag)
				return ase.DfLa, nil
			}
			// Annotate mp4a entries with the ESDS ObjectType so we can
			// tell AAC (0x40) apart from non-standard codecs a remote
			// encoder might stuff into an mp4a wrapper.
			detail := tag
			if ase.Esds != nil && ase.Esds.DecConfigDescriptor != nil {
				detail = fmt.Sprintf("%s(oti=0x%02x)", tag, ase.Esds.DecConfigDescriptor.ObjectType)
			}
			seenEntries = append(seenEntries, detail)
		}
	}
	return nil, fmt.Errorf("no FLAC track (dfLa descriptor) found in moov; saw sample entries: %v", seenEntries)
}

// writeFlacHeader emits the raw FLAC stream prelude: the four-byte "fLaC"
// magic followed by the metadata blocks carried inside the dfLa box.
//
// Each FLAC metadata block is re-emitted in its native form (4-byte header
// plus block data). The last-metadata-block flag is forced on the last block
// so downstream decoders know when audio frames begin, even if the dfLa
// content didn't have the flag set correctly (mp4ff's decoder stops reading
// once it sees a last-block flag, but source streams may also omit it for
// older writers).
func writeFlacHeader(w io.Writer, dfla *mp4.DfLaBox) error {
	if len(dfla.MetadataBlocks) == 0 {
		return fmt.Errorf("dfLa has no metadata blocks (STREAMINFO is required)")
	}
	if _, err := w.Write([]byte("fLaC")); err != nil {
		return err
	}
	for i, block := range dfla.MetadataBlocks {
		header := [4]byte{
			block.BlockType & 0x7F,
			byte(block.Length >> 16),
			byte(block.Length >> 8),
			byte(block.Length),
		}
		if i == len(dfla.MetadataBlocks)-1 {
			header[0] |= 0x80 // last-metadata-block flag
		}
		if _, err := w.Write(header[:]); err != nil {
			return err
		}
		if _, err := w.Write(block.BlockData); err != nil {
			return err
		}
	}
	return nil
}
