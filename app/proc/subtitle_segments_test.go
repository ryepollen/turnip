package proc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSubtitleSegmentsVTT(t *testing.T) {
	vtt := `WEBVTT
Kind: captions

NOTE some comment

00:00:01.000 --> 00:00:04.500
Привет, это <c.colorE5E5E5>первая</c> реплика

00:00:04.500 --> 00:00:08.000
Вторая реплика
на двух строках

01:02:03.000 --> 01:02:05.000
Час спустя`

	segs := ParseSubtitleSegments(vtt)
	require.Len(t, segs, 3)
	assert.Equal(t, 1.0, segs[0].Start)
	assert.Equal(t, "Привет, это первая реплика", segs[0].Text)
	assert.Equal(t, "Вторая реплика на двух строках", segs[1].Text)
	assert.Equal(t, 3723.0, segs[2].Start)
	assert.Equal(t, 3725.0, segs[2].End)
}

func TestParseSubtitleSegmentsSRT(t *testing.T) {
	srt := `1
00:00:01,000 --> 00:00:04,000
First line

2
00:00:04,000 --> 00:00:07,000
First line

3
00:00:07,000 --> 00:00:09,500
Second line`

	segs := ParseSubtitleSegments(srt)
	require.Len(t, segs, 2, "consecutive duplicates merged")
	assert.Equal(t, "First line", segs[0].Text)
	assert.Equal(t, 7.0, segs[0].End, "merged cue extends the end")
	assert.Equal(t, "Second line", segs[1].Text)

	tr := segmentsToTranscript(segs)
	require.NotNil(t, tr)
	assert.Equal(t, 9.5, tr.DurationSec)

	assert.Nil(t, segmentsToTranscript(nil))
}

func TestParseSubTimestamp(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"00:00:01.000", 1, true},
		{"00:01:02,500", 62.5, true},
		{"01:02.500", 62.5, true}, // VTT short form
		{"1:02:03.000", 3723, true},
		{"garbage", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseSubTimestamp(tt.in)
		assert.Equal(t, tt.ok, ok, tt.in)
		assert.Equal(t, tt.want, got, tt.in)
	}
}
