//go:build linux

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
)

func TestImportQueueCannotEnableWithoutImportFence(t *testing.T) {
	s := New(newLocalQueueStore(t), nil, nil)
	if err := s.EnableImportQueues(); err == nil || s.queues != nil {
		t.Fatal("unfenced durable queue enabled")
	}
	w := httptest.NewRecorder()
	s.createImportQueue(w, ownerRequest("POST", "/api/import-queues", `{}`, "42"))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestImportQueueCancelDownloadingStopsPublicationAndReleasesBudget(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	entered := make(chan struct{}, 1)
	released := make(chan struct{})
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 100, Body: &queueBlockingBody{ctx: r.Context(), entered: entered, released: released}, Header: make(http.Header), Request: r}, nil
	})
	q, _ := queueSubmit(t, s, strings.Repeat("f", 32), "42", queueDirect("cancelled"))
	s.dispatchQueueWork()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("download did not begin")
	}
	w := httptest.NewRecorder()
	r := ownerRequest("POST", "/cancel", "", "42")
	r.SetPathValue("id", q.manifest.ID)
	r.SetPathValue("action", "cancel")
	s.controlImportQueue(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	s.imports.workers.Wait()
	queueWait(t, q, "cancelled")
	if _, err := disk.StatObject(context.Background(), "users/42/target/cancelled"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("cancel published destination", err)
	}
	s.imports.mu.Lock()
	defer s.imports.mu.Unlock()
	if len(s.imports.slots) != 0 || s.imports.reserved != 0 || len(s.imports.destinations) != 0 {
		t.Fatal("cancel leaked admission/staging")
	}
}

func TestImportQueueRejectsInvalidJSONUnicodeBeforeDecode(t *testing.T) {
	for _, body := range [][]byte{[]byte("{\"path\":\"" + string([]byte{0xff}) + "\"}"), []byte(`{"path":"\ud800"}`), []byte(`{"path":"\udc00"}`), []byte(`{"path":"\ud800x"}`)} {
		if validQueueJSON(body) {
			t.Fatalf("invalid Unicode accepted %q", body)
		}
	}
	for _, body := range []string{`{"path":"\ud83d\ude00"}`, `{"path":"\\ud800"}`, `{"path":"valid"}`} {
		if !validQueueJSON([]byte(body)) {
			t.Fatalf("valid Unicode rejected %q", body)
		}
	}
}
