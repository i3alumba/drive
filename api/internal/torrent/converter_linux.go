//go:build linux

package torrent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"remote-drive/api/internal/mediaadmission"
)

const maxConversionInput int64 = 64 << 30
const maxConversionOutput int64 = 32 << 30

// Converter operates on the retained local input without staging a second copy.
// It returns only after all tracked child processes have stopped.
type NativeConverter struct{ admission *mediaadmission.Admission }

func NewNativeConverter(admission *mediaadmission.Admission) (*NativeConverter, error) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, errors.New("conversion_tools_unavailable")
		}
	}
	if admission == nil {
		admission = mediaadmission.New()
	}
	return &NativeConverter{admission: admission}, nil
}

type conversionStream struct {
	Index       int    `json:"index"`
	Type        string `json:"codec_type"`
	Codec       string `json:"codec_name"`
	Pixel       string `json:"pix_fmt"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Transfer    string `json:"color_transfer"`
	Primaries   string `json:"color_primaries"`
	Disposition struct {
		Default  int `json:"default"`
		Attached int `json:"attached_pic"`
	} `json:"disposition"`
	SideData []struct {
		Type string `json:"side_data_type"`
	} `json:"side_data_list"`
	Profile    string `json:"profile"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
}
type conversionMedia struct {
	Streams []conversionStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
		Name     string `json:"format_name"`
	} `json:"format"`
}

type boundedMediaLog struct {
	bytes.Buffer
	limit int
}

func (b *boundedMediaLog) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > b.limit {
		return 0, errors.New("media_diagnostic_limit")
	}
	return b.Buffer.Write(p)
}

// Child diagnostics are intentionally not returned to clients: they can contain
// source paths and media URLs. The bounded buffers are used only for parsing.
func runConversionProcess(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runConversionProcessFiles(ctx, nil, name, args...)
}

func runConversionProcessFiles(ctx context.Context, files []*os.File, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	out := &boundedMediaLog{limit: 1 << 20}
	diagnostic := &boundedMediaLog{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, diagnostic
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("conversion_timeout")
	}
	if ctx.Err() != nil {
		return nil, errors.New("conversion_cancelled")
	}
	if err != nil {
		return nil, errors.New("media_process_failed")
	}
	return out.Bytes(), nil
}
func probeConversion(ctx context.Context, input string) (conversionMedia, error) {
	var m conversionMedia
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	data, err := runConversionProcess(ctx, "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", input)
	if err != nil {
		return m, err
	}
	if json.Unmarshal(data, &m) != nil {
		return m, errors.New("invalid_probe")
	}
	return m, nil
}
func mediaDuration(m conversionMedia) (float64, error) {
	d, err := strconv.ParseFloat(m.Format.Duration, 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d <= 0 || d > 6*60*60 {
		return 0, errors.New("unsupported_duration")
	}
	return d, nil
}

func (c *NativeConverter) Convert(ctx context.Context, input, output string) error {
	return c.ConvertAndPublish(ctx, input, output, nil)
}

// ConvertAndPublish keeps the shared token through the joined storage operation.
func (c *NativeConverter) ConvertAndPublish(ctx context.Context, input, output string, publish func() error) error {
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	if err := c.admission.Acquire(ctx); err != nil {
		return err
	}
	defer c.admission.Release()
	info, err := os.Lstat(input)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("invalid_conversion_input")
	}
	if info.Size() > maxConversionInput {
		return errors.New("conversion_input_too_large")
	}
	m, err := probeConversion(ctx, input)
	if err != nil {
		return err
	}
	supported := false
	for _, name := range strings.Split(m.Format.Name, ",") {
		switch name {
		case "matroska", "avi", "mov", "mp4", "m4v":
			supported = true
		}
	}
	if !supported {
		return errors.New("unsupported_video")
	}
	duration, err := mediaDuration(m)
	if err != nil {
		return err
	}
	var video, audio *conversionStream
	for i := range m.Streams {
		s := &m.Streams[i]
		if s.Type == "video" && s.Disposition.Attached == 0 && video == nil {
			video = s
		}
		if s.Type == "audio" && (audio == nil || s.Disposition.Default != 0 && audio.Disposition.Default == 0) {
			audio = s
		}
	}
	if video == nil {
		return errors.New("unsupported_video")
	}
	if video.Transfer == "smpte2084" || video.Transfer == "arib-std-b67" || video.Primaries == "bt2020" {
		return errors.New("unsupported_hdr")
	}
	for _, side := range video.SideData {
		if strings.Contains(strings.ToLower(side.Type), "dovi") || strings.Contains(strings.ToLower(side.Type), "mastering") || strings.Contains(strings.ToLower(side.Type), "content light") {
			return errors.New("unsupported_hdr")
		}
	}
	copyVideo := video.Codec == "h264" && video.Pixel == "yuv420p" && video.Width > 0 && video.Height > 0 && video.Width <= 1920 && video.Height <= 1080
	args := []string{"-nostdin", "-v", "error", "-xerror", "-i", input, "-map", fmt.Sprintf("0:%d", video.Index)}
	if audio != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", audio.Index))
	}
	args = append(args, "-map_metadata", "-1", "-sn", "-dn")
	if copyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "veryfast", "-crf", "23", "-vf", "scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1")
	}
	if audio == nil {
		args = append(args, "-an")
	} else if audio.Codec == "aac" && audio.Profile == "LC" && audio.Channels >= 1 && audio.Channels <= 2 {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", "192k")
	}
	owned, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("conversion_output_exists")
	}
	defer owned.Close()
	args = append(args, "-movflags", "+faststart", "-fs", strconv.FormatInt(maxConversionOutput, 10), "-f", "mp4", "-y", "/proc/self/fd/3")
	if _, err := runConversionProcessFiles(ctx, []*os.File{owned}, "ffmpeg", args...); err != nil {
		return err
	}
	result, err := os.Stat(output)
	if err != nil || result.Size() <= 0 {
		return errors.New("invalid_conversion_output")
	}
	if result.Size() >= maxConversionOutput-1<<20 {
		return errors.New("conversion_output_limit")
	}
	if err := validateFinalMP4(output, result.Size()); err != nil {
		return err
	}
	verified, err := probeConversion(ctx, output)
	if err != nil {
		return errors.New("invalid_conversion_output")
	}
	actual, err := mediaDuration(verified)
	if err != nil || math.Abs(actual-duration) > 0.5 {
		return errors.New("incomplete_conversion_output")
	}
	nv, na := 0, 0
	for _, s := range verified.Streams {
		if s.Type == "video" {
			nv++
			if s.Codec != "h264" || s.Pixel != "yuv420p" {
				return errors.New("invalid_conversion_codec")
			}
		}
		if s.Type == "audio" {
			na++
			if s.Codec != "aac" {
				return errors.New("invalid_conversion_codec")
			}
		}
	}
	if nv != 1 || audio == nil && na != 0 || audio != nil && na != 1 {
		return errors.New("invalid_conversion_tracks")
	}
	// Decode the finalized output to EOF; probing alone cannot prove completeness.
	if _, err = runConversionProcess(ctx, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", output, "-map", "0:v:0", "-map", "0:a?", "-f", "null", "-"); err != nil {
		return errors.New("invalid_conversion_output")
	}
	if publish != nil {
		return publish()
	}
	return nil
}

var _ io.Writer = (*boundedMediaLog)(nil)

// Check finalized top-level layout without loading the output into memory.
func validateFinalMP4(path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("invalid_conversion_output")
	}
	defer f.Close()
	moov, mdat := false, false
	var header [16]byte
	var offset int64
	for count := 0; offset < size; count++ {
		if count >= 64 || size-offset < 8 {
			return errors.New("invalid_conversion_output")
		}
		if _, err := f.ReadAt(header[:8], offset); err != nil {
			return errors.New("invalid_conversion_output")
		}
		boxSize := uint64(binary.BigEndian.Uint32(header[:4]))
		head := uint64(8)
		if boxSize == 1 {
			if size-offset < 16 {
				return errors.New("invalid_conversion_output")
			}
			if _, err := f.ReadAt(header[8:], offset+8); err != nil {
				return errors.New("invalid_conversion_output")
			}
			boxSize = binary.BigEndian.Uint64(header[8:])
			head = 16
		}
		if boxSize < head || boxSize > uint64(size-offset) {
			return errors.New("invalid_conversion_output")
		}
		switch string(header[4:8]) {
		case "moov":
			if moov || mdat || boxSize > 64<<20 {
				return errors.New("invalid_conversion_output")
			}
			moov = true
		case "mdat":
			if !moov || mdat {
				return errors.New("invalid_conversion_output")
			}
			mdat = true
		case "free": // FFmpeg may leave an empty free box before media.
		case "ftyp":
			if offset != 0 {
				return errors.New("invalid_conversion_output")
			}
		default:
			return errors.New("invalid_conversion_output")
		}
		offset += int64(boxSize)
	}
	if !moov || !mdat {
		return errors.New("invalid_conversion_output")
	}
	return nil
}
