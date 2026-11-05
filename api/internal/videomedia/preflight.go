package videomedia

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/Eyevinn/mp4ff/bits"
)

const maxInit = 1 << 20
const maxSegment = 64 << 20
const emptyMetadataHex = "0000003d75647461000000356d657461000000000000002168646c7200000000000000006d6469726170706c00000000000000000000000008696c7374"

var emptyMetadata, _ = hex.DecodeString(emptyMetadataHex)

type boxSpan struct {
	typ              string
	start, body, end int
	children         []*boxSpan
}

func (n *boxSpan) child(t string) *boxSpan {
	for _, c := range n.children {
		if c.typ == t {
			return c
		}
	}
	return nil
}
func u32(b []byte) uint32    { return binary.BigEndian.Uint32(b) }
func u16(b []byte) uint16    { return binary.BigEndian.Uint16(b) }
func invalid(s string) error { return fmt.Errorf("videomedia: %s", s) }

// All sizes/counts are checked here, before any library decoder sees a byte.
func preflight(b []byte, init bool) ([]*boxSpan, error) {
	limit := maxSegment
	root := "segment"
	if init {
		limit = maxInit
		root = "init"
	}
	if len(b) == 0 || len(b) > limit {
		return nil, invalid("artifact size")
	}
	count := 0
	return walk(b, 0, len(b), root, 1, &count)
}
func walk(b []byte, start, end int, parent string, depth int, count *int) ([]*boxSpan, error) {
	if depth > 8 {
		return nil, invalid("box depth")
	}
	var out []*boxSpan
	seen := map[string]int{}
	samples := uint64(0)
	for p := start; p < end; {
		if end-p < 8 {
			return nil, invalid("truncated header")
		}
		size := uint64(u32(b[p:]))
		typ := string(b[p+4 : p+8])
		h := 8
		if size == 1 {
			if typ != "mdat" || end-p < 16 {
				return nil, invalid("extended header")
			}
			size = binary.BigEndian.Uint64(b[p+8:])
			h = 16
		}
		if size < uint64(h) || size > uint64(end-p) {
			return nil, invalid("box size")
		}
		*count++
		if *count > 256 {
			return nil, invalid("box count")
		}
		seen[typ]++
		max := 1
		if (parent == "moov" && typ == "trak") || (parent == "mvex" && typ == "trex") || (parent == "moof" && typ == "traf") {
			max = 2
		}
		if parent == "traf" && typ == "trun" {
			max = 32
		}
		if seen[typ] > max {
			return nil, invalid("duplicate " + typ)
		}
		n := &boxSpan{typ: typ, start: p, body: p + h, end: p + int(size)}
		d := b[n.body:n.end]
		childOffset := -1
		childParent := typ
		allowed := false
		switch parent {
		case "init":
			allowed = typ == "ftyp" || typ == "moov"
		case "segment":
			allowed = typ == "styp" || typ == "moof" || typ == "mdat"
		case "moov":
			allowed = typ == "mvhd" || typ == "trak" || typ == "mvex" || typ == "udta"
		case "trak":
			allowed = typ == "tkhd" || typ == "mdia"
		case "mdia":
			allowed = typ == "mdhd" || typ == "hdlr" || typ == "minf"
		case "minf":
			allowed = typ == "vmhd" || typ == "smhd" || typ == "dinf" || typ == "stbl"
		case "dinf":
			allowed = typ == "dref"
		case "dref":
			allowed = typ == "url "
		case "stbl":
			allowed = typ == "stsd" || typ == "stts" || typ == "stsc" || typ == "stsz" || typ == "stco"
		case "stsd":
			allowed = typ == "avc1" || typ == "mp4a"
		case "avc1":
			allowed = typ == "avcC" || typ == "pasp" || typ == "btrt"
		case "mp4a":
			allowed = typ == "esds" || typ == "btrt"
		case "mvex":
			allowed = typ == "trex"
		case "moof":
			allowed = typ == "mfhd" || typ == "traf"
		case "traf":
			allowed = typ == "tfhd" || typ == "tfdt" || typ == "trun"
		}
		if !allowed {
			return nil, invalid("unexpected " + typ + " in " + parent)
		}
		// Full-box headers and their exact count-derived lengths.
		switch typ {
		case "moov", "trak", "mdia", "minf", "dinf", "stbl", "mvex", "moof", "traf":
			childOffset = 0
		case "udta":
			if !bytes.Equal(b[p:n.end], emptyMetadata) {
				return nil, invalid("metadata")
			}
			// Exact constant contains meta/hdlr/ilst, still charge its depth/count.
			if depth+2 > 8 {
				return nil, invalid("metadata depth")
			}
			*count += 3
			if *count > 256 {
				return nil, invalid("box count")
			}
		case "ftyp", "styp":
			if len(d) < 8 || len(d) > 256 || len(d)%4 != 0 {
				return nil, invalid("brands")
			}
		case "mdat":
			if len(d) == 0 {
				return nil, invalid("empty mdat")
			}
		case "avc1":
			if len(d) < 78 || !bytes.Equal(d[:6], make([]byte, 6)) || u16(d[6:]) != 1 {
				return nil, invalid("visual entry")
			}
			childOffset = 78
		case "mp4a":
			if len(d) < 28 || !bytes.Equal(d[:6], make([]byte, 6)) || u16(d[6:]) != 1 || !bytes.Equal(d[8:16], make([]byte, 8)) || u16(d[18:]) != 16 || u32(d[20:]) != 0 || u16(d[26:]) != 0 {
				return nil, invalid("audio entry")
			}
			childOffset = 28
		case "avcC":
			if err := preflightAVC(d); err != nil {
				return nil, err
			}
		case "pasp":
			if len(d) != 8 || u32(d) == 0 || u32(d[4:]) == 0 {
				return nil, invalid("pasp")
			}
		case "btrt":
			if len(d) != 12 {
				return nil, invalid("btrt")
			}
		default:
			if len(d) < 4 {
				return nil, invalid("full box")
			}
			v := d[0]
			flags := u32(d) & 0xffffff
			switch typ {
			case "mvhd", "tkhd", "mdhd":
				if v > 1 {
					return nil, invalid("header version")
				}
				want := 0
				switch typ {
				case "mvhd":
					want = 100
				case "tkhd":
					want = 84
				case "mdhd":
					want = 24
				}
				if v == 1 {
					want += 12
				}
				if len(d) != want || (typ != "tkhd" && flags != 0) || (typ == "tkhd" && (flags&^uint32(7) != 0)) {
					return nil, invalid("movie header")
				}
			case "hdlr":
				if v != 0 || flags != 0 || len(d) < 25 || len(d) > 256 || d[len(d)-1] != 0 {
					return nil, invalid("handler")
				}
			case "vmhd":
				if v != 0 || flags != 1 || len(d) != 12 {
					return nil, invalid("vmhd")
				}
			case "smhd":
				if v != 0 || flags != 0 || len(d) != 8 {
					return nil, invalid("smhd")
				}
			case "url ":
				if v != 0 || flags != 1 || len(d) != 4 {
					return nil, invalid("external reference")
				}
			case "dref", "stsd":
				if v != 0 || flags != 0 || len(d) < 8 || u32(d[4:]) != 1 {
					return nil, invalid("entry count")
				}
				childOffset = 8
			case "stts", "stsc", "stco":
				if v != 0 || flags != 0 || len(d) != 8 || u32(d[4:]) != 0 {
					return nil, invalid("nonempty table")
				}
			case "stsz":
				if v != 0 || flags != 0 || len(d) != 12 || u32(d[4:]) != 0 || u32(d[8:]) != 0 {
					return nil, invalid("nonempty sizes")
				}
			case "trex":
				if v != 0 || flags != 0 || len(d) != 24 || u32(d[8:]) != 1 {
					return nil, invalid("trex")
				}
			case "mfhd":
				if v != 0 || flags != 0 || len(d) != 8 {
					return nil, invalid("mfhd")
				}
			case "tfdt":
				if v > 1 || flags != 0 || len(d) != 8+4*int(v) {
					return nil, invalid("tfdt")
				}
			case "tfhd":
				if v != 0 || flags&0x020000 == 0 || flags&^uint32(0x02003a) != 0 {
					return nil, invalid("tfhd flags")
				}
				want := 8
				for _, f := range []uint32{2, 8, 16, 32} {
					if flags&f != 0 {
						want += 4
					}
				}
				if len(d) != want || (flags&2 != 0 && u32(d[8:]) != 1) {
					return nil, invalid("tfhd length/description")
				}
			case "trun":
				if v > 1 || flags&1 == 0 || flags&^uint32(0xf05) != 0 || flags&4 != 0 && flags&0x400 != 0 || len(d) < 12 {
					return nil, invalid("trun flags")
				}
				c := uint64(u32(d[4:]))
				samples += c
				if c == 0 || samples > 4096 {
					return nil, invalid("sample count")
				}
				fields := uint64(0)
				for _, f := range []uint32{0x100, 0x200, 0x400, 0x800} {
					if flags&f != 0 {
						fields++
					}
				}
				want := uint64(12) + c*fields*4
				if flags&4 != 0 {
					want += 4
				}
				if uint64(len(d)) != want {
					return nil, invalid("trun length")
				}
			case "esds":
				if v != 0 || flags != 0 {
					return nil, invalid("esds flags")
				}
				if _, err := preflightESDS(d[4:]); err != nil {
					return nil, err
				}
			default:
				return nil, invalid("unsupported leaf")
			}
		}
		if typ == "moof" && n.end-n.start > 256<<10 {
			return nil, invalid("moof size")
		}
		if childOffset >= 0 {
			var err error
			n.children, err = walk(b, n.body+childOffset, n.end, childParent, depth+1, count)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, n)
		p = n.end
	}
	need := func(ts ...string) bool {
		for _, t := range ts {
			if seen[t] != 1 {
				return false
			}
		}
		return true
	}
	ok := true
	switch parent {
	case "init":
		ok = need("ftyp", "moov") && len(out) == 2 && out[0].typ == "ftyp"
	case "segment":
		ok = need("moof", "mdat") && out[len(out)-1].typ == "mdat" && (len(out) == 2 || len(out) == 3 && out[0].typ == "styp")
	case "moov":
		ok = need("mvhd", "mvex") && seen["trak"] >= 1
	case "trak":
		ok = need("tkhd", "mdia")
	case "mdia":
		ok = need("mdhd", "hdlr", "minf")
	case "minf":
		ok = need("dinf", "stbl") && seen["vmhd"]+seen["smhd"] == 1
	case "dinf":
		ok = need("dref")
	case "dref":
		ok = need("url ")
	case "stbl":
		ok = need("stsd", "stts", "stsc", "stsz", "stco")
	case "stsd":
		ok = seen["avc1"]+seen["mp4a"] == 1
	case "avc1":
		ok = need("avcC")
	case "mp4a":
		ok = need("esds")
	case "mvex":
		ok = seen["trex"] >= 1
	case "moof":
		ok = need("mfhd") && seen["traf"] >= 1
	case "traf":
		ok = need("tfhd", "tfdt") && seen["trun"] >= 1
	}
	if !ok {
		return nil, invalid("missing/order in " + parent)
	}
	return out, nil
}

func preflightAVC(d []byte) error {
	if len(d) < 7 || d[0] != 1 || d[4] != 0xff || d[5] != 0xe1 {
		return invalid("avc configuration")
	}
	p := 6
	for i := 0; i < 2; i++ {
		if p+2 > len(d) {
			return invalid("parameter length")
		}
		l := int(u16(d[p:]))
		p += 2
		if l == 0 || l > 4096 || l > len(d)-p {
			return invalid("parameter size")
		}
		nal := d[p : p+l]
		expected := byte(7 + i)
		if nal[0]&0x80 != 0 || nal[0]&31 != expected || nal[0]&0x60 == 0 {
			return invalid("parameter NAL header")
		}
		r := bits.NewEBSPReaderFromSlice(nal)
		r.Read(8)
		if i == 0 {
			r.Read(24)
			if r.ReadExpGolomb() > 31 {
				return invalid("SPS ID")
			}
		} else {
			if r.ReadExpGolomb() > 255 || r.ReadExpGolomb() > 31 {
				return invalid("PPS IDs")
			}
			r.Read(2)
			if r.ReadExpGolomb() != 0 {
				return invalid("slice groups")
			}
		}
		if r.AccError() != nil {
			return invalid("parameter header")
		}
		p += l
		if i == 0 {
			if p >= len(d) || d[p] != 1 {
				return invalid("pps count")
			}
			p++
		}
	}
	if p != len(d) {
		// High-profile chroma/bit depth extension, with no SPS extensions.
		if d[1] != 100 || len(d)-p != 4 || !bytes.Equal(d[p:], []byte{0xfd, 0xf8, 0xf8, 0}) {
			return invalid("avc trailing syntax")
		}
	}
	return nil
}

// Descriptor lengths use at most four base-128 bytes, including FFmpeg's padded encoding.
func descriptor(d []byte, tag byte) ([]byte, []byte, error) {
	if len(d) < 2 || d[0] != tag {
		return nil, nil, invalid("descriptor tag")
	}
	n := 0
	p := 1
	for i := 0; i < 4; i++ {
		if p >= len(d) {
			break
		}
		v := d[p]
		p++
		n = n<<7 | int(v&127)
		if v&128 == 0 {
			if n > len(d)-p {
				return nil, nil, invalid("descriptor length")
			}
			return d[p : p+n], d[p+n:], nil
		}
	}
	return nil, nil, invalid("descriptor length encoding")
}
func preflightESDS(d []byte) ([]byte, error) {
	es, rest, err := descriptor(d, 3)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 || len(es) < 3 || es[2] != 0 {
		return nil, invalid("ES flags")
	}
	dc, sl, err := descriptor(es[3:], 4)
	if err != nil {
		return nil, err
	}
	if len(dc) < 13 || dc[0] != 0x40 || dc[1] != 0x15 {
		return nil, invalid("decoder configuration")
	}
	asc, rest, err := descriptor(dc[13:], 5)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 || len(asc) < 2 || len(asc) > 8 {
		return nil, invalid("decoder specific data")
	}
	s, rest, err := descriptor(sl, 6)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 || !bytes.Equal(s, []byte{2}) {
		return nil, invalid("SL configuration")
	}
	return asc, nil
}
