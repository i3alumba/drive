//go:build linux

package videoworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type HLSConversion struct {
	Outcome, Code string
	SegmentCount  int
	DurationUS    int64
}

type hlsFileStamp struct {
	dev, ino          uint64
	size              int64
	digest            string
	modified, changed unix.Timespec
}

func hlsOpenArtifact(dir int, name string, limit int64) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, st, err
	}
	f := os.NewFile(uintptr(fd), name)
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || st.Size < 0 || st.Size > limit {
		f.Close()
		return nil, st, failure("protocol_error")
	}
	return f, st, nil
}

func hlsReadArtifact(dir int, name string, limit int64) ([]byte, error) {
	f, st, err := hlsOpenArtifact(dir, name, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) != st.Size {
		return nil, failure("protocol_error")
	}
	return b, nil
}

func hlsHashArtifact(dir int, name string, limit int64) (hlsFileStamp, error) {
	var result hlsFileStamp
	f, st, err := hlsOpenArtifact(dir, name, limit)
	if err != nil {
		return result, err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, limit+1))
	var after, path unix.Stat_t
	if err != nil || n <= 0 || n != st.Size || unix.Fstat(int(f.Fd()), &after) != nil || unix.Fstatat(dir, name, &path, unix.AT_SYMLINK_NOFOLLOW) != nil || !hlsSameFile(st, after) || !hlsSameFile(path, after) {
		return result, failure("protocol_error")
	}
	return hlsFileStamp{dev: st.Dev, ino: st.Ino, size: n, digest: hex.EncodeToString(hash.Sum(nil)), modified: st.Mtim, changed: st.Ctim}, nil
}

func hlsSameFile(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Size == b.Size && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Uid == b.Uid && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

func hlsKnownFile(name string) (int64, bool) {
	switch name {
	case HLSInitName, HLSInitName + ".tmp":
		return HLSInitMaxBytes, true
	case HLSPlaylistName, HLSPlaylistName + ".tmp":
		return 1 << 20, true
	}
	base := strings.TrimSuffix(name, ".tmp")
	if len(base) != 18 || !strings.HasPrefix(base, "segment-") || !strings.HasSuffix(base, ".m4s") {
		return 0, false
	}
	index, err := strconv.Atoi(base[8:14])
	expected, e := HLSSegmentName(index)
	return HLSSegmentMaxBytes, err == nil && e == nil && expected == base
}

// Account every regular file, including currently open fragments and playlist
// temporary files. This polling development bound is not a hard filesystem quota.
func hlsAudit(dir int, limit int64, final bool, published map[string]hlsFileStamp) error {
	fd, err := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return failure("protocol_error")
	}
	d := os.NewFile(uintptr(fd), HLSDirectoryName)
	defer d.Close()
	names, err := d.Readdirnames(HLSMaxSegments*2 + 8)
	if err != nil && !errors.Is(err, io.EOF) {
		return failure("protocol_error")
	}
	if len(names) >= HLSMaxSegments*2+8 {
		return failure("protocol_error")
	}
	var total int64
	for _, name := range names {
		bound, known := hlsKnownFile(name)
		if !known || (final && strings.HasSuffix(name, ".tmp")) {
			return failure("protocol_error")
		}
		f, st, err := hlsOpenArtifact(dir, name, bound)
		// FFmpeg atomically renames temporary files while the watcher scans.
		if errors.Is(err, unix.ENOENT) && !final {
			continue
		}
		if err != nil {
			return failure("protocol_error")
		}
		f.Close()
		if st.Size > limit-total {
			return failure("quota_exceeded")
		}
		total += st.Size
	}
	for name, stamp := range published {
		f, st, err := hlsOpenArtifact(dir, name, HLSSegmentMaxBytes)
		if err != nil {
			return failure("protocol_error")
		}
		f.Close()
		if st.Dev != stamp.dev || st.Ino != stamp.ino || st.Size != stamp.size || st.Mtim != stamp.modified || st.Ctim != stamp.changed {
			return failure("protocol_error")
		}
	}
	return nil
}

func hlsArguments(m media, format string) ([]string, string) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-threads", "1"}
	args = append(args, inputArgs(format)...)
	args = append(args, "-i", "/proc/self/fd/3", "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "-1", "-map_metadata:s", "-1", "-map_chapters", "-1")
	mode := "transcode"
	if m.hlsCompatible() {
		mode = "remux"
		args = append(args, "-c", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "23", "-threads", "1", "-filter_threads", "1", "-vf", "scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,fps=fps='min(source_fps,30)'", "-force_key_frames", "expr:gte(t,n_forced*4)", "-g", "120", "-keyint_min", "120", "-sc_threshold", "0", "-flags:v", "+cgop", "-c:a", "aac", "-profile:a", "aac_low", "-ac", "2", "-b:a", "128k")
	}
	args = append(args, "-fflags", "+bitexact", "-flags:v", "+bitexact", "-flags:a", "+bitexact", "-f", "hls", "-hls_flags", "temp_file+independent_segments", "-hls_segment_type", "fmp4", "-hls_playlist_type", "event", "-hls_list_size", "0", "-hls_time", "4", "-hls_fmp4_init_filename", HLSInitName, "-hls_segment_filename", "/proc/self/fd/4/segment-%06d.m4s", "-hls_segment_options", "fflags=+bitexact:movflags=+default_base_moof+negative_cts_offsets+skip_sidx:use_editlist=0", "/proc/self/fd/4/"+HLSPlaylistName)
	return args, mode
}

// RunDevelopmentHLS emits conversion facts only. Successful return is never a
// sandbox teardown receipt. Artifacts are retained; no unknown file is removed.
func RunDevelopmentHLS(ctx context.Context, root, id, format string, options Options, maxOutputBytes int64, emit func(HLSEvent) error) (conversion HLSConversion, err error) {
	conversion.Outcome = "failed"
	conversion.Code = "conversion_failed"
	emitFailed := false
	defer func() {
		if err != nil {
			var e *Error
			if errors.As(err, &e) && hlsSafeCode(e.Code) {
				conversion.Code = e.Code
			}
			if ctx.Err() != nil || conversion.Code == "cancelled" {
				conversion.Outcome = "cancelled"
				conversion.Code = "cancelled"
			}
		}
		if emit != nil && !emitFailed {
			if e := emit(HLSEvent{V: HLSVersion, Type: "conversion_end", Outcome: conversion.Outcome, Code: conversion.Code, SegmentCount: conversion.SegmentCount, DurationUS: conversion.DurationUS}); err == nil && e != nil {
				err = e
			}
		}
	}()
	if !jobID.MatchString(id) || !(request{format, maxOutputBytes}).valid() {
		return conversion, failure("invalid_request")
	}
	normalized, e := NewHandler(root, options)
	if e != nil {
		return conversion, failure("invalid_request")
	}
	h := normalized.(*handler)
	if maxOutputBytes > h.options.MaxOutputBytes {
		return conversion, failure("invalid_request")
	}
	runctx, cancel := context.WithTimeout(ctx, h.options.Timeout)
	defer cancel()
	dir, e := openJob(root, id)
	if e != nil {
		return conversion, failure("invalid_request")
	}
	defer unix.Close(dir)
	input, e := openInput(dir, h.options.MaxInputBytes)
	if e != nil {
		return conversion, failure("invalid_media")
	}
	defer input.Close()
	if !absent(dir, OutputName) || !absent(dir, "output.part.mp4") {
		return conversion, failure("invalid_request")
	}
	scan, e := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return conversion, failure("invalid_request")
	}
	contents := os.NewFile(uintptr(scan), "")
	names, e := contents.Readdirnames(2)
	contents.Close()
	if (e != nil && e != io.EOF) || len(names) != 1 || names[0] != InputName {
		return conversion, failure("invalid_request")
	}
	if unix.Mkdirat(dir, HLSDirectoryName, 0700) != nil {
		return conversion, failure("invalid_request")
	}
	fd, e := unix.Openat(dir, HLSDirectoryName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return conversion, failure("invalid_request")
	}
	output := os.NewFile(uintptr(fd), HLSDirectoryName)
	defer output.Close()
	m, e := h.probe(runctx, input, format)
	if e != nil {
		return conversion, e
	}
	args, mode := hlsArguments(m, format)
	emitEvent := func(event HLSEvent) error {
		if _, e := event.fields(); e != nil {
			return e
		}
		if emit != nil {
			e := emit(event)
			if e != nil {
				emitFailed = true
			}
			return e
		}
		return nil
	}
	if e = emitEvent(HLSEvent{V: HLSVersion, Type: "started", Mode: mode, ExpectedDurationUS: int64(m.Duration * 1000000)}); e != nil {
		return conversion, e
	}
	if _, e = input.Seek(0, io.SeekStart); e != nil {
		return conversion, failure("invalid_media")
	}
	cmd := exec.CommandContext(runctx, h.options.FFmpeg, args...)
	cmd.ExtraFiles = []*os.File{input, output}
	cmd.WaitDelay = time.Second
	logs := &boundedBuffer{limit: 32 << 10}
	cmd.Stderr = logs
	cmd.Stdout = io.Discard
	if cmd.Start() != nil {
		return conversion, failure("conversion_failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	watchDone := make(chan struct{})
	watchErr := make(chan error, 1)
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runctx.Done():
				return
			case <-ticker.C:
				if e := hlsAudit(fd, maxOutputBytes, false, nil); e != nil {
					watchErr <- e
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		if !waited {
			<-done
		}
		<-watchDone
	}()
	published := map[string]hlsFileStamp{}
	var durations []int64
	publish := func(final bool) error {
		if e := hlsAudit(fd, maxOutputBytes, final, published); e != nil {
			return e
		}
		data, e := hlsReadArtifact(fd, HLSPlaylistName, 1<<20)
		if errors.Is(e, unix.ENOENT) && !final {
			return nil
		}
		if e != nil {
			return failure("protocol_error")
		}
		timeline, ended, e := parseHLSPlaylist(data)
		if e != nil {
			return e
		}
		if final && !ended {
			return failure("protocol_error")
		}
		if len(timeline) < len(durations) {
			return failure("protocol_error")
		}
		for i, d := range durations {
			if timeline[i] != d {
				return failure("protocol_error")
			}
		}
		if len(timeline) > 0 && len(published) == 0 {
			stamp, e := hlsHashArtifact(fd, HLSInitName, HLSInitMaxBytes)
			if e != nil {
				return failure("protocol_error")
			}
			if e = emitEvent(HLSEvent{V: HLSVersion, Type: "init", Bytes: stamp.size, SHA256: stamp.digest}); e != nil {
				return e
			}
			published[HLSInitName] = stamp
		}
		for index := len(durations); index < len(timeline); index++ {
			if timeline[index] > int64(m.Duration*1000000)+2000000-conversion.DurationUS {
				return failure("protocol_error")
			}
			name, _ := HLSSegmentName(index)
			stamp, e := hlsHashArtifact(fd, name, HLSSegmentMaxBytes)
			if e != nil {
				return failure("protocol_error")
			}
			if e = emitEvent(HLSEvent{V: HLSVersion, Type: "segment", Index: index, Bytes: stamp.size, SHA256: stamp.digest, DurationUS: timeline[index]}); e != nil {
				return e
			}
			published[name] = stamp
			durations = append(durations, timeline[index])
			conversion.SegmentCount++
			conversion.DurationUS += timeline[index]
		}
		if final {
			scan, e := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if e != nil {
				return failure("protocol_error")
			}
			f := os.NewFile(uintptr(scan), "")
			names, e := f.Readdirnames(HLSMaxSegments + 4)
			f.Close()
			if e != nil && e != io.EOF {
				return failure("protocol_error")
			}
			for _, name := range names {
				if name != HLSPlaylistName {
					if _, ok := published[name]; !ok {
						return failure("protocol_error")
					}
				}
			}
			if e := hlsAudit(fd, maxOutputBytes, true, published); e != nil {
				return e
			}
		}
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case e = <-watchErr:
			return conversion, e
		case <-runctx.Done():
			select {
			case e = <-watchErr:
				return conversion, e
			default:
			}
			return conversion, failure("cancelled")
		case <-ticker.C:
			if e = publish(false); e != nil {
				return conversion, e
			}
		case e = <-done:
			waited = true
			wasCancelled := runctx.Err() != nil
			cancel()
			<-watchDone
			select {
			case e = <-watchErr:
				return conversion, e
			default:
			}
			if wasCancelled {
				return conversion, failure("cancelled")
			}
			if e != nil || logs.exceeded || strings.TrimSpace(logs.String()) != "" {
				return conversion, failure("conversion_failed")
			}
			if e = publish(true); e != nil {
				return conversion, e
			}
			if conversion.SegmentCount == 0 || !durationMatches(m.Duration, float64(conversion.DurationUS)/1000000) {
				return conversion, failure("conversion_failed")
			}
			conversion.Outcome = "complete"
			conversion.Code = ""
			return conversion, nil
		}
	}
}
