package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type probeBackend struct {
	Store
	check func(context.Context) error
}

func (b probeBackend) Ready(ctx context.Context) error { return b.check(ctx) }

func TestReadinessIsAnonymousButDoesNotBypassAPIAuthOrLeakBackendErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"healthy", nil, http.StatusOK},
		{"unavailable", errors.New("private backend details"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(probeBackend{check: func(context.Context) error { return tc.err }}, nil, nil).Routes()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != tc.status || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("probe response: %d %s", w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/files", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatal("readiness bypass extended to protected API")
			}
		})
	}
}

func TestBlockedProbeRetainsSlotAfterRequestCancellation(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	handler := New(probeBackend{check: func(context.Context) error {
		calls.Add(1)
		close(started)
		<-release
		return nil
	}}, nil, nil).Routes()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
		close(finished)
	}()
	<-started
	cancel()
	<-finished
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatal("blocked probe did not fail closed")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("repeated probes created blocked workers")
	}
}
