package videomedia

import "github.com/Eyevinn/mp4ff/avc"

// colorMetadata accepts only SDR BT709 or unspecified signalling, not proof of
// pixel integrity, tone mapping, or HDR support.
type colorMetadata struct {
	primaries, transfer, matrix uint16
	hasRange, fullRange         bool
}

func supportedColor(v uint16) bool { return v == 1 || v == 2 }
func preflightColor(d []byte) (colorMetadata, error) {
	var c colorMetadata
	if len(d) != 10 && len(d) != 11 {
		return c, invalid("colr size")
	}
	switch string(d[:4]) {
	case "nclx":
		if len(d) != 11 || d[10]&0x7f != 0 {
			return c, invalid("nclx syntax")
		}
		c.hasRange = true
		c.fullRange = d[10]&0x80 != 0
	case "nclc":
		if len(d) != 10 {
			return c, invalid("nclc syntax")
		}
	default:
		return c, invalid("unsupported colr type")
	}
	c.primaries, c.transfer, c.matrix = u16(d[4:]), u16(d[6:]), u16(d[8:])
	if !supportedColor(c.primaries) || !supportedColor(c.transfer) || !supportedColor(c.matrix) {
		return c, invalid("unsupported colr signalling")
	}
	return c, nil
}
func initColor(b []byte, nodes []*boxSpan) *colorMetadata {
	for _, n := range nodes {
		if n.typ == "colr" {
			c, _ := preflightColor(b[n.body:n.end])
			return &c
		}
		if c := initColor(b, n.children); c != nil {
			return c
		}
	}
	return nil
}
func validateColorSPS(c colorMetadata, s *avc.SPS) error {
	v := s.VUI
	if v == nil {
		return nil
	}
	if c.hasRange && v.VideoSignalTypePresentFlag && c.fullRange != v.VideoFullRangeFlag {
		return invalid("colr/SPS range conflict")
	}
	if v.ColourDescriptionFlag {
		for _, pair := range [][2]uint{{uint(c.primaries), v.ColourPrimaries}, {uint(c.transfer), v.TransferCharacteristics}, {uint(c.matrix), v.MatrixCoefficients}} {
			if pair[1] != 1 && pair[1] != 2 {
				return invalid("unsupported SPS colour signalling")
			}
			if pair[0] != 2 && pair[1] != 2 && pair[0] != pair[1] {
				return invalid("colr/SPS colour conflict")
			}
		}
	}
	return nil
}
