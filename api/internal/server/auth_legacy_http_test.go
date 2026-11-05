package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestExplicitLegacyHTTPAuthRetainsLiveGrantAndBearer(t *testing.T) {
	var granted atomic.Bool
	granted.Store(true)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/api/user/" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"id":42,"username":"fixture","services":{"drive":%t}}`, granted.Load())
	}))
	defer upstream.Close()
	endpoint := "http://auth:8000/api/user/"
	if NewAuthValidator(endpoint) != nil {
		t.Fatal("HTTPS default accepted plaintext Auth")
	}
	validator := NewLegacyHTTPAuthValidator(endpoint)
	if validator == nil {
		t.Fatal("explicit legacy endpoint rejected")
	}
	validator.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	user, ok := validator.Validate(context.Background(), "fixture-token")
	if !ok || user.ID != "42" || !user.DriveAllowed {
		t.Fatal("live identity mismatch", user, ok)
	}
	preview := httptest.NewRequest("POST", "/api/video-previews", nil)
	preview.Header.Set("Authorization", "Bearer fixture-token")
	unavailable := httptest.NewRecorder()
	New(testTransferStore(), nil, validator).Routes().ServeHTTP(unavailable, preview)
	if unavailable.Code != 503 {
		t.Fatal("unconfigured HLS did not fail closed", unavailable.Code)
	}
	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	request := httptest.NewRequest("GET", "/api/files", nil)
	request.Header.Set("Authorization", "Bearer fixture-token")
	granted.Store(false)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 403 || hits.Load() < 2 {
		t.Fatal("revoked live grant accepted", response.Code)
	}
	request = httptest.NewRequest("POST", "/api/video-previews", nil)
	request.AddCookie(&http.Cookie{Name: "access_token", Value: "fixture-token"})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("cookie-only mutation accepted", response.Code)
	}
}

func TestExplicitLegacyHTTPAuthRejectsAliasesAndRedirects(t *testing.T) {
	for _, endpoint := range []string{"https://auth/api/user/", "http://auth/api/user/?x=1", "http://auth/api/user/#x", "http://auth/api/%75ser/", "http://user@auth/api/user/", "http://auth:0/api/user/", "http://auth/api/user/?", "http://auth/api/user/#", "http://other:8000/api/user/", "http://auth:80/api/user/", "http://127.0.0.1:8000/api/user/"} {
		if NewLegacyHTTPAuthValidator(endpoint) != nil {
			t.Fatal("invalid legacy endpoint accepted", endpoint)
		}
	}
	var destinationHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationHits.Add(1); w.WriteHeader(500) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/api/user/", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	validator := NewLegacyHTTPAuthValidator("http://auth:8000/api/user/")
	validator.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, source.Listener.Addr().String())
	}
	if _, ok := validator.Validate(context.Background(), "fixture-token"); ok || destinationHits.Load() != 0 {
		t.Fatal("legacy Auth redirect followed")
	}
	transport, ok := validator.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("environment proxy enabled for private Auth")
	}
}
