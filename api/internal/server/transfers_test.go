package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
)

type transferStore struct {
	Store
	mu   sync.Mutex
	data map[string][]byte
}

func testTransferStore() *transferStore {
	return &transferStore{data: map[string][]byte{"users/42/file.txt": []byte("original file")}}
}
func (s *transferStore) PutObject(ctx context.Context, key string, r io.Reader, size int64, kind string) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = body
	return nil
}
func (s *transferStore) GetObject(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.data[key]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(body))), storage.ObjectInfo{Size: int64(len(body)), ContentType: "text/plain"}, nil
}
func (s *transferStore) StatObject(ctx context.Context, key string) (storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.data[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}
	return storage.Object{Size: int64(len(body)), Name: "file.txt", Path: key}, nil
}
func (s *transferStore) DeleteObject(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; !ok {
		return storage.ErrNotFound
	}
	delete(s.data, key)
	return nil
}
func (s *transferStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []storage.Object{}
	for key := range s.data {
		if strings.HasPrefix(key, prefix) {
			out = append(out, storage.Object{Name: strings.TrimPrefix(key, prefix), Path: key})
		}
	}
	return out, nil
}
func ownerRequest(method, target, body, owner string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	return r.WithContext(context.WithValue(r.Context(), userContextKey, User{ID: owner, Username: "fixture"}))
}

func TestPublicLinkSnapshotRevocationAndRestart(t *testing.T) {
	store := testTransferStore()
	server := New(store, nil, nil)
	w := httptest.NewRecorder()
	server.createPublicLink(w, ownerRequest("POST", "/api/public-links", `{"path":"file.txt"}`, "42"))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created struct {
		Link PublicLink `json:"link"`
		URL  string     `json:"downloadUrl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.data["users/42/file.txt"] = []byte("private replacement")
	store.mu.Unlock()
	// Snapshot capabilities survive a process restart but do not expose a newly
	// uploaded file at the same path. Tokens cannot be reconstructed from lists.
	server = New(store, nil, nil)
	shared, _ := url.Parse(created.URL)
	token := shared.Fragment
	attachment := publicDownloadPath("42", created.Link.ID)
	var cookie *http.Cookie
	get := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", target, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		server.Routes().ServeHTTP(w, r)
		return w
	}
	if get(attachment).Code != 403 {
		t.Fatal("identifier alone authorized download")
	}
	payload, _ := json.Marshal(map[string]string{"token": token})
	exchange := httptest.NewRequest("POST", attachment+"/session", bytes.NewReader(payload))
	exchange.Header.Set("Content-Type", "application/json")
	exchanged := httptest.NewRecorder()
	server.Routes().ServeHTTP(exchanged, exchange)
	if exchanged.Code != 204 || len(exchanged.Result().Cookies()) != 1 {
		t.Fatal("capability exchange failed", exchanged.Code)
	}
	cookie = exchanged.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.Path != attachment || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge > 300 {
		t.Fatal("incorrect scoped cookie flags")
	}
	response := get(attachment)
	if response.Code != 200 || response.Body.String() != "original file" || response.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("snapshot download failed", response.Code)
	}
	list := httptest.NewRecorder()
	server.listPublicLinks(list, ownerRequest("GET", "/api/public-links?path=file.txt", "", "42"))
	if strings.Contains(list.Body.String(), token) || !strings.Contains(list.Body.String(), created.Link.ID) {
		t.Fatal("token leaked or list missing")
	}
	wrongOwner := httptest.NewRecorder()
	server.deletePublicLink(wrongOwner, ownerRequest("DELETE", "/api/public-links/"+created.Link.ID, "", "43"))
	if wrongOwner.Code != 204 || get(attachment).Code != 200 {
		t.Fatal("other user revoked link")
	}
	revoke := httptest.NewRecorder()
	server.deletePublicLink(revoke, ownerRequest("DELETE", "/api/public-links/"+created.Link.ID, "", "42"))
	if revoke.Code != 204 || get(attachment).Code != 404 {
		t.Fatal("revocation failed")
	}
	if get(strings.Replace(attachment, "/42/", "/43/", 1)).Code != 404 {
		t.Fatal("cross-owner link accepted")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if string(store.data["users/42/file.txt"]) != "private replacement" {
		t.Fatal("original file modified")
	}
}
func TestPublicLinkExpiryAndNarrowAnonymousRoute(t *testing.T) {
	store := testTransferStore()
	server := New(store, nil, nil)
	w := httptest.NewRecorder()
	server.createPublicLink(w, ownerRequest("POST", "/api/public-links", `{"path":"file.txt"}`, "42"))
	var created struct {
		Link PublicLink `json:"link"`
		URL  string     `json:"downloadUrl"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	created.Link.ExpiresAt = time.Now().Add(-time.Second)
	body, _ := json.Marshal(created.Link)
	store.data[linkMetadata("42", created.Link.ID)] = body
	attachment := publicDownloadPath("42", created.Link.ID)
	for _, target := range []string{attachment, "/api/public/42/nope", "/api/public/../file.txt"} {
		w = httptest.NewRecorder()
		server.Routes().ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code == 200 {
			t.Fatal("invalid capability accepted")
		}
	}
	w = httptest.NewRecorder()
	server.Routes().ServeHTTP(w, httptest.NewRequest("POST", attachment, nil))
	if w.Code != 503 {
		t.Fatal("public route bypassed mutation auth")
	}
}
func TestRemoteDestinationsAndRedirectsFailClosed(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "https://user:pass@example.com/a", "http://127.0.0.1/a", "http://localhost/a", "http://192.168.1.1", "http://[::1]/", "http://[::ffff:127.0.0.1]/", "http://100.100.100.200/", "https://example.com:22/a", "https://example.com/#fragment"} {
		u, _ := url.Parse(raw)
		if validateRemoteURL(u) == nil {
			t.Error("accepted", raw)
		}
	}
	for _, raw := range []string{"https://example.com/a?signature=fixture", "http://example.com/file"} {
		u, _ := url.Parse(raw)
		if err := validateRemoteURL(u); err != nil {
			t.Error(err)
		}
	}
	for _, raw := range []string{"169.254.169.254", "198.18.1.1", "64:ff9b::7f00:1", "2002:7f00:1::", "fc00::1", "fe80::1", "fec0::1"} {
		if publicRemoteIP(netip.MustParseAddr(raw)) {
			t.Error("accepted special IP", raw)
		}
	}
	client := remoteHTTPClient()
	request, _ := http.NewRequest("GET", "http://127.0.0.1/", nil)
	if client.CheckRedirect(request, nil) == nil {
		t.Fatal("private redirect accepted")
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("ambient proxy enabled")
	}
	if conn, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:80"); err == nil {
		conn.Close()
		t.Fatal("dialed unchecked local address")
	}
}

type remoteRoundTrip func(*http.Request) (*http.Response, error)

func (f remoteRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestImportStreamsToOwnerAndHidesURL(t *testing.T) {
	store := testTransferStore()
	server := New(store, nil, nil)
	server.imports.client = &http.Client{Transport: remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials forwarded")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("remote fixture")), ContentLength: 14, Header: http.Header{"Content-Disposition": {`attachment; filename="remote.txt"`}}, Request: r}, nil
	})}
	w := httptest.NewRecorder()
	server.startImport(w, ownerRequest("POST", "/api/imports", `{"url":"https://example.com/file?secret=not-for-job-json","path":"docs"}`, "42"))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started ImportJob
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		server.imports.mu.Lock()
		status := server.imports.jobs[started.ID].Status
		server.imports.mu.Unlock()
		if status == "failed" {
			t.Fatal("import failed")
		}
		if status == "complete" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	store.mu.Lock()
	body := string(store.data["users/42/docs/remote.txt"])
	store.mu.Unlock()
	if body != "remote fixture" {
		t.Fatal("missing imported bytes")
	}
	w = httptest.NewRecorder()
	server.listImports(w, ownerRequest("GET", "/api/imports", "", "42"))
	if strings.Contains(w.Body.String(), "secret=") {
		t.Fatal("source URL leaked")
	}
	w = httptest.NewRecorder()
	server.cancelImport(w, ownerRequest("DELETE", "/api/imports/"+started.ID, "", "43"))
	if w.Code != 404 {
		t.Fatal("cross-user import exposed")
	}
}
func TestImportRejectsInvalidNamesAndSharedSpaces(t *testing.T) {
	s := New(testTransferStore(), nil, nil)
	for _, body := range []string{`{"url":"http://127.0.0.1/a"}`, `{"url":"https://example.com/a","name":"../private"}`, `{"url":"https://example.com/a","space":"share:other"}`} {
		w := httptest.NewRecorder()
		s.startImport(w, ownerRequest("POST", "/api/imports", body, "42"))
		if w.Code == 202 {
			t.Fatal("invalid request started")
		}
	}
}
