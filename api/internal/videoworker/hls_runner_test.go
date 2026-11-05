//go:build linux

package videoworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHLSPlaylistStrict(t *testing.T) {
	good, err := HLSManifest([]int64{4000000, 250000}, true)
	if err != nil {
		t.Fatal(err)
	}
	ds, end, err := parseHLSPlaylist([]byte(good))
	if err != nil || !end || len(ds) != 2 {
		t.Fatal(ds, end, err)
	}
	for _, bad := range []string{strings.Replace(good, "segment-000000.m4s", "../segment-000000.m4s", 1), strings.Replace(good, "segment-000001.m4s", "https://host/a", 1), strings.Replace(good, "4.000000", "8.000001", 1), strings.Replace(good, "#EXT-X-ENDLIST", "#EXT-X-KEY:METHOD=AES-128", 1), strings.Replace(good, "segment-000001", "segment-000002", 1), good + "#EXT-X-ENDLIST\n", strings.Replace(good, "#EXTM3U", "#EXTM3U\r", 1), strings.Replace(good, "#EXT-X-MAP:URI=\"init.mp4\"", "#EXT-X-MAP:URI=\"/init.mp4\"", 1)} {
		if _, _, err := parseHLSPlaylist([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := HLSSegmentName(HLSMaxSegments); err == nil {
		t.Fatal("count bound")
	}
	var many strings.Builder
	many.WriteString(strings.SplitN(good, "#EXTINF:", 2)[0])
	for i := 0; i <= HLSMaxSegments; i++ {
		fmt.Fprintf(&many, "#EXTINF:0.000001,\nsegment-%06d.m4s\n", i)
	}
	if _, _, err := parseHLSPlaylist([]byte(many.String())); err == nil {
		t.Fatal("accepted too many segments")
	}
}

func TestHLSArtifactsBoundsAndMutation(t *testing.T) {
	for _, kind := range []string{"unknown", "partial", "aggregate", "symlink", "hardlink", "growth", "same-size", "oversized", "initoversized"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			fd, err := openDirectory(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			name := "segment-000000.m4s"
			path := filepath.Join(dir, name)
			if err = os.WriteFile(path, []byte("artifact"), 0600); err != nil {
				t.Fatal(err)
			}
			stamp, err := hlsHashArtifact(fd, name, 100)
			if err != nil {
				t.Fatal(err)
			}
			published := map[string]hlsFileStamp{name: stamp}
			final := false
			limit := int64(100)
			switch kind {
			case "unknown":
				os.WriteFile(filepath.Join(dir, "unknown"), []byte("x"), 0600)
			case "partial":
				os.WriteFile(filepath.Join(dir, "segment-000001.m4s.tmp"), []byte("x"), 0600)
				final = true
			case "aggregate":
				os.WriteFile(filepath.Join(dir, "segment-000001.m4s.tmp"), make([]byte, 99), 0600)
			case "symlink":
				os.Symlink(path, filepath.Join(dir, HLSInitName))
			case "hardlink":
				os.Link(path, filepath.Join(dir, HLSInitName))
			case "growth":
				f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				f.Write([]byte("x"))
				f.Close()
			case "same-size":
				os.WriteFile(path, []byte("mutation"), 0600)
			case "oversized":
				os.Truncate(path, HLSSegmentMaxBytes+1)
				limit = HLSSegmentMaxBytes * 2
			case "initoversized":
				os.WriteFile(filepath.Join(dir, HLSInitName), nil, 0600)
				os.Truncate(filepath.Join(dir, HLSInitName), HLSInitMaxBytes+1)
				limit = HLSSegmentMaxBytes
			}
			if err = hlsAudit(fd, limit, final, published); err == nil {
				t.Fatal("accepted", kind)
			}
			if kind == "unknown" {
				if _, err = os.Stat(filepath.Join(dir, "unknown")); err != nil {
					t.Fatal("unknown removed")
				}
			}
		})
	}
}

func hlsFixture(t *testing.T, dir, kind string, seconds int) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x90:rate=10:duration=%d", seconds)}
	if kind == "remuxaudio" {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds), "-c:a", "aac", "-ac", "2", "-b:a", "128k", "-shortest")
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-threads", "1")
	switch kind {
	case "transcode":
		args = append(args, "-c:v", "mpeg4", "-f", "matroska")
	case "longgop":
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "200", "-keyint_min", "200", "-sc_threshold", "0", "-bf", "0", "-f", "matroska")
	default:
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "40", "-keyint_min", "40", "-sc_threshold", "0", "-bf", "0", "-f", "matroska")
	}
	path := filepath.Join(dir, InputName)
	args = append(args, path)
	if b, err := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, b)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
}

func hlsRequireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("set VIDEO_WORKER_INTEGRATION=1 for synthetic FFmpeg tests")
	}
}

func TestRunDevelopmentHLSSynthetic(t *testing.T) {
	hlsRequireIntegration(t)
	for _, kind := range []string{"remux", "remuxaudio", "transcode", "longgop", "quota", "callback", "cancel", "preexisting"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := newJob(t)
			seconds := 5
			if kind == "longgop" {
				seconds = 9
			}
			hlsFixture(t, dir, kind, seconds)
			if kind == "preexisting" {
				os.Mkdir(filepath.Join(dir, HLSDirectoryName), 0700)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			maxBytes := int64(16 << 20)
			if kind == "quota" {
				maxBytes = 1024
			}
			sequence := HLSSequence{Identity: hlsClientIdentity, MaxOutputBytes: maxBytes}
			seenEnd := false
			result, err := RunDevelopmentHLS(ctx, root, testID, "mkv", Options{}, maxBytes, func(e HLSEvent) error {
				if kind == "callback" && e.Type == "segment" {
					return errors.New("test callback")
				}
				if kind == "cancel" && e.Type == "started" {
					cancel()
				}
				if e.Type == "terminal" {
					t.Error("runner fabricated terminal")
				}
				if e.Type == "conversion_end" {
					seenEnd = true
				}
				if e.Type == "init" || e.Type == "segment" {
					name := HLSInitName
					if e.Type == "segment" {
						name, _ = HLSSegmentName(e.Index)
					}
					b, err := os.ReadFile(filepath.Join(dir, HLSDirectoryName, name))
					if err != nil {
						return err
					}
					sum := sha256.Sum256(b)
					if int64(len(b)) != e.Bytes || hex.EncodeToString(sum[:]) != e.SHA256 {
						t.Error("hash/size mismatch")
					}
				}
				return sequence.Accept(e)
			})
			if kind == "remux" || kind == "remuxaudio" || kind == "transcode" {
				if err != nil || result.Outcome != "complete" || result.SegmentCount < 2 || !seenEnd {
					t.Fatal(result, err, sequence)
				}
				expectedMode := kind
				if kind == "remuxaudio" {
					expectedMode = "remux"
				}
				if sequence.Mode != expectedMode {
					t.Fatal(sequence.Mode)
				}
			} else if err == nil || result.Outcome == "complete" {
				t.Fatal("unexpected success", result, err)
			}
			if kind == "longgop" && result.SegmentCount != 0 {
				t.Fatal("published long GOP", result)
			}
			if kind == "quota" && result.Code != "quota_exceeded" {
				t.Fatal(result, err)
			}
		})
	}
}

// The fixture-only -re wrapper gates production in media time: when the first
// usable fragment callback runs, FFmpeg has not yet produced ENDLIST. The
// callback gate explicitly observes that prefix before allowing completion.
func TestRunDevelopmentHLSPrefixBeforeProducerComplete(t *testing.T) {
	hlsRequireIntegration(t)
	root, dir := newJob(t)
	hlsFixture(t, dir, "remux", 9)
	wrapper := filepath.Join(t.TempDir(), "realtime-ffmpeg")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /usr/bin/ffmpeg -re \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first := make(chan struct{})
	gate := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := RunDevelopmentHLS(ctx, root, testID, "mkv", Options{FFmpeg: wrapper}, 16<<20, func(e HLSEvent) error {
			if e.Type == "segment" && e.Index == 0 {
				close(first)
				select {
				case <-gate:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		done <- err
	}()
	select {
	case <-first:
	case err := <-done:
		t.Fatal("no prefix", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	data, err := os.ReadFile(filepath.Join(dir, HLSDirectoryName, HLSPlaylistName))
	if err != nil {
		t.Fatal(err)
	}
	ds, end, err := parseHLSPlaylist(data)
	if err != nil || end || len(ds) != 1 {
		t.Fatal("producer complete before prefix", string(data), err)
	}
	select {
	case err := <-done:
		t.Fatal("completed through gate", err)
	default:
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunDevelopmentHLSUnsafeInput(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "empty", "oversized", "directory", "unknown", "output", "jobsymlink", "invalid-id"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := newJob(t)
			path := filepath.Join(dir, InputName)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			id := testID
			switch kind {
			case "symlink":
				os.Symlink(target, path)
			case "hardlink":
				os.Link(target, path)
			case "empty":
				os.WriteFile(path, nil, 0600)
			case "oversized":
				os.WriteFile(path, make([]byte, 9), 0600)
			case "directory":
				os.Mkdir(path, 0700)
			case "unknown":
				os.WriteFile(path, []byte("data"), 0600)
				os.WriteFile(filepath.Join(dir, "unknown"), []byte("untouched"), 0600)
			case "output":
				os.WriteFile(path, []byte("data"), 0600)
				os.WriteFile(filepath.Join(dir, OutputName), []byte("untouched"), 0600)
			case "jobsymlink":
				os.Remove(dir)
				os.Symlink(filepath.Dir(target), dir)
			case "invalid-id":
				id = "../" + testID
			}
			result, err := RunDevelopmentHLS(context.Background(), root, id, "mkv", Options{MaxInputBytes: 8, FFmpeg: "/must-not-execute", FFprobe: "/must-not-execute"}, 1024, nil)
			if err == nil || result.Outcome == "complete" {
				t.Fatal(result, err)
			}
			if _, err = os.Lstat(filepath.Join(dir, HLSDirectoryName)); !os.IsNotExist(err) {
				t.Fatal("created output for unsafe input", err)
			}
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "data" {
				t.Fatal("external target changed", err)
			}
		})
	}
}

func TestRunDevelopmentHLSLateMutation(t *testing.T) {
	hlsRequireIntegration(t)
	for _, kind := range []string{"growth", "partial", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := newJob(t)
			hlsFixture(t, dir, "remux", 5)
			result, err := RunDevelopmentHLS(context.Background(), root, testID, "mkv", Options{}, 16<<20, func(e HLSEvent) error {
				if e.Type == "segment" && e.Index == 1 {
					out := filepath.Join(dir, HLSDirectoryName)
					switch kind {
					case "growth":
						f, err := os.OpenFile(filepath.Join(out, "segment-000001.m4s"), os.O_WRONLY|os.O_APPEND, 0600)
						if err != nil {
							return err
						}
						_, err = f.Write([]byte("mutation"))
						f.Close()
						return err
					case "partial":
						return os.WriteFile(filepath.Join(out, "segment-000002.m4s.tmp"), []byte("partial"), 0600)
					case "unknown":
						return os.WriteFile(filepath.Join(out, "unknown"), []byte("retain"), 0600)
					}
				}
				return nil
			})
			if err == nil || result.Outcome == "complete" {
				t.Fatal("late mutation accepted", result, err)
			}
			if kind == "unknown" {
				b, err := os.ReadFile(filepath.Join(dir, HLSDirectoryName, "unknown"))
				if err != nil || string(b) != "retain" {
					t.Fatal("unknown deleted", err)
				}
			}
		})
	}
}

func TestRunDevelopmentHLSDeadlineJoinsProducer(t *testing.T) {
	hlsRequireIntegration(t)
	root, dir := newJob(t)
	hlsFixture(t, dir, "remux", 9)
	wrapper := filepath.Join(t.TempDir(), "realtime-ffmpeg")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /usr/bin/ffmpeg -re \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ended := false
	result, err := RunDevelopmentHLS(ctx, root, testID, "mkv", Options{FFmpeg: wrapper}, 16<<20, func(e HLSEvent) error {
		if e.Type == "conversion_end" {
			ended = true
			if e.Outcome != "cancelled" {
				t.Error(e)
			}
		}
		return nil
	})
	if err == nil || result.Outcome != "cancelled" || !ended {
		t.Fatal(result, err, ended)
	}
	files, err := os.ReadDir(filepath.Join(dir, HLSDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, file := range files {
		s, err := file.Info()
		if err != nil {
			t.Fatal(err)
		}
		size += s.Size()
	}
	time.Sleep(50 * time.Millisecond)
	files, err = os.ReadDir(filepath.Join(dir, HLSDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	var after int64
	for _, file := range files {
		s, err := file.Info()
		if err != nil {
			t.Fatal(err)
		}
		after += s.Size()
	}
	if after != size {
		t.Fatal("producer continued writing after return", size, after)
	}
}
