package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func awaitImport(t *testing.T, s *Server, body string) ImportJob {
	t.Helper()
	w := httptest.NewRecorder()
	s.startImport(w, ownerRequest("POST", "/api/imports", body, "42"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status %d", w.Code)
	}
	var started ImportJob
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		s.imports.mu.Lock()
		job := *s.imports.jobs[started.ID]
		s.imports.mu.Unlock()
		if job.Status == "failed" || job.Status == "complete" || job.Status == "cancelled" {
			s.imports.workers.Wait()
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("URL import deadline exceeded")
	return ImportJob{}
}
func configuredImporter(t *testing.T, store Store, max int64) *Server {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("secure URL work staging requires Linux")
	}
	s := New(store, nil, nil)
	options := ImportOptions{Root: filepath.Join(t.TempDir(), "imports"), MaxFileBytes: max, BudgetBytes: max * 2, ReserveBytes: 0, Timeout: 2 * time.Hour}
	if err := s.EnableImports(options); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.CloseImports(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func TestURLImportIntegrityBoundsAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name           string
		length         int64
		data, checksum string
		want           string
	}{
		{"known", 3, "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "complete"},
		{"unknown", -1, "abc", "", "complete"},
		{"bad_checksum", 3, "abc", strings.Repeat("0", 64), "failed"},
		{"declared_oversize", 33, "abc", "", "failed"},
		{"unknown_oversize", -1, strings.Repeat("x", 33), "", "failed"},
		{"truncated", 8, "abc", "", "failed"},
		{"declared_too_short", 2, "abc", "", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testTransferStore()
			s := configuredImporter(t, store, 32)
			s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, ContentLength: tc.length, Body: io.NopCloser(strings.NewReader(tc.data)), Header: make(http.Header), Request: r}, nil
			})
			body, _ := json.Marshal(map[string]string{"url": "https://example.com/file?secret=not-returned", "name": "import.bin", "sha256": tc.checksum})
			job := awaitImport(t, s, string(body))
			if job.Status != tc.want {
				t.Fatalf("status %s", job.Status)
			}
			store.mu.Lock()
			_, exists := store.data["users/42/import.bin"]
			store.mu.Unlock()
			if exists != (tc.want == "complete") {
				t.Fatal("publication mismatch")
			}
			if job.Status == "complete" && len(job.SHA256) != 64 {
				t.Fatal("missing digest")
			}
			s.imports.mu.Lock()
			reserved := s.imports.reserved
			s.imports.mu.Unlock()
			if reserved != 0 {
				t.Fatal("staging reservation leaked")
			}
			entries, err := os.ReadDir(s.imports.options.Root)
			if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
				t.Fatal("temporary staging leaked")
			}
		})
	}
}
func TestURLImportOptionsStagingLockAndReservation(t *testing.T) {
	s := configuredImporter(t, testTransferStore(), 32)
	if s.imports.client.Timeout != 2*time.Hour {
		t.Fatal("HTTP client retained old ten-minute deadline")
	}
	if !s.imports.reserveStaging(32) || !s.imports.reserveStaging(32) || s.imports.reserveStaging(1) {
		t.Fatal("staging budget not enforced")
	}
	s.imports.releaseStaging(64)
	f, cleanup, err := s.imports.temporary()
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("temporary file not private")
	}
	cleanup()
	another := New(testTransferStore(), nil, nil)
	if err := another.EnableImports(s.imports.options); err == nil {
		another.CloseImports()
		t.Fatal("concurrent staging writer accepted")
	}
	for _, change := range []func(*ImportOptions){func(o *ImportOptions) { o.MaxFileBytes = 0 }, func(o *ImportOptions) { o.MaxFileBytes = 1 << 61 }, func(o *ImportOptions) { o.BudgetBytes = 1 }, func(o *ImportOptions) { o.ReserveBytes = -1 }, func(o *ImportOptions) { o.Timeout = 0 }, func(o *ImportOptions) { o.Timeout = 25 * time.Hour }} {
		options := defaultImportOptions()
		change(&options)
		if options.validate() == nil {
			t.Fatal("invalid limits accepted")
		}
	}
}
func TestURLImportRejectsSymlinkStagingRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only secure staging")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "imports")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	s := New(testTransferStore(), nil, nil)
	options := defaultImportOptions()
	options.Root = root
	if s.EnableImports(options) == nil {
		t.Fatal("symlink staging root accepted")
	}
}
func TestURLImportRejectsInvalidChecksumBeforeNetwork(t *testing.T) {
	s := New(testTransferStore(), nil, nil)
	for _, checksum := range []string{"invalid", strings.Repeat("A", 64), strings.Repeat("0", 62)} {
		body, _ := json.Marshal(map[string]string{"url": "https://example.com/file", "sha256": checksum})
		w := httptest.NewRecorder()
		s.startImport(w, ownerRequest("POST", "/api/imports", string(body), "42"))
		if w.Code != 400 {
			t.Fatal("invalid checksum accepted")
		}
	}
}

type importZeros struct{}

func (importZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type importCountingStore struct {
	Store
	mu     sync.Mutex
	bytes  int64
	digest string
}

func (s *importCountingStore) PutObject(ctx context.Context, key string, r io.Reader, size int64, kind string) error {
	hash := sha256.New()
	n, err := io.Copy(hash, r)
	s.mu.Lock()
	s.bytes = n
	s.digest = hex.EncodeToString(hash.Sum(nil))
	s.mu.Unlock()
	return err
}
func TestURLImportRealFileAboveOneGiB(t *testing.T) {
	const size int64 = (1 << 30) + 17
	store := &importCountingStore{Store: testTransferStore()}
	s := configuredImporter(t, store, size)
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(importZeros{}, size)); err != nil {
		t.Fatal(err)
	}
	expected := hex.EncodeToString(hash.Sum(nil))
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: size, Body: io.NopCloser(io.LimitReader(importZeros{}, size)), Header: make(http.Header), Request: r}, nil
	})
	body, _ := json.Marshal(map[string]string{"url": "https://example.com/large", "name": "large.bin", "sha256": expected})
	job := awaitImport(t, s, string(body))
	if job.Status != "complete" || job.SHA256 != expected {
		t.Fatalf("large import %s", job.Status)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.bytes != size || store.digest != expected {
		t.Fatal("large payload differs")
	}
}
