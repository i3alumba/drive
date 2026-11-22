// Package videoreader provides a job-scoped private HTTP input for media workers.
// It must never be mounted on the public API router. Transport isolation and
// decoder egress qualification are the caller's responsibility.
package videoreader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"remote-drive/api/internal/storage"
)

type Store interface {
	GetObjectRange(context.Context, string, *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error)
}

// Limits are mandatory. MaxRequestBytes bounds each backend read, not the HTTP
// range. Budget counts bytes reserved, including failed reads;
// retries cannot bypass the amplification bound. Deadline bounds the job epoch.
type Limits struct {
	Deadline        time.Time
	MaxRequestBytes int64
	MaxReadBytes    int64
	Concurrency     int
}

// Reader binds one immutable source selection to one execution capability. A new
// job/generation needs a new Reader. Revoke before admitting its replacement.
type Reader struct {
	store     Store
	key, path string
	size      int64
	limits    Limits
	ctx       context.Context
	cancel    context.CancelFunc
	slots     chan struct{}
	mu        sync.Mutex
	reserved  int64
}

func New(ctx context.Context, store Store, key string, size int64, limits Limits) (*Reader, error) {
	if store == nil || key == "" || size <= 0 || !limits.Deadline.After(time.Now()) || limits.MaxRequestBytes <= 0 || limits.MaxReadBytes <= 0 || limits.Concurrency <= 0 {
		return nil, errors.New("invalid private reader limits")
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithDeadline(ctx, limits.Deadline)
	return &Reader{store: store, key: key, size: size, path: "/input/" + hex.EncodeToString(token), limits: limits, ctx: ctx, cancel: cancel, slots: make(chan struct{}, limits.Concurrency)}, nil
}

// Path is an execution secret, not a public playback URL. Do not log it.
func (r *Reader) Path() string { return r.path }
func (r *Reader) Revoke()      { r.cancel() }

func (r *Reader) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if req.URL.Path != r.path || req.URL.RawQuery != "" {
		http.NotFound(w, req)
		return
	}
	if r.ctx.Err() != nil {
		http.Error(w, "revoked", http.StatusGone)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	controller := http.NewResponseController(w)
	if req.Method == http.MethodGet {
		if err := controller.SetWriteDeadline(r.limits.Deadline); err != nil {
			http.Error(w, "unsupported transport", 500)
			return
		}
		expired := make(chan struct{})
		expire := context.AfterFunc(r.ctx, func() { defer close(expired); _ = controller.SetWriteDeadline(time.Now()) })
		defer func() {
			if !expire() {
				<-expired
			}
		}()
		requestExpired := make(chan struct{})
		expireRequest := context.AfterFunc(req.Context(), func() { defer close(requestExpired); _ = controller.SetWriteDeadline(time.Now()) })
		defer func() {
			if !expireRequest() {
				<-requestExpired
			}
		}()

	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	if req.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(r.size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	start, end, partial, err := parseRange(req.Header.Get("Range"), r.size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", r.size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	count := end - start + 1
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		http.Error(w, "busy", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	if r.ctx.Err() != nil {
		cancel()
		http.Error(w, "revoked", http.StatusGone)
		return
	}
	// The HTTP range describes the decoder's logical request, not one backend
	// allocation. Serve it in bounded reservations so open-ended FFmpeg reads work.
	for offset := start; offset <= end; {
		n := min(r.limits.MaxRequestBytes, end-offset+1)
		r.mu.Lock()
		allowed := n <= r.limits.MaxReadBytes-r.reserved
		if allowed {
			r.reserved += n
		}
		r.mu.Unlock()
		if !allowed {
			if offset == start {
				http.Error(w, "read budget", 429)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		body, info, e := r.store.GetObjectRange(ctx, r.key, &storage.ByteRange{Start: offset, End: offset + n - 1})
		if e != nil {
			if offset == start {
				http.Error(w, "source unavailable", 502)
			}
			return
		}
		var once sync.Once
		closeBody := func() { once.Do(func() { _ = body.Close() }) }
		stopClose := context.AfterFunc(ctx, closeBody)
		if info.Size != r.size || ctx.Err() != nil {
			stopClose()
			closeBody()
			if offset == start {
				http.Error(w, "source changed", 502)
			}
			return
		}
		if offset == start {
			w.Header().Set("Content-Length", strconv.FormatInt(count, 10))
			if partial {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, r.size))
				w.WriteHeader(206)
			}
		}
		_, e = io.CopyN(w, body, n)
		stopClose()
		closeBody()
		if e != nil {
			return
		}
		offset += n
	}

}

func parseRange(value string, size int64) (start, end int64, partial bool, err error) {
	end = size - 1
	if value == "" {
		return
	}
	partial = true
	invalid := errors.New("invalid range")
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		err = invalid
		return
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok {
		err = invalid
		return
	}
	number := func(s string) (int64, error) {
		if s == "" {
			return 0, invalid
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, invalid
			}
		}
		return strconv.ParseInt(s, 10, 64)
	}
	if a == "" {
		n, e := number(b)
		if e != nil || n <= 0 {
			err = invalid
			return
		}
		if n < size {
			start = size - n
		}
		return
	}
	start, err = number(a)
	if err != nil || start >= size {
		err = invalid
		return
	}
	if b != "" {
		end, err = number(b)
		if err != nil || end < start {
			err = invalid
			return
		}
		if end >= size {
			end = size - 1
		}
	}
	return
}
