package torrent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"remote-drive/api/internal/storage"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusDownloading Status = "downloading"
	StatusUploading   Status = "uploading"
	StatusPausing     Status = "pausing"
	StatusPaused      Status = "paused"
	StatusCancelled   Status = "cancelled"
	StatusComplete    Status = "complete"
	StatusFailed      Status = "failed"
)

type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TargetDir string    `json:"targetDir"`
	Status    Status    `json:"status"`
	Progress  float64   `json:"progress"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	torrentFile     string
	workDir         string
	cancel          context.CancelFunc
	pauseRequested  bool
	cancelRequested bool
}

type Manager struct {
	store   *storage.Store
	workDir string
	timeout time.Duration
	mu      sync.RWMutex
	jobs    map[string]*Job
}

func NewManager(store *storage.Store, workDir string, timeout time.Duration) *Manager {
	return &Manager{store: store, workDir: workDir, timeout: timeout, jobs: map[string]*Job{}}
}

func (m *Manager) Start(torrentFile string, name string, targetDir string) Job {
	id := uuid.NewString()
	job := &Job{
		ID:          id,
		Name:        name,
		TargetDir:   storage.DirPrefix(targetDir),
		Status:      StatusQueued,
		Progress:    0,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		torrentFile: torrentFile,
		workDir:     filepath.Join(m.workDir, id),
	}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()
	go m.run(job.ID)
	return publicJob(job)
}

func (m *Manager) Pause(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return Job{}, ErrNotFound
	}
	if job.Status == StatusPaused {
		return publicJob(job), nil
	}
	if job.Status != StatusQueued && job.Status != StatusDownloading {
		return Job{}, fmt.Errorf("cannot pause job in %s state", job.Status)
	}
	job.pauseRequested = true
	job.cancelRequested = false
	if job.cancel != nil {
		job.Status = StatusPausing
		job.cancel()
	} else {
		job.Status = StatusPaused
	}
	job.UpdatedAt = time.Now()
	return publicJob(job), nil
}

func (m *Manager) Resume(id string) (Job, error) {
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return Job{}, ErrNotFound
	}
	if job.Status != StatusPaused {
		public := publicJob(job)
		m.mu.Unlock()
		return public, nil
	}
	job.Status = StatusQueued
	job.Error = ""
	job.pauseRequested = false
	job.cancelRequested = false
	job.UpdatedAt = time.Now()
	public := publicJob(job)
	m.mu.Unlock()
	go m.run(id)
	return public, nil
}

func (m *Manager) Cancel(id string) (Job, error) {
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return Job{}, ErrNotFound
	}
	if terminal(job.Status) {
		public := publicJob(job)
		m.mu.Unlock()
		return public, nil
	}
	wasPaused := job.Status == StatusPaused
	job.cancelRequested = true
	job.pauseRequested = false
	job.Status = StatusCancelled
	job.Error = ""
	job.UpdatedAt = time.Now()
	cancel := job.cancel
	workDir := job.workDir
	torrentFile := job.torrentFile
	public := publicJob(job)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cancel == nil || wasPaused {
		go cleanup(workDir, torrentFile)
	}
	return public, nil
}

func (m *Manager) Get(id string) (Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return publicJob(job), true
}

func (m *Manager) List() []Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := make([]Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, publicJob(job))
	}
	return jobs
}

var ErrNotFound = errors.New("torrent job not found")

func (m *Manager) run(id string) {
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return
	}
	if job.Status == StatusCancelled || job.Status == StatusPaused || job.Status == StatusPausing {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	job.cancel = cancel
	job.pauseRequested = false
	job.cancelRequested = false
	job.UpdatedAt = time.Now()
	jobWorkDir := job.workDir
	torrentFile := job.torrentFile
	m.mu.Unlock()
	defer cancel()

	if err := os.MkdirAll(jobWorkDir, 0o755); err != nil {
		m.fail(id, err)
		cleanup(jobWorkDir, torrentFile)
		return
	}

	m.update(id, StatusDownloading, currentProgress(id, m, 0.1), "")
	cmd := exec.CommandContext(ctx, "aria2c", "--continue=true", "--seed-time=0", "--allow-overwrite=true", "--dir", jobWorkDir, torrentFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if m.wasRequested(id, StatusPaused) {
			m.markPaused(id)
			return
		}
		if m.wasRequested(id, StatusCancelled) {
			cleanup(jobWorkDir, torrentFile)
			return
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		m.fail(id, fmt.Errorf("aria2c failed: %s", message))
		cleanup(jobWorkDir, torrentFile)
		return
	}

	if m.wasRequested(id, StatusCancelled) {
		cleanup(jobWorkDir, torrentFile)
		return
	}
	m.update(id, StatusUploading, 1, "")
	jobSnapshot, _ := m.Get(id)
	if err := filepath.WalkDir(jobWorkDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".aria2") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		rel, err := filepath.Rel(jobWorkDir, path)
		if err != nil {
			return err
		}
		objectPath := storage.CleanObjectPath(jobSnapshot.TargetDir + filepath.ToSlash(rel))
		if err := m.store.PutObject(ctx, objectPath, file, info.Size(), ""); err != nil {
			return fmt.Errorf("upload %s: %w", objectPath, err)
		}
		return nil
	}); err != nil {
		if m.wasRequested(id, StatusCancelled) {
			cleanup(jobWorkDir, torrentFile)
			return
		}
		slog.Error("torrent upload failed", "job", id, "err", err)
		m.fail(id, err)
		cleanup(jobWorkDir, torrentFile)
		return
	}
	m.update(id, StatusComplete, 1, "")
	cleanup(jobWorkDir, torrentFile)
}

func currentProgress(id string, m *Manager, fallback float64) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if job := m.jobs[id]; job != nil && job.Progress > fallback {
		return job.Progress
	}
	return fallback
}

func (m *Manager) wasRequested(id string, status Status) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job := m.jobs[id]
	if job == nil {
		return false
	}
	switch status {
	case StatusPaused:
		return job.pauseRequested || job.Status == StatusPaused
	case StatusCancelled:
		return job.cancelRequested || job.Status == StatusCancelled
	default:
		return false
	}
}

func (m *Manager) update(id string, status Status, progress float64, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.Status == StatusCancelled || job.Status == StatusPaused || job.Status == StatusPausing {
		return
	}
	job.Status = status
	job.Progress = progress
	job.Error = message
	job.UpdatedAt = time.Now()
}

func (m *Manager) fail(id string, err error) {
	m.update(id, StatusFailed, 0, err.Error())
}

func (m *Manager) markPaused(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.Status == StatusCancelled {
		return
	}
	job.Status = StatusPaused
	job.cancel = nil
	job.UpdatedAt = time.Now()
}

func publicJob(job *Job) Job {
	return Job{
		ID:        job.ID,
		Name:      job.Name,
		TargetDir: job.TargetDir,
		Status:    job.Status,
		Progress:  job.Progress,
		Error:     job.Error,
		CreatedAt: job.CreatedAt,
		UpdatedAt: job.UpdatedAt,
	}
}

func terminal(status Status) bool {
	return status == StatusComplete || status == StatusCancelled
}

func cleanup(workDir string, torrentFile string) {
	if workDir != "" {
		_ = os.RemoveAll(workDir)
	}
	if torrentFile != "" {
		_ = os.Remove(torrentFile)
	}
}
