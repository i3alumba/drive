package server

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ImportOptions bounds server-side downloads independently of browser uploads.
// Root is a private child of the already mounted torrent work directory.
type ImportOptions struct {
	Root         string
	MaxFileBytes int64
	BudgetBytes  int64
	ReserveBytes int64
	Timeout      time.Duration
}

func defaultImportOptions() ImportOptions {
	return ImportOptions{MaxFileBytes: 10 << 30, BudgetBytes: 20 << 30, ReserveBytes: 2 << 30, Timeout: 2 * time.Hour}
}
func (o ImportOptions) validate() error {
	if o.MaxFileBytes <= 0 || o.MaxFileBytes > 1<<60 || o.BudgetBytes < o.MaxFileBytes || o.BudgetBytes > 1<<60 || o.ReserveBytes < 0 || o.ReserveBytes > 1<<60 || o.Timeout <= 0 || o.Timeout > 24*time.Hour {
		return errors.New("invalid URL import limits")
	}
	return nil
}

// EnableImports is startup-only. No migration or cleanup of existing files occurs.
func (s *Server) EnableImports(options ImportOptions) error {
	if err := options.validate(); err != nil {
		return err
	}
	if options.Root == "" {
		return errors.New("URL import staging root is required")
	}
	parent, err := uploadOpenDir(nil, filepath.Dir(options.Root))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(options.Root)
	if err := uploadMkdir(parent, name); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	root, err := uploadOpenDir(parent, name)
	if err != nil {
		return err
	}
	closeRoot := true
	defer func() {
		if closeRoot {
			root.Close()
		}
	}()
	if err := uploadPrivateDir(root); err != nil {
		return err
	}
	lock, err := uploadOpenFile(root, ".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	if info, err := lock.Stat(); err != nil || info.Mode().Perm()&0o077 != 0 {
		lock.Close()
		return os.ErrPermission
	}
	if err := uploadTryLock(lock); err != nil {
		lock.Close()
		return err
	}
	s.imports.mu.Lock()
	defer s.imports.mu.Unlock()
	if s.imports.root != nil || s.imports.closed || len(s.imports.slots) > 0 {
		lock.Close()
		return errors.New("URL importer is already configured or active")
	}
	s.imports.options = options
	s.imports.client.Timeout = options.Timeout
	s.imports.root = root
	s.imports.lock = lock
	closeRoot = false
	return nil
}
func (s *Server) CloseImports() error {
	s.closeImportQueues()
	m := s.imports
	m.mu.Lock()
	m.closed = true
	for _, j := range m.jobs {
		if j.cancel != nil && (j.Status == "downloading" || j.Status == "saving") {
			j.cancel()
		}
	}
	m.mu.Unlock()
	m.workers.Wait()
	if m.lock != nil {
		m.lock.Close()
	}
	if m.root != nil {
		return m.root.Close()
	}
	return nil
}
func (m *importManager) freeBytes() (int64, error) {
	if runtime.GOOS != "linux" {
		return math.MaxInt64, nil
	} // Preserve legacy non-Linux API support.
	if m.root != nil {
		return uploadFreeBytes(m.root)
	}
	dir, err := os.Open(os.TempDir())
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	return uploadFreeBytes(dir)
}
func (m *importManager) reserveStaging(bytes int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	free, err := m.freeBytes()
	if err != nil || bytes < 0 || bytes > m.options.BudgetBytes-m.reserved || free < m.options.ReserveBytes || m.reserved > free-m.options.ReserveBytes || bytes > free-m.options.ReserveBytes-m.reserved {
		return false
	}
	m.reserved += bytes
	return true
}
func (m *importManager) releaseStaging(bytes int64) { m.mu.Lock(); m.reserved -= bytes; m.mu.Unlock() }
func (m *importManager) temporary() (*os.File, func(), error) {
	if m.root == nil {
		f, err := os.CreateTemp("", "drive-url-*")
		if err != nil {
			return nil, nil, err
		}
		return f, func() { f.Close(); os.Remove(f.Name()) }, nil
	}
	name := ".url-" + randomID()
	f, err := uploadOpenFile(m.root, name, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close(); uploadUnlink(m.root, name, false) }, nil
}
