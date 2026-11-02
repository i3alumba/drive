package server

import (
	"context"
	"net/http"
	"time"
)

type readinessBackend interface{ Ready(context.Context) error }

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	unavailable := func() { writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"}) }
	if s.store == nil {
		unavailable()
		return
	}
	backend, ok := s.store.(readinessBackend)
	if !ok {
		unavailable()
		return
	}
	// Hard NFS syscalls may outlive Go context deadlines. Retain this slot
	// until the actual check returns: repeated probes cannot leak workers.
	select {
	case s.probe <- struct{}{}:
	default:
		unavailable()
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		err := backend.Ready(ctx)
		result <- err
		<-s.probe
	}()
	select {
	case err := <-result:
		if err != nil {
			unavailable()
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case <-ctx.Done():
		unavailable()
	}
}
