//go:build linux

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/videoworker"
)

var videoMediaOnce sync.Once
var videoMediaError error
var videoInitFixture []byte
var videoSegmentFixtures [][]byte

func videoTestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func videoTestMedia(t *testing.T) {
	t.Helper()
	videoMediaOnce.Do(func() {
		root, err := os.MkdirTemp("", "video-api-fixture-")
		if err != nil {
			videoMediaError = err
			return
		}
		defer os.RemoveAll(root) // Exclusively owned synthetic fixture directory.
		args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=160x120:r=10", "-t", "8", "-fflags", "+bitexact", "-map_metadata", "-1", "-map_metadata:s", "-1", "-map_chapters", "-1", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-flags:v", "+bitexact", "-force_key_frames", "expr:gte(t,n_forced*4)", "-g", "40", "-keyint_min", "40", "-sc_threshold", "0", "-f", "hls", "-hls_time", "4", "-hls_segment_type", "fmp4", "-hls_flags", "temp_file+independent_segments", "-hls_list_size", "0", "-hls_playlist_type", "event", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_options", "fflags=+bitexact:movflags=+default_base_moof+negative_cts_offsets+skip_sidx:use_editlist=0", "-hls_segment_filename", filepath.Join(root, "segment-%06d.m4s"), filepath.Join(root, "stream.m3u8")}
		out, err := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput()
		if err != nil {
			videoMediaError = fmt.Errorf("synthetic generation: %w %s", err, out)
			return
		}
		videoInitFixture, videoMediaError = os.ReadFile(filepath.Join(root, "init.mp4"))
		for index := 0; index < 2 && videoMediaError == nil; index++ {
			name, _ := videoworker.HLSSegmentName(index)
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				videoMediaError = err
				return
			}
			videoSegmentFixtures = append(videoSegmentFixtures, data)
		}
	})
	if videoMediaError != nil {
		t.Fatal(videoMediaError)
	}
}

type videoProcessFunc func(context.Context, videoworker.HLSIdentity, string, int64, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error)

func (f videoProcessFunc) HLSReady(context.Context) (string, error) {
	return strings.Repeat("a", 32), nil
}
func (f videoProcessFunc) StreamHLS(ctx context.Context, identity videoworker.HLSIdentity, format string, limit int64, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
	return f(ctx, identity, format, limit, emit)
}
func (f videoProcessFunc) LookupHLS(_ context.Context, identity videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	return videoTestReceipt(identity, "cancelled"), nil
}
func (f videoProcessFunc) CancelHLS(ctx context.Context, identity videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	return f.LookupHLS(ctx, identity)
}

// A test-only owned lifecycle fixture, never a selectable production backend.
func videoTestReceipt(identity videoworker.HLSIdentity, outcome string) videoworker.HLSStatus {
	code := ""
	if outcome == "cancelled" {
		code = "cancelled"
	}
	return videoworker.HLSStatus{Identity: identity, State: "quiescent", Terminal: &videoworker.HLSEvent{V: 1, Type: "terminal", Outcome: outcome, Code: code, JobID: identity.JobID, ControllerEpoch: identity.ControllerEpoch, ExecutionID: identity.ExecutionID, Quiescence: "confirmed"}}
}

func emitVideoFixture(ctx context.Context, root string, identity videoworker.HLSIdentity, limit int64, emit func(videoworker.HLSEvent) error, afterFirst func() error) (videoworker.HLSSequence, error) {
	ledger := videoworker.HLSSequence{Identity: identity, MaxOutputBytes: limit}
	publish := func(event videoworker.HLSEvent) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ledger.Accept(event); err != nil {
			return err
		}
		return emit(event)
	}
	dir := filepath.Join(root, "incoming", identity.JobID, videoworker.HLSDirectoryName)
	if err := os.Mkdir(dir, 0700); err != nil {
		return ledger, err
	}
	if err := publish(videoworker.HLSEvent{V: 1, Type: "started", Mode: "transcode", ExpectedDurationUS: 8000000}); err != nil {
		return ledger, err
	}
	if err := os.WriteFile(filepath.Join(dir, videoworker.HLSInitName), videoInitFixture, 0600); err != nil {
		return ledger, err
	}
	if err := publish(videoworker.HLSEvent{V: 1, Type: "init", Bytes: int64(len(videoInitFixture)), SHA256: videoTestDigest(videoInitFixture)}); err != nil {
		return ledger, err
	}
	for index, data := range videoSegmentFixtures {
		name, _ := videoworker.HLSSegmentName(index)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return ledger, err
		}
		if err := publish(videoworker.HLSEvent{V: 1, Type: "segment", Index: index, Bytes: int64(len(data)), SHA256: videoTestDigest(data), DurationUS: 4000000}); err != nil {
			return ledger, err
		}
		if index == 0 && afterFirst != nil {
			if err := afterFirst(); err != nil {
				return ledger, err
			}
		}
	}
	if err := publish(videoworker.HLSEvent{V: 1, Type: "conversion_end", Outcome: "complete", SegmentCount: 2, DurationUS: 8000000}); err != nil {
		return ledger, err
	}
	terminal := *videoTestReceipt(identity, "complete").Terminal
	err := publish(terminal)
	return ledger, err
}

type videoFixture struct {
	server  *Server
	store   *transferStore
	root    string
	revoked atomic.Bool
}

func newVideoFixture(t *testing.T, process func(string) videoProcessor) *videoFixture {
	t.Helper()
	videoTestMedia(t)
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
	var processor videoProcessor = videoProcessFunc(func(ctx context.Context, identity videoworker.HLSIdentity, format string, limit int64, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
		return emitVideoFixture(ctx, f.root, identity, limit, emit, nil)
	})
	if process != nil {
		processor = process(f.root)
	}
	if err := f.server.EnableVideoPreviews(VideoPreviewOptions{Root: f.root, MaxInputBytes: 1 << 20, MaxOutputBytes: 2 << 20, BudgetBytes: 8 << 20, TTL: time.Hour, Timeout: time.Second, processor: processor}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.server.CloseVideoPreviews)
	return f
}

func (f *videoFixture) request(method, target, token, body, byteRange string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if byteRange != "" {
		r.Header.Set("Range", byteRange)
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
		t.Fatal("invalid status", w.Body)
	}
	return status
}
func waitVideoState(t *testing.T, manager *videoPreviewManager, id, want string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		job := *manager.jobs[id]
		busy := manager.busy
		manager.mu.Unlock()
		if job.Status == want && (want != "cancelled" || !busy) {
			return
		}
		if job.Status == "failed" && want != job.Status {
			t.Fatalf("unexpected state: %+v", job.videoPreviewStatus)
		}
		time.Sleep(time.Millisecond)
	}
	manager.mu.Lock()
	status := manager.jobs[id].videoPreviewStatus
	manager.mu.Unlock()
	t.Fatalf("video did not reach %s: %+v", want, status)
}

func TestVideoHLSProgressivePrivateRangeAndCompletion(t *testing.T) {
	first := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f := newVideoFixture(t, func(root string) videoProcessor {
		return videoProcessFunc(func(ctx context.Context, identity videoworker.HLSIdentity, _ string, limit int64, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
			return emitVideoFixture(ctx, root, identity, limit, emit, func() error {
				once.Do(func() { close(first) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		})
	})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	created := f.create(t)
	waitVideoSignal(t, first)
	base := "/api/video-previews/" + created.ID + "/"
	w := f.request("GET", base+videoworker.HLSPlaylistName, "owner", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "ENDLIST") || !strings.Contains(w.Body.String(), "segment-000000.m4s") || strings.Contains(w.Body.String(), "segment-000001.m4s") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("premature/incomplete playlist", w.Code, w.Body)
	}
	name, _ := videoworker.HLSSegmentName(0)
	w = f.request("GET", base+name, "owner", "", "bytes=2-5")
	if w.Code != 206 || w.Body.String() != string(videoSegmentFixtures[0][2:6]) {
		t.Fatal("private range failed", w.Code, w.Body)
	}
	w = f.request("HEAD", base+videoworker.HLSInitName, "owner", "", "")
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD failed", w.Code)
	}
	w = f.request("GET", base+name, "owner", "", "bytes=99999999-")
	if w.Code != 416 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("range policy failed", w.Code, w.Header())
	}
	for _, asset := range []string{videoworker.HLSPlaylistName, videoworker.HLSInitName, name} {
		if denied := f.request("GET", base+asset, "other", "", ""); denied.Code != 404 {
			t.Fatal("foreign owner", denied.Code)
		}
	}
	f.revoked.Store(true)
	if denied := f.request("GET", base+name, "owner", "", ""); denied.Code != 403 {
		t.Fatal("revoked grant served", denied.Code)
	}
	f.revoked.Store(false)
	unblock.Do(func() { close(release) })
	waitVideoState(t, f.server.video, created.ID, "complete")
	w = f.request("GET", base+videoworker.HLSPlaylistName, "owner", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatal("confirmed completion missing", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(f.root, videoFenceName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed execution left fence", err)
	}
}

func TestVideoHLSBearerAndPathValidation(t *testing.T) {
	f := newVideoFixture(t, nil)
	r := httptest.NewRequest("POST", "/api/video-previews", strings.NewReader(`{"path":"movie.mkv"}`))
	r.AddCookie(&http.Cookie{Name: "access_token", Value: "owner"})
	w := httptest.NewRecorder()
	f.server.Routes().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("cookie mutation accepted")
	}
	for _, body := range []string{`{"path":"../movie.mkv"}`, `{"path":"/movie.mkv"}`, `{"path":"a//movie.mkv"}`, `{"path":"movie.mkv","url":"https://invalid.example"}`, `{"path":"movie.mkv"}{}`, strings.Repeat("x", 4097)} {
		if w := f.request("POST", "/api/video-previews", "owner", body, ""); w.Code != 400 {
			t.Fatal("invalid request accepted", w.Code, w.Body)
		}
	}
	if w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mp4"}`, ""); w.Code != 415 {
		t.Fatal("wrong container")
	}
}

func TestVideoHLSSharedSourceRevocationAndChange(t *testing.T) {
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
	if json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatal(w.Body)
	}
	waitVideoState(t, f.server.video, created.ID, "complete")
	if err := f.server.shares.Delete(context.Background(), owner, share.ID); err != nil {
		t.Fatal(err)
	}
	for _, asset := range []string{videoworker.HLSPlaylistName, videoworker.HLSInitName, "segment-000000.m4s"} {
		if w := f.request("GET", "/api/video-previews/"+created.ID+"/"+asset, "owner", "", ""); w.Code != 404 {
			t.Fatal("revoked shared media", w.Code)
		}
	}
	personal := f.create(t)
	waitVideoState(t, f.server.video, personal.ID, "complete")
	f.store.mu.Lock()
	f.store.data["users/42/movie.mkv"] = []byte("changed original object")
	f.store.mu.Unlock()
	if w := f.request("GET", "/api/video-previews/"+personal.ID+"/"+videoworker.HLSPlaylistName, "owner", "", ""); w.Code != 409 {
		t.Fatal("stale source served", w.Code)
	}
}

type uncertainVideoProcessor struct {
	videoProcessFunc
	confirmed atomic.Bool
	wrong     bool
}

func (p *uncertainVideoProcessor) LookupHLS(_ context.Context, identity videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	if p.confirmed.Load() {
		if p.wrong {
			identity.ExecutionID = strings.Repeat("f", 32)
		}
		return videoTestReceipt(identity, "cancelled"), nil
	}
	return videoworker.HLSStatus{Identity: identity, State: "unknown"}, nil
}
func (p *uncertainVideoProcessor) CancelHLS(ctx context.Context, identity videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	return p.LookupHLS(ctx, identity)
}

func TestVideoHLSUnknownExecutionRemainsFencedAndReconciles(t *testing.T) {
	controller := &uncertainVideoProcessor{videoProcessFunc: videoProcessFunc(func(context.Context, videoworker.HLSIdentity, string, int64, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
		return videoworker.HLSSequence{}, errors.New("lost transport")
	})}
	f := newVideoFixture(t, func(string) videoProcessor { return controller })
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "stopping")
	if _, err := os.Stat(filepath.Join(f.root, videoFenceName)); err != nil {
		t.Fatal("unknown execution fence lost", err)
	}
	if w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv"}`, ""); w.Code != 503 && w.Code != 429 {
		// 429 covers the still-running first reconciliation; 503 covers its
		// completed unknown result. Neither may admit another execution.
		t.Fatal("unknown execution admitted next job", w.Code)
	}
	controller.confirmed.Store(true)
	if w := f.request("DELETE", "/api/video-previews/"+created.ID, "owner", "", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	waitVideoState(t, f.server.video, created.ID, "cancelled")
	if _, err := os.Stat(filepath.Join(f.root, videoFenceName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("matching reconciliation did not clear fence", err)
	}
}

func TestVideoHLSRestartPreservesUnknownFence(t *testing.T) {
	controller := &uncertainVideoProcessor{videoProcessFunc: videoProcessFunc(func(context.Context, videoworker.HLSIdentity, string, int64, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
		return videoworker.HLSSequence{}, errors.New("lost transport")
	})}
	f := newVideoFixture(t, func(string) videoProcessor { return controller })
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "stopping")
	options := f.server.video.options
	f.server.CloseVideoPreviews()
	restarted := New(f.store, nil, nil)
	if err := restarted.EnableVideoPreviews(options); err != nil {
		t.Fatal(err)
	}
	defer restarted.CloseVideoPreviews()
	restarted.video.mu.Lock()
	fenced := restarted.video.fenced
	restarted.video.mu.Unlock()
	if !fenced {
		t.Fatal("restart erased execution uncertainty")
	}
	if _, err := os.Stat(filepath.Join(f.root, "incoming", created.ID, videoworker.InputName)); err != nil {
		t.Fatal("unknown execution input removed", err)
	}
}

func TestVideoHLSPrivateCopiesRejectMutationAndBadHash(t *testing.T) {
	t.Run("retained worker fd", func(t *testing.T) {
		var held *os.File
		f := newVideoFixture(t, func(root string) videoProcessor {
			return videoProcessFunc(func(ctx context.Context, identity videoworker.HLSIdentity, _ string, limit int64, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
				return emitVideoFixture(ctx, root, identity, limit, emit, func() error {
					var err error
					held, err = os.OpenFile(filepath.Join(root, "incoming", identity.JobID, videoworker.HLSDirectoryName, "segment-000000.m4s"), os.O_RDWR, 0)
					if err != nil {
						return err
					}
					_, err = held.WriteAt([]byte("EVIL"), 0)
					return err
				})
			})
		})
		created := f.create(t)
		waitVideoState(t, f.server.video, created.ID, "complete")
		defer held.Close()
		w := f.request("GET", "/api/video-previews/"+created.ID+"/segment-000000.m4s", "owner", "", "")
		if w.Code != 200 || w.Body.String() != string(videoSegmentFixtures[0]) {
			t.Fatal("retained worker FD changed cache", w.Code)
		}
	})
	t.Run("bad digest", func(t *testing.T) {
		f := newVideoFixture(t, func(root string) videoProcessor {
			return videoProcessFunc(func(ctx context.Context, identity videoworker.HLSIdentity, _ string, limit int64, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
				return emitVideoFixture(ctx, root, identity, limit, func(event videoworker.HLSEvent) error {
					if event.Type == "init" {
						event.SHA256 = strings.Repeat("0", 64)
					}
					return emit(event)
				}, nil)
			})
		})
		created := f.create(t)
		waitVideoState(t, f.server.video, created.ID, "failed")
		if w := f.request("GET", "/api/video-previews/"+created.ID+"/"+videoworker.HLSPlaylistName, "owner", "", ""); w.Code != 409 {
			t.Fatal("invalid private bytes advertised", w.Code)
		}
	})
}

func TestVideoHLSFailedPrefixIsDeniedBeforeTeardown(t *testing.T) {
	failed := make(chan struct{})
	f := newVideoFixture(t, func(root string) videoProcessor {
		return videoProcessFunc(func(ctx context.Context, identity videoworker.HLSIdentity, _ string, limit int64, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
			return emitVideoFixture(ctx, root, identity, limit, emit, func() error {
				if err := emit(videoworker.HLSEvent{V: 1, Type: "conversion_end", Outcome: "failed", Code: "conversion_failed", SegmentCount: 1, DurationUS: 4000000}); err != nil {
					return err
				}
				close(failed)
				<-ctx.Done()
				return ctx.Err()
			})
		})
	})
	created := f.create(t)
	waitVideoSignal(t, failed)
	if w := f.request("GET", "/api/video-previews/"+created.ID+"/"+videoworker.HLSPlaylistName, "owner", "", ""); w.Code != 409 {
		t.Fatal("failed prefix remains advertised", w.Code)
	}
	f.server.video.mu.Lock()
	busy := f.server.video.busy
	f.server.video.mu.Unlock()
	if !busy {
		t.Fatal("failure released active execution")
	}
}

type unqualifiedVideoProcessor struct{ videoProcessFunc }

func (unqualifiedVideoProcessor) HLSReady(context.Context) (string, error) {
	return "", errors.New("development role is not a controller")
}

func TestVideoHLSUnqualifiedControllerRejectedBeforeStaging(t *testing.T) {
	f := newVideoFixture(t, func(string) videoProcessor { return unqualifiedVideoProcessor{} })
	reader := newGatedVideoReader()
	t.Cleanup(reader.unblock)
	f.server.store = &gatedVideoStore{transferStore: f.store, reader: reader, context: make(chan context.Context, 1)}
	if w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv"}`, ""); w.Code != 503 {
		t.Fatal("unqualified controller accepted", w.Code)
	}
	if reader.reads.Load() != 0 {
		t.Fatal("NAS staging started before qualification")
	}
	if _, err := os.Stat(filepath.Join(f.root, videoFenceName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected controller accepted execution", err)
	}
}

func TestVideoHLSWrongExecutionReceiptCannotClearFence(t *testing.T) {
	controller := &uncertainVideoProcessor{wrong: true, videoProcessFunc: videoProcessFunc(func(context.Context, videoworker.HLSIdentity, string, int64, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
		return videoworker.HLSSequence{}, errors.New("lost transport")
	})}
	controller.confirmed.Store(true)
	f := newVideoFixture(t, func(string) videoProcessor { return controller })
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "stopping")
	f.server.CloseVideoPreviews()
	if _, err := os.Stat(filepath.Join(f.root, videoFenceName)); err != nil {
		t.Fatal("wrong execution receipt cleared fence", err)
	}
}

func TestVideoHLSRealFFmpegUnixAndAuthenticatedAPI(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("set VIDEO_WORKER_INTEGRATION=1 for synthetic FFmpeg/Unix integration")
	}
	for _, channels := range []int{0, 1, 2, 6} {
		t.Run(fmt.Sprintf("audio-channels=%d", channels), func(t *testing.T) { testVideoHLSRealFFmpegUnix(t, channels) })
	}
}

func testVideoHLSRealFFmpegUnix(t *testing.T, channels int) {
	f := newVideoFixture(t, nil)
	socketRoot, err := os.MkdirTemp("", "vh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	movie := filepath.Join(socketRoot, "movie.mkv")
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=160x120:r=10"}
	if channels > 0 {
		layout := map[int]string{1: "mono", 2: "stereo", 6: "5.1"}[channels]
		args = append(args, "-f", "lavfi", "-i", "anullsrc=r=48000:cl="+layout, "-map", "0:v:0", "-map", "1:a:0", "-c:a", "aac", "-profile:a", "aac_low", "-ac", fmt.Sprint(channels))
	}
	args = append(args, "-t", "8", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-g", "40", movie)
	if output, err := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("synthetic input: %v %s", err, output)
	}
	data, err := os.ReadFile(movie)
	if err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	f.store.data["users/42/movie.mkv"] = data
	f.store.mu.Unlock()
	socket := filepath.Join(socketRoot, "worker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var finished atomic.Bool
	// Test-only synthetic lifecycle receipt. This wrapper is NOT a production
	// sandbox controller and proves no compromised-decoder containment.
	controller := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "isolation": videoworker.HLSControllerRole, "controller_epoch": strings.Repeat("a", 32)})
			return
		}
		if r.Method != "POST" {
			identity := videoworker.HLSIdentity{JobID: filepath.Base(r.URL.Path), ControllerEpoch: r.URL.Query().Get("controller_epoch"), ExecutionID: r.URL.Query().Get("execution_id")}
			state := "unknown"
			var terminal *videoworker.HLSEvent
			if finished.Load() {
				state = "quiescent"
				terminal = videoTestReceipt(identity, "cancelled").Terminal
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "job_id": identity.JobID, "controller_epoch": identity.ControllerEpoch, "execution_id": identity.ExecutionID, "state": state, "terminal": terminal})
			return
		}
		var body struct {
			ExecutionID     string `json:"execution_id"`
			ControllerEpoch string `json:"controller_epoch"`
			Format          string `json:"format"`
			MaxOutputBytes  int64  `json:"max_output_bytes"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		identity := videoworker.HLSIdentity{JobID: parts[3], ControllerEpoch: body.ControllerEpoch, ExecutionID: body.ExecutionID}
		w.Header().Set("X-Video-Controller-Epoch", identity.ControllerEpoch)
		w.Header().Set("X-Video-Execution-ID", identity.ExecutionID)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, runError := videoworker.RunDevelopmentHLS(r.Context(), filepath.Join(f.root, "incoming"), identity.JobID, body.Format, videoworker.Options{FFmpeg: "/usr/bin/ffmpeg", FFprobe: "/usr/bin/ffprobe", MaxInputBytes: 1 << 20, MaxOutputBytes: body.MaxOutputBytes, Timeout: 5 * time.Second}, body.MaxOutputBytes, func(event videoworker.HLSEvent) error {
			if err := videoworker.EncodeHLSEvent(w, event); err != nil {
				return err
			}
			w.(http.Flusher).Flush()
			return nil
		})
		finished.Store(true)
		if runError == nil {
			_ = videoworker.EncodeHLSEvent(w, *videoTestReceipt(identity, "complete").Terminal)
		}
	})}
	go func() { _ = controller.Serve(listener) }()
	defer controller.Close()
	f.server.video.processor = videoworker.NewClient(socket)
	f.server.video.options.Timeout = 10 * time.Second
	created := f.create(t)
	waitVideoState(t, f.server.video, created.ID, "complete")
	f.server.video.mu.Lock()
	actualMode := f.server.video.jobs[created.ID].Mode
	f.server.video.mu.Unlock()
	expectedMode := "remux"
	if channels == 6 {
		expectedMode = "transcode"
	}
	if actualMode != expectedMode {
		t.Fatalf("audio channels %d: got %s, want %s", channels, actualMode, expectedMode)
	}
	w := f.request("GET", "/api/video-previews/"+created.ID+"/"+videoworker.HLSPlaylistName, "owner", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatal("real producer/private validation failed", w.Code, w.Body)
	}
	w = f.request("GET", "/api/video-previews/"+created.ID+"/segment-000000.m4s", "owner", "", "bytes=0-15")
	if w.Code != 206 || w.Body.Len() != 16 {
		t.Fatal("real fragment range failed", w.Code, w.Body)
	}
}

func TestVideoHLSScratchRootAndStaleInputFailClosed(t *testing.T) {
	f := newVideoFixture(t, nil)
	other := New(f.store, nil, nil)
	options := f.server.video.options
	if err := other.EnableVideoPreviews(options); err == nil {
		other.CloseVideoPreviews()
		t.Fatal("second manager acquired scratch")
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	options.Root = link
	if err := other.EnableVideoPreviews(options); err == nil {
		other.CloseVideoPreviews()
		t.Fatal("symlink root accepted")
	}
	orphan := filepath.Join(f.root, "incoming", strings.Repeat("b", 32))
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "unknown"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if w := f.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv"}`, ""); w.Code != 507 {
			t.Fatal("stale input adopted", w.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(orphan, "unknown")); err != nil {
		t.Fatal("unknown data deleted", err)
	}
}
