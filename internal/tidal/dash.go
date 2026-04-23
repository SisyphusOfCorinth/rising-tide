package tidal

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// This file contains a minimal DASH MPD XML parser sufficient for Tidal's
// HI_RES_LOSSLESS manifests. Tidal ships a static MPD with a single audio
// AdaptationSet, a single Representation, and a SegmentTemplate backed by a
// SegmentTimeline. We only need to derive:
//   - the codec string (to verify FLAC before kicking off playback),
//   - the initialization segment URL,
//   - the full ordered list of media segment URLs.
//
// Anything more elaborate (multi-period MPDs, multi-representation ABR,
// BaseURL layering, $Time$ addressing, multi-bitrate audio) would require a
// full MPEG-DASH implementation and is out of scope.

// dashMPD models the subset of MPD we need. Unused XML attributes are simply
// not declared so the decoder ignores them.
type dashMPD struct {
	XMLName xml.Name     `xml:"MPD"`
	Periods []dashPeriod `xml:"Period"`
}

type dashPeriod struct {
	AdaptationSets []dashAdaptationSet `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	ContentType     string             `xml:"contentType,attr"`
	MimeType        string             `xml:"mimeType,attr"`
	Representations []dashRepresentation `xml:"Representation"`
}

type dashRepresentation struct {
	ID              string               `xml:"id,attr"`
	Bandwidth       string               `xml:"bandwidth,attr"`
	Codecs          string               `xml:"codecs,attr"`
	SegmentTemplate *dashSegmentTemplate `xml:"SegmentTemplate"`
}

type dashSegmentTemplate struct {
	Timescale       string                `xml:"timescale,attr"`
	StartNumber     string                `xml:"startNumber,attr"`
	Initialization  string                `xml:"initialization,attr"`
	Media           string                `xml:"media,attr"`
	SegmentTimeline *dashSegmentTimeline  `xml:"SegmentTimeline"`
}

type dashSegmentTimeline struct {
	Segments []dashTimelineSegment `xml:"S"`
}

type dashTimelineSegment struct {
	T string `xml:"t,attr"` // start time (unused, but carried for completeness)
	D string `xml:"d,attr"` // duration
	R string `xml:"r,attr"` // repeat count; "" = single segment
}

// parseDashMPD parses a DASH MPD XML document and returns the codec string,
// the initialisation segment URL, and the ordered list of media segment
// URLs. Only audio AdaptationSets are considered, and only the first
// Representation within the first such set is used (Tidal streams one audio
// Representation per quality tier).
func parseDashMPD(data []byte) (codec, initURL string, mediaURLs []string, err error) {
	var mpd dashMPD
	if err := xml.Unmarshal(data, &mpd); err != nil {
		return "", "", nil, fmt.Errorf("parse MPD: %w", err)
	}
	rep, err := selectAudioRepresentation(&mpd)
	if err != nil {
		return "", "", nil, err
	}
	tmpl := rep.SegmentTemplate
	if tmpl == nil {
		return "", "", nil, fmt.Errorf("representation %q has no SegmentTemplate", rep.ID)
	}
	if tmpl.Initialization == "" || tmpl.Media == "" {
		return "", "", nil, fmt.Errorf("SegmentTemplate missing initialization or media attribute")
	}

	startNumber := 1
	if tmpl.StartNumber != "" {
		if n, perr := strconv.Atoi(tmpl.StartNumber); perr == nil {
			startNumber = n
		}
	}

	count, err := countSegments(tmpl.SegmentTimeline)
	if err != nil {
		return "", "", nil, err
	}
	if count == 0 {
		return "", "", nil, fmt.Errorf("SegmentTimeline has no segments")
	}

	initURL = substituteTemplate(tmpl.Initialization, rep.ID, 0)
	mediaURLs = make([]string, count)
	for i := 0; i < count; i++ {
		mediaURLs[i] = substituteTemplate(tmpl.Media, rep.ID, startNumber+i)
	}
	return rep.Codecs, initURL, mediaURLs, nil
}

// selectAudioRepresentation returns the first Representation within the first
// audio AdaptationSet. It accepts either contentType="audio" or a mimeType
// beginning with "audio/" so Tidal's authoring quirks don't matter.
func selectAudioRepresentation(mpd *dashMPD) (*dashRepresentation, error) {
	for i := range mpd.Periods {
		for j := range mpd.Periods[i].AdaptationSets {
			set := &mpd.Periods[i].AdaptationSets[j]
			if !isAudioSet(set) {
				continue
			}
			if len(set.Representations) == 0 {
				continue
			}
			return &set.Representations[0], nil
		}
	}
	return nil, fmt.Errorf("no audio Representation found in MPD")
}

func isAudioSet(set *dashAdaptationSet) bool {
	if set.ContentType == "audio" {
		return true
	}
	return strings.HasPrefix(set.MimeType, "audio/")
}

// countSegments totals up the segments described by a SegmentTimeline. Each
// <S> element contributes (r+1) segments, where r defaults to 0. A nil
// timeline counts as zero segments; the caller reports that as an error.
func countSegments(tl *dashSegmentTimeline) (int, error) {
	if tl == nil {
		return 0, nil
	}
	total := 0
	for i, s := range tl.Segments {
		repeat := 0
		if s.R != "" {
			r, perr := strconv.Atoi(s.R)
			if perr != nil {
				return 0, fmt.Errorf("SegmentTimeline S[%d] has invalid r=%q", i, s.R)
			}
			repeat = r
			// DASH allows r = -1 meaning "continue to end of period". Tidal
			// manifests in practice use explicit repeat counts, so we don't
			// implement period-duration resolution here.
			if repeat < 0 {
				return 0, fmt.Errorf("SegmentTimeline S[%d] has negative r=%d which requires Period duration resolution (unsupported)", i, repeat)
			}
		}
		total += repeat + 1
	}
	return total, nil
}

// substituteTemplate expands the DASH template placeholders used by Tidal:
// $RepresentationID$ and $Number$. Other tokens ($Time$, $Bandwidth$,
// formatted variants like $Number%05d$) aren't produced by Tidal for audio
// manifests today -- if they start appearing, this is where to extend.
func substituteTemplate(tmpl, repID string, number int) string {
	out := strings.ReplaceAll(tmpl, "$RepresentationID$", repID)
	out = strings.ReplaceAll(out, "$Number$", strconv.Itoa(number))
	return out
}
