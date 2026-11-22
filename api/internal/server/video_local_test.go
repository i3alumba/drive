//go:build linux

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/videoworker"
)

func enableLocalFixture(t *testing.T, f *videoFixture) {
	t.Helper()
	options := f.server.video.options
	f.server.CloseVideoPreviews()
	options.processor = nil
	options.RemoteInput = false
	options.LocalExecution = true
	options.Timeout = 15 * time.Second
	if e := f.server.EnableVideoPreviews(options); e != nil {
		t.Fatal(e)
	}
}
func localInput(t *testing.T, f *videoFixture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.mp4")
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=9", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "40", "-bf", "2", "-threads", "1", path}
	if b, e := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, b)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	f.store.mu.Lock()
	f.store.data["users/42/movie.mp4"] = data
	f.store.mu.Unlock()
}
func TestVideoLocalAPIRealFFmpegAndSeek(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("synthetic FFmpeg integration")
	}
	f := newVideoFixture(t, nil)
	localInput(t, f)
	store := &remoteRangeTestStore{transferStore: f.store}
	f.server.store = store
	enableLocalFixture(t, f)
	var previous string
	for _, start := range []float64{0, 4.3} {
		body := fmt.Sprintf(`{"path":"movie.mp4","space":"personal","startTime":%v,"replaceID":%q}`, start, previous)
		w := f.request("POST", "/api/video-previews", "owner", body, "")
		if w.Code != 202 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var status videoPreviewStatus
		_ = json.Unmarshal(w.Body.Bytes(), &status)
		waitVideoState(t, f.server.video, status.ID, "complete")
		if status.ID == previous {
			t.Fatal("reused generation")
		}
		previous = status.ID
		m := f.server.video
		m.mu.Lock()
		job := *m.jobs[status.ID]
		busy := m.busy
		m.mu.Unlock()
		if job.Origin != start || job.Duration < 9 || job.sourceSHA != "" || busy {
			t.Fatalf("%+v busy=%v", job.videoPreviewStatus, busy)
		}
		w = f.request("GET", "/api/video-previews/"+status.ID+"/stream.m3u8", "owner", "", "")
		if w.Code != 200 {
			t.Fatalf("validated playlist %d %s", w.Code, w.Body)
		}
		p := m.processor.(*localVideoProcessor)
		if _, e := p.HLSRemoteReady(context.Background()); e == nil {
			t.Fatal("local falsely qualified remote")
		}
	}
	if store.full.Load() != 0 || store.reads.Load() == 0 {
		t.Fatal("local did not use only ranges")
	}
}

type localBlockedStore struct {
	*remoteRangeTestStore
	block   func() bool
	entered chan struct{}
	once    sync.Once
}

func (s *localBlockedStore) GetObjectRange(ctx context.Context, key string, r *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	if s.block() {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
		return nil, storage.ObjectInfo{}, ctx.Err()
	}
	return s.remoteRangeTestStore.GetObjectRange(ctx, key, r)
}
func TestVideoLocalCancelJoinsActiveDecoderAndSource(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("synthetic FFmpeg integration")
	}
	f := newVideoFixture(t, nil)
	localInput(t, f)
	enableLocalFixture(t, f)
	m := f.server.video
	store := &localBlockedStore{remoteRangeTestStore: &remoteRangeTestStore{transferStore: f.store}, entered: make(chan struct{})}
	store.block = func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, job := range m.jobs {
			if job.Mode != "" {
				return true
			}
		}
		return false
	}
	f.server.store = store
	w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mp4","space":"personal"}`, "")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	var job videoPreviewStatus
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no active decoder source read")
	}
	w = f.request("DELETE", "/api/video-previews/"+job.ID, "owner", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	waitVideoState(t, m, job.ID, "cancelled")
	p := m.processor.(*localVideoProcessor)
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	if active {
		t.Fatal("tracked decoder not joined")
	}
	if _, e := os.Stat(filepath.Join(f.root, videoFenceName)); !os.IsNotExist(e) {
		t.Fatal("fence retained after joined cancellation")
	}
}
func TestVideoLocalStartupFailsClosed(t *testing.T) {
	f := newVideoFixture(t, nil)
	options := f.server.video.options
	f.server.CloseVideoPreviews()
	options.processor = nil
	options.LocalExecution = true
	options.RemoteInput = true
	if f.server.EnableVideoPreviews(options) == nil {
		t.Fatal("mixed local/remote accepted")
	}
	options.RemoteInput = false
	t.Setenv("PATH", "")
	if f.server.EnableVideoPreviews(options) == nil {
		t.Fatal("missing tools accepted")
	}
}
func TestVideoLocalStaleFenceSurvivesRestart(t *testing.T) {
	f := newVideoFixture(t, nil)
	enableLocalFixture(t, f)
	m := f.server.video
	identity := videoworker.HLSIdentity{JobID: randomID()[:32], ControllerEpoch: randomID()[:32], ExecutionID: randomID()[:32]}
	if e := m.createFence(identity); e != nil {
		t.Fatal(e)
	}
	enableLocalFixture(t, f)
	w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv","space":"personal"}`, "")
	if w.Code != 503 {
		t.Fatal("stale execution admitted", w.Code, w.Body)
	}
	if _, e := os.Stat(filepath.Join(f.root, videoFenceName)); e != nil {
		t.Fatal("stale fence removed")
	}
}

func TestVideoLocalInvalidMediaFailsAfterJoin(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("synthetic FFmpeg integration")
	}
	f := newVideoFixture(t, nil)
	f.server.store = &remoteRangeTestStore{transferStore: f.store}
	enableLocalFixture(t, f)
	job := f.create(t)
	waitVideoState(t, f.server.video, job.ID, "failed")
	m := f.server.video
	m.mu.Lock()
	busy := m.busy
	m.mu.Unlock()
	if busy {
		t.Fatal("invalid media retained admission after tracked join")
	}
	if _, e := os.Stat(filepath.Join(f.root, videoFenceName)); !os.IsNotExist(e) {
		t.Fatal("failed tracked job did not clear fence")
	}
}
func TestVideoLocalDoesNotInitializeUnknownScratch(t *testing.T) {
	root := t.TempDir()
	if e := os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	foreign := filepath.Join(root, "foreign")
	if e := os.WriteFile(foreign, []byte("retain"), 0600); e != nil {
		t.Fatal(e)
	}
	s := New(testTransferStore(), nil, nil)
	if e := s.EnableVideoPreviews(VideoPreviewOptions{LocalExecution: true, Root: root, MaxInputBytes: 1 << 20, MaxOutputBytes: 2 << 20, BudgetBytes: 8 << 20, TTL: time.Hour, Timeout: time.Minute}); e == nil {
		t.Fatal("uninitialized scratch accepted")
	}
	if b, e := os.ReadFile(foreign); e != nil || string(b) != "retain" {
		t.Fatal("foreign data altered")
	}
	if _, e := os.Stat(filepath.Join(root, ".video-worker-v1")); !os.IsNotExist(e) {
		t.Fatal("unknown scratch adopted")
	}
}
