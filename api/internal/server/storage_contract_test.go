package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"remote-drive/api/internal/storage"
)

type memoryShares struct {
	body   []byte
	err    error
	writes int
}

func (m *memoryShares) GetObject(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	if key != sharesObjectKey {
		return nil, storage.ObjectInfo{}, errors.New("unexpected key")
	}
	if m.err != nil {
		return nil, storage.ObjectInfo{}, m.err
	}
	if m.body == nil {
		return nil, storage.ObjectInfo{}, fmt.Errorf("metadata: %w", storage.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(m.body)), storage.ObjectInfo{Size: int64(len(m.body)), ContentType: "application/json"}, nil
}

func (m *memoryShares) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	if key != sharesObjectKey || contentType != "application/json" {
		return errors.New("unexpected metadata write")
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.body = data
	m.writes++
	return nil
}

func TestShareMetadataWorksWithoutS3Types(t *testing.T) {
	backend := &memoryShares{}
	shares := NewShareStore(backend)
	owner := User{ID: "42", Username: "alice"}
	created, err := shares.Create(context.Background(), owner, "bob", "docs", true, permissionRead)
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := shares.Incoming(context.Background(), User{ID: "43", Username: "bob"})
	if err != nil || len(incoming) != 1 || incoming[0].ID != created.ID || incoming[0].OwnerID != "42" {
		t.Fatalf("incoming metadata was not preserved: %v", err)
	}
	if err := shares.Delete(context.Background(), owner, created.ID); err != nil {
		t.Fatal(err)
	}
	if string(backend.body) != "[]" {
		t.Fatalf("unexpected metadata after delete: %s", backend.body)
	}
}

func TestShareMetadataFailureNeverBecomesEmptyStorage(t *testing.T) {
	for _, err := range []error{errors.New("NAS unavailable"), errors.New("upstream failure mentions NoSuchKey"), context.Canceled} {
		t.Run(err.Error(), func(t *testing.T) {
			backend := &memoryShares{body: []byte("preserved metadata"), err: err}
			_, got := NewShareStore(backend).Create(context.Background(), User{ID: "42", Username: "alice"}, "bob", "docs", true, permissionRead)
			if !errors.Is(got, err) || backend.writes != 0 || string(backend.body) != "preserved metadata" {
				t.Fatal("storage outage caused a metadata overwrite")
			}
		})
	}
}

func TestCorruptShareMetadataIsNotReset(t *testing.T) {
	backend := &memoryShares{body: []byte("{incomplete")}
	_, err := NewShareStore(backend).Create(context.Background(), User{ID: "42", Username: "alice"}, "bob", "docs", true, permissionRead)
	if err == nil || backend.writes != 0 {
		t.Fatal("corrupt metadata was accepted or overwritten")
	}
}

type rangeBackend struct {
	Store
	closed     bool
	key        string
	rangeValue *storage.ByteRange
}

type trackedReader struct {
	io.Reader
	closed *bool
}

func (r *trackedReader) Close() error {
	*r.closed = true
	return nil
}

func (b *rangeBackend) GetObjectRange(_ context.Context, key string, value *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	b.key, b.rangeValue = key, value
	return &trackedReader{Reader: strings.NewReader("bcd"), closed: &b.closed}, storage.ObjectInfo{Size: 6, ContentType: "text/plain"}, nil
}

func TestRangeStreamingUsesBackendNeutralReaderAndPreservesUserPrefix(t *testing.T) {
	backend := &rangeBackend{}
	s := New(backend, nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/view?path=notes.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	r = r.WithContext(context.WithValue(r.Context(), userContextKey, User{ID: "42", Username: "alice"}))
	w := httptest.NewRecorder()
	s.viewFile(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "bcd" || w.Header().Get("Content-Range") != "bytes 1-3/6" || w.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("unexpected streamed response: %d %v", w.Code, w.Header())
	}
	if backend.key != "users/42/notes.txt" || backend.rangeValue == nil || backend.rangeValue.Start != 1 || backend.rangeValue.End != 3 || !backend.closed {
		t.Fatal("backend range, isolation prefix, or reader close contract was lost")
	}
}
