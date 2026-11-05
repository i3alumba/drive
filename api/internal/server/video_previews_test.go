//go:build linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/videoworker"
)

type videoProcessFunc func(context.Context, string, string, int64) (videoworker.Result, error)

func (f videoProcessFunc) Process(ctx context.Context, id, format string, limit int64) (videoworker.Result, error) {
	return f(ctx, id, format, limit)
}

type videoFixture struct {
	server  *Server
	store   *transferStore
	root    string
	revoked atomic.Bool
}

func newVideoFixture(t *testing.T, process func(string) videoProcessor) *videoFixture {
	t.Helper()
	f := &videoFixture{root: t.TempDir(), store: testTransferStore()}
	if err := os.Chmod(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := videoworker.Initialize(f.root); err != nil {
		t.Fatal(err)
	}
	f.store.data["users/42/movie.mkv"] = []byte("original media")
	auth := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "42"
		if r.Header.Get("Authorization") == "Bearer other" {
			id = "43"
		}
		fmt.Fprintf(w, `{"id":%s,"username":"fixture","services":{"drive":%t}}`, id, !f.revoked.Load())
	}))
	t.Cleanup(auth.Close)
	validator := NewAuthValidator(auth.URL + "/api/user/")
	trustAuthFixture(t, validator, auth)
	f.server = New(f.store, nil, validator)
	processor := videoProcessor(videoProcessFunc(func(ctx context.Context, id, format string, limit int64) (videoworker.Result, error) {
		err := os.WriteFile(filepath.Join(f.root, "incoming", id, videoworker.OutputName), []byte("0123456789"), 0600)
		return videoworker.Result{Mode: "remux", Size: 10, Duration: 1}, err
	}))
	if process != nil {
		processor = process(f.root)
	}
	if err := f.server.EnableVideoPreviews(VideoPreviewOptions{Root: f.root, MaxInputBytes: 64, MaxOutputBytes: 128, BudgetBytes: 512, TTL: time.Hour, Timeout: time.Second, processor: processor}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.server.CloseVideoPreviews)
	return f
}

func (f *videoFixture) request(method, target, token, body, rangeValue string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if rangeValue != "" {
		r.Header.Set("Range", rangeValue)
	}
	w := httptest.NewRecorder()
	f.server.Routes().ServeHTTP(w, r)
	return w
}

func (f *videoFixture) create(t *testing.T) videoPreviewStatus {
	t.Helper()
	w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv","space":"personal"}`, "")
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var status videoPreviewStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || !validVideoID(status.ID) {
		t.Fatalf("invalid status: %s", w.Body)
	}
	return status
}

func waitVideoState(t *testing.T, m *videoPreviewManager, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		job := *m.jobs[id]
		busy := m.busy
		m.mu.Unlock()
		if job.Status == want && (want != "cancelled" || !busy) {
			return
		}
		if job.Status == "failed" && want != "failed" {
			t.Fatalf("unexpected failure: %+v", job.videoPreviewStatus)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("video did not reach %s", want)
}

func TestVideoPreviewHTTPRangeOwnerAndRevocation(t *testing.T) {
	f := newVideoFixture(t, nil)
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "ready")
	base := "/api/video-previews/" + created.ID
	for _, suffix := range []string{"", "/content"} {
		w := f.request("GET", base+suffix, "other", "", "")
		if w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cross-owner access: %d %s", w.Code, w.Body)
		}
	}
	w := f.request("GET", base+"/content", "owner", "", "bytes=2-5")
	if w.Code != 206 || w.Body.String() != "2345" || w.Header().Get("Content-Range") != "bytes 2-5/10" || w.Header().Get("Content-Type") != "video/mp4" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("range: %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = f.request("HEAD", base+"/content", "owner", "", "")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" {
		t.Fatal("HEAD failed", w.Code, w.Header())
	}
	w = f.request("GET", base+"/content", "owner", "", "bytes=100-200")
	if w.Code != 416 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invalid range accepted or cache policy lost", w.Code, w.Header())
	}
	f.revoked.Store(true)
	for _, suffix := range []string{"", "/content"} {
		if w = f.request("GET", base+suffix, "owner", "", ""); w.Code != 403 {
			t.Fatal("revoked Drive grant accepted", w.Code)
		}
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if string(f.store.data["users/42/movie.mkv"]) != "original media" {
		t.Fatal("original was changed")
	}
}

func TestVideoPreviewFencesPrivateRootAndSecondManager(t *testing.T) {
	f := newVideoFixture(t, nil)
	other := New(f.store, nil, nil)
	options := f.server.video.options
	if err := other.EnableVideoPreviews(options); err == nil {
		other.CloseVideoPreviews()
		t.Fatal("second API manager acquired the same scratch")
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	options.Root = link
	if err := other.EnableVideoPreviews(options); err == nil {
		other.CloseVideoPreviews()
		t.Fatal("symlink scratch root accepted")
	}
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "ready")
}

func TestVideoPreviewBearerAndRequestValidation(t *testing.T) {
	f := newVideoFixture(t, nil)
	r := httptest.NewRequest("POST", "/api/video-previews", strings.NewReader(`{"path":"movie.mkv"}`))
	r.AddCookie(&http.Cookie{Name: "access_token", Value: "owner"})
	w := httptest.NewRecorder()
	f.server.Routes().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("cookie mutation accepted", w.Code)
	}
	for _, body := range []string{`{"path":"../movie.mkv"}`, `{"path":"/movie.mkv"}`, `{"path":"a//movie.mkv"}`, `{"path":"movie.mkv","url":"https://invalid.example"}`, `{"path":"movie.mkv"}{}`, strings.Repeat("x", 4097)} {
		if w = f.request("POST", "/api/video-previews", "owner", body, ""); w.Code != 400 {
			t.Fatalf("invalid request accepted: %d %s", w.Code, w.Body)
		}
	}
	if w = f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mp4"}`, ""); w.Code != 415 {
		t.Fatal("unsupported container accepted")
	}
}

func TestVideoPreviewBusyCancellationAndCleanup(t *testing.T) {
	f := newVideoFixture(t, func(root string) videoProcessor {
		return videoProcessFunc(func(ctx context.Context, id, format string, limit int64) (videoworker.Result, error) {
			<-ctx.Done()
			return videoworker.Result{}, ctx.Err()
		})
	})
	created := f.create(t)
	w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv"}`, "")
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("concurrent conversion admitted", w.Code)
	}
	w = f.request("DELETE", "/api/video-previews/"+created.ID, "owner", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	waitVideoState(t, f.server.video, created.ID, "cancelled")
	for _, dir := range []string{"incoming", "ready"} {
		if _, err := os.Lstat(filepath.Join(f.root, dir, created.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("cancelled job files survived", dir, err)
		}
	}
}

func TestVideoPreviewTimeoutCapacityAndStaleIncoming(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f := newVideoFixture(t, func(root string) videoProcessor {
			return videoProcessFunc(func(ctx context.Context, id, format string, limit int64) (videoworker.Result, error) {
				<-ctx.Done()
				return videoworker.Result{}, ctx.Err()
			})
		})
		f.server.video.options.Timeout = 20 * time.Millisecond
		created := f.create(t)
		waitVideoState(t, f.server.video, created.ID, "failed")
		f.server.video.mu.Lock()
		defer f.server.video.mu.Unlock()
		if f.server.video.jobs[created.ID].Error != "video_preparation_timeout" {
			t.Fatal("missing safe timeout status")
		}
	})
	for _, dir := range []string{"incoming", "ready"} {
		t.Run(dir, func(t *testing.T) {
			f := newVideoFixture(t, nil)
			orphan := filepath.Join(f.root, dir, strings.Repeat("a", 32))
			if err := os.Mkdir(orphan, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(orphan, "unknown"), make([]byte, 400), 0600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv"}`, "")
				if w.Code != 507 {
					t.Fatal("capacity/stale input check bypassed", i, w.Code, w.Body)
				}
			}
			if _, err := os.Stat(filepath.Join(orphan, "unknown")); err != nil {
				t.Fatal("unknown data was cleaned", err)
			}
		})
	}
}

func TestVideoPreviewCacheIsNotWorkerInodeAndExpires(t *testing.T) {
	var held *os.File
	f := newVideoFixture(t, func(root string) videoProcessor {
		return videoProcessFunc(func(ctx context.Context, id, format string, limit int64) (videoworker.Result, error) {
			var err error
			held, err = os.OpenFile(filepath.Join(root, "incoming", id, videoworker.OutputName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err == nil {
				_, err = held.WriteString("0123456789")
			}
			return videoworker.Result{Mode: "transcode", Size: 10, Duration: 1}, err
		})
	})
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "ready")
	defer held.Close()
	if _, err := held.WriteAt([]byte("EVIL"), 0); err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", "/api/video-previews/"+created.ID+"/content", "owner", "", "")
	if w.Code != 200 || w.Body.String() != "0123456789" {
		t.Fatal("worker FD modified private cache", w.Code, w.Body)
	}
	f.server.video.mu.Lock()
	if len(f.server.video.jobs[created.ID].sourceSHA) != 64 {
		t.Fatal("staged snapshot hash missing")
	}
	f.server.video.jobs[created.ID].ExpiresAt = time.Now().Add(-time.Second)
	f.server.video.expireLocked(time.Now())
	f.server.video.mu.Unlock()
	w = f.request("GET", "/api/video-previews/"+created.ID, "owner", "", "")
	if w.Code != 404 {
		t.Fatal("expired job still visible")
	}
	if _, err := os.Stat(filepath.Join(f.root, "ready", created.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired cache not removed", err)
	}
}

func TestVideoPreviewSharedReadPermissionIsRechecked(t *testing.T) {
	f := newVideoFixture(t, nil)
	f.store.mu.Lock()
	f.store.data["users/84/movie.mkv"] = []byte("shared media")
	f.store.mu.Unlock()
	owner := User{ID: "84", Username: "sender"}
	share, err := f.server.shares.Create(context.Background(), owner, "fixture", "movie.mkv", false, permissionRead)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": "movie.mkv", "space": shareSpaceID(share.ID)})
	w := f.request("POST", "/api/video-previews", "owner", string(body), "")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	var created videoPreviewStatus
	if err = json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitVideoState(t, f.server.video, created.ID, "ready")
	if err = f.server.shares.Delete(context.Background(), owner, share.ID); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/content"} {
		w = f.request("GET", "/api/video-previews/"+created.ID+suffix, "owner", "", "")
		if w.Code != 404 {
			t.Fatal("revoked shared source still accessible", w.Code)
		}
	}
}

func TestVideoPreviewActualUnixWorker(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("opt-in tiny synthetic ffmpeg integration")
	}
	f := newVideoFixture(t, func(root string) videoProcessor {
		return videoworker.NewClient(filepath.Join(root, "incoming", videoworker.SocketName))
	})
	m := f.server.video
	m.options.MaxInputBytes, m.options.MaxOutputBytes, m.options.BudgetBytes = 1<<20, 1<<20, 4<<20
	m.options.Timeout = 5 * time.Second
	input := filepath.Join(t.TempDir(), "synthetic.mkv")
	command := exec.Command("/usr/bin/ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25", "-t", "1", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", input)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("synthetic generation: %v %s", err, out)
	}
	original, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	f.store.data["users/42/movie.mkv"] = original
	f.store.mu.Unlock()
	handler, err := videoworker.NewHandler(filepath.Join(f.root, "incoming"), videoworker.Options{MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(f.root, "incoming", videoworker.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	worker := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go worker.Serve(listener)
	t.Cleanup(func() { _ = worker.Close() })
	created := f.create(t)
	waitVideoState(t, m, created.ID, "ready")
	w := f.request("GET", "/api/video-previews/"+created.ID+"/content", "owner", "", "bytes=0-31")
	if w.Code != 206 || w.Body.Len() != 32 || !strings.Contains(w.Body.String(), "ftyp") {
		t.Fatal("actual MP4 range failed", w.Code, w.Body)
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if string(f.store.data["users/42/movie.mkv"]) != string(original) {
		t.Fatal("actual worker changed original")
	}
}

func TestVideoPreviewSourceChangeDeniesCachedContent(t *testing.T) {
	f := newVideoFixture(t, nil)
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "ready")
	f.store.mu.Lock()
	f.store.data["users/42/movie.mkv"] = []byte("a changed original object")
	f.store.mu.Unlock()
	w := f.request("GET", "/api/video-previews/"+created.ID+"/content", "owner", "", "")
	if w.Code != 409 {
		t.Fatal("changed source served stale cache", w.Code)
	}
}
