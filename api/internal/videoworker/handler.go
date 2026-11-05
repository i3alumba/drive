//go:build linux

package videoworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type handler struct {
	root    string
	options Options
}

var active = make(chan struct{}, 1)

func NewHandler(root string, options Options) (http.Handler, error) {
	if options.FFmpeg == "" {
		options.FFmpeg = "ffmpeg"
	}
	if options.FFprobe == "" {
		options.FFprobe = "ffprobe"
	}
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = 10 << 30
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = 10 << 30
	}
	if options.Timeout == 0 {
		options.Timeout = time.Hour
	}
	if options.MaxInputBytes < 0 || options.MaxOutputBytes < 0 || options.Timeout < 0 || options.Timeout > 2*time.Hour {
		return nil, errors.New("invalid worker bounds")
	}
	fd, err := openDirectory(root)
	if err != nil {
		return nil, err
	}
	err = privateDirectory(fd)
	unix.Close(fd)
	if err != nil {
		return nil, err
	}
	return &handler{root, options}, nil
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	result, err := h.process(r, func() { _ = controller.SetReadDeadline(time.Time{}) })
	if err != nil {
		code := "conversion_failed"
		var e *Error
		if errors.As(err, &e) {
			code = e.Code
		}
		status := http.StatusUnprocessableEntity
		switch code {
		case "busy":
			status = 429
		case "invalid_request":
			status = 400
		case "worker_unavailable":
			status = 503
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(Error{code})
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}
func (h *handler) process(r *http.Request, bodyRead func()) (Result, error) {
	var zero Result
	id := strings.TrimPrefix(r.URL.Path, "/jobs/")
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/jobs/") || !jobID.MatchString(id) || r.URL.RawQuery != "" {
		return zero, failure("invalid_request")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	bodyRead()
	if err != nil || len(data) > 1024 {
		return zero, failure("invalid_request")
	}
	var req request
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || !req.valid() || req.MaxOutputBytes > h.options.MaxOutputBytes {
		return zero, failure("invalid_request")
	}
	select {
	case active <- struct{}{}:
		defer func() { <-active }()
	default:
		return zero, failure("busy")
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.options.Timeout)
	defer cancel()
	dir, err := openJob(h.root, id)
	if err != nil {
		return zero, failure("invalid_request")
	}
	defer unix.Close(dir)
	input, err := openInput(dir, h.options.MaxInputBytes)
	if err != nil {
		return zero, failure("invalid_media")
	}
	defer input.Close()
	const partial = "output.part.mp4"
	if !absent(dir, OutputName) || !absent(dir, partial) {
		return zero, failure("invalid_request")
	}
	media, err := h.probe(ctx, input, req.Format)
	if err != nil {
		return zero, err
	}
	fd, err := unix.Openat(dir, partial, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return zero, failure("conversion_failed")
	}
	output := os.NewFile(uintptr(fd), partial)
	defer output.Close()
	// Only unlink the exact inode created here, never an unknown replacement.
	defer func() {
		var a, b unix.Stat_t
		if unix.Fstat(fd, &a) == nil && unix.Fstatat(dir, partial, &b, unix.AT_SYMLINK_NOFOLLOW) == nil && a.Ino == b.Ino && a.Dev == b.Dev {
			_ = unix.Unlinkat(dir, partial, 0)
		}
	}()
	mode, err := h.convert(ctx, input, output, media, req.Format, req.MaxOutputBytes)
	if err != nil {
		return zero, err
	}
	st, err := output.Stat()
	if err != nil || st.Size() <= 0 || st.Size() >= req.MaxOutputBytes {
		return zero, failure("conversion_failed")
	}
	checked, err := h.probe(ctx, output, "mp4")
	if err != nil || !checked.compatible() || !durationMatches(media.Duration, checked.Duration) {
		return zero, failure("conversion_failed")
	}
	var a, b unix.Stat_t
	if unix.Fstat(fd, &a) != nil || unix.Fstatat(dir, partial, &b, unix.AT_SYMLINK_NOFOLLOW) != nil || a.Ino != b.Ino || a.Dev != b.Dev || a.Nlink != 1 {
		return zero, failure("conversion_failed")
	}
	if output.Sync() != nil || unix.Renameat2(dir, partial, dir, OutputName, unix.RENAME_NOREPLACE) != nil {
		return zero, failure("conversion_failed")
	}
	return Result{mode, st.Size(), checked.Duration}, nil
}
