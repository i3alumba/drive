package videomedia

import (
	"encoding/hex"
	"testing"

	"github.com/Eyevinn/mp4ff/avc"
)

func TestColorMetadataNarrowSyntax(t *testing.T) {
	valid, _ := hex.DecodeString("6e636c7800010001000100")
	if _, err := preflightColor(valid); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"short", "trailing", "reserved", "hdr", "icc", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			d := append([]byte(nil), valid...)
			switch kind {
			case "short":
				d = d[:10]
			case "trailing":
				d = append(d, 0)
			case "reserved":
				d[10] = 1
			case "hdr":
				d[5] = 9
			case "icc":
				copy(d, "prof")
			case "unknown":
				copy(d, "zzzz")
			}
			if _, err := preflightColor(d); err == nil {
				t.Fatal("accepted unsupported metadata")
			}
		})
	}
	nclc := append([]byte(nil), valid[:10]...)
	copy(nclc, "nclc")
	if _, err := preflightColor(nclc); err != nil {
		t.Fatal(err)
	}
}
func TestColorMetadataSPSConsistency(t *testing.T) {
	c := colorMetadata{primaries: 1, transfer: 1, matrix: 1, hasRange: true}
	if err := validateColorSPS(c, &avc.SPS{}); err != nil {
		t.Fatal("missing VUI is not contradiction", err)
	}
	v := &avc.VUIParameters{VideoSignalTypePresentFlag: true, VideoFullRangeFlag: true}
	if err := validateColorSPS(c, &avc.SPS{VUI: v}); err == nil {
		t.Fatal("accepted range conflict")
	}
	c.hasRange = false
	if err := validateColorSPS(c, &avc.SPS{VUI: v}); err != nil {
		t.Fatal("nclc inferred range", err)
	}
	v.ColourDescriptionFlag = true
	v.ColourPrimaries = 2
	v.TransferCharacteristics = 2
	v.MatrixCoefficients = 2
	if err := validateColorSPS(c, &avc.SPS{VUI: v}); err != nil {
		t.Fatal("unspecified is not contradictory", err)
	}
	v.TransferCharacteristics = 16
	if err := validateColorSPS(c, &avc.SPS{VUI: v}); err == nil {
		t.Fatal("accepted HDR signalling")
	}
}

func TestColorMetadataInValidatedInit(t *testing.T) {
	init, segments := fixture(t, true, 40, false)
	entry := findSpan(t, init, true, "avc1")
	color, _ := hex.DecodeString("00000013636f6c726e636c7800010001000100")
	add := func(payload []byte) []byte {
		nodes, err := preflight(init, true)
		if err != nil {
			t.Fatal(err)
		}
		copyInit := append([]byte(nil), init...)
		var grow func([]*boxSpan)
		grow = func(nodes []*boxSpan) {
			for _, n := range nodes {
				if n.start <= entry.start && n.end >= entry.end {
					put32(copyInit[n.start:], uint32(n.end-n.start+len(payload)))
				}
				grow(n.children)
			}
		}
		grow(nodes)
		result := append([]byte(nil), copyInit[:entry.end]...)
		result = append(result, payload...)
		result = append(result, copyInit[entry.end:]...)
		return result
	}
	config, err := ValidateInit(add(color))
	if err != nil {
		t.Fatal(err)
	}
	var previous Timeline
	for i, b := range segments {
		s, err := ValidateSegment(b, config, previous)
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		previous = s.Timeline
	}
	if _, err := ValidateInit(add(append(append([]byte(nil), color...), color...))); err == nil {
		t.Fatal("accepted duplicate colr")
	}
	for _, header := range []byte{0, 1} {
		bad := append([]byte(nil), color...)
		put32(bad, uint32(header))
		if _, err := ValidateInit(add(bad)); err == nil {
			t.Fatal("accepted noncanonical colr header")
		}
	}
}
