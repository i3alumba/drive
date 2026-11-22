//go:build linux

package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"remote-drive/api/internal/storage"
)

// ConfigureConverter is startup-only. A nil converter leaves opt-in unavailable.
func (m *Manager) ConfigureConverter(c Converter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.converter = c
	m.conversionSlots = make(chan struct{}, 1)
}
func (m *Manager) ConversionAvailable() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store.(interface {
		PutObjectIfAbsent(context.Context, string, io.Reader, int64, string) error
	})
	return m.converter != nil && ok
}

type completedInput struct {
	path, relative string
}

func completedInputs(root string) ([]completedInput, int64, error) {
	var files []completedInput
	var total int64
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return errors.New("invalid_input")
		}
		if e.IsDir() || strings.HasSuffix(e.Name(), ".aria2") {
			return nil
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("invalid_input")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return errors.New("invalid_input")
		}
		if len(files) >= 4096 || info.Size() > maxConversionInput || total > maxConversionInput-info.Size() {
			return errors.New("conversion_input_too_large")
		}
		total += info.Size()
		files = append(files, completedInput{path, rel})
		return nil
	})
	return files, total, err
}
func (m *Manager) conversionFree(root string) bool {
	if m.conversionFreeSpace != nil {
		return m.conversionFreeSpace(root)
	}
	var stat unix.Statfs_t
	return unix.Statfs(root, &stat) == nil && stat.Bavail*uint64(stat.Bsize) >= uint64(maxConversionOutput+(8<<30))
}
func (m *Manager) conversionState(id, state string, result *ConversionResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		j.ConversionStatus = state
		j.UpdatedAt = time.Now()
		if result != nil {
			j.ConversionResults = append(j.ConversionResults, *result)
		}
	}
}
func (m *Manager) convertCompleted(id, root, target string) {
	files, total, err := completedInputs(root)
	reject := func(code string) { m.conversionState(id, "failed", &ConversionResult{Status: "failed", Error: code}) }
	if err != nil {
		if err.Error() == "conversion_input_too_large" {
			reject("conversion_input_too_large")
		} else {
			reject("conversion_invalid_input")
		}
		return
	}
	m.mu.Lock()
	if m.converter == nil {
		m.mu.Unlock()
		reject("conversion_unavailable")
		return
	}
	if m.retainedJobs >= 4 {
		m.mu.Unlock()
		reject("conversion_queue_full")
		return
	}
	// Reserve the entire possible output alongside all retained inputs within 64Gi.
	if total > (64<<30)-maxConversionOutput-m.retainedBytes {
		m.mu.Unlock()
		reject("conversion_budget_exceeded")
		return
	}
	if !m.conversionFree(root) {
		m.mu.Unlock()
		reject("conversion_no_space")
		return
	}
	j := m.jobs[id]
	if j.cancelRequested {
		m.mu.Unlock()
		reject("conversion_cancelled")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	m.retainedJobs++
	m.retainedBytes += total
	converter, slots := m.converter, m.conversionSlots
	m.mu.Unlock()
	defer func() { cancel(); m.mu.Lock(); m.retainedJobs--; m.retainedBytes -= total; m.mu.Unlock() }()
	m.conversionState(id, "queued", nil)
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		m.conversionState(id, "cancelled", nil)
		return
	}
	m.conversionState(id, "converting", nil)
	failed, converted := false, false
	for i, file := range files {
		if ctx.Err() != nil {
			m.conversionState(id, "cancelled", nil)
			return
		}
		ext := strings.ToLower(filepath.Ext(file.path))
		switch ext {
		case ".mkv", ".avi", ".mp4", ".m4v", ".mov":
		default:
			m.conversionState(id, "converting", &ConversionResult{Name: file.relative, Status: "skipped"})
			continue
		}
		result := ConversionResult{Name: file.relative, Status: "failed"}
		if !m.conversionFree(root) {
			result.Error = "conversion_no_space"
		} else {
			ownedDir, mkdirErr := os.MkdirTemp(root, ".browser-conversion-")
			if mkdirErr != nil {
				reject("conversion_no_space")
				return
			}
			output := filepath.Join(ownedDir, "output.mp4")
			fileTimeout := m.conversionFileTimeout
			if fileTimeout == 0 {
				fileTimeout = 24 * time.Hour
			}
			fileCtx, stopFile := context.WithTimeout(ctx, fileTimeout)
			publish := func() error {
				var err error
				m.conversionState(id, "saving", nil)
				stem := strings.TrimSuffix(filepath.Base(file.relative), filepath.Ext(file.relative))
				if len(stem) > 80 {
					stem = stem[:80]
					for !utf8.ValidString(stem) {
						stem = stem[:len(stem)-1]
					}
				}
				relative := filepath.Join(filepath.Dir(file.relative), fmt.Sprintf("%s.browser-%s-%d.mp4", stem, id, i))
				f, openErr := os.Open(output)
				if openErr != nil {
					err = errors.New("conversion_output_unavailable")
				} else {
					info, statErr := f.Stat()
					if statErr != nil {
						err = statErr
					} else if publisher, ok := m.store.(interface {
						PutObjectIfAbsent(context.Context, string, io.Reader, int64, string) error
					}); !ok {
						err = errors.New("conditional_publication_unavailable")
					} else {
						err = publisher.PutObjectIfAbsent(fileCtx, storage.CleanObjectPath(target+filepath.ToSlash(relative)), f, info.Size(), "video/mp4")
					}
					f.Close()
				}
				if err == nil {
					result.Name = filepath.ToSlash(relative)
					result.Status = "complete"
					converted = true
				}
				return err
			}
			watchDone, stopWatch := make(chan struct{}), make(chan struct{})
			noSpace := false
			go func() {
				defer close(watchDone)
				ticker := time.NewTicker(500 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopWatch:
						return
					case <-fileCtx.Done():
						return
					case <-ticker.C:
						var stat unix.Statfs_t
						available := unix.Statfs(root, &stat) == nil && stat.Bavail*uint64(stat.Bsize) >= 8<<30
						if m.conversionFreeSpace != nil {
							available = m.conversionFreeSpace(root)
						}
						if !available {
							noSpace = true
							stopFile()
							return
						}
					}
				}
			}()

			if publishing, ok := converter.(interface {
				ConvertAndPublish(context.Context, string, string, func() error) error
			}); ok {
				err = publishing.ConvertAndPublish(fileCtx, file.path, output, publish)
			} else {
				err = converter.Convert(fileCtx, file.path, output)
				if err == nil {
					err = publish()
				}
			}
			cleanupFailed := errors.Is(err, storage.ErrMultipartCleanup)
			close(stopWatch)
			<-watchDone
			if err != nil && errors.Is(fileCtx.Err(), context.DeadlineExceeded) {
				err = errors.New("conversion_timeout")
			}
			stopFile()
			if noSpace && result.Status != "complete" {
				err = errors.New("conversion_no_space")
			}
			_ = os.RemoveAll(ownedDir)
			if err != nil {
				result.Error = "conversion_failed"
				switch err.Error() {
				case "conversion_no_space", "unsupported_hdr", "unsupported_video", "unsupported_duration", "conversion_input_too_large", "conversion_output_limit", "conversion_timeout", "conversion_cancelled":
					result.Error = err.Error()
					if err.Error() == "unsupported_video" {
						result.Status = "skipped"
					}
				}
				if cleanupFailed {
					result.Error = "publication_cleanup_failed"
				} else if errors.Is(err, storage.ErrAlreadyExists) {
					result.Error = "derivative_exists"
				}
			}
		}
		if result.Status == "failed" {
			failed = true
		}
		m.conversionState(id, "converting", &result)
	}
	state := "complete"
	if failed {
		state = "failed"
		if converted {
			state = "partial"
		}
	}
	if ctx.Err() != nil {
		state = "cancelled"
	}
	m.conversionState(id, state, nil)
}
