// Package videomedia validates a deliberately narrow private fragmented-MP4 subset.
// Callers must supply completed API-private snapshots, serialize validation, and
// release artifact bytes afterward. This is not compressed-frame integrity,
// data authorization, a sandbox, or permission to activate a worker.
package videomedia

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/mp4"
)

// Config retains only private, immutable codec and track configuration.
type Config struct {
	tracks   [2]trackConfig
	count    int
	identity [32]byte
}
type trackConfig struct {
	id, scale uint32
	video     bool
	trex      mp4.TrexBox
	sps       *avc.SPS
	pps       *avc.PPS
}

// Timeline's zero value denotes the first fragment. Value copies never share mutable state.
type Timeline struct {
	config    [32]byte
	tracks    [2]trackTime
	fragments uint32
	sequence  uint32
}
type trackTime struct {
	seen                                            bool
	origin, end, presentationStart, presentationEnd int64
}

// Segment is derived from the actual video presentation span, not a worker assertion.
type Segment struct {
	DurationUS int64
	Timeline   Timeline
}

func decode(b []byte, n *boxSpan) (mp4.Box, error) {
	sr := bits.NewFixedSliceReader(b[n.start:n.end])
	box, err := mp4.DecodeBoxSR(uint64(n.start), sr)
	if err != nil {
		return nil, fmt.Errorf("videomedia decode %s: %w", n.typ, err)
	}
	if box == nil || box.Type() != n.typ || sr.AccError() != nil || sr.NrRemainingBytes() != 0 {
		return nil, invalid("incomplete decoder result")
	}
	switch n.typ {
	case "ftyp":
		if _, ok := box.(*mp4.FtypBox); !ok {
			return nil, invalid("ftyp type")
		}
	case "styp":
		if _, ok := box.(*mp4.StypBox); !ok {
			return nil, invalid("styp type")
		}
	case "moov":
		if _, ok := box.(*mp4.MoovBox); !ok {
			return nil, invalid("moov type")
		}
	case "moof":
		if _, ok := box.(*mp4.MoofBox); !ok {
			return nil, invalid("moof type")
		}
	default:
		return nil, invalid("decoder input")
	}
	return box, nil
}

// ValidateInit validates a completed private initialization snapshot.
func ValidateInit(privateBytes []byte) (config *Config, err error) {
	defer func() {
		if recover() != nil {
			config = nil
			err = invalid("decoder panic")
		}
	}()
	nodes, err := preflight(privateBytes, true)
	if err != nil {
		return nil, err
	}
	var moov *mp4.MoovBox
	for _, n := range nodes {
		b, e := decode(privateBytes, n)
		if e != nil {
			return nil, e
		}
		if m, ok := b.(*mp4.MoovBox); ok {
			moov = m
		}
	}
	c := &Config{identity: sha256.Sum256(privateBytes)}
	if moov.Mvhd.Timescale == 0 || moov.Mvhd.Timescale > 1000000000 || len(moov.Mvex.Trexs) != len(moov.Traks) {
		return nil, invalid("movie timescale/trex count")
	}
	videoCount := 0
	audioCount := 0
	for i, tr := range moov.Traks {
		t := trackConfig{id: tr.Tkhd.TrackID, scale: tr.Mdia.Mdhd.Timescale}
		if t.id == 0 || t.scale == 0 || t.scale > 1000000000 {
			return nil, invalid("track identity/timescale")
		}
		for j := 0; j < i; j++ {
			if c.tracks[j].id == t.id {
				return nil, invalid("duplicate track")
			}
		}
		matches := 0
		for _, trex := range moov.Mvex.Trexs {
			if trex.TrackID == t.id {
				t.trex = *trex
				matches++
			}
		}
		if matches != 1 {
			return nil, invalid("track defaults")
		}
		stsd := tr.Mdia.Minf.Stbl.Stsd
		switch tr.Mdia.Hdlr.HandlerType {
		case "vide":
			videoCount++
			t.video = true
			v := stsd.AvcX
			if v == nil || tr.Mdia.Minf.Vmhd == nil {
				return nil, invalid("video entry/handler")
			}
			ac := v.AvcC
			if ac == nil || len(ac.SPSnalus) != 1 || len(ac.PPSnalus) != 1 || len(ac.TrailingBytes) != 0 || ac.NumSPSExt != 0 {
				return nil, invalid("AVC configuration")
			}
			t.sps, err = avc.ParseSPSNALUnit(ac.SPSnalus[0], false)
			if err != nil {
				return nil, err
			}
			s := t.sps
			// Width/Height are cropped display dimensions, not decoder allocation
			// bounds. Bound coded macroblocks before arithmetic; allow the normal
			// 1920x1088 coded picture cropped to a 1920x1080 display.
			if s.PicWidthInMbsMinus1 >= 120 || s.PicHeightInMapUnitsMinus1 >= 68 {
				return nil, invalid("coded AVC picture exceeds rendition bound")
			}
			if s.Profile != 66 && s.Profile != 77 && s.Profile != 100 || s.ChromaFormatIDC != 1 || s.SeparateColourPlaneFlag || s.BitDepthLumaMinus8 != 0 || s.BitDepthChromaMinus8 != 0 || !s.FrameMbsOnlyFlag || s.Width == 0 || s.Height == 0 || s.Width > 1920 || s.Height > 1080 || s.Width != uint(v.Width) || s.Height != uint(v.Height) || byte(s.Profile) != ac.AVCProfileIndication || byte(s.ProfileCompatibility) != ac.ProfileCompatibility || byte(s.Level) != ac.AVCLevelIndication {
				return nil, invalid("unsupported AVC parameters")
			}
			t.pps, err = avc.ParsePPSNALUnit(ac.PPSnalus[0], map[uint32]*avc.SPS{s.ParameterID: s})
			if err != nil {
				return nil, err
			}
			if t.pps.SeqParameterSetID != s.ParameterID || t.pps.NumSliceGroupsMinus1 != 0 {
				return nil, invalid("PPS parameters")
			}
		case "soun":
			audioCount++
			a := stsd.Mp4a
			if a == nil || tr.Mdia.Minf.Smhd == nil {
				return nil, invalid("audio entry/handler")
			}
			// Locate bounded descriptor bytes in the independently validated tree.
			var asc []byte
			tn := nodes[1].children
			trackIndex := -1
			for _, n := range tn {
				if n.typ == "trak" {
					trackIndex++
					if trackIndex == i {
						en := n.child("mdia").child("minf").child("stbl").child("stsd").child("mp4a").child("esds")
						asc, err = preflightESDS(privateBytes[en.body+4 : en.end])
						break
					}
				}
			}
			if err != nil {
				return nil, err
			}
			if err = validateAAC(asc, a, t.scale); err != nil {
				return nil, err
			}
		default:
			return nil, invalid("unsupported handler")
		}
		c.tracks[i] = t
		c.count++
	}
	if videoCount != 1 || audioCount > 1 {
		return nil, invalid("track roles")
	}
	return c, nil
}

func validateAAC(d []byte, a *mp4.AudioSampleEntryBox, scale uint32) error {
	h, err := aac.DecodeAudioSpecificConfigHeader(bytes.NewReader(d))
	if err != nil {
		return err
	}
	if h.ObjectType != aac.AAClc || h.ChannelConfiguration < 1 || h.ChannelConfiguration > 2 || h.SamplingFrequency < 7350 || h.SamplingFrequency > 96000 {
		return invalid("AAC header")
	}
	ac, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(d))
	if err != nil {
		return err
	}
	if ac.FrameLengthFlag || ac.DependsOnCoreCoder || ac.ExtensionFlag || ac.SBRPresentFlag || ac.PSPresentFlag || ac.PCE != nil || ac.USAC != nil || ac.PSSyncExtensionPresent || ac.ExtensionFlag3 {
		return invalid("AAC extensions")
	}
	// Canonical LC GASpecificConfig is 16 bits. FFmpeg also emits the exact
	// backward-compatible sync extension explicitly signalling SBR absent.
	if len(d) != 2 && !(len(d) == 5 && bytes.Equal(d[2:], []byte{0x56, 0xe5, 0})) {
		return invalid("AAC trailing syntax")
	}
	if d[1]&7 != 0 || int(a.ChannelCount) != ac.NrChannels() || int(a.SampleRate) != ac.SamplingFrequency || scale != uint32(ac.SamplingFrequency) {
		return invalid("AAC entry mismatch")
	}
	return nil
}

type payloadSpan struct{ start, end int }
type presentationSpan struct{ start, end int64 }

// ValidateSegment validates one private fragment against an immutable prefix.
func ValidateSegment(privateBytes []byte, config *Config, previous Timeline) (segment Segment, err error) {
	defer func() {
		if recover() != nil {
			segment = Segment{}
			err = invalid("decoder panic")
		}
	}()
	if config == nil || config.count < 1 || config.count > 2 {
		return Segment{}, invalid("configuration")
	}
	if previous.fragments >= 8192 || previous.fragments != 0 && previous.config != config.identity {
		return Segment{}, invalid("timeline configuration/count")
	}
	nodes, err := preflight(privateBytes, false)
	if err != nil {
		return Segment{}, err
	}
	var moof *mp4.MoofBox
	var mn, mdat *boxSpan
	for _, n := range nodes {
		if n.typ == "mdat" {
			mdat = n
			continue
		}
		b, e := decode(privateBytes, n)
		if e != nil {
			return Segment{}, e
		}
		if m, ok := b.(*mp4.MoofBox); ok {
			moof = m
			mn = n
		}
	}
	seq := moof.Mfhd.SequenceNumber
	if seq == 0 || previous.fragments != 0 && (previous.sequence == ^uint32(0) || seq != previous.sequence+1) {
		return Segment{}, invalid("fragment sequence")
	}
	next := previous
	next.config = config.identity
	next.fragments++
	next.sequence = seq
	var payload []payloadSpan
	seen := [2]bool{}
	videoSeen := false
	durationUS := int64(0)
	for _, traf := range moof.Trafs {
		idx := -1
		for i := 0; i < config.count; i++ {
			if config.tracks[i].id == traf.Tfhd.TrackID {
				idx = i
			}
		}
		if idx < 0 || seen[idx] {
			return Segment{}, invalid("foreign/duplicate track")
		}
		seen[idx] = true
		t := config.tracks[idx]
		base := traf.Tfdt.BaseMediaDecodeTime()
		limit := int64(t.scale) * 21600
		if base > uint64(limit) {
			return Segment{}, invalid("decode timestamp")
		}
		clock := int64(base)
		old := previous.tracks[idx]
		if old.seen && clock != old.end {
			return Segment{}, invalid("decode discontinuity")
		}
		var pts []presentationSpan
		sampleIndex := 0
		for _, run := range traf.Truns {
			run.AddSampleDefaultValues(traf.Tfhd, &t.trex)
			offset := int64(mn.start) + int64(run.DataOffset)
			for _, sample := range run.Samples {
				if sample.Dur == 0 || sample.Size == 0 || int64(sample.Dur) > 8*int64(t.scale) || offset < int64(mdat.body) || offset > int64(mdat.end) || int64(sample.Size) > int64(mdat.end)-offset {
					return Segment{}, invalid("sample span/duration")
				}
				if clock > limit-int64(sample.Dur) || int64(sample.CompositionTimeOffset) > 8*int64(t.scale) || int64(sample.CompositionTimeOffset) < -8*int64(t.scale) {
					return Segment{}, invalid("sample timestamp")
				}
				// Version zero offsets are unsigned in the wire format; mp4ff stores int32.
				if run.Version == 0 && sample.CompositionTimeOffset < 0 {
					return Segment{}, invalid("unsigned composition offset")
				}
				p := clock + int64(sample.CompositionTimeOffset)
				end := p + int64(sample.Dur)
				if p < -8*int64(t.scale) || end > limit {
					return Segment{}, invalid("presentation timestamp")
				}
				if !validSampleFlags(sample.Flags) {
					return Segment{}, invalid("sample flags")
				}
				if t.video {
					if sampleIndex == 0 && !sample.IsSync() {
						return Segment{}, invalid("non-sync start")
					}
					if err = validateNALs(privateBytes[int(offset):int(offset)+int(sample.Size)], &t, sampleIndex == 0); err != nil {
						return Segment{}, err
					}
				}
				pts = append(pts, presentationSpan{p, end})
				payload = append(payload, payloadSpan{int(offset), int(offset) + int(sample.Size)})
				offset += int64(sample.Size)
				clock += int64(sample.Dur)
				sampleIndex++
			}
		}
		if len(pts) == 0 {
			return Segment{}, invalid("empty track")
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].start < pts[j].start })
		for i := 1; i < len(pts); i++ {
			if pts[i].start != pts[i-1].end {
				return Segment{}, invalid("presentation gap/overlap")
			}
		}
		first, last := pts[0].start, pts[len(pts)-1].end
		if last-first > 8*int64(t.scale) || clock-int64(base) > 8*int64(t.scale) {
			return Segment{}, invalid("fragment exceeds eight seconds")
		}
		if old.seen && first != old.presentationEnd {
			return Segment{}, invalid("presentation discontinuity")
		}
		nt := old
		if !old.seen {
			nt = trackTime{seen: true, origin: int64(base), presentationStart: first}
		}
		nt.end = clock
		nt.presentationEnd = last
		if clock-nt.origin > limit || last-nt.presentationStart > limit {
			return Segment{}, invalid("timeline exceeds six hours")
		}
		next.tracks[idx] = nt
		if t.video {
			videoSeen = true
			durationUS = ((last-first)*1000000 + int64(t.scale) - 1) / int64(t.scale)
		}
	}
	if !videoSeen {
		return Segment{}, invalid("missing video")
	}
	sort.Slice(payload, func(i, j int) bool { return payload[i].start < payload[j].start })
	p := mdat.body
	for _, s := range payload {
		if s.start != p {
			return Segment{}, invalid("overlap/unaccounted payload")
		}
		p = s.end
	}
	if p != mdat.end {
		return Segment{}, invalid("unaccounted payload")
	}
	return Segment{DurationUS: durationUS, Timeline: next}, nil
}

func validSampleFlags(f uint32) bool {
	if f&0xf0000000 != 0 {
		return false
	}
	for _, shift := range []uint{26, 24, 22, 20} {
		if f>>shift&3 == 3 {
			return false
		}
	}
	return true
}

func validateNALs(d []byte, t *trackConfig, first bool) error {
	sm := map[uint32]*avc.SPS{t.sps.ParameterID: t.sps}
	pm := map[uint32]*avc.PPS{t.pps.PicParameterSetID: t.pps}
	count := 0
	vcl := 0
	var identity avc.SliceHeader
	var sh avc.SliceHeader
	var nalType byte
	var reference bool
	for p := 0; p < len(d); {
		count++
		if count > 64 || len(d)-p < 4 {
			return invalid("NAL count/framing")
		}
		n := uint64(u32(d[p:]))
		p += 4
		if n == 0 || n > uint64(len(d)-p) {
			return invalid("NAL length")
		}
		nal := d[p : p+int(n)]
		p += int(n)
		typ := nal[0] & 31
		if nal[0]&0x80 != 0 {
			return invalid("NAL forbidden bit")
		}
		switch typ {
		case 1, 5:
			if first && typ != 5 {
				return invalid("non-IDR start")
			}
			if typ == 5 && nal[0]&0x60 == 0 {
				return invalid("IDR reference")
			}
			view := nal
			if len(view) > 4096 {
				view = view[:4096]
			}
			r := bits.NewEBSPReaderFromSlice(view)
			r.Read(8)
			if r.ReadExpGolomb() >= uint((t.sps.PicWidthInMbsMinus1+1)*(t.sps.PicHeightInMapUnitsMinus1+1)) || r.ReadExpGolomb() > 9 || r.ReadExpGolomb() > 255 || r.AccError() != nil {
				return invalid("slice leading fields")
			}
			if err := avc.ParseSliceHeaderInto(&sh, view, sm, pm); err != nil {
				return fmt.Errorf("videomedia slice: %w", err)
			}
			if typ == 5 && (sh.SliceType%5 != 2 || sh.FrameNum != 0) {
				return invalid("IDR slice type/frame")
			}
			if sh.PicParamID != t.pps.PicParameterSetID || sh.SeqParamID != t.sps.ParameterID || sh.FieldPicFlag || sh.FirstMBInSlice >= uint32((t.sps.PicWidthInMbsMinus1+1)*(t.sps.PicHeightInMapUnitsMinus1+1)) {
				return invalid("slice configuration")
			}
			if vcl == 0 {
				if sh.FirstMBInSlice != 0 {
					return invalid("missing first slice")
				}
				identity = sh
				nalType = typ
				reference = nal[0]&0x60 != 0
			} else if (nal[0]&0x60 != 0) != reference || typ != nalType || sh.FrameNum != identity.FrameNum || sh.PicParamID != identity.PicParamID || sh.IDRPicID != identity.IDRPicID || sh.PicOrderCntLsb != identity.PicOrderCntLsb || sh.DeltaPicOrderCnt != identity.DeltaPicOrderCnt || sh.DeltaPicOrderCntBottom != identity.DeltaPicOrderCntBottom || sh.FirstMBInSlice <= identity.FirstMBInSlice {
				return invalid("slice identity")
			}
			identity.FirstMBInSlice = sh.FirstMBInSlice
			vcl++
		case 6, 9, 12: // SEI, access-unit delimiter, filler; no parameter-set changes.
		default:
			return invalid("unsupported/configuration-changing NAL")
		}
	}
	if vcl == 0 {
		return invalid("missing VCL")
	}
	return nil
}
