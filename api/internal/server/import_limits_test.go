package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestImportCancellationAndCapacity(t *testing.T) {
	s := New(testTransferStore(), nil, nil)
	called := make(chan struct{})
	s.imports.client = &http.Client{Transport: remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		close(called)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	w := httptest.NewRecorder()
	s.startImport(w, ownerRequest("POST", "/api/imports", `{"url":"https://example.com/file"}`, "42"))
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var job ImportJob
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("import did not start")
	}
	w = httptest.NewRecorder()
	s.cancelImport(w, ownerRequest("DELETE", "/api/imports/"+job.ID, "", "42"))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	deadline := time.Now().Add(time.Second)
	for len(s.imports.slots) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.imports.slots) != 0 {
		t.Fatal("cancel leaked worker slot")
	}
	s.imports.mu.Lock()
	status := s.imports.jobs[job.ID].Status
	s.imports.mu.Unlock()
	if status != "cancelled" {
		t.Fatal(status)
	}
	for i := 0; i < cap(s.imports.slots); i++ {
		s.imports.slots <- struct{}{}
	}
	w = httptest.NewRecorder()
	s.startImport(w, ownerRequest("POST", "/api/imports", `{"url":"https://example.com/file"}`, "43"))
	if w.Code != 429 {
		t.Fatal("capacity not bounded")
	}
}
func TestImportOversizedOrTruncatedResponseNeverPublished(t *testing.T) {
	for _, size := range []int64{maxImportBytes + 1, 30} {
		t.Run(strings.Repeat("x", int(size%3)+1), func(t *testing.T) {
			store := testTransferStore()
			s := New(store, nil, nil)
			s.imports.client = &http.Client{Transport: remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, ContentLength: size, Body: io.NopCloser(strings.NewReader("short")), Header: make(http.Header), Request: r}, nil
			})}
			w := httptest.NewRecorder()
			s.startImport(w, ownerRequest("POST", "/api/imports", `{"url":"https://example.com/file","name":"must-not-exist.txt"}`, "42"))
			var job ImportJob
			_ = json.Unmarshal(w.Body.Bytes(), &job)
			deadline := time.Now().Add(time.Second)
			for len(s.imports.slots) > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			s.imports.mu.Lock()
			status := s.imports.jobs[job.ID].Status
			s.imports.mu.Unlock()
			if status != "failed" {
				t.Fatal(status)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if _, ok := store.data["users/42/must-not-exist.txt"]; ok {
				t.Fatal("partial file was published")
			}
		})
	}
}
func TestImportSavingCannotBeMisreportedAsCancelled(t *testing.T) {
	s := New(testTransferStore(), nil, nil)
	s.imports.jobs["saving"] = &ImportJob{ID: "saving", owner: "42", Status: "saving", cancel: func() { t.Error("cancelled committed save") }}
	w := httptest.NewRecorder()
	s.cancelImport(w, ownerRequest("DELETE", "/api/imports/saving", "", "42"))
	if w.Code != 409 {
		t.Fatal("saving cancellation must be explicit conflict")
	}
}
func TestRemoteRedirectDoesNotLeakSignedQueryOrDowngradeTLS(t *testing.T) {
	c := remoteHTTPClient()
	from, _ := http.NewRequestWithContext(context.Background(), "GET", "https://example.com/file?private=signature", nil)
	next, _ := http.NewRequest("GET", "https://other.example/file", nil)
	next.Header.Set("Referer", from.URL.String())
	if err := c.CheckRedirect(next, []*http.Request{from}); err != nil || next.Header.Get("Referer") != "" {
		t.Fatal("signed source referrer leaked")
	}
	downgrade, _ := http.NewRequest("GET", "http://other.example/file", nil)
	if c.CheckRedirect(downgrade, []*http.Request{from}) == nil {
		t.Fatal("TLS downgrade accepted")
	}
}

func TestRemoteImportDoesNotKnowinglyOverwriteExistingPrivateFile(t *testing.T) {
	store := testTransferStore()
	s := New(store, nil, nil)
	s.imports.client = &http.Client{Transport: remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 3, Body: io.NopCloser(strings.NewReader("new")), Header: make(http.Header), Request: r}, nil
	})}
	w := httptest.NewRecorder()
	s.startImport(w, ownerRequest("POST", "/api/imports", `{"url":"https://example.com/file","name":"file.txt"}`, "42"))
	var job ImportJob
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	deadline := time.Now().Add(time.Second)
	for len(s.imports.slots) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s.imports.mu.Lock()
	status := s.imports.jobs[job.ID].Status
	s.imports.mu.Unlock()
	if status != "failed" {
		t.Fatal(status)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if string(store.data["users/42/file.txt"]) != "original file" {
		t.Fatal("remote file replaced private data")
	}
}
