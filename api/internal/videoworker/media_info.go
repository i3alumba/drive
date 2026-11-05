package videoworker

import (
	"math"
	"strconv"
)

type stream struct {
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	PixFmt     string `json:"pix_fmt"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Profile    string `json:"profile"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
}

type media struct {
	Video    stream
	Audio    *stream
	Duration float64
}

func validDuration(d float64) bool {
	return !math.IsNaN(d) && !math.IsInf(d, 0) && d > 0 && d <= 6*60*60
}

func durationMatches(expected, actual float64) bool {
	return validDuration(expected) && validDuration(actual) && math.Abs(expected-actual) <= 2 && actual >= expected-math.Max(0.1, expected*0.01)
}

// HLS remux must establish the stricter private AAC-LC subset. Unknown,
// surround or extended profiles use the existing stereo conversion path.
func (m media) hlsCompatible() bool {
	if !m.compatible() {
		return false
	}
	if m.Audio == nil {
		return true
	}
	rate, err := strconv.Atoi(m.Audio.SampleRate)
	return err == nil && m.Audio.Profile == "LC" && m.Audio.Channels >= 1 && m.Audio.Channels <= 2 && rate >= 7350 && rate <= 96000
}

func (m media) compatible() bool {
	return m.Video.CodecName == "h264" && m.Video.PixFmt == "yuv420p" && m.Video.Width > 0 && m.Video.Height > 0 && m.Video.Width <= 1920 && m.Video.Height <= 1080 && (m.Audio == nil || m.Audio.CodecName == "aac")
}
