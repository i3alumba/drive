//go:build linux

package videoworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const hlsDevelopmentRole = "development-artifact-runner-v1"

type developmentHLSHandler struct {
	root     string
	options  Options
	epoch    string
	mu       sync.Mutex
	accepted bool
	identity HLSIdentity
	cancel   context.CancelFunc
}

// NewDevelopmentHLSHandler is permanently unqualified for production clients.
// Admission is deliberately never reopened, even after conversion/cancellation.
func NewDevelopmentHLSHandler(root string, options Options) (http.Handler, error) {
	normalized, err := NewHandler(root, options)
	if err != nil {
		return nil, err
	}
	var epoch [16]byte
	if _, err = rand.Read(epoch[:]); err != nil {
		return nil, err
	}
	return &developmentHLSHandler{root: root, options: normalized.(*handler).options, epoch: hex.EncodeToString(epoch[:])}, nil
}

func (h *developmentHLSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	reject := func(code string, status int) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(Error{Code: code}) }
	if r.Method == http.MethodGet && r.URL.Path == "/v1/capabilities" && r.URL.RawQuery == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"v": HLSVersion, "isolation": hlsDevelopmentRole, "controller_epoch": h.epoch})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	stream := strings.HasSuffix(path, "/events")
	id := strings.TrimSuffix(path, "/events")
	if !strings.HasPrefix(r.URL.Path, "/v1/jobs/") || !jobID.MatchString(id) {
		reject("invalid_request", 400)
		return
	}
	if !stream && (r.Method == http.MethodGet || r.Method == http.MethodDelete) {
		query, err := parseHLSControlQuery(r)
		identity := HLSIdentity{JobID: id, ControllerEpoch: query["controller_epoch"], ExecutionID: query["execution_id"]}
		if err != nil || !identity.Valid() {
			reject("invalid_request", 400)
			return
		}
		if r.Method == http.MethodDelete {
			h.mu.Lock()
			if h.identity == identity && h.cancel != nil {
				h.cancel()
			}
			h.mu.Unlock()
		}
		// Development can acknowledge intent, not absence, teardown or quiescence.
		_ = json.NewEncoder(w).Encode(map[string]any{"v": HLSVersion, "job_id": id, "controller_epoch": identity.ControllerEpoch, "execution_id": identity.ExecutionID, "state": "unknown"})
		return
	}
	if !stream || r.Method != http.MethodPost || r.URL.RawQuery != "" {
		reject("invalid_request", 400)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil || len(data) > 1024 {
		reject("invalid_request", 400)
		return
	}
	if _, err = hlsObject(data, []string{"format", "max_output_bytes", "controller_epoch", "execution_id"}); err != nil {
		reject("invalid_request", 400)
		return
	}
	var req hlsRequest
	if json.Unmarshal(data, &req) != nil || !(request{req.Format, req.MaxOutputBytes}).valid() || req.MaxOutputBytes > h.options.MaxOutputBytes || req.ControllerEpoch != h.epoch || !jobID.MatchString(req.ExecutionID) {
		reject("invalid_request", 400)
		return
	}
	identity := HLSIdentity{JobID: id, ControllerEpoch: req.ControllerEpoch, ExecutionID: req.ExecutionID}
	ctx, cancel := context.WithTimeout(r.Context(), h.options.Timeout)
	defer cancel()
	h.mu.Lock()
	if h.accepted {
		h.mu.Unlock()
		reject("busy", 429)
		return
	}
	h.accepted = true
	h.identity = identity
	h.cancel = cancel
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Video-Controller-Epoch", h.epoch)
	w.Header().Set("X-Video-Execution-ID", req.ExecutionID)
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	_, _ = RunDevelopmentHLS(ctx, h.root, id, req.Format, h.options, req.MaxOutputBytes, func(event HLSEvent) error {
		_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := EncodeHLSEvent(w, event); err != nil {
			return err
		}
		return rc.Flush()
	})
}

func parseHLSControlQuery(r *http.Request) (map[string]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 2 || len(q["controller_epoch"]) != 1 || len(q["execution_id"]) != 1 {
		return nil, failure("invalid_request")
	}
	return map[string]string{"controller_epoch": q.Get("controller_epoch"), "execution_id": q.Get("execution_id")}, nil
}
