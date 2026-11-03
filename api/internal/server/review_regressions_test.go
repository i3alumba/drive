package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/torrent"
)

func TestTorrentHTTPPersonalRootAndSubdirectory(t *testing.T) {
	// Test the real multipart handler/manager boundary without starting networking.
	t.Setenv("PATH", t.TempDir())
	for _, directory := range []string{"", "docs"} {
		store := testTransferStore()
		manager := torrent.NewManager(store, t.TempDir(), time.Second)
		s := New(store, manager, nil)
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		_ = form.WriteField("path", directory)
		file, _ := form.CreateFormFile("torrent", "owned.torrent")
		_, _ = io.WriteString(file, "invalid fixture metadata")
		_ = form.Close()
		r := ownerRequest("POST", "/api/torrents", "", "42")
		r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
		r.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		s.uploadTorrent(w, r)
		if w.Code != 202 {
			t.Fatalf("directory %q rejected with %d", directory, w.Code)
		}
		var job torrent.Job
		_ = json.Unmarshal(w.Body.Bytes(), &job)
		_, _ = manager.Cancel(job.ID)
		if job.TargetDir != storage.DirPrefix("users/42/"+directory) {
			t.Fatal("wrong target namespace")
		}
	}
}
func TestTorrentHTTPRejectsCrossOwnerAndAdjacentOwnerPrefix(t *testing.T) {
	for _, owner := range []string{"43", "420"} {
		store := testTransferStore()
		s := New(store, torrent.NewManager(store, t.TempDir(), time.Second), nil)
		share, err := s.shares.Create(context.Background(), User{ID: owner, Username: "other"}, "fixture", "shared", true, permissionEdit)
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		_ = form.WriteField("space", shareSpaceID(share.ID))
		file, _ := form.CreateFormFile("torrent", "owned.torrent")
		_, _ = io.WriteString(file, "invalid")
		_ = form.Close()
		r := ownerRequest("POST", "/api/torrents", "", "42")
		r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
		r.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		s.uploadTorrent(w, r)
		if w.Code != 403 || len(s.torrents.List()) != 0 {
			t.Fatal("cross-owner torrent started", w.Code)
		}
	}
}
func TestImportRejectsWhitespaceDotNamesAtHTTPBoundary(t *testing.T) {
	s := New(testTransferStore(), nil, nil)
	for _, directory := range []string{"", "docs"} {
		for _, name := range []string{".. ", " ..", ". ", "\t..\n", " "} {
			input, _ := json.Marshal(map[string]string{"url": "https://example.com/safe.txt", "name": name, "path": directory})
			w := httptest.NewRecorder()
			s.startImport(w, ownerRequest("POST", "/api/imports", string(input), "42"))
			if w.Code != 400 {
				t.Fatal("dot/whitespace name accepted", w.Code)
			}
		}
	}
}
func TestImportRemoteDotDispositionNeverEscapesDestination(t *testing.T) {
	for _, directory := range []string{"", "docs"} {
		store := testTransferStore()
		s := New(store, nil, nil)
		s.imports.client = &http.Client{Transport: remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: 3, Body: io.NopCloser(strings.NewReader("abc")), Header: http.Header{"Content-Disposition": {`attachment; filename=".. "`}}, Request: r}, nil
		})}
		input, _ := json.Marshal(map[string]string{"url": "https://example.com/safe.txt", "path": directory})
		w := httptest.NewRecorder()
		s.startImport(w, ownerRequest("POST", "/api/imports", string(input), "42"))
		if w.Code != 202 {
			t.Fatal(w.Code)
		}
		deadline := time.Now().Add(time.Second)
		for len(s.imports.slots) > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		store.mu.Lock()
		if string(store.data[storage.JoinObjectPath("users/42/"+directory, "safe.txt")]) != "abc" {
			t.Error("safe fallback not published")
		}
		for key := range store.data {
			if !strings.HasPrefix(key, storage.DirPrefix("users/42")) {
				t.Error("out-of-owner write", key)
			}
		}
		store.mu.Unlock()
	}
}

type deletionFailureStore struct {
	*transferStore
	failDataOnce   bool
	cancelMetadata context.CancelFunc
}

func (s *deletionFailureStore) DeleteObject(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.HasPrefix(key, "_drive/public-link-data/") && s.failDataOnce {
		s.failDataOnce = false
		return errors.New("owned fixture transient failure")
	}
	return s.transferStore.DeleteObject(ctx, key)
}
func (s *deletionFailureStore) PutObject(ctx context.Context, key string, r io.Reader, size int64, kind string) error {
	if strings.HasPrefix(key, "_drive/public-links/") && s.cancelMetadata != nil {
		s.cancelMetadata()
		return context.Canceled
	}
	return s.transferStore.PutObject(ctx, key, r, size, kind)
}
func TestRevokedSnapshotCleanupRemainsListableAndRetryable(t *testing.T) {
	store := &deletionFailureStore{transferStore: testTransferStore()}
	s := New(store, nil, nil)
	w := httptest.NewRecorder()
	s.createPublicLink(w, ownerRequest("POST", "/api/public-links", `{"path":"file.txt"}`, "42"))
	var result struct {
		Link PublicLink `json:"link"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	store.failDataOnce = true
	w = httptest.NewRecorder()
	s.deletePublicLink(w, ownerRequest("DELETE", "/api/public-links/"+result.Link.ID, "", "42"))
	if w.Code == 204 {
		t.Fatal("cleanup failure hidden")
	}
	list := httptest.NewRecorder()
	s.listPublicLinks(list, ownerRequest("GET", "/api/public-links", "", "42"))
	if !strings.Contains(list.Body.String(), result.Link.ID) || !strings.Contains(list.Body.String(), `"cleanupOnly":true`) {
		t.Fatal("orphan cleanup reference lost")
	}
	w = httptest.NewRecorder()
	s.deletePublicLink(w, ownerRequest("DELETE", "/api/public-links/"+result.Link.ID, "", "42"))
	if w.Code != 204 {
		t.Fatal("retry failed")
	}
	if _, _, err := store.GetObject(context.Background(), linkData("42", result.Link.ID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("snapshot retained after cleanup retry")
	}
}
func TestSnapshotCreationCancellationUsesIndependentBoundedCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &deletionFailureStore{transferStore: testTransferStore(), cancelMetadata: cancel}
	s := New(store, nil, nil)
	r := ownerRequest("POST", "/api/public-links", `{"path":"file.txt"}`, "42")
	r = r.WithContext(context.WithValue(ctx, userContextKey, currentUser(r)))
	w := httptest.NewRecorder()
	s.createPublicLink(w, r)
	store.mu.Lock()
	defer store.mu.Unlock()
	for key := range store.data {
		if strings.HasPrefix(key, "_drive/public-link-data/") {
			t.Fatal("cancelled request prevented independent cleanup")
		}
	}
}
