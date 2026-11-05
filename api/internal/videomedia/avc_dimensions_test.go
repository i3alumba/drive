package videomedia

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/mp4"
)

func croppedSPS(t *testing.T, macroWidth, macroHeight, cropRight, cropBottom uint) []byte {
	t.Helper()
	w := bits.NewFixedSliceWriter(128)
	w.WriteUint8(66)
	w.WriteUint8(0)
	w.WriteUint8(42)
	w.WriteExpGolomb(0) // SPS id
	w.WriteExpGolomb(0) // log2_max_frame_num_minus4
	w.WriteExpGolomb(0)
	w.WriteExpGolomb(0) // POC type and log2 maximum
	w.WriteExpGolomb(1)
	w.WriteBits(0, 1) // reference count, no frame gaps
	w.WriteExpGolomb(macroWidth - 1)
	w.WriteExpGolomb(macroHeight - 1)
	w.WriteBits(1, 1)
	w.WriteBits(1, 1)
	w.WriteBits(1, 1) // progressive, direct inference, cropping
	w.WriteExpGolomb(0)
	w.WriteExpGolomb(cropRight)
	w.WriteExpGolomb(0)
	w.WriteExpGolomb(cropBottom)
	w.WriteBits(0, 1)
	w.WriteBits(1, 1)
	w.FlushBits() // no VUI, rbsp trailing bits
	if err := w.AccError(); err != nil {
		t.Fatal(err)
	}
	result := []byte{0x67}
	zeros := 0
	for _, b := range w.Bytes() {
		if zeros == 2 && b <= 3 {
			result = append(result, 3)
			zeros = 0
		}
		result = append(result, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return result
}

func TestValidateCodedPictureBoundsBeforeCropping(t *testing.T) {
	init, _ := fixture(t, false, 40, false)
	for _, tc := range []struct {
		name                                 string
		mw, mh, right, bottom, width, height uint
		reject                               bool
	}{
		{"oversized coded picture hidden by cropping", 4096, 4096, 32688, 32723, 160, 90, true},
		{"ordinary cropped 1080p padding", 120, 68, 0, 4, 1920, 1080, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sps := croppedSPS(t, tc.mw, tc.mh, tc.right, tc.bottom)
			parsed, err := avc.ParseSPSNALUnit(sps, false)
			if err != nil || parsed.Width != tc.width || parsed.Height != tc.height {
				t.Fatalf("invalid regression fixture: %+v %v", parsed, err)
			}
			nodes, err := preflight(init, true)
			if err != nil {
				t.Fatal(err)
			}
			box, err := decode(init, nodes[1])
			if err != nil {
				t.Fatal(err)
			}
			moov := box.(*mp4.MoovBox)
			video := moov.Traks[0].Mdia.Minf.Stbl.Stsd.AvcX
			video.Width = uint16(tc.width)
			video.Height = uint16(tc.height)
			video.AvcC.SPSnalus = [][]byte{sps}
			video.AvcC.AVCProfileIndication = 66
			video.AvcC.ProfileCompatibility = 0
			video.AvcC.AVCLevelIndication = 42
			var output bytes.Buffer
			first, err := decode(init, nodes[0])
			if err != nil {
				t.Fatal(err)
			}
			if err = first.Encode(&output); err != nil {
				t.Fatal(err)
			}
			if err = moov.Encode(&output); err != nil {
				t.Fatal(err)
			}
			_, err = ValidateInit(output.Bytes())
			if tc.reject && err == nil {
				t.Fatal("accepted oversized coded picture after cropping")
			}
			if !tc.reject && err != nil {
				t.Fatalf("legitimate macroblock padding rejected: %v", err)
			}
		})
	}
}
