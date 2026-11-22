package videoreader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
)

type memoryStore struct {
	mu    sync.Mutex
	calls []storage.ByteRange
	keys  []string
}

func (s *memoryStore) GetObjectRange(_ context.Context, key string, r *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, *r)
	s.keys = append(s.keys, key)
	return io.NopCloser(strings.NewReader("0123456789"[r.Start : r.End+1])), storage.ObjectInfo{Size: 10}, nil
}
func newReader(t *testing.T, s Store, limit int64) *Reader {
	t.Helper()
	r, err := New(context.Background(), s, "selected", 10, Limits{Deadline: time.Now().Add(time.Minute), MaxRequestBytes: 10, MaxReadBytes: limit, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Revoke)
	return r
}
func request(r *Reader, method, path, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if value != "" {
		req.Header.Set("Range", value)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(deadlineRecorder{w}, req)
	return w
}
func TestReaderRandomAccess(t *testing.T) {
	s := &memoryStore{}
	r := newReader(t, s, 20)
	for _, tc := range []struct{ rangeValue, body, contentRange string }{{"bytes=8-9", "89", "bytes 8-9/10"}, {"bytes=1-3", "123", "bytes 1-3/10"}, {"bytes=-2", "89", "bytes 8-9/10"}, {"bytes=9-", "9", "bytes 9-9/10"}} {
		w := request(r, "GET", r.Path(), tc.rangeValue)
		if w.Code != 206 || w.Body.String() != tc.body || w.Header().Get("Content-Range") != tc.contentRange {
			t.Fatalf("%s: %d %q %v", tc.rangeValue, w.Code, w.Body.String(), w.Header())
		}
	}
	if len(s.calls) != 4 || s.calls[0].Start != 8 {
		t.Fatalf("unexpected reads: %v", s.calls)
	}
	for _, key := range s.keys {
		if key != "selected" {
			t.Fatal(key)
		}
	}
}
func TestReaderAuthorizationAndRevocation(t *testing.T) {
	s := &memoryStore{}
	r := newReader(t, s, 20)
	other := newReader(t, s, 20)
	for _, path := range []string{"/input", other.Path(), r.Path() + "?key=other"} {
		if w := request(r, "GET", path, "bytes=0-1"); w.Code != 404 {
			t.Fatal(w.Code)
		}
	}
	if w := request(r, "POST", r.Path(), ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
	r.Revoke()
	if w := request(r, "GET", r.Path(), "bytes=0-1"); w.Code != 410 {
		t.Fatal(w.Code)
	}
	if len(s.calls) != 0 {
		t.Fatal("unauthorized source read")
	}
}
func TestReaderHEADRangesAndBudget(t *testing.T) {
	s := &memoryStore{}
	r := newReader(t, s, 3)
	w := request(r, "HEAD", r.Path(), "")
	if w.Code != 200 || w.Header().Get("Content-Length") != "10" || w.Body.Len() != 0 || len(s.calls) != 0 {
		t.Fatal(w)
	}
	for _, value := range []string{"bytes=10-", "bytes=2-1", "bytes=0-1,3-4", "bytes=-0", "bytes=+1-2", "bytes=999999999999999999999-"} {
		if w := request(r, "GET", r.Path(), value); w.Code != 416 || w.Header().Get("Content-Range") != "bytes */10" {
			t.Fatalf("%s: %d", value, w.Code)
		}
	}
	if w := request(r, "GET", r.Path(), "bytes=0-2"); w.Code != 206 {
		t.Fatal(w.Code)
	}
	if w := request(r, "GET", r.Path(), "bytes=0-0"); w.Code != 429 {
		t.Fatal(w.Code)
	}
	if len(s.calls) != 1 {
		t.Fatal(s.calls)
	}
}

type blockingStore struct{ entered chan struct{} }

func (s *blockingStore) GetObjectRange(ctx context.Context, _ string, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, storage.ObjectInfo{}, ctx.Err()
}
func TestReaderConcurrencyAndCancel(t *testing.T) {
	s := &blockingStore{entered: make(chan struct{})}
	r := newReader(t, s, 100)
	done := make(chan struct{})
	go func() { defer close(done); request(r, "GET", r.Path(), "bytes=0-0") }()
	<-s.entered
	if w := request(r, "GET", r.Path(), "bytes=0-0"); w.Code != 429 {
		t.Fatal(w.Code)
	}
	r.Revoke()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read not canceled")
	}
}
func TestReaderFullGETAndRequestLimit(t *testing.T) {
	s := &memoryStore{}
	r := newReader(t, s, 100)
	if w := request(r, "GET", r.Path(), ""); w.Code != http.StatusOK || w.Body.String() != "0123456789" {
		t.Fatal(w)
	}
	r.limits.MaxRequestBytes = 2
	if w := request(r, "GET", r.Path(), "bytes=0-2"); w.Code != 206 {
		t.Fatal(w.Code)
	}
}

func TestReaderDeadline(t *testing.T) {
	s := &memoryStore{}
	r, err := New(context.Background(), s, "selected", 10, Limits{Deadline: time.Now().Add(20 * time.Millisecond), MaxRequestBytes: 10, MaxReadBytes: 10, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Revoke()
	<-r.ctx.Done()
	if w := request(r, "GET", r.Path(), "bytes=0-0"); w.Code != http.StatusGone {
		t.Fatal(w.Code)
	}
	if len(s.calls) != 0 {
		t.Fatal("expired reader accessed source")
	}
}

type deadlineRecorder struct{ *httptest.ResponseRecorder }

func (deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

type endlessStore struct {
	closeCount atomic.Int32
	closed     chan struct{}
	once       sync.Once
}
type endlessBody struct{ s *endlessStore }

func (b endlessBody) Read(p []byte) (int, error) {
	select {
	case <-b.s.closed:
		return 0, io.EOF
	default:
		clear(p)
		return len(p), nil
	}
}
func (b endlessBody) Close() error {
	b.s.closeCount.Add(1)
	b.s.once.Do(func() { close(b.s.closed) })
	return nil
}
func (s *endlessStore) GetObjectRange(context.Context, string, *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return endlessBody{s}, storage.ObjectInfo{Size: 64 << 20}, nil
}
func TestReaderRevocationUnblocksNonreadingClient(t *testing.T) {
	store := &endlessStore{closed: make(chan struct{})}
	r, err := New(context.Background(), store, "selected", 64<<20, Limits{Deadline: time.Now().Add(time.Minute), MaxRequestBytes: 64 << 20, MaxReadBytes: 64 << 20, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Revoke()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { defer close(done); r.ServeHTTP(w, q) }))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: private\r\n\r\n", r.Path())
	// Wait for acquisition and a blocked socket writer without draining payload.
	deadline := time.Now().Add(time.Second)
	for len(r.slots) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	r.Revoke()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked output retained admission")
	}
	select {
	case <-store.closed:
	default:
		t.Fatal("acquired body not closed")
	}
	if store.closeCount.Load() != 1 {
		t.Fatalf("body closed %d times", store.closeCount.Load())
	}
	if len(r.slots) != 0 {
		t.Fatal("slot retained")
	}
}

type progressingStore struct{ data []byte }

func (s progressingStore) GetObjectRange(ctx context.Context, _ string, r *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	select {
	case <-ctx.Done():
		return nil, storage.ObjectInfo{}, ctx.Err()
	case <-time.After(650 * time.Millisecond):
	}
	return io.NopCloser(bytes.NewReader(s.data[r.Start : r.End+1])), storage.ObjectInfo{Size: int64(len(s.data))}, nil
}
func TestReaderHealthyResponseBeyondFiveSeconds(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 10*32768)
	r, e := New(context.Background(), progressingStore{data}, "selected", int64(len(data)), Limits{Deadline: time.Now().Add(15 * time.Second), MaxRequestBytes: 32768, MaxReadBytes: int64(len(data)), Concurrency: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Revoke()
	server := httptest.NewServer(r)
	defer server.Close()
	start := time.Now()
	resp, e := http.Get(server.URL + r.Path())
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	got, e := io.ReadAll(resp.Body)
	if e != nil || !bytes.Equal(got, data) || time.Since(start) < 6*time.Second {
		t.Fatalf("bytes=%d error=%v elapsed=%v", len(got), e, time.Since(start))
	}
}
