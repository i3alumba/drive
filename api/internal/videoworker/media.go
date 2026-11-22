//go:build linux

package videoworker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if len(p) > left {
		b.exceeded = true
		p = p[:left]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func demux(format string) string {
	switch format {
	case "mkv":
		return "matroska"
	case "avi":
		return "avi"
	default:
		return "mov"
	}
}
func inputArgs(format string) []string {
	return []string{"-protocol_whitelist", "file,pipe", "-format_whitelist", demux(format), "-f", demux(format)}
}
func (h *handler) probe(ctx context.Context, f *os.File, format string) (media, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return media{}, failure("invalid_media")
	}
	return h.probeInput(ctx, f, format, "")
}
func remoteInputArgs(format string) []string {
	return []string{"-protocol_whitelist", "http,tcp", "-http_proxy", "", "-seekable", "1", "-rw_timeout", "5000000", "-format_whitelist", demux(format), "-f", demux(format)}
}
func (h *handler) probeInput(ctx context.Context, f *os.File, format, endpoint string) (media, error) {
	var result media
	// Remote probing shares the explicit job/source deadline, including network
	// latency. Do not silently impose a shorter local-file probe budget.
	timeout := 15 * time.Second
	if endpoint != "" {
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline)
		} else {
			return result, failure("invalid_request")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"-v", "error"}
	input := "/proc/self/fd/3"
	if endpoint != "" {
		args = append(args, remoteInputArgs(format)...)
		input = endpoint
	} else {
		args = append(args, inputArgs(format)...)
	}
	args = append(args, "-show_entries", "stream=codec_type,codec_name,has_b_frames,pix_fmt,width,height,profile,channels,sample_rate:format=duration", "-of", "json", input)
	cmd := exec.CommandContext(ctx, h.options.FFprobe, args...)
	if endpoint == "" {
		cmd.ExtraFiles = []*os.File{f}
	}
	configureMediaProcess(cmd)
	defer killMediaGroup(cmd)
	cmd.WaitDelay = time.Second
	out := &boundedBuffer{limit: 64 << 10}
	logs := &boundedBuffer{limit: 16 << 10}
	cmd.Stdout = out
	cmd.Stderr = logs
	if cmd.Run() != nil || out.exceeded || logs.exceeded || logs.Len() != 0 {
		return result, failure("invalid_media")
	}
	var raw struct {
		Streams []stream `json:"streams"`
		Format  struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out.Bytes(), &raw) != nil {
		return result, failure("invalid_media")
	}
	result.Duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	if !validDuration(result.Duration) {
		return result, failure("invalid_media")
	}
	videos, audios := 0, 0
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			videos++
			if videos == 1 {
				result.Video = s
			}
		case "audio":
			audios++
			if audios == 1 {
				v := s
				result.Audio = &v
			}
		}
	}
	if videos == 0 || result.Video.CodecName == "" || result.Video.Width <= 0 || result.Video.Height <= 0 || (result.Audio != nil && result.Audio.CodecName == "") {
		return result, failure("unsupported_media")
	}
	if format == "mp4" && (videos != 1 || audios > 1 || len(raw.Streams) != videos+audios) {
		return result, failure("conversion_failed")
	}
	return result, nil
}
func (h *handler) convert(ctx context.Context, input, output *os.File, m media, format string, limit int64) (string, error) {
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return "", failure("conversion_failed")
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "1"}
	args = append(args, inputArgs(format)...)
	args = append(args, "-i", "/proc/self/fd/3", "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "-1", "-map_metadata:s", "-1", "-map_chapters", "-1")
	mode := "transcode"
	if m.compatible() {
		mode = "remux"
		args = append(args, "-c", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "23", "-threads", "1", "-filter_threads", "1", "-vf", "scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2", "-c:a", "aac", "-ac", "2", "-b:a", "128k")
	}
	args = append(args, "-movflags", "+faststart", "-fs", strconv.FormatInt(limit, 10), "-f", "mp4", "/proc/self/fd/4")
	cmd := exec.CommandContext(ctx, h.options.FFmpeg, args...)
	cmd.ExtraFiles = []*os.File{input, output}
	configureMediaProcess(cmd)
	defer killMediaGroup(cmd)
	cmd.WaitDelay = time.Second
	logs := &boundedBuffer{limit: 32 << 10}
	cmd.Stdout = io.Discard
	cmd.Stderr = logs
	if cmd.Run() != nil || logs.exceeded || strings.TrimSpace(logs.String()) != "" {
		return "", failure("conversion_failed")
	}
	// FFmpeg may exit successfully at -fs, including below the requested limit.
	// Conservatively refuse the final 64 KiB rather than publish a capped stream.
	st, err := output.Stat()
	if err != nil || st.Size() >= limit-(64<<10) {
		return "", failure("conversion_failed")
	}
	return mode, nil
}
