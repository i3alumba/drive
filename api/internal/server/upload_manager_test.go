//go:build linux

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testUploadManager(t *testing.T, max int64) *uploadManager {
	t.Helper()
	m, err := newUploadManager(UploadOptions{Root: filepath.Join(t.TempDir(), "sessions"), MaxFileBytes: max, BudgetBytes: 2 * max})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.close)
	return m
}

func createTestUpload(t *testing.T, m *uploadManager, owner string, size int64) UploadStatus {
	t.Helper()
	status, err := m.create(context.Background(), owner, "users/"+owner+"/file", createUploadRequest{Name: "file", Space: "personal", Size: &size, ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func testUploadDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func uploadErrorCode(t *testing.T, err error, expected int) {
	t.Helper()
	var problem *uploadError
	if !errors.As(err, &problem) || problem.code != expected {
		t.Fatalf("expected HTTP %d, got %v", expected, err)
	}
}

func waitUploadState(t *testing.T, m *uploadManager, id, state string) UploadStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err := m.status("42", id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == state {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("upload did not reach %s", state)
	return UploadStatus{}
}

func TestUploadManagerSmallEmptyAndLargeStreaming(t *testing.T) {
	sizes := []int64{0, 17, 101<<20 + 3}
	if os.Getenv("DRIVE_UPLOAD_LARGE_INTEGRATION") == "1" {
		sizes = append(sizes, 2<<30+3)
	}
	for _, size := range sizes {
		t.Run(stringSize(size), func(t *testing.T) {
			maximum := int64(128 << 20)
			if size > maximum {
				maximum = size
			}
			m := testUploadManager(t, maximum)
			status := createTestUpload(t, m, "42", size)
			whole := sha256.New()
			for index := 0; int64(index) < uploadPartCount(size); index++ {
				part := bytes.Repeat([]byte{byte(index + 1)}, int(uploadExpectedPart(size, index)))
				whole.Write(part)
				var err error
				status, err = m.acceptPart(context.Background(), "42", status.ID, index, testUploadDigest(part), bytes.NewReader(part))
				if err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			var actual string
			_, err := m.complete("42", status.ID, func(ctx context.Context, record uploadRecord, reader io.Reader) error {
				calls.Add(1)
				hash := sha256.New()
				n, err := io.Copy(hash, reader)
				if err != nil {
					return err
				}
				if n != size {
					return errors.New("wrong completed size")
				}
				actual = hex.EncodeToString(hash.Sum(nil))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			completed := waitUploadState(t, m, status.ID, "complete")
			if completed.SHA256 != hex.EncodeToString(whole.Sum(nil)) || actual != completed.SHA256 || calls.Load() != 1 {
				t.Fatal("completed content/hash mismatch")
			}
			if _, err := m.complete("42", status.ID, func(context.Context, uploadRecord, io.Reader) error {
				t.Error("completed session published twice")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.disk.part(status.ID, 0); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed staging not reclaimed: %v", err)
			}
		})
	}
}

func stringSize(size int64) string {
	if size == 0 {
		return "empty"
	}
	if size < uploadChunkSize {
		return "small"
	}
	if size > 1<<30 {
		return "multiGiB_streaming"
	}
	return "over_cloudflare_100MiB"
}

func TestUploadManagerPartValidationAndIdempotentRetry(t *testing.T) {
	m := testUploadManager(t, 1024)
	status := createTestUpload(t, m, "42", 3)
	for _, input := range []struct {
		index int
		body  string
		hash  string
	}{
		{-1, "abc", testUploadDigest([]byte("abc"))},
		{1, "abc", testUploadDigest([]byte("abc"))},
		{0, "ab", testUploadDigest([]byte("ab"))},
		{0, "abcd", testUploadDigest([]byte("abcd"))},
		{0, "abc", testUploadDigest([]byte("bad"))},
		{0, "abc", "invalid"},
	} {
		_, err := m.acceptPart(context.Background(), "42", status.ID, input.index, input.hash, bytes.NewBufferString(input.body))
		uploadErrorCode(t, err, 400)
	}
	body := []byte("abc")
	for i := 0; i < 2; i++ {
		accepted, err := m.acceptPart(context.Background(), "42", status.ID, 0, testUploadDigest(body), bytes.NewReader(body))
		if err != nil || len(accepted.Parts) != 1 {
			t.Fatalf("identical retry failed: %v", err)
		}
	}
	_, err := m.acceptPart(context.Background(), "42", status.ID, 0, testUploadDigest([]byte("xyz")), bytes.NewBufferString("xyz"))
	uploadErrorCode(t, err, 409)
}

func TestUploadManagerIsolationQuotaCancellationAndExpiry(t *testing.T) {
	m := testUploadManager(t, 1024)
	first := createTestUpload(t, m, "42", 1024)
	if _, err := m.status("43", first.ID); err == nil {
		t.Fatal("another owner read a session")
	}
	if _, err := m.cancelSession("43", first.ID); err == nil {
		t.Fatal("another owner cancelled a session")
	}
	if len(m.list("43")) != 0 {
		t.Fatal("another owner listed a session")
	}
	second := createTestUpload(t, m, "42", 1024)
	zero := int64(0)
	_, err := m.create(context.Background(), "42", "users/42/other", createUploadRequest{Name: "other", Size: &zero})
	uploadErrorCode(t, err, 429)
	one := int64(1)
	_, err = m.create(context.Background(), "43", "users/43/other", createUploadRequest{Name: "other", Size: &one})
	uploadErrorCode(t, err, 507)
	if _, err := m.cancelSession("42", first.ID); err != nil {
		t.Fatal(err)
	}
	createTestUpload(t, m, "43", 1024)
	m.mu.Lock()
	m.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	m.sweepLocked()
	m.mu.Unlock()
	if _, err := m.status("42", second.ID); err == nil {
		t.Fatal("expired session not reclaimed")
	}
}

func TestUploadManagerRecoveryAndUncertainPublication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	options := UploadOptions{Root: root, MaxFileBytes: 1024, BudgetBytes: 2048}
	m, err := newUploadManager(options)
	if err != nil {
		t.Fatal(err)
	}
	status := createTestUpload(t, m, "42", 3)
	if _, err := m.acceptPart(context.Background(), "42", status.ID, 0, testUploadDigest([]byte("abc")), bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	m.close()
	m, err = newUploadManager(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := m.status("42", status.ID)
	if err != nil || len(loaded.Parts) != 1 {
		t.Fatal("durable part receipt not recovered")
	}
	m.mu.Lock()
	record := m.sessions[status.ID].record
	record.Status.State = "committing" // simulate a crash after durable intent
	if err := m.disk.save(record); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	m.close()
	m, err = newUploadManager(options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	loaded, err = m.status("42", status.ID)
	if err != nil || loaded.State != "uncertain" {
		t.Fatal("interrupted publication was not fenced")
	}
	_, err = m.complete("42", status.ID, func(context.Context, uploadRecord, io.Reader) error {
		t.Error("uncertain publication retried blindly")
		return nil
	})
	uploadErrorCode(t, err, 409)
}

func TestUploadManagerCorruptStageNeverPublishes(t *testing.T) {
	m := testUploadManager(t, 1024)
	status := createTestUpload(t, m, "42", 3)
	if _, err := m.acceptPart(context.Background(), "42", status.ID, 0, testUploadDigest([]byte("abc")), bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.options.Root, status.ID, uploadPartName(0)), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	var published atomic.Bool
	_, err := m.complete("42", status.ID, func(context.Context, uploadRecord, io.Reader) error {
		published.Store(true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUploadState(t, m, status.ID, "uncertain")
	if published.Load() {
		t.Fatal("corrupt stage reached object storage")
	}
}

func TestUploadManagerCreationRetryAndCancelledCreation(t *testing.T) {
	m := testUploadManager(t, 1024)
	size := int64(3)
	input := createUploadRequest{ClientID: uuid.NewString(), Name: "file", Space: "personal", Size: &size, ContentType: "text/plain"}
	for i := 0; i < 4; i++ {
		status, err := m.create(context.Background(), "42", "users/42/file", input)
		if err != nil || status.ID != input.ClientID {
			t.Fatalf("creation retry was not idempotent: %v", err)
		}
	}
	if len(m.list("42")) != 1 {
		t.Fatal("creation retry reserved duplicate sessions")
	}
	_, err := m.create(context.Background(), "43", "users/43/file", input)
	uploadErrorCode(t, err, 409)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input.ClientID = uuid.NewString()
	_, err = m.create(ctx, "42", "users/42/file", input)
	uploadErrorCode(t, err, 408)
	if len(m.list("42")) != 1 {
		t.Fatal("cancelled creation left an orphan session")
	}
}

type gatedUploadReader struct {
	reader  io.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedUploadReader) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.reader.Read(data)
}

func TestUploadManagerConcurrentDuplicateAndCancellation(t *testing.T) {
	m := testUploadManager(t, 1024)
	status := createTestUpload(t, m, "42", 3)
	reader := &gatedUploadReader{reader: bytes.NewBufferString("abc"), entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(reader.release) })
	done := make(chan error, 1)
	go func() {
		_, err := m.acceptPart(context.Background(), "42", status.ID, 0, testUploadDigest([]byte("abc")), reader)
		done <- err
	}()
	<-reader.entered
	_, err := m.acceptPart(context.Background(), "42", status.ID, 0, testUploadDigest([]byte("abc")), bytes.NewBufferString("abc"))
	uploadErrorCode(t, err, 429)
	if _, err := m.cancelSession("42", status.ID); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(reader.release) })
	uploadErrorCode(t, <-done, 409)
	m.mu.Lock()
	released := m.sessions[status.ID].record.Released
	m.mu.Unlock()
	if !released {
		t.Fatal("cancelled in-flight part did not release its reservation after cleanup")
	}
}

func TestUploadManagerRecoversInterruptedSessionCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	options := UploadOptions{Root: root, MaxFileBytes: 1024, BudgetBytes: 2048}
	m, err := newUploadManager(options)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if err := uploadMkdir(m.disk.root, id); err != nil {
		t.Fatal(err)
	}
	m.close()
	m, err = newUploadManager(options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	if _, err := os.Stat(filepath.Join(root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty interrupted session was not reclaimed")
	}
}
