package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/videomedia"
	"remote-drive/api/internal/videoworker"
)

type videoProcessor interface {
	HLSReady(context.Context) (string, error)
	StreamHLS(context.Context, videoworker.HLSIdentity, string, int64, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error)
	LookupHLS(context.Context, videoworker.HLSIdentity) (videoworker.HLSStatus, error)
	CancelHLS(context.Context, videoworker.HLSIdentity) (videoworker.HLSStatus, error)
}

// VideoPreviewOptions enables only explicitly configured private scratch. The
// default client requires an isolated controller, never the development runner.
type VideoPreviewOptions struct {
	Root           string
	MaxInputBytes  int64
	MaxOutputBytes int64
	BudgetBytes    int64
	ReserveBytes   int64
	TTL            time.Duration
	Timeout        time.Duration
	processor      videoProcessor
}

type videoPreviewStatus struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
	Mode             string    `json:"mode,omitempty"`
	Duration         float64   `json:"duration,omitempty"`
	PreparedDuration float64   `json:"preparedDuration,omitempty"`
	Segments         int       `json:"segments"`
	CreatedAt        time.Time `json:"createdAt"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

type videoPreviewJob struct {
	videoPreviewStatus
	owner, sourceKey, sourcePath, space string
	source                              storage.Object
	sourceSHA                           string
	cancel                              context.CancelFunc
	execution                           context.Context
	identity                            videoworker.HLSIdentity
	running, attempted, uncertain       bool
	expired                             bool
	assets                              map[string]int64
	durations                           []int64
	bytes                               int64
	config                              *videomedia.Config
	timeline                            videomedia.Timeline
}

type videoPreviewManager struct {
	mu                    sync.Mutex
	options               VideoPreviewOptions
	root, incoming, ready *os.File
	lock                  *os.File
	processor             videoProcessor
	jobs                  map[string]*videoPreviewJob
	busy, closed, fenced  bool
	workers               sync.WaitGroup
	stop, stopped         chan struct{}
}

func (s *Server) EnableVideoPreviews(options VideoPreviewOptions) error {
	if !filepath.IsAbs(options.Root) || options.MaxInputBytes < 1 || options.MaxOutputBytes < 1 || options.BudgetBytes < options.MaxInputBytes || options.MaxOutputBytes > (options.BudgetBytes-options.MaxInputBytes)/2 || options.ReserveBytes < 0 || options.TTL <= 0 || options.TTL > 24*time.Hour || options.Timeout <= 0 || options.Timeout > 2*time.Hour {
		return errors.New("invalid video preview configuration")
	}
	root, err := videoOpenRoot(options.Root)
	if err != nil {
		return errors.New("video scratch must be separately initialized")
	}
	m := &videoPreviewManager{options: options, root: root, jobs: make(map[string]*videoPreviewJob), stop: make(chan struct{}), stopped: make(chan struct{})}
	fail := func() error { m.closeFiles(); return errors.New("invalid or unfenced video scratch") }
	if uploadPrivateDir(root) != nil {
		return fail()
	}
	marker, err := uploadOpenFile(root, ".video-worker-v1", os.O_RDONLY)
	if err != nil {
		return fail()
	}
	markerInfo, err := marker.Stat()
	marker.Close()
	if err != nil || markerInfo.Size() != 0 || markerInfo.Mode().Perm()&0077 != 0 {
		return fail()
	}
	if m.incoming, err = uploadOpenDir(root, "incoming"); err != nil || uploadPrivateDir(m.incoming) != nil {
		return fail()
	}
	if m.ready, err = uploadOpenDir(root, "ready"); err != nil || uploadPrivateDir(m.ready) != nil {
		return fail()
	}
	m.lock, err = uploadOpenFile(root, ".api.lock", os.O_CREATE|os.O_RDWR)
	if err != nil || uploadTryLock(m.lock) != nil {
		return fail()
	}
	// A restart cannot turn an unknown execution into available capacity.
	fence, fenceErr := uploadOpenFile(root, videoFenceName, os.O_RDONLY)
	if fenceErr == nil {
		fence.Close()
		m.fenced = true
	} else if !errors.Is(fenceErr, os.ErrNotExist) {
		m.fenced = true
	}
	m.processor = options.processor
	if m.processor == nil {
		m.processor = videoworker.NewClient(filepath.Join(options.Root, "incoming", videoworker.SocketName))
	}
	s.video = m
	go m.expirer()
	return nil
}

func (m *videoPreviewManager) expirer() {
	defer close(m.stopped)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			m.mu.Lock()
			m.expireLocked(now)
			m.mu.Unlock()
		}
	}
}

func (m *videoPreviewManager) closeFiles() {
	for _, file := range []*os.File{m.lock, m.ready, m.incoming, m.root} {
		if file != nil {
			_ = file.Close()
		}
	}
}

func (s *Server) CloseVideoPreviews() {
	if s.video == nil {
		return
	}
	m := s.video
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stop)
	for _, job := range m.jobs {
		job.cancel()
	}
	m.mu.Unlock()
	m.workers.Wait()
	<-m.stopped
	m.closeFiles()
}

func writeVideoError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	if code == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, code, map[string]string{"error": message})
}

func videoPathValid(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.HasPrefix(value, "/") && path.Clean(value) == value && validUploadPath(value)
}

func (s *Server) createVideoPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	m := s.video
	if m == nil {
		writeVideoError(w, 503, "video_preview_unavailable")
		return
	}
	var body struct {
		Path  string `json:"path"`
		Space string `json:"space"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.Decode(new(any)) != io.EOF || !videoPathValid(body.Path) {
		writeVideoError(w, 400, "invalid_video_request")
		return
	}
	format := strings.ToLower(strings.TrimPrefix(filepath.Ext(body.Path), "."))
	if format != "mkv" && format != "avi" {
		writeVideoError(w, 415, "unsupported_video_container")
		return
	}
	resolved, err := s.resolvePath(videoRequestSpace(r, body.Space), body.Path, false)
	if err != nil {
		writeVideoError(w, 404, "video_source_unavailable")
		return
	}
	info, err := s.store.StatObject(r.Context(), resolved.Key)
	if err != nil || info.IsDir || info.Size <= 0 || info.Size > m.options.MaxInputBytes {
		writeVideoError(w, 422, "video_source_unavailable_or_too_large")
		return
	}
	// Reject unqualified development execution before reading any source bytes.
	control, controlCancel := context.WithTimeout(r.Context(), 5*time.Second)
	epoch, err := m.processor.HLSReady(control)
	controlCancel()
	if err != nil || !validVideoID(epoch) {
		writeVideoError(w, 503, "video_controller_unavailable_or_unqualified")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(time.Now())
	if m.closed || m.fenced {
		writeVideoError(w, 503, "video_execution_fenced")
		return
	}
	if m.busy || len(m.jobs) >= 32 {
		writeVideoError(w, 429, "video_preview_busy")
		return
	}
	used, err := m.scratchUsageLocked()
	free, freeErr := uploadFreeBytes(m.root)
	need := info.Size + 2*m.options.MaxOutputBytes
	if err != nil || freeErr != nil || used > m.options.BudgetBytes-need || free < m.options.ReserveBytes || need > free-m.options.ReserveBytes {
		writeVideoError(w, 507, "video_preview_capacity")
		return
	}
	id := randomID()[:32]
	identity := videoworker.HLSIdentity{JobID: id, ControllerEpoch: epoch, ExecutionID: randomID()[:32]}
	if err := m.createFence(identity); err != nil {
		m.fenced = true
		writeVideoError(w, 503, "video_execution_fenced")
		return
	}
	if err := uploadMkdir(m.incoming, id); err != nil {
		m.fenced = m.removeFence(identity) != nil
		writeVideoError(w, 503, "video_preview_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.options.Timeout)
	now := time.Now().UTC()
	job := &videoPreviewJob{videoPreviewStatus: videoPreviewStatus{ID: id, Status: "staging", CreatedAt: now, ExpiresAt: now.Add(m.options.TTL)}, owner: currentUser(r).ID, sourceKey: resolved.Key, sourcePath: body.Path, space: body.Space, source: info, identity: identity, cancel: cancel, execution: ctx, running: true, assets: make(map[string]int64)}
	m.jobs[id] = job
	m.busy = true
	m.workers.Add(1)
	go s.prepareVideo(ctx, m, job, format)
	writeJSON(w, 202, job.videoPreviewStatus)
}

func videoRequestSpace(r *http.Request, space string) *http.Request {
	request := r.Clone(r.Context())
	copyURL := *r.URL
	request.URL = &copyURL
	query := request.URL.Query()
	query.Set("space", space)
	request.URL.RawQuery = query.Encode()
	return request
}

func (s *Server) videoJob(w http.ResponseWriter, r *http.Request) (*videoPreviewManager, *videoPreviewJob) {
	w.Header().Set("Cache-Control", "no-store")
	m := s.video
	if m == nil {
		writeVideoError(w, 503, "video_preview_unavailable")
		return nil, nil
	}
	m.mu.Lock()
	m.expireLocked(time.Now())
	job := m.jobs[r.PathValue("id")]
	if job == nil || job.owner != currentUser(r).ID || m.closed || job.expired {
		m.mu.Unlock()
		writeVideoError(w, 404, "video_preview_not_found")
		return nil, nil
	}
	snapshot := *job
	// Do not copy or expose the mutable artifact ledger outside its lock.
	snapshot.assets, snapshot.durations, snapshot.config = nil, nil, nil
	m.mu.Unlock()
	resolved, err := s.resolvePath(videoRequestSpace(r, snapshot.space), snapshot.sourcePath, false)
	if err != nil || resolved.Key != snapshot.sourceKey {
		writeVideoError(w, 404, "video_preview_not_found")
		return nil, nil
	}
	info, err := s.store.StatObject(r.Context(), snapshot.sourceKey)
	if err != nil || info.Size != snapshot.source.Size || !info.LastModified.Equal(snapshot.source.LastModified) {
		writeVideoError(w, 409, "video_source_changed")
		return nil, nil
	}
	return m, &snapshot
}

func (s *Server) getVideoPreview(w http.ResponseWriter, r *http.Request) {
	_, job := s.videoJob(w, r)
	if job != nil {
		writeJSON(w, 200, job.videoPreviewStatus)
	}
}

func (s *Server) cancelVideoPreview(w http.ResponseWriter, r *http.Request) {
	m, snapshot := s.videoJob(w, r)
	if snapshot == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[snapshot.ID]
	if job == nil {
		writeVideoError(w, 404, "video_preview_not_found")
		return
	}
	job.cancel()
	if job.running {
		job.Status, job.Error = "cancelling", ""
	} else if job.uncertain {
		job.Status, job.Error, job.running = "cancelling", "", true
		m.workers.Add(1)
		go m.reconcileVideo(job)
	} else {
		job.Status, job.Error = "cancelled", ""
		_ = m.cleanVideoDirectory(m.ready, job.ID)
	}
	writeJSON(w, 200, job.videoPreviewStatus)
}

func (s *Server) serveVideoPreview(w http.ResponseWriter, r *http.Request) {
	m, snapshot := s.videoJob(w, r)
	if snapshot == nil {
		return
	}
	asset := r.PathValue("asset")
	if asset == "" || asset == "content" {
		asset = videoworker.HLSPlaylistName
	}
	m.mu.Lock()
	m.expireLocked(time.Now())
	job := m.jobs[snapshot.ID]
	if job == nil || job.expired || (job.Status != "streaming" && job.Status != "complete") || job.Segments == 0 {
		m.mu.Unlock()
		writeVideoError(w, 409, "video_stream_not_ready")
		return
	}
	if asset == videoworker.HLSPlaylistName {
		durations := append([]int64(nil), job.durations...)
		complete := job.Status == "complete"
		m.mu.Unlock()
		playlist, err := videoworker.HLSManifest(durations, complete)
		if err != nil {
			writeVideoError(w, 503, "video_stream_unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(videoPreviewResponse{w}, r, asset, time.Time{}, strings.NewReader(playlist))
		return
	}
	if !videoAssetName(asset) || job.assets[asset] <= 0 {
		m.mu.Unlock()
		writeVideoError(w, 404, "video_artifact_not_found")
		return
	}
	dir, err := uploadOpenDir(m.ready, job.ID)
	if err != nil {
		m.mu.Unlock()
		writeVideoError(w, 404, "video_artifact_not_found")
		return
	}
	file, err := uploadOpenFile(dir, asset, os.O_RDONLY)
	dir.Close()
	m.mu.Unlock()
	if err != nil {
		writeVideoError(w, 404, "video_artifact_not_found")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.ServeContent(videoPreviewResponse{w}, r, asset, time.Time{}, file)
}

type videoPreviewResponse struct{ http.ResponseWriter }

func (w videoPreviewResponse) WriteHeader(code int) {
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(code)
}

func (m *videoPreviewManager) expireLocked(now time.Time) {
	for id, job := range m.jobs {
		// Serving/status denial is independent of cooperative I/O returning.
		// Never release capacity, clean files or cancel a completed cache here.
		if job.running && job.execution != nil && job.execution.Err() != nil && (job.Status == "staging" || job.Status == "preparing" || job.Status == "streaming") {
			job.Status, job.Error = "stopping", videoFailureCode(job.execution)
		}
		if job.ExpiresAt.After(now) {
			continue
		}
		if job.running || job.uncertain {
			job.expired = true
			job.cancel()
			if job.running {
				job.Status = "cancelling"
			}
			continue
		}
		_ = m.cleanVideoDirectory(m.ready, id)
		delete(m.jobs, id)
	}
}

func videoAssetName(name string) bool {
	if name == videoworker.HLSInitName {
		return true
	}
	if len(name) != len("segment-000000.m4s") || !strings.HasPrefix(name, "segment-") || !strings.HasSuffix(name, ".m4s") {
		return false
	}
	value := name[len("segment-") : len(name)-len(".m4s")]
	index, err := strconv.Atoi(value)
	if err != nil {
		return false
	}
	canonical, err := videoworker.HLSSegmentName(index)
	return err == nil && canonical == name
}

func validVideoID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
