package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicFragmentExchangeTLSCookieAndRevocation(t *testing.T) {
	store := testTransferStore()
	s := New(store, nil, nil)
	created := httptest.NewRecorder()
	s.createPublicLink(created, ownerRequest("POST", "/api/public-links", `{"path":"file.txt"}`, "42"))
	if created.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("capability creation response cacheable")
	}
	if created.Code != 201 {
		t.Fatal(created.Code)
	}
	var result struct {
		Link PublicLink `json:"link"`
		URL  string     `json:"downloadUrl"`
	}
	_ = json.Unmarshal(created.Body.Bytes(), &result)
	share, _ := url.Parse(result.URL)
	token := share.Fragment
	if !hexID(token) || strings.Contains(share.Path, token) || share.RawQuery != "" || !strings.HasPrefix(share.Path, "/public/") {
		t.Fatal("capability must be fragment-only")
	}
	attachment := publicDownloadPath("42", result.Link.ID)
	var requests atomic.Int64
	var active atomic.Pointer[Server]
	active.Store(s)
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.Contains(r.RequestURI, token) {
			t.Error("long-lived token in request URI")
		}
		active.Load().Routes().ServeHTTP(w, r)
	}))
	defer tlsServer.Close()
	client := tlsServer.Client()
	client.Jar, _ = cookiejar.New(nil)
	get := func(method string) *http.Response {
		r, _ := http.NewRequest(method, tlsServer.URL+attachment, nil)
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := get("GET")
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("identifier alone authorized download")
	}
	exchange := func(raw, origin, kind string) *http.Response {
		body, _ := json.Marshal(map[string]string{"token": raw})
		request, _ := http.NewRequest("POST", tlsServer.URL+attachment+"/session", bytes.NewReader(body))
		request.Header.Set("Content-Type", kind)
		request.Header.Set("Origin", origin)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	for _, bad := range []struct {
		token, origin, kind string
		status              int
	}{{randomID(), tlsServer.URL, "application/json", 404}, {token, "https://foreign.invalid", "application/json", 403}, {token, tlsServer.URL, "text/plain", 415}} {
		response = exchange(bad.token, bad.origin, bad.kind)
		response.Body.Close()
		if response.StatusCode != bad.status || len(response.Cookies()) != 0 {
			t.Fatal("invalid exchange accepted", response.StatusCode)
		}
	}
	response = exchange(token, tlsServer.URL, "application/json")
	response.Body.Close()
	if response.StatusCode != 204 || len(response.Cookies()) != 1 {
		t.Fatal("exchange failed", response.StatusCode)
	}
	cookie := response.Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != attachment || cookie.MaxAge <= 0 || cookie.MaxAge > 300 || strings.Contains(cookie.Value, token) {
		t.Fatal("invalid short scoped cookie")
	}
	response = get("GET")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "original file" {
		t.Fatal("native streaming download failed")
	}
	response = get("HEAD")
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || len(body) != 0 {
		t.Fatal("HEAD contract failed")
	}
	// Per-process cookie keys are deliberately ephemeral, not a persistent secret.
	s = New(store, nil, nil)
	active.Store(s)
	response = get("GET")
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("old process cookie survived restart")
	}
	response = exchange(token, tlsServer.URL, "application/json")
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("durable link cannot renew session")
	}
	revoked := httptest.NewRecorder()
	s.deletePublicLink(revoked, ownerRequest("DELETE", "/api/public-links/"+result.Link.ID, "", "42"))
	response = get("GET")
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("cookie bypassed revocation")
	}
	response = exchange(token, tlsServer.URL, "application/json")
	response.Body.Close()
	if response.StatusCode != 404 || len(response.Cookies()) != 0 {
		t.Fatal("revoked link exchanged")
	}
	if requests.Load() < 10 {
		t.Fatal("TLS boundary not exercised")
	}
}

func TestPublicCookieScopeTamperAndLifetime(t *testing.T) {
	s := New(testTransferStore(), nil, nil)
	owner, id := "42", randomID()
	cookie := func(expiry int64, signature string) *http.Cookie {
		return &http.Cookie{Name: publicCookieName, Value: strconv.FormatInt(expiry, 10) + "." + signature}
	}
	now := time.Now().Unix()
	validExpiry := now + 120
	validSignature := s.publicGrant(owner, id, strconv.FormatInt(validExpiry, 10))
	r := httptest.NewRequest("GET", publicDownloadPath(owner, id), nil)
	r.AddCookie(cookie(validExpiry, validSignature))
	if !s.validPublicSession(r, owner, id) {
		t.Fatal("valid grant rejected")
	}
	for _, scope := range [][2]string{{"43", id}, {owner, randomID()}} {
		if s.validPublicSession(r, scope[0], scope[1]) {
			t.Fatal("grant crossed snapshot scope")
		}
	}
	for _, expiry := range []int64{now - 1, now + 600} {
		r = httptest.NewRequest("GET", publicDownloadPath(owner, id), nil)
		r.AddCookie(cookie(expiry, s.publicGrant(owner, id, strconv.FormatInt(expiry, 10))))
		if s.validPublicSession(r, owner, id) {
			t.Fatal("invalid lifetime accepted")
		}
	}
	r = httptest.NewRequest("GET", publicDownloadPath(owner, id), nil)
	r.AddCookie(cookie(validExpiry, randomID()))
	if s.validPublicSession(r, owner, id) {
		t.Fatal("tampered grant accepted")
	}
}
