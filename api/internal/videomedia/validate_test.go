package videomedia

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func fixture(t testing.TB, audio bool, gop int, remux bool, channels ...int) ([]byte, [][]byte) {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=12"}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=12")
	}
	args = append(args, "-map", "0:v:0")
	if audio {
		args = append(args, "-map", "1:a:0")
	}
	args = append(args, "-map_metadata", "-1", "-map_metadata:s", "-1", "-map_chapters", "-1", "-c:v", "libx264", "-threads:v", "1", "-pix_fmt", "yuv420p", "-g", fmt.Sprint(gop), "-keyint_min", fmt.Sprint(gop), "-sc_threshold", "0", "-bf", "2", "-flags:v", "+bitexact")
	if audio {
		channelCount := 2
		if len(channels) > 0 {
			channelCount = channels[0]
		}
		args = append(args, "-c:a", "aac", "-ac", fmt.Sprint(channelCount), "-flags:a", "+bitexact")
	}
	if remux {
		runFFmpeg(t, append(append([]string{}, args...), "-fflags", "+bitexact", filepath.Join(dir, "source.mp4")))
		args = []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", filepath.Join(dir, "source.mp4"), "-map", "0", "-c", "copy", "-map_metadata", "-1", "-map_metadata:s", "-1", "-map_chapters", "-1", "-flags:v", "+bitexact", "-flags:a", "+bitexact"}
	}
	args = append(args, "-fflags", "+bitexact", "-f", "hls", "-hls_segment_type", "fmp4", "-hls_time", "4", "-hls_playlist_type", "event", "-hls_flags", "temp_file+independent_segments", "-hls_segment_options", "fflags=+bitexact:movflags=+default_base_moof+negative_cts_offsets+skip_sidx:use_editlist=0", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "segment-%06d.m4s"), filepath.Join(dir, "stream.m3u8"))
	runFFmpeg(t, args)
	init := readFixture(t, filepath.Join(dir, "init.mp4"))
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	var segs [][]byte
	for _, p := range paths {
		segs = append(segs, readFixture(t, p))
	}
	if len(segs) == 0 {
		t.Fatal("no segments")
	}
	return init, segs
}
func runFFmpeg(t testing.TB, args []string) {
	t.Helper()
	out, err := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}
func readFixture(t testing.TB, p string) []byte {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func put32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func findSpan(t *testing.T, b []byte, init bool, typ string) *boxSpan {
	t.Helper()
	ns, e := preflight(b, init)
	if e != nil {
		t.Fatal(e)
	}
	var find func([]*boxSpan) *boxSpan
	find = func(ns []*boxSpan) *boxSpan {
		for _, n := range ns {
			if n.typ == typ {
				return n
			}
			if c := find(n.children); c != nil {
				return c
			}
		}
		return nil
	}
	n := find(ns)
	if n == nil {
		t.Fatalf("missing %s", typ)
	}
	return n
}

func TestValidateGeneratedPrefixes(t *testing.T) {
	for _, audio := range []bool{false, true} {
		for _, remux := range []bool{false, true} {
			t.Run(fmt.Sprintf("audio=%t/remux=%t", audio, remux), func(t *testing.T) {
				init, segs := fixture(t, audio, 40, remux)
				c, e := ValidateInit(init)
				if e != nil {
					t.Fatal(e)
				}
				// Config must not retain mutable input slices.
				for i := range init {
					init[i] = 0
				}
				var prev Timeline
				for i, b := range segs {
					snapshot := prev
					s, e := ValidateSegment(b, c, prev)
					if e != nil {
						t.Fatalf("segment %d: %v", i, e)
					}
					if s.DurationUS <= 0 || s.DurationUS > 8000000 {
						t.Fatalf("duration %d", s.DurationUS)
					}
					if prev != snapshot {
						t.Fatal("prior timeline changed")
					}
					again, e := ValidateSegment(b, c, snapshot)
					if e != nil || again != s {
						t.Fatalf("not deterministic: %v", e)
					}
					prev = s.Timeline
				}
				if prev.fragments != uint32(len(segs)) {
					t.Fatal("prefix count")
				}
			})
		}
	}
}

func TestOwnedApprovedMetadata(t *testing.T) {
	// The test suite normally generates its own fixtures; this optional read-only
	// qualification also pins the operator-supplied exact-byte exception.
	p := "/home/i3alumba/.cache/dvtmp/hls-exact-output-l39eo6p3/init.mp4"
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) {
		t.Skip("owned fixture not present")
	}
	if e != nil {
		t.Fatal(e)
	}
	n := findSpan(t, b, true, "udta")
	if !bytes.Equal(b[n.start:n.end], emptyMetadata) {
		t.Fatal("constant differs")
	}
	if _, e = ValidateInit(b); e != nil {
		t.Fatal(e)
	}
	bad, e := os.ReadFile("/home/i3alumba/.cache/dvtmp/hls-shape-ok-5c7uz1kc/init.mp4")
	if e == nil {
		if _, e = ValidateInit(bad); e == nil {
			t.Fatal("97-byte encoder udta accepted")
		}
	}
}

func TestValidateInitRejectsMutations(t *testing.T) {
	init, _ := fixture(t, true, 40, false)
	cases := map[string]func([]byte) []byte{
		"zero-size":           func(b []byte) []byte { put32(b, 0); return b },
		"extended-metadata":   func(b []byte) []byte { put32(b, 1); return b },
		"unknown":             func(b []byte) []byte { copy(b[4:], "junk"); return b },
		"truncated":           func(b []byte) []byte { return b[:len(b)-1] },
		"trailing":            func(b []byte) []byte { return append(b, 0) },
		"count-amplification": func(b []byte) []byte { n := findSpan(t, b, true, "stts"); put32(b[n.body+4:], 0xffffffff); return b },
		"sizes-amplification": func(b []byte) []byte { n := findSpan(t, b, true, "stsz"); put32(b[n.body+8:], 0xffffffff); return b },
		"description-count":   func(b []byte) []byte { n := findSpan(t, b, true, "stsd"); put32(b[n.body+4:], 2); return b },
		"dref-external":       func(b []byte) []byte { n := findSpan(t, b, true, "url "); put32(b[n.body:], 0); return b },
		"esds-URL": func(b []byte) []byte {
			n := findSpan(t, b, true, "esds")
			es := n.body + 4 // skip descriptor variable-length size and ES_ID
			p := es + 1
			for b[p]&128 != 0 {
				p++
			}
			p++
			b[p+2] = 0x40
			return b
		},
		"sample-entry-reference": func(b []byte) []byte { n := findSpan(t, b, true, "avc1"); b[n.body+7] = 2; return b },
		"quicktime-audio":        func(b []byte) []byte { n := findSpan(t, b, true, "mp4a"); b[n.body+9] = 1; return b },
		"metadata-altered":       func(b []byte) []byte { n := findSpan(t, b, true, "udta"); b[n.end-1] ^= 1; return b },
		"metadata-duplicate": func(b []byte) []byte {
			n := findSpan(t, b, true, "udta")
			m := findSpan(t, b, true, "moov")
			put32(b[m.start:], uint32(m.end-m.start+n.end-n.start))
			return append(b, b[n.start:n.end]...)
		},
		"metadata-wrong-placement": func(b []byte) []byte {
			n := findSpan(t, b, true, "udta")
			m := findSpan(t, b, true, "moov")
			part := append([]byte{}, b[n.start:n.end]...)
			b = append(b[:n.start], b[n.end:]...)
			put32(b[m.start:], uint32(m.end-m.start-len(part)))
			return append(part, b...)
		},
		"metadata-appended": func(b []byte) []byte {
			n := findSpan(t, b, true, "udta")
			m := findSpan(t, b, true, "moov")
			put32(b[n.start:], uint32(n.end-n.start+1))
			put32(b[m.start:], uint32(m.end-m.start+1))
			return append(b, 0)
		},
		"duplicate-track-id": func(b []byte) []byte {
			ns, _ := preflight(b, true)
			var ts []*boxSpan
			for _, n := range ns[1].children {
				if n.typ == "trak" {
					ts = append(ts, n.child("tkhd"))
				}
			}
			copy(b[ts[1].body+12:ts[1].body+16], b[ts[0].body+12:ts[0].body+16])
			return b
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := mutate(bytes.Clone(init))
			if _, e := ValidateInit(b); e == nil {
				t.Fatal("accepted mutation")
			}
		})
	}
	for i := 0; i < len(init); i++ {
		if _, e := ValidateInit(init[:i]); e == nil {
			t.Fatalf("accepted truncated init %d", i)
		}
	}
}

func TestValidateSegmentRejectsMutations(t *testing.T) {
	init, segs := fixture(t, true, 40, false)
	c, e := ValidateInit(init)
	if e != nil {
		t.Fatal(e)
	}
	good := segs[0]
	cases := map[string]func([]byte) []byte{
		"zero-size":           func(b []byte) []byte { put32(b, 0); return b },
		"truncation":          func(b []byte) []byte { return b[:len(b)-1] },
		"unknown-index":       func(b []byte) []byte { copy(b[4:], "sidx"); return b },
		"count-amplification": func(b []byte) []byte { n := findSpan(t, b, false, "trun"); put32(b[n.body+4:], 0xffffffff); return b },
		"foreign-track":       func(b []byte) []byte { n := findSpan(t, b, false, "tfhd"); put32(b[n.body+4:], 99); return b },
		"absolute-offset":     func(b []byte) []byte { n := findSpan(t, b, false, "tfhd"); b[n.body+1] = 0; return b },
		"negative-offset":     func(b []byte) []byte { n := findSpan(t, b, false, "trun"); put32(b[n.body+8:], 0x80000000); return b },
		"payload-gap": func(b []byte) []byte {
			n := findSpan(t, b, false, "trun")
			put32(b[n.body+8:], u32(b[n.body+8:])+1)
			return b
		},
		"unaccounted-payload": func(b []byte) []byte {
			n := findSpan(t, b, false, "mdat")
			put32(b[n.start:], uint32(n.end-n.start+1))
			return append(b, 0)
		},
		"tfdt-overflow": func(b []byte) []byte {
			n := findSpan(t, b, false, "tfdt")
			for i := n.body + 4; i < n.end; i++ {
				b[i] = 255
			}
			return b
		},
		"forged-sync-non-IDR": func(b []byte) []byte {
			n := findSpan(t, b, false, "mdat")
			p := n.body
			for p+4 < n.end {
				l := int(u32(b[p:]))
				p += 4
				if l <= 0 || l > n.end-p {
					t.Fatal("NAL fixture")
				}
				if b[p]&31 == 5 {
					b[p] = (b[p] & 0xe0) | 1
					return b
				}
				p += l
			}
			t.Fatal("no IDR")
			return nil
		},
		"changed-SPS": func(b []byte) []byte {
			n := findSpan(t, b, false, "mdat")
			p := n.body + 4
			b[p] = (b[p] & 0xe0) | 7
			return b
		},
		"sequence-zero": func(b []byte) []byte { n := findSpan(t, b, false, "mfhd"); put32(b[n.body+4:], 0); return b },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := ValidateSegment(mutate(bytes.Clone(good)), c, Timeline{}); e == nil {
				t.Fatal("accepted mutation")
			}
		})
	}
	s, e := ValidateSegment(good, c, Timeline{})
	if e != nil {
		t.Fatal(e)
	}
	before := s.Timeline
	if _, e = ValidateSegment(good, c, s.Timeline); e == nil {
		t.Fatal("accepted repeated fragment")
	}
	if s.Timeline != before {
		t.Fatal("mutated failed prefix")
	}
	if _, e = ValidateSegment(segs[2], c, s.Timeline); e == nil {
		t.Fatal("accepted skipped fragment")
	}
	if _, e = ValidateSegment(good, nil, Timeline{}); e == nil {
		t.Fatal("nil config")
	}
	other := *c
	other.identity[0] ^= 1
	if _, e = ValidateSegment(segs[1], &other, s.Timeline); e == nil {
		t.Fatal("wrong config")
	}
	if !reflect.DeepEqual(before, s.Timeline) {
		t.Fatal("prefix mutation")
	}
}

func TestValidateLongGOP(t *testing.T) {
	init, segs := fixture(t, false, 120, false)
	c, e := ValidateInit(init)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ValidateSegment(segs[0], c, Timeline{}); e == nil {
		t.Fatal("accepted twelve-second GOP")
	}
}

func TestPreflightDescriptorRejectsUnknowns(t *testing.T) {
	for _, d := range [][]byte{{3, 0x80, 0x80, 0x80, 0x80, 0}, {3, 3, 0, 1, 0}, {3, 3, 0, 1, 0x20}, {3, 0xff, 0xff, 0xff, 0x7f}} {
		if _, e := preflightESDS(d); e == nil {
			t.Fatalf("accepted %x", d)
		}
	}
}

func FuzzValidatePrivate(f *testing.F) {
	init, segs := fixture(f, true, 40, false)
	config, err := ValidateInit(init)
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte{}, true)
	f.Add([]byte{}, false)
	f.Add([]byte{0, 0, 0, 0, 0}, true)
	f.Fuzz(func(t *testing.T, changes []byte, isInit bool) {
		if len(changes) > 4096 {
			return
		}
		// Small mutation programs keep corpus minimization bounded while exercising
		// both full permitted decoders rather than only random rejected headers.
		source := segs[0]
		if isInit {
			source = init
		}
		b := bytes.Clone(source)
		for p := 0; p+5 <= len(changes); p += 5 {
			offset := uint64(u32(changes[p:]))
			b[offset%uint64(len(b))] = changes[p+4]
		}
		if isInit {
			_, _ = ValidateInit(b)
			_, _ = ValidateInit(changes)
		} else {
			_, _ = ValidateSegment(b, config, Timeline{})
			_, _ = ValidateSegment(changes, config, Timeline{})
		}
	})
}

func rawBox(typ string, body ...[]byte) []byte {
	n := 8
	for _, b := range body {
		n += len(b)
	}
	out := make([]byte, 8, n)
	put32(out, uint32(n))
	copy(out[4:], typ)
	for _, b := range body {
		out = append(out, b...)
	}
	return out
}
func rawFull(typ string, flags uint32, fields ...uint32) []byte {
	b := make([]byte, 4+len(fields)*4)
	put32(b, flags)
	for i, v := range fields {
		put32(b[4+i*4:], v)
	}
	return rawBox(typ, b)
}
func rawFragment(runs int, samples uint32) []byte {
	cs := [][]byte{rawFull("tfhd", 0x020018, 1, 1, 1), rawFull("tfdt", 0, 0)}
	for i := 0; i < runs; i++ {
		cs = append(cs, rawFull("trun", 1, samples, 0))
	}
	moof := rawBox("moof", rawFull("mfhd", 0, 1), rawBox("traf", cs...))
	return append(moof, rawBox("mdat", []byte{1})...)
}

func TestPreflightBounds(t *testing.T) {
	for _, tc := range []struct {
		runs    int
		samples uint32
		valid   bool
	}{{1, 4096, true}, {2, 2048, true}, {32, 128, true}, {1, 4097, false}, {2, 2049, false}, {33, 1, false}, {1, 0, false}} {
		b := rawFragment(tc.runs, tc.samples)
		_, e := preflight(b, false)
		if (e == nil) != tc.valid {
			t.Fatalf("runs=%d samples=%d error=%v", tc.runs, tc.samples, e)
		}
	}
	b := rawFragment(1, 1)
	n := findSpan(t, b, false, "mdat")
	extended := make([]byte, 16)
	put32(extended, 1)
	copy(extended[4:], "mdat")
	binary.BigEndian.PutUint64(extended[8:], 17)
	extended = append(extended, 1)
	b = append(bytes.Clone(b[:n.start]), extended...)
	if _, e := preflight(b, false); e != nil {
		t.Fatal(e)
	}
	binary.BigEndian.PutUint64(b[n.start+8:], 18)
	if _, e := preflight(b, false); e == nil {
		t.Fatal("extended truncation accepted")
	}
	if _, e := preflight(make([]byte, maxInit+1), true); e == nil {
		t.Fatal("init bound")
	}
	if _, e := preflight(make([]byte, maxSegment+1), false); e == nil {
		t.Fatal("segment bound")
	}
	count := 256
	if _, e := walk(rawBox("ftyp", make([]byte, 8)), 0, 16, "init", 1, &count); e == nil {
		t.Fatal("count bound")
	}
	count = 0
	if _, e := walk(nil, 0, 0, "moov", 9, &count); e == nil {
		t.Fatal("depth bound")
	}
	// A moof is rejected before traversing or decoding its oversized children.
	big := append(rawBox("moof", make([]byte, 256<<10)), rawBox("mdat", []byte{1})...)
	if _, e := preflight(big, false); e == nil {
		t.Fatal("moof bound")
	}
}

func TestPayloadOverlapAndTimelineBounds(t *testing.T) {
	init, segs := fixture(t, true, 40, false)
	c, e := ValidateInit(init)
	if e != nil {
		t.Fatal(e)
	}
	b := bytes.Clone(segs[0])
	ns, _ := preflight(b, false)
	var runs []*boxSpan
	for _, n := range ns {
		if n.typ == "moof" {
			for _, tr := range n.children {
				if tr.typ == "traf" {
					runs = append(runs, tr.child("trun"))
				}
			}
		}
	}
	if len(runs) != 2 {
		t.Fatal("need two tracks")
	}
	copy(b[runs[1].body+8:runs[1].body+12], b[runs[0].body+8:runs[0].body+12])
	if _, e := ValidateSegment(b, c, Timeline{}); e == nil {
		t.Fatal("sample overlap accepted")
	}
	s, e := ValidateSegment(segs[0], c, Timeline{})
	if e != nil {
		t.Fatal(e)
	}
	for _, fragments := range []uint32{8192, ^uint32(0)} {
		prev := s.Timeline
		prev.fragments = fragments
		if _, e := ValidateSegment(segs[1], c, prev); e == nil {
			t.Fatal("fragment cap")
		}
	}
	prev := s.Timeline
	for i := 0; i < c.count; i++ {
		prev.tracks[i].origin = -21600 * int64(c.tracks[i].scale)
		prev.tracks[i].presentationStart = prev.tracks[i].origin
	}
	if _, e := ValidateSegment(segs[1], c, prev); e == nil {
		t.Fatal("cumulative time cap")
	}
}

func TestDescriptorAndParameterBounds(t *testing.T) {
	init, _ := fixture(t, true, 40, false)
	for _, typ := range []string{"avcC", "esds", "dref", "stsd", "trex", "mdhd", "tkhd"} {
		t.Run(typ, func(t *testing.T) {
			b := bytes.Clone(init)
			n := findSpan(t, b, true, typ)
			b[n.body] = 255
			if _, e := ValidateInit(b); e == nil {
				t.Fatal("invalid version accepted")
			}
		})
	}
	n := findSpan(t, init, true, "esds")
	d := init[n.body+4 : n.end]
	es, _, e := descriptor(d, 3)
	if e != nil {
		t.Fatal(e)
	}
	dc, sl, e := descriptor(es[3:], 4)
	if e != nil {
		t.Fatal(e)
	}
	// Unknown descriptors and unaccounted bytes cannot reach mp4ff.
	for _, alter := range []func([]byte) []byte{
		func(b []byte) []byte { return append(b, 0) },
		func(b []byte) []byte { b[0] = 7; return b },
	} {
		if _, e := preflightESDS(alter(bytes.Clone(d))); e == nil {
			t.Fatal("unknown descriptor accepted")
		}
	}
	if len(dc) < 15 || len(sl) == 0 {
		t.Fatal("descriptor fixture")
	}
	for _, asc := range [][]byte{{0x12, 0x10, 0}, {0x12, 0x18}, {0x2a, 0x10}, {0x12, 0x11}} {
		if e := validateAAC(asc, nil, 48000); e == nil {
			t.Fatal("AAC extension accepted")
		}
	}
	av := findSpan(t, init, true, "avcC")
	data := bytes.Clone(init[av.body:av.end])
	data[6] = 0x10
	data[7] = 1
	if e := preflightAVC(data); e == nil {
		t.Fatal("SPS oversize accepted")
	}
}

func TestMonoAAC(t *testing.T) {
	init, segs := fixture(t, true, 40, false, 1)
	c, e := ValidateInit(init)
	if e != nil {
		t.Fatal(e)
	}
	var prev Timeline
	for _, b := range segs {
		s, e := ValidateSegment(b, c, prev)
		if e != nil {
			t.Fatal(e)
		}
		prev = s.Timeline
	}
}

func TestDurationRationalCeilingAndInitialClock(t *testing.T) {
	init, segs := fixture(t, false, 40, false)
	mdhd := findSpan(t, init, true, "mdhd")
	if init[mdhd.body] != 0 {
		t.Fatal("mdhd version fixture")
	}
	put32(init[mdhd.body+12:], 15361)
	c, e := ValidateInit(init)
	if e != nil {
		t.Fatal(e)
	}
	var prev Timeline
	for _, original := range segs {
		b := bytes.Clone(original)
		n := findSpan(t, b, false, "tfdt")
		if b[n.body] != 1 {
			t.Fatal("tfdt fixture version")
		}
		base := binary.BigEndian.Uint64(b[n.body+4:])
		binary.BigEndian.PutUint64(b[n.body+4:], base+123)
		s, e := ValidateSegment(b, c, prev)
		if e != nil {
			t.Fatal(e)
		}
		// Original fragment is 40 frames, 1024 video ticks/frame.
		if s.DurationUS != (40960*1000000+15360)/15361 {
			t.Fatalf("duration did not round actual span up: %d", s.DurationUS)
		}
		prev = s.Timeline
	}
}

func TestBoundedNALFraming(t *testing.T) {
	init, segs := fixture(t, false, 40, false)
	c, e := ValidateInit(init)
	if e != nil {
		t.Fatal(e)
	}
	track := c.tracks[0]
	ns, _ := preflight(segs[0], false)
	var mn *boxSpan
	for _, n := range ns {
		if n.typ == "moof" {
			mn = n
		}
	}
	box, e := decode(segs[0], mn)
	if e != nil {
		t.Fatal(e)
	}
	moof := box.(*mp4.MoofBox)
	run := moof.Trafs[0].Truns[0]
	run.AddSampleDefaultValues(moof.Trafs[0].Tfhd, &track.trex)
	p := mn.start + int(run.DataOffset)
	sample := bytes.Clone(segs[0][p : p+int(run.Samples[0].Size)])
	if e = validateNALs(sample, &track, true); e != nil {
		t.Fatal(e)
	}
	makeNAL := func(nal []byte) []byte { b := make([]byte, 4); put32(b, uint32(len(nal))); return append(b, nal...) }
	cases := [][]byte{
		nil, {0}, {0, 0, 0, 0}, {0xff, 0xff, 0xff, 0xff}, makeNAL([]byte{0x89}), makeNAL([]byte{0x67, 0}), makeNAL([]byte{0x68, 0}), makeNAL([]byte{0x65}), makeNAL([]byte{0x41, 0}), makeNAL([]byte{0x69, 0}),
		append(bytes.Repeat(makeNAL([]byte{9, 0xf0}), 64), sample...),
		append(bytes.Repeat(makeNAL([]byte{6}), 65), sample...),
		makeNAL(append([]byte{0x65}, make([]byte, 4096)...)),
	}
	for i, b := range cases {
		if e = validateNALs(b, &track, true); e == nil {
			t.Fatalf("accepted malformed NAL %d", i)
		}
	}
}

func TestExactEightSecondBoundary(t *testing.T) {
	init, segs := fixture(t, false, 80, false)
	c, e := ValidateInit(init)
	if e != nil {
		t.Fatal(e)
	}
	s, e := ValidateSegment(segs[0], c, Timeline{})
	if e != nil {
		t.Fatal(e)
	}
	if s.DurationUS != 8000000 {
		t.Fatalf("eight-second boundary: %d", s.DurationUS)
	}
	if _, e = ValidateSegment(segs[1], c, s.Timeline); e != nil {
		t.Fatal(e)
	}
}
