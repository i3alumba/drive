//go:build linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/videoworker"
)

// Deliberately ignores context and blocks its second small filesystem-like read.
// Tests release it explicitly: cancellation must never abandon this activity.
type gatedVideoReader struct {
	data    []byte
	offset  int
	reads   atomic.Int32
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedVideoReader() *gatedVideoReader {
	return &gatedVideoReader{data: []byte("original media"), blocked: make(chan struct{}), release: make(chan struct{})}
}

func (r *gatedVideoReader) Read(p []byte) (int, error) {
	if r.reads.Add(1) == 2 {
		close(r.blocked)
		<-r.release
	}
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:min(r.offset+4, len(r.data))])
	r.offset += n
	return n, nil
}

func (r *gatedVideoReader) Close() error { return nil }
func (r *gatedVideoReader) unblock()     { r.once.Do(func() { close(r.release) }) }

type gatedVideoStore struct {
	*transferStore
	reader  *gatedVideoReader
	context chan context.Context
}

func (s *gatedVideoStore) GetObject(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if key == "users/42/movie.mkv" {
		s.context <- ctx
		return s.reader, storage.ObjectInfo{Size: int64(len(s.reader.data))}, nil
	}
	return s.transferStore.GetObject(ctx, key)
}

func waitVideoSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for deterministic video I/O fixture")
	}
}

func TestVideoSourceCopyCancellationFencesAdmissionUntilIOStops(t *testing.T) {
	for _, reason := range []string{"cancel", "deadline"} {
		t.Run(reason, func(t *testing.T) {
			var processed atomic.Int32
			f := newVideoFixture(t, func(string) videoProcessor {
				return videoProcessFunc(func(context.Context, videoworker.HLSIdentity, string, int64, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
					processed.Add(1)
					return videoworker.HLSSequence{}, errors.New("processor must not be invoked")
				})
			})
			reader := newGatedVideoReader()
			t.Cleanup(reader.unblock)
			store := &gatedVideoStore{transferStore: f.store, reader: reader, context: make(chan context.Context, 1)}
			f.server.store = store
			created := f.create(t)
			waitVideoSignal(t, reader.blocked)
			ctx := <-store.context
			want := "cancelled"
			if reason == "cancel" {
				w := f.request("DELETE", "/api/video-previews/"+created.ID, "owner", "", "")
				var response videoPreviewStatus
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.Status != "cancelling" {
					t.Fatal("cancellation falsely acknowledged completion", w.Code, w.Body)
				}
			} else {
				waitVideoSignal(t, ctx.Done())
				want = "failed"
			}
			w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv"}`, "")
			if w.Code != 429 {
				t.Fatal("admission reopened while prior read was still blocked", w.Code, w.Body)
			}
			if reader.reads.Load() != 2 {
				t.Fatal("unexpected source drain before releasing fixture")
			}
			reader.unblock()
			waitVideoState(t, f.server.video, created.ID, want)
			if reader.reads.Load() != 2 || processed.Load() != 0 {
				t.Fatal("cancelled source was drained or decoder was invoked", reader.reads.Load(), processed.Load())
			}
			f.server.video.mu.Lock()
			busy := f.server.video.busy
			problem := f.server.video.jobs[created.ID].Error
			f.server.video.mu.Unlock()
			if busy || (reason == "deadline" && problem != "video_preparation_timeout") {
				t.Fatal("admission remained busy or deadline was misreported", busy, problem)
			}
			for _, parent := range []string{"incoming", "ready"} {
				if _, err := os.Lstat(filepath.Join(f.root, parent, created.ID)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("cancelled source left known files", parent, err)
				}
			}
		})
	}
}

func TestVideoOutputCopyCancellationDoesNotPublishOrDrain(t *testing.T) {
	f := newVideoFixture(t, nil)
	reader := newGatedVideoReader()
	ctx, cancel := context.WithCancel(context.Background())
	id := strings.Repeat("b", 32)
	finished := make(chan struct{})
	var copyError error
	go func() {
		copyError = f.server.video.copyVideoAsset(ctx, id, videoworker.HLSInitName, reader, int64(len(reader.data)), videoTestDigest(reader.data), func([]byte) error { return errors.New("cancelled bytes must not reach validation") })
		close(finished)
	}()
	t.Cleanup(func() { cancel(); reader.unblock(); <-finished })
	waitVideoSignal(t, reader.blocked)
	cancel()
	select {
	case <-finished:
		t.Fatal("copy abandoned a still-blocked filesystem read")
	default:
	}
	w := f.request("GET", "/api/video-previews/"+id+"/content", "owner", "", "")
	if w.Code != 404 {
		t.Fatal("incomplete private output became accessible", w.Code)
	}
	reader.unblock()
	waitVideoSignal(t, finished)
	if !errors.Is(copyError, context.Canceled) || reader.reads.Load() != 2 {
		t.Fatal("cancelled output was drained or cancellation lost", copyError, reader.reads.Load())
	}
	if _, err := os.Lstat(filepath.Join(f.root, "ready", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled output left a private cache file", err)
	}
}
