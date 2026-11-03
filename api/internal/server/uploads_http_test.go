//go:build linux

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChunkedUploadHTTPRealHandlersAndAuth(t *testing.T) {
	var revoked atomic.Bool
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "42"
		if r.Header.Get("Authorization") == "Bearer fixture-other" {
			id = "43"
		}
		fmt.Fprintf(w, `{"id":%s,"username":"fixture","services":{"drive":%t}}`, id, !revoked.Load())
	}))
	defer auth.Close()
	store := testTransferStore()
	s := New(store, nil, NewAuthValidator(auth.URL))
	if err := s.EnableUploads(UploadOptions{Root: filepath.Join(t.TempDir(), "sessions"), MaxFileBytes: 1 << 20, BudgetBytes: 2 << 20}); err != nil {
		t.Fatal(err)
	}
	defer s.CloseUploads()
	api := httptest.NewServer(s.Routes())
	defer api.Close()
	request := func(method, path, token string, data []byte, checksum string) (int, []byte) {
		t.Helper()
		r, err := http.NewRequest(method, api.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("Content-Type", "application/json")
		if checksum != "" {
			r.Header.Set("X-Chunk-SHA256", checksum)
			r.Header.Set("Content-Type", "application/octet-stream")
		}
		response, err := api.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	code, _ := request("POST", "/api/uploads", "", []byte(`{}`), "")
	if code != 401 {
		t.Fatal("unauthenticated mutation accepted")
	}
	for _, name := range []string{"..", " .. ", "../escape", "folder/file"} {
		body, _ := json.Marshal(map[string]any{"name": name, "size": 3})
		code, _ := request("POST", "/api/uploads", "fixture-owner", body, "")
		if code != 403 {
			t.Fatalf("unsafe filename accepted: HTTP %d", code)
		}
	}
	code, body := request("POST", "/api/uploads", "fixture-owner", []byte(`{"name":"fixture.txt","path":"docs","space":"personal","size":3,"contentType":"text/plain"}`), "")
	if code != 201 {
		t.Fatal(code, string(body))
	}
	var status UploadStatus
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ method, suffix string }{{"GET", ""}, {"DELETE", ""}, {"POST", "/complete"}, {"PUT", "/parts/0"}} {
		code, _ := request(item.method, "/api/uploads/"+status.ID+item.suffix, "fixture-other", []byte("abc"), testUploadDigest([]byte("abc")))
		if code != 404 {
			t.Fatalf("owner isolation failed: %s HTTP %d", item.method, code)
		}
	}
	code, body = request("POST", "/api/uploads/"+status.ID+"/complete", "fixture-owner", nil, "")
	if code != 409 {
		t.Fatal("missing part accepted")
	}
	code, body = request("PUT", "/api/uploads/"+status.ID+"/parts/0", "fixture-owner", []byte("abc"), testUploadDigest([]byte("abc")))
	if code != 200 {
		t.Fatal(code, string(body))
	}
	code, body = request("POST", "/api/uploads/"+status.ID+"/complete", "fixture-owner", nil, "")
	if code != 202 {
		t.Fatal(code, string(body))
	}
	waitUploadState(t, s.uploads, status.ID, "complete")
	store.mu.Lock()
	actual := string(store.data["users/42/docs/fixture.txt"])
	store.mu.Unlock()
	if actual != "abc" {
		t.Fatal("real HTTP upload published wrong data")
	}
	code, _ = request("GET", "/api/uploads/"+status.ID, "fixture-owner", nil, "")
	if code != 200 {
		t.Fatal(code)
	}
	revoked.Store(true)
	code, _ = request("GET", "/api/uploads/"+status.ID, "fixture-owner", nil, "")
	if code != 403 {
		t.Fatal("revoked Drive grant accepted")
	}
}

func TestChunkedUploadHTTPRequiresBearerForMutations(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":42,"username":"fixture","services":{"drive":true}}`)
	}))
	defer auth.Close()
	s := New(testTransferStore(), nil, NewAuthValidator(auth.URL))
	request := httptest.NewRequest("POST", "/api/uploads", strings.NewReader(`{"name":"file","size":0}`))
	request.AddCookie(&http.Cookie{Name: "access_token", Value: "fixture-cookie"})
	response := httptest.NewRecorder()
	s.Routes().ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("cookie-only mutation accepted")
	}
}
