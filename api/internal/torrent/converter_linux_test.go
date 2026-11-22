//go:build linux

package torrent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeConverterSyntheticFormatMatrix(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	converter, err := NewNativeConverter(nil)
	if err != nil {
		t.Skip(err)
	}
	for _, format := range []string{"mkv", "avi", "mp4", "m4v", "mov"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input."+format)
			output := filepath.Join(dir, "output.mp4")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=12:duration=0.5", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.5", "-c:v", "mpeg4", "-c:a", "ac3", "-shortest", "-f", map[string]string{"mkv": "matroska", "avi": "avi", "mp4": "mp4", "m4v": "mp4", "mov": "mov"}[format], input}
			if data, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, data)
			}
			before, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := converter.Convert(ctx, input, output); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(input)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("input changed")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 2 {
				t.Fatalf("unexpected input staging: %v %v", files, err)
			}
			result, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			moov, mdat := bytes.Index(result, []byte("moov")), bytes.Index(result, []byte("mdat"))
			if moov < 0 || mdat < 0 || moov > mdat {
				t.Fatal("output is not faststart")
			}
			if err := converter.Convert(ctx, input, output); err == nil {
				t.Fatal("overwrote output")
			}
			unchanged, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(result, unchanged) {
				t.Fatal("existing derivative changed")
			}
		})
	}
}

func TestNativeConverterCancelledAdmission(t *testing.T) {
	converter, err := NewNativeConverter(nil)
	if err != nil {
		t.Skip(err)
	}
	if !converter.admission.TryAcquire() {
		t.Fatal("admission")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := converter.Convert(ctx, "unused", "unused"); err == nil {
		t.Fatal("cancel ignored")
	}
	converter.admission.Release()
	if !converter.admission.TryAcquire() {
		t.Fatal("token leaked")
	}
	converter.admission.Release()
}

func TestNativeConverterCopiesCompatibleVideoAndSilentVideo(t *testing.T) {
	converter, err := NewNativeConverter(nil)
	if err != nil {
		t.Skip(err)
	}
	for _, audio := range []bool{false, true} {
		t.Run(fmt.Sprint(audio), func(t *testing.T) {
			root := t.TempDir()
			input, output := filepath.Join(root, "input.mkv"), filepath.Join(root, "output.mp4")
			args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=12:duration=0.5"}
			if audio {
				args = append(args, "-f", "lavfi", "-i", "sine=duration=0.5", "-c:a", "ac3")
			}
			args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-bf", "0", "-threads", "1", input)
			if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture %v %s", err, data)
			}
			if err := converter.ConvertAndPublish(context.Background(), input, output, func() error {
				if converter.admission.TryAcquire() {
					converter.admission.Release()
					t.Error("admission released before publication joined")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !converter.admission.TryAcquire() {
				t.Fatal("publication leaked admission")
			}
			converter.admission.Release()
			elementary := func(path string) []byte {
				data, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:v:0", "-c:v", "copy", "-f", "h264", "-").Output()
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			if !bytes.Equal(elementary(input), elementary(output)) {
				t.Fatal("compatible video was not copied")
			}
		})
	}
}

func TestConversionProcessCancellationJoinsGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runConversionProcess(ctx, "sh", "-c", "sleep 60 & wait")
	if err == nil || err.Error() != "conversion_timeout" {
		t.Fatalf("%v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("process group did not join promptly")
	}
}
