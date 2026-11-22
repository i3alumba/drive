//go:build linux

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/videoworker"
)

type remoteRangeTestStore struct {
	*transferStore
	reads atomic.Int64
	full  atomic.Int64
}

func (s *remoteRangeTestStore) GetObject(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	s.full.Add(1)
	return s.transferStore.GetObject(ctx, key)
}
func (s *remoteRangeTestStore) GetObjectRange(ctx context.Context, key string, r *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	if e := ctx.Err(); e != nil {
		return nil, storage.ObjectInfo{}, e
	}
	if r == nil || r.End-r.Start+1 > 64<<10 {
		return nil, storage.ObjectInfo{}, fmt.Errorf("unbounded read")
	}
	s.reads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[key]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	if r.Start < 0 || r.End >= int64(len(data)) {
		return nil, storage.ObjectInfo{}, storage.ErrInvalidRange
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data[r.Start : r.End+1]))), storage.ObjectInfo{Size: int64(len(data))}, nil
}
func TestVideoRemoteRejectsLegacyBeforeRead(t *testing.T) {
	f := newVideoFixture(t, nil)
	f.server.video.options.RemoteInput = true
	store := &remoteRangeTestStore{transferStore: f.store}
	f.server.store = store
	w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv","space":"personal"}`, "")
	if w.Code != 503 || store.reads.Load() != 0 || store.full.Load() != 0 {
		t.Fatalf("%d reads=%d full=%d", w.Code, store.reads.Load(), store.full.Load())
	}
}
func TestVideoRemoteAPIUnixRealFFmpeg(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("synthetic FFmpeg integration")
	}
	f := newVideoFixture(t, nil)
	m := f.server.video
	m.options.RemoteInput = true
	m.options.Timeout = 15 * time.Second
	store := &remoteRangeTestStore{transferStore: f.store}
	f.server.store = store
	input := filepath.Join(t.TempDir(), "source.mp4")
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=9", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "40", "-bf", "0", "-threads", "1", input}
	if b, e := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, b)
	}
	data, e := os.ReadFile(input)
	if e != nil {
		t.Fatal(e)
	}
	store.data["users/42/movie.mp4"] = data
	socket := filepath.Join(t.TempDir(), "controller.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	var endpoint string
	// This owned synthetic fake advertises test-only trust, never real isolation.
	controller := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/capabilities" {
			fmt.Fprintf(w, `{"v":3,"isolation":%q,"controller_epoch":%q,"source":"bounded-http-range-timeline-v3","egress":"selected-broker-only-no-redirect-no-proxy"}`, videoworker.RemoteControllerRole, strings.Repeat("a", 32))
			return
		}
		if r.Method != "POST" || !strings.HasPrefix(r.URL.Path, "/v3/jobs/") {
			http.NotFound(w, r)
			return
		}
		var request videoworker.RemoteHLSRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || !request.Source.ValidFor(request.Identity) {
			http.Error(w, "bad contract", 400)
			return
		}
		endpoint = request.Source.Endpoint
		w.Header().Set("X-Video-Controller-Epoch", request.Identity.ControllerEpoch)
		w.Header().Set("X-Video-Execution-ID", request.Identity.ExecutionID)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		conversion, e := videoworker.RunDevelopmentBrokerHLS(r.Context(), filepath.Join(f.root, "incoming"), request.Identity, request.Format, videoworker.Options{}, request.MaxOutputBytes, request.Source, func(event videoworker.HLSEvent) error {
			if e := videoworker.EncodeHLSEvent(w, event); e != nil {
				return e
			}
			w.(http.Flusher).Flush()
			return nil
		})
		t.Logf("decoder result %+v error %v", conversion, e)
		if e == nil {
			terminal := *videoTestReceipt(request.Identity, conversion.Outcome).Terminal
			terminal.SegmentCount = conversion.SegmentCount
			terminal.DurationUS = conversion.DurationUS
			_ = videoworker.EncodeHLSEvent(w, terminal)
		}
	})}
	go func() { _ = controller.Serve(listener) }()
	defer controller.Close()
	m.processor = videoworker.NewClient(socket)
	w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mp4","space":"personal"}`, "")
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var status videoPreviewStatus
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	waitVideoState(t, m, status.ID, "complete")
	m.mu.Lock()
	sha := m.jobs[status.ID].sourceSHA
	m.mu.Unlock()
	if sha != "" || store.full.Load() != 0 || store.reads.Load() == 0 {
		t.Fatalf("sha=%q full=%d range=%d", sha, store.full.Load(), store.reads.Load())
	}
	if _, e = os.Stat(filepath.Join(f.root, "incoming", status.ID, videoworker.InputName)); !os.IsNotExist(e) {
		t.Fatal("input staged")
	}
	if _, e = http.Get(endpoint); e == nil {
		t.Fatal("source listener survived completion")
	}
	w = f.request("GET", "/api/video-previews/"+status.ID+"/stream.m3u8", "owner", "", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	replacementBody := fmt.Sprintf(`{"path":"movie.mp4","space":"personal","startTime":4.3,"replaceID":%q}`, status.ID)
	replaced := f.request("POST", "/api/video-previews", "owner", replacementBody, "")
	if replaced.Code != 202 {
		t.Fatalf("replacement %d %s", replaced.Code, replaced.Body)
	}
	var next videoPreviewStatus
	_ = json.Unmarshal(replaced.Body.Bytes(), &next)
	if next.ID == status.ID {
		t.Fatal("reused generation")
	}
	waitVideoState(t, m, next.ID, "complete")
	m.mu.Lock()
	origin, duration := m.jobs[next.ID].Origin, m.jobs[next.ID].Duration
	m.mu.Unlock()
	if origin != 4.3 || duration < 9 {
		t.Fatalf("origin=%v duration=%v", origin, duration)
	}
	repeated := f.request("POST", "/api/video-previews", "owner", replacementBody, "")
	if repeated.Code != 409 {
		t.Fatalf("duplicate replacement %d", repeated.Code)
	}
	if old := f.request("GET", "/api/video-previews/"+status.ID+"/stream.m3u8", "owner", "", ""); old.Code != 200 {
		t.Fatal("old immutable assets replaced")
	}

}

type remoteProcessFixture struct {
	videoProcessFunc
	run func(context.Context, videoworker.RemoteSource) error
}

func (f remoteProcessFixture) HLSRemoteReady(context.Context) (string, error) {
	return strings.Repeat("a", 32), nil
}
func (f remoteProcessFixture) StreamRemoteHLS(ctx context.Context, id videoworker.HLSIdentity, format string, limit int64, source videoworker.RemoteSource, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
	return videoworker.HLSSequence{}, f.run(ctx, source)
}
func TestVideoRemoteSourceDenialsAndBudget(t *testing.T) {
	for _, kind := range []string{"owner-revoked", "source-changed", "budget"} {
		t.Run(kind, func(t *testing.T) {
			f := newVideoFixture(t, nil)
			m := f.server.video
			m.options.RemoteInput = true
			m.options.Timeout = 10 * time.Second
			store := &remoteRangeTestStore{transferStore: f.store}
			f.server.store = store
			result := make(chan error, 1)
			m.processor = remoteProcessFixture{run: func(ctx context.Context, source videoworker.RemoteSource) error {
				if kind == "owner-revoked" {
					f.revoked.Store(true)
				}
				if kind == "source-changed" {
					store.mu.Lock()
					store.data["users/42/movie.mkv"] = []byte("changed")
					store.mu.Unlock()
				}
				count := 1
				if kind == "budget" {
					count = 9
				}
				for i := 0; i < count; i++ {
					req, _ := http.NewRequestWithContext(ctx, "GET", source.Endpoint, nil)
					resp, e := http.DefaultClient.Do(req)
					if e != nil {
						result <- e
						return e
					}
					_, e = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					expected := 403
					if kind == "budget" {
						expected = 200
						if i == 8 {
							expected = 429
						}
					}
					if resp.StatusCode != expected || e != nil {
						e = fmt.Errorf("%s request %d status=%d error=%v", kind, i, resp.StatusCode, e)
						result <- e
						return e
					}
				}
				result <- nil
				return fmt.Errorf("test controlled stop")
			}}
			created := f.create(t)
			if e := <-result; e != nil {
				t.Fatal(e)
			}
			waitVideoState(t, m, created.ID, "failed")
			if store.full.Load() != 0 {
				t.Fatal("staged full input")
			}
			expectedReads := int64(0)
			if kind == "budget" {
				expectedReads = 8
			}
			if store.reads.Load() != expectedReads {
				t.Fatalf("reads=%d want %d", store.reads.Load(), expectedReads)
			}
		})
	}
}
func TestVideoRemoteCancelRevokesAndJoins(t *testing.T) {
	f := newVideoFixture(t, nil)
	m := f.server.video
	m.options.RemoteInput = true
	m.options.Timeout = 10 * time.Second
	store := &remoteRangeTestStore{transferStore: f.store}
	f.server.store = store
	started := make(chan string, 1)
	m.processor = remoteProcessFixture{run: func(ctx context.Context, source videoworker.RemoteSource) error {
		started <- source.Endpoint
		<-ctx.Done()
		return ctx.Err()
	}}
	created := f.create(t)
	endpoint := <-started
	w := f.request("DELETE", "/api/video-previews/"+created.ID, "owner", "", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	waitVideoState(t, m, created.ID, "cancelled")
	if _, e := http.Get(endpoint); e == nil {
		t.Fatal("source survived cancellation")
	}
	m.mu.Lock()
	busy := m.busy
	m.mu.Unlock()
	if busy {
		t.Fatal("admission retained after confirmed teardown")
	}
}

func TestVideoRemoteLiveShareRevocation(t *testing.T) {
	f := newVideoFixture(t, nil)
	m := f.server.video
	m.options.RemoteInput = true
	m.options.Timeout = 10 * time.Second
	store := &remoteRangeTestStore{transferStore: f.store}
	f.server.store = store
	store.data["users/84/movie.mkv"] = []byte("shared media")
	owner := User{ID: "84", Username: "sender"}
	share, e := f.server.shares.Create(context.Background(), owner, "fixture", "movie.mkv", false, permissionRead)
	if e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	m.processor = remoteProcessFixture{run: func(ctx context.Context, source videoworker.RemoteSource) error {
		for i := 0; i < 2; i++ {
			if i == 1 {
				if e := f.server.shares.Delete(ctx, owner, share.ID); e != nil {
					result <- e
					return e
				}
			}
			resp, e := http.Get(source.Endpoint)
			if e != nil {
				result <- e
				return e
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			expected := 200
			if i == 1 {
				expected = 403
			}
			if resp.StatusCode != expected {
				e = fmt.Errorf("read %d status %d", i, resp.StatusCode)
				result <- e
				return e
			}
		}
		result <- nil
		return fmt.Errorf("controlled stop")
	}}
	body, _ := json.Marshal(map[string]string{"path": "movie.mkv", "space": shareSpaceID(share.ID)})
	w := f.request("POST", "/api/video-previews", "owner", string(body), "")
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var created videoPreviewStatus
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if e := <-result; e != nil {
		t.Fatal(e)
	}
	waitVideoState(t, m, created.ID, "failed")
	if store.reads.Load() != 1 {
		t.Fatal("source accessed after share revocation")
	}
}

type unknownRemoteFixture struct{ remoteProcessFixture }

func (unknownRemoteFixture) LookupHLS(_ context.Context, id videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	return videoworker.HLSStatus{Identity: id, State: "unknown"}, nil
}
func (f unknownRemoteFixture) CancelHLS(ctx context.Context, id videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	return f.LookupHLS(ctx, id)
}
func TestVideoRemoteUnknownTeardownRevokesButRetainsFence(t *testing.T) {
	f := newVideoFixture(t, nil)
	m := f.server.video
	m.options.RemoteInput = true
	endpoint := make(chan string, 1)
	m.processor = unknownRemoteFixture{remoteProcessFixture{run: func(ctx context.Context, source videoworker.RemoteSource) error {
		endpoint <- source.Endpoint
		return fmt.Errorf("transport lost")
	}}}
	created := f.create(t)
	sourceURL := <-endpoint
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		uncertain := m.jobs[created.ID].uncertain
		m.mu.Unlock()
		if uncertain {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unknown teardown not fenced")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e := http.Get(sourceURL); e == nil {
		t.Fatal("source capability survived unknown teardown")
	}
	m.mu.Lock()
	busy, fenced := m.busy, m.fenced
	m.mu.Unlock()
	if !busy || !fenced {
		t.Fatalf("busy=%v fenced=%v", busy, fenced)
	}
	w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv","space":"personal"}`, "")
	if w.Code != 503 {
		t.Fatalf("admitted with unknown teardown: %d", w.Code)
	}
}

type changedAcquiredStore struct {
	*remoteRangeTestStore
	read   atomic.Bool
	closed atomic.Bool
}
type changedAcquiredBody struct{ s *changedAcquiredStore }

func (b changedAcquiredBody) Read(p []byte) (int, error) {
	b.s.read.Store(true)
	return copy(p, "replacement!!!"), io.EOF
}
func (b changedAcquiredBody) Close() error { b.s.closed.Store(true); return nil }
func (s *changedAcquiredStore) GetObjectRange(context.Context, string, *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return changedAcquiredBody{s}, storage.ObjectInfo{Size: 14, Modified: time.Now()}, nil
}
func TestVideoRemoteRejectsChangedAcquiredMetadata(t *testing.T) {
	f := newVideoFixture(t, nil)
	m := f.server.video
	m.options.RemoteInput = true
	store := &changedAcquiredStore{remoteRangeTestStore: &remoteRangeTestStore{transferStore: f.store}}
	f.server.store = store
	result := make(chan error, 1)
	m.processor = remoteProcessFixture{run: func(ctx context.Context, source videoworker.RemoteSource) error {
		resp, e := http.Get(source.Endpoint)
		if e != nil {
			result <- e
			return e
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 502 || bytes.Contains(data, []byte("replacement")) {
			e = fmt.Errorf("changed source exposed: %d %s", resp.StatusCode, data)
		}
		result <- e
		return fmt.Errorf("controlled stop")
	}}
	created := f.create(t)
	if e := <-result; e != nil {
		t.Fatal(e)
	}
	waitVideoState(t, m, created.ID, "failed")
	if store.read.Load() || !store.closed.Load() {
		t.Fatal("mismatched acquired object was read or leaked")
	}
}

func TestVideoRemoteRejectsUnboundedStartTime(t *testing.T) {
	f := newVideoFixture(t, nil)
	f.server.video.options.RemoteInput = true
	for _, value := range []string{"-1", "21600", "1e999", `"NaN"`} {
		w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv","space":"personal","startTime":`+value+`}`, "")
		if w.Code != 400 {
			t.Fatalf("start %s accepted: %d", value, w.Code)
		}
	}
}
