package videoworker

import "testing"

func TestHLSRemuxRequiresEstablishedAACSubset(t *testing.T) {
	video := stream{CodecName: "h264", PixFmt: "yuv420p", Width: 160, Height: 90}
	for _, tc := range []struct {
		name, profile, rate string
		channels            int
		want                bool
	}{
		{"mono LC", "LC", "48000", 1, true}, {"stereo LC", "LC", "44100", 2, true},
		{"surround LC", "LC", "48000", 6, false}, {"HE AAC", "HE-AAC", "48000", 2, false},
		{"unknown profile", "", "48000", 2, false}, {"unknown channels", "LC", "48000", 0, false},
		{"unknown rate", "LC", "", 2, false}, {"invalid rate", "LC", "NaN", 2, false},
		{"out of subset rate", "LC", "192000", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := media{Video: video, Audio: &stream{CodecName: "aac", Profile: tc.profile, SampleRate: tc.rate, Channels: tc.channels}}
			if got := m.hlsCompatible(); got != tc.want {
				t.Fatalf("compatibility %t want %t", got, tc.want)
			}
		})
	}
	if !(media{Video: video}).hlsCompatible() {
		t.Fatal("video-only remux rejected")
	}
}
