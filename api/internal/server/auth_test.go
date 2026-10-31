package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDecodeAuthIdentity(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		id      string
		allowed bool
		invalid bool
	}{
		{"existing i3 identity", `[{"id":1,"username":"i3alumba","services":{"drive":true}}]`, "1", true, false},
		{"existing gore identity", `[{"id":2,"username":"gore","services":{"drive":true}}]`, "2", true, false},
		{"new separate identity", `{"id":3,"username":"procsima","services":{"drive":false}}`, "3", false, false},
		{"large integer is not rounded", `{"id":9007199254740993,"username":"user","services":{"drive":true}}`, "9007199254740993", true, false},
		{"string integer", `{"id":"1","username":"user","services":{"drive":true}}`, "1", true, false},
		{"missing grant", `{"id":1,"username":"user"}`, "1", false, false},
		{"string grant denied", `{"id":1,"username":"user","services":{"drive":"true"}}`, "1", false, false},
		{"object grant denied", `{"id":1,"username":"user","services":{"drive":{"enabled":true}}}`, "1", false, false},
		{"path owner rejected", `{"id":"../2","username":"user"}`, "", false, true},
		{"anonymous owner rejected", `{"id":"anonymous","username":"user"}`, "", false, true},
		{"zero rejected", `{"id":0,"username":"user"}`, "", false, true},
		{"negative rejected", `{"id":-1,"username":"user"}`, "", false, true},
		{"fraction rejected", `{"id":1.2,"username":"user"}`, "", false, true},
		{"noncanonical integer rejected", `{"id":"01","username":"user"}`, "", false, true},
		{"numeric username rejected", `{"id":1,"username":2}`, "", false, true},
		{"multiple users rejected", `[{"id":1,"username":"first"},{"id":2,"username":"second"}]`, "", false, false},
		{"empty list rejected", `[]`, "", false, false},
		{"trailing JSON rejected", `{"id":1,"username":"user"}{}`, "", false, true},
		{"malformed JSON rejected", `{`, "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := &http.Response{Body: io.NopCloser(strings.NewReader(tt.body))}
			user, err := decodeUser(response)
			if (err != nil) != tt.invalid || user.ID != tt.id || user.DriveAllowed != tt.allowed {
				t.Fatalf("user=%+v error=%v", user, err)
			}
		})
	}
}

func TestAuthMiddlewareFailsClosedAndKeepsStorageOwner(t *testing.T) {
	tests := []struct {
		name       string
		configured bool
		path       string
		method     string
		header     bool
		cookie     bool
		body       string
		status     int
		owner      string
	}{
		{"unconfigured denies", false, "/api/files", "GET", false, false, "", 503, ""},
		{"unconfigured health works", false, "/healthz", "GET", false, false, "", 200, ""},
		{"configured health works", true, "/healthz", "GET", false, false, "", 200, ""},
		{"missing token", true, "/api/files", "GET", false, false, "", 401, ""},
		{"header preserves owner one", true, "/api/files", "GET", true, false, `[{"id":1,"username":"i3alumba","services":{"drive":true}}]`, 200, "1"},
		{"media cookie preserves owner two", true, "/api/view", "GET", false, true, `[{"id":2,"username":"gore","services":{"drive":true}}]`, 200, "2"},
		{"mutation cookie cannot forge request", true, "/api/directories", "POST", false, true, "", 401, ""},
		{"header mutation works", true, "/api/directories", "POST", true, false, `[{"id":1,"username":"i3alumba","services":{"drive":true}}]`, 200, "1"},
		{"no service grant", true, "/api/files", "GET", true, false, `[{"id":1,"username":"i3alumba"}]`, 403, ""},
		{"procsima denied without Drive", true, "/api/files", "GET", true, false, `[{"id":3,"username":"procsima","services":{"drive":false}}]`, 403, ""},
		{"unexpected identity schema", true, "/api/files", "GET", true, false, `[]`, 401, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("authorization header not forwarded correctly")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer upstream.Close()
			url := ""
			if tt.configured {
				url = upstream.URL
			}
			validator := NewAuthValidator(url)
			called := false
			handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if tt.owner != "" && currentUser(r).ID != tt.owner {
					t.Error("storage owner changed")
				}
				w.WriteHeader(200)
			}))
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.header {
				req.Header.Set("Authorization", "Bearer test-token")
			}
			if tt.cookie {
				req.AddCookie(&http.Cookie{Name: "access_token", Value: "test-token"})
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.status || called != (tt.status == 200) {
				t.Fatalf("status=%d handlerCalled=%v", recorder.Code, called)
			}
		})
	}
}

func TestAuthDoesNotFollowRedirectOrAcceptUnavailableUpstream(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, code := range []int{http.StatusFound, http.StatusUnauthorized, http.StatusInternalServerError} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code == http.StatusFound {
				w.Header().Set("Location", target.URL)
			}
			w.WriteHeader(code)
		}))
		_, ok := NewAuthValidator(upstream.URL).Validate(context.Background(), "test-token")
		upstream.Close()
		if ok {
			t.Fatalf("accepted HTTP %d", code)
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("followed authentication redirect")
	}
}
