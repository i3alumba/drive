//go:build !linux

package videoworker

import "net/http"

// Preserve portable API builds; executing previews requires the Linux worker.
func NewHandler(string, Options) (http.Handler, error) {
	return nil, failure("worker_unavailable")
}

func Initialize(string) error { return failure("worker_unavailable") }
