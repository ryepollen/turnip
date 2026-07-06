package proc

import (
	"regexp"
	"strconv"
	"strings"
)

// subTimeRe matches SRT (00:01:02,500) and VTT (00:01:02.500 or 01:02.500) timestamps
var subTimeRe = regexp.MustCompile(`(?:(\d+):)?(\d{1,2}):(\d{2})[.,](\d{1,3})`)

// subTagRe strips inline markup like <c>, <00:00:01.000>, {\an8}
var subTagRe = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)

// parseSubTimestamp converts a subtitle timestamp to seconds
func parseSubTimestamp(s string) (float64, bool) {
	m := subTimeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	h := 0
	if m[1] != "" {
		h, _ = strconv.Atoi(m[1])
	}
	mm, _ := strconv.Atoi(m[2])
	ss, _ := strconv.Atoi(m[3])
	ms, _ := strconv.Atoi(m[4])
	return float64(h)*3600 + float64(mm)*60 + float64(ss) + float64(ms)/1000, true
}

// ParseSubtitleSegments parses SRT or VTT content into timed segments,
// keeping timestamps (unlike ParseSubtitles, which flattens to plain text).
// Used for official podcast transcripts and manual YouTube subtitles, so the
// L1 markdown keeps its [MM:SS] markers.
func ParseSubtitleSegments(content string) []TranscriptSegment {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")

	var segs []TranscriptSegment
	var cur *TranscriptSegment
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.Text) != "" {
			cur.Text = strings.TrimSpace(cur.Text)
			segs = append(segs, *cur)
		}
		cur = nil
	}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.Contains(line, "-->") {
			flush()
			parts := strings.SplitN(line, "-->", 2)
			start, ok := parseSubTimestamp(parts[0])
			if !ok {
				continue
			}
			cur = &TranscriptSegment{Start: start}
			if end, eok := parseSubTimestamp(parts[1]); eok {
				cur.End = end
			}
			continue
		}
		if line == "" {
			flush()
			continue
		}
		if cur == nil {
			continue // WEBVTT header, NOTE blocks, cue counters
		}
		text := strings.TrimSpace(subTagRe.ReplaceAllString(line, ""))
		if text == "" {
			continue
		}
		if cur.Text != "" {
			cur.Text += " "
		}
		cur.Text += text
	}
	flush()

	// rolling auto-subs repeat the same text in consecutive cues — dedupe
	var out []TranscriptSegment
	for _, seg := range segs {
		if len(out) > 0 && out[len(out)-1].Text == seg.Text {
			out[len(out)-1].End = seg.End
			continue
		}
		out = append(out, seg)
	}
	return out
}

// segmentsToTranscript wraps parsed segments into a Transcript
func segmentsToTranscript(segs []TranscriptSegment) *Transcript {
	if len(segs) == 0 {
		return nil
	}
	tr := &Transcript{Segments: segs}
	tr.DurationSec = segs[len(segs)-1].End
	return tr
}
