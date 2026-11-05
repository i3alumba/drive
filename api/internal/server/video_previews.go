package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/videoworker"
)

type videoProcessor interface {
	Process(context.Context, string, string, int64) (videoworker.Result, error)
}

// VideoPreviewOptions enables only an explicitly configured, isolated scratch
// volume. The decoder is a separate Unix-socket worker, never an API subprocess.
// An empty configuration leaves the feature off without changing existing media.
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
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type videoPreviewJob struct {
	videoPreviewStatus
	owner, sourceKey, sourcePath, space string
	source                              storage.Object
	sourceSHA                           string
	cancel                              context.CancelFunc
}

type videoPreviewManager struct {
	mu                    sync.Mutex
	options               VideoPreviewOptions
	root, incoming, ready *os.File
	lock                  *os.File
	processor             videoProcessor
	jobs                  map[string]*videoPreviewJob
	busy, closed          bool
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
	for _, f := range []*os.File{m.lock, m.ready, m.incoming, m.root} {
		if f != nil {
			_ = f.Close()
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
	if s.video == nil {
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
	request := videoRequestSpace(r, body.Space)
	resolved, err := s.resolvePath(request, body.Path, false)
	if err != nil {
		writeVideoError(w, 404, "video_source_unavailable")
		return
	}
	info, err := s.store.StatObject(r.Context(), resolved.Key)
	if err != nil || info.IsDir || info.Size <= 0 || info.Size > s.video.options.MaxInputBytes {
		writeVideoError(w, 422, "video_source_unavailable_or_too_large")
		return
	}
	m := s.video
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(time.Now())
	if m.closed {
		writeVideoError(w, 503, "video_preview_unavailable")
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
	if err := uploadMkdir(m.incoming, id); err != nil {
		writeVideoError(w, 503, "video_preview_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.options.Timeout)
	now := time.Now().UTC()
	job := &videoPreviewJob{videoPreviewStatus: videoPreviewStatus{ID: id, Status: "preparing", CreatedAt: now, ExpiresAt: now.Add(m.options.TTL)}, owner: currentUser(r).ID, sourceKey: resolved.Key, sourcePath: body.Path, space: body.Space, source: info, cancel: cancel}
	m.jobs[id] = job
	m.busy = true
	m.workers.Add(1)
	go s.prepareVideo(ctx, m, job, format)
	writeJSON(w, 202, job.videoPreviewStatus)
}

func videoRequestSpace(r *http.Request, space string) *http.Request {
	request := r.Clone(r.Context())
	urlCopy := *r.URL
	request.URL = &urlCopy
	query := request.URL.Query()
	query.Set("space", space)
	request.URL.RawQuery = query.Encode()
	return request
}

func (s *Server) prepareVideo(ctx context.Context, m *videoPreviewManager, job *videoPreviewJob, format string) {
	defer m.workers.Done()
	defer job.cancel()
	problem := "video_preparation_failed"
	result := videoworker.Result{}
	dir, err := uploadOpenDir(m.incoming, job.ID)
	if err == nil {
		defer dir.Close()
		err = s.stageVideoSource(ctx, m, job, dir)
	}
	if err == nil {
		result, err = m.processor.Process(ctx, job.ID, format, m.options.MaxOutputBytes)
	}
	if err == nil {
		err = m.copyVideoResult(ctx, job.ID, dir, result)
	}
	// Ready cache uses a fresh API-owned inode, not a rename/hardlink of the
	// worker output: a decoder retaining an old FD cannot mutate cached bytes.
	if dir != nil {
		for _, name := range []string{videoworker.InputName, videoworker.OutputName, "output.part.mp4"} {
			_ = uploadUnlink(dir, name, false)
		}
		_ = uploadUnlink(m.incoming, job.ID, true)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Do not publish ready after cancellation/deadline, including one that
	// arrived during the final fsync. Outstanding I/O is never abandoned.
	if ctx.Err() != nil {
		err = ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			problem = "video_preparation_timeout"
		}
	}
	m.busy = false
	if job.Status == "cancelling" || m.closed {
		job.Status = "cancelled"
		job.Error = ""
		m.cleanJobLocked(job.ID)
		return
	}
	if err != nil {
		job.Status = "failed"
		job.Error = problem
		m.cleanJobLocked(job.ID)
		return
	}
	job.Status = "ready"
	job.Mode = result.Mode
}

func (m *videoPreviewManager) copyVideoResult(ctx context.Context, id string, sourceDir *os.File, result videoworker.Result) error {
	output, err := uploadOpenFile(sourceDir, videoworker.OutputName, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer output.Close()
	stat, err := output.Stat()
	if err != nil || stat.Size() <= 0 || stat.Size() != result.Size || stat.Size() > m.options.MaxOutputBytes || (result.Mode != "remux" && result.Mode != "transcode") {
		return os.ErrInvalid
	}
	return m.copyVideoOutput(ctx, id, output, result.Size)
}

// copyVideoOutput owns only its new private cache inode. It is separate from
// worker-output validation so its cancellation/cleanup boundary can be tested
// with a filesystem-like reader that deliberately ignores context.
func (m *videoPreviewManager) copyVideoOutput(ctx context.Context, id string, output io.Reader, size int64) (err error) {
	if !validVideoID(id) || size <= 0 || size > m.options.MaxOutputBytes {
		return os.ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = uploadMkdir(m.ready, id); err != nil {
		return err
	}
	dir, err := uploadOpenDir(m.ready, id)
	if err != nil {
		_ = uploadUnlink(m.ready, id, true)
		return err
	}
	ownedOutput := false
	defer func() {
		if err != nil && ownedOutput {
			_ = uploadUnlink(dir, videoworker.OutputName, false)
		}
		dir.Close()
		if err != nil {
			_ = uploadUnlink(m.ready, id, true)
		}
	}()
	file, err := uploadOpenFile(dir, videoworker.OutputName, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	ownedOutput = true
	defer file.Close()
	count, err := copyVideoBytes(ctx, file, io.LimitReader(output, size+1))
	if err != nil {
		return err
	}
	if count != size {
		return errors.New("invalid video output copy")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return file.Close()
}

// copyVideoBytes checks cancellation between bounded I/O operations, including
// after a reader that ignores context returns. It cannot interrupt a kernel-
// blocked read/write or fsync; the caller must keep admission fenced until that
// operation actually returns, rather than abandoning a goroutine.
func copyVideoBytes(ctx context.Context, writer io.Writer, reader io.Reader) (int64, error) {
	buffer := make([]byte, 32<<10)
	var copied int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		n, readErr := reader.Read(buffer)
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if n > 0 {
			emptyReads = 0
			written, err := writer.Write(buffer[:n])
			copied += int64(written)
			if err != nil {
				return copied, err
			}
			if written != n {
				return copied, io.ErrShortWrite
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return copied, io.ErrNoProgress
			}
		}
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if readErr == io.EOF {
			return copied, nil
		}
		if readErr != nil {
			return copied, readErr
		}
	}
}

func (s *Server) stageVideoSource(ctx context.Context, m *videoPreviewManager, job *videoPreviewJob, dir *os.File) error {
	source, info, err := s.store.GetObject(ctx, job.sourceKey)
	if err != nil {
		return err
	}
	defer source.Close()
	if info.Size != job.source.Size || info.Size > m.options.MaxInputBytes {
		return os.ErrInvalid
	}
	file, err := uploadOpenFile(dir, videoworker.InputName, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	checksum := sha256.New()
	count, err := copyVideoBytes(ctx, io.MultiWriter(file, checksum), io.LimitReader(source, info.Size+1))
	if err != nil {
		return err
	}
	if count != info.Size {
		return errors.New("invalid video source read")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	current, err := s.store.StatObject(ctx, job.sourceKey)
	if err != nil || current.Size != job.source.Size || !current.LastModified.Equal(job.source.LastModified) {
		return errors.New("video source changed")
	}
	// Hash describes the exact staged snapshot, never an authoritative repair
	// claim about NAS/header integrity. No reuse by filename/size alone.
	m.mu.Lock()
	job.sourceSHA = hex.EncodeToString(checksum.Sum(nil))
	m.mu.Unlock()
	return file.Close()
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
	if job == nil || job.owner != currentUser(r).ID || m.closed {
		m.mu.Unlock()
		writeVideoError(w, 404, "video_preview_not_found")
		return nil, nil
	}
	copyJob := *job
	m.mu.Unlock()
	resolved, err := s.resolvePath(videoRequestSpace(r, copyJob.space), copyJob.sourcePath, false)
	if err != nil || resolved.Key != copyJob.sourceKey {
		writeVideoError(w, 404, "video_preview_not_found")
		return nil, nil
	}
	info, err := s.store.StatObject(r.Context(), copyJob.sourceKey)
	if err != nil || info.Size != copyJob.source.Size || !info.LastModified.Equal(copyJob.source.LastModified) {
		writeVideoError(w, 409, "video_source_changed")
		return nil, nil
	}
	return m, &copyJob
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
	wasPreparing := job.Status == "preparing" || job.Status == "cancelling"
	job.cancel()
	job.Error = ""
	if wasPreparing {
		// A cancellation request is not a receipt that blocked I/O has ended.
		job.Status = "cancelling"
	} else {
		job.Status = "cancelled"
		m.cleanJobLocked(job.ID)
	}
	writeJSON(w, 200, job.videoPreviewStatus)
}

func (s *Server) serveVideoPreview(w http.ResponseWriter, r *http.Request) {
	m, job := s.videoJob(w, r)
	if job == nil {
		return
	}
	if job.Status != "ready" {
		writeVideoError(w, 409, "video_preview_not_ready")
		return
	}
	m.mu.Lock()
	dir, err := uploadOpenDir(m.ready, job.ID)
	if err != nil {
		m.mu.Unlock()
		writeVideoError(w, 404, "video_preview_not_found")
		return
	}
	file, err := uploadOpenFile(dir, videoworker.OutputName, os.O_RDONLY)
	dir.Close()
	m.mu.Unlock()
	if err != nil {
		writeVideoError(w, 404, "video_preview_not_found")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", `inline; filename="video-preview.mp4"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.ServeContent(videoPreviewResponse{w}, r, "video-preview.mp4", time.Time{}, file)
}

type videoPreviewResponse struct{ http.ResponseWriter }

func (w videoPreviewResponse) WriteHeader(code int) {
	// ServeContent otherwise strips Cache-Control on errors such as 416.
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(code)
}

func (m *videoPreviewManager) expireLocked(now time.Time) {
	for id, job := range m.jobs {
		if job.Status != "preparing" && job.Status != "cancelling" && !job.ExpiresAt.After(now) {
			m.cleanJobLocked(id)
			delete(m.jobs, id)
		}
	}
}

func (m *videoPreviewManager) cleanJobLocked(id string) {
	// Only exact known files from this manager's jobs; never recursively delete
	// unknown files/directories, imports, uploads, torrents or original objects.
	for _, parent := range []*os.File{m.incoming, m.ready} {
		dir, err := uploadOpenDir(parent, id)
		if err != nil {
			continue
		}
		for _, name := range []string{videoworker.InputName, videoworker.OutputName, "output.part.mp4"} {
			_ = uploadUnlink(dir, name, false)
		}
		dir.Close()
		_ = uploadUnlink(parent, id, true)
	}
}

func (m *videoPreviewManager) scratchUsageLocked() (int64, error) {
	var total int64
	for _, name := range []string{"incoming", "ready"} {
		parent, err := uploadOpenDir(m.root, name)
		if err != nil {
			return 0, err
		}
		defer parent.Close()
		entries, err := parent.ReadDir(257)
		if err != nil && err != io.EOF || len(entries) > 256 {
			return 0, errors.New("video scratch inventory unavailable")
		}
		for _, entry := range entries {
			if entry.Name() == videoworker.SocketName && entry.Type()&os.ModeSocket != 0 {
				continue
			}
			// Idle incoming must contain only the control socket. Unknown or
			// crash-left jobs are never adopted or exposed to another decoder.
			if name == "incoming" || !validVideoID(entry.Name()) || !entry.IsDir() {
				return 0, os.ErrInvalid
			}
			dir, err := uploadOpenDir(parent, entry.Name())
			if err != nil {
				return 0, err
			}
			files, err := dir.ReadDir(8)
			if err != nil && err != io.EOF || len(files) > 7 {
				dir.Close()
				return 0, os.ErrInvalid
			}
			for _, f := range files {
				file, err := uploadOpenFile(dir, f.Name(), os.O_RDONLY)
				if err != nil {
					dir.Close()
					return 0, err
				}
				info, err := file.Stat()
				file.Close()
				if err != nil || info.Size() > m.options.BudgetBytes-total {
					dir.Close()
					return 0, os.ErrInvalid
				}
				total += info.Size()
			}
			dir.Close()
		}
	}
	return total, nil
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
