//go:build linux

package videoworker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var hlsClientIdentity = HLSIdentity{JobID: testID, ControllerEpoch: "11111111111111111111111111111111", ExecutionID: "22222222222222222222222222222222"}

func hlsUnixClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "hls-unix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "worker.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	return NewClient(path)
}

func hlsTestTerminal() HLSEvent {
	return HLSEvent{V: 1, Type: "terminal", Outcome: "failed", Code: "conversion_failed", JobID: hlsClientIdentity.JobID, ControllerEpoch: hlsClientIdentity.ControllerEpoch, ExecutionID: hlsClientIdentity.ExecutionID, Quiescence: "confirmed"}
}

func TestHLSClientQualifiedUnix(t *testing.T) {
	var posts atomic.Int32
	c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			json.NewEncoder(w).Encode(map[string]any{"v": 1, "isolation": HLSControllerRole, "controller_epoch": hlsClientIdentity.ControllerEpoch})
		case "/v1/jobs/" + testID + "/events":
			posts.Add(1)
			var req hlsRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.ControllerEpoch != hlsClientIdentity.ControllerEpoch || req.ExecutionID != hlsClientIdentity.ExecutionID {
				t.Error("request identity")
			}
			w.Header().Set("X-Video-Controller-Epoch", req.ControllerEpoch)
			w.Header().Set("X-Video-Execution-ID", req.ExecutionID)
			EncodeHLSEvent(w, HLSEvent{V: 1, Type: "conversion_end", Outcome: "failed", Code: "conversion_failed"})
			EncodeHLSEvent(w, hlsTestTerminal())
		default:
			if r.Method != http.MethodGet && r.Method != http.MethodDelete {
				t.Error("control method")
			}
			if r.URL.Query().Get("execution_id") != hlsClientIdentity.ExecutionID {
				t.Error("control identity")
			}
			terminal := hlsTestTerminal()
			var b strings.Builder
			EncodeHLSEvent(&b, terminal)
			w.Write([]byte(`{"v":1,"job_id":"` + testID + `","controller_epoch":"` + hlsClientIdentity.ControllerEpoch + `","execution_id":"` + hlsClientIdentity.ExecutionID + `","state":"quiescent","terminal":` + b.String() + `}`))
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	epoch, err := c.HLSReady(ctx)
	if err != nil || epoch != hlsClientIdentity.ControllerEpoch {
		t.Fatal(epoch, err)
	}
	sequence, err := c.StreamHLS(ctx, hlsClientIdentity, "mkv", 1024, nil)
	if err != nil || !sequence.Terminal || posts.Load() != 1 {
		t.Fatal(sequence, err, posts.Load())
	}
	for _, fn := range []func(context.Context, HLSIdentity) (HLSStatus, error){c.LookupHLS, c.CancelHLS} {
		s, err := fn(ctx, hlsClientIdentity)
		if err != nil || !s.ConfirmedFor(hlsClientIdentity) {
			t.Fatal(s, err)
		}
	}
}

func TestHLSClientRejectsDevelopmentBeforePOST(t *testing.T) {
	root, _ := newJob(t)
	h, err := NewDevelopmentHLSHandler(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	if _, err = c.StreamHLS(context.Background(), hlsClientIdentity, "mkv", 1024, nil); err == nil || posts.Load() != 0 {
		t.Fatal("development accepted", err)
	}
}

func TestHLSClientStrictControl(t *testing.T) {
	prefix := `{"v":1,"job_id":"` + testID + `","controller_epoch":"` + hlsClientIdentity.ControllerEpoch + `","execution_id":"` + hlsClientIdentity.ExecutionID + `",`
	var terminal strings.Builder
	EncodeHLSEvent(&terminal, hlsTestTerminal())
	for name, body := range map[string]string{
		"empty": "", "eof": "{", "missing": `{"state":"quiescent"}`, "duplicate": prefix + `"state":"running","state":"running"}`, "extra": prefix + `"state":"running","extra":1}`, "null": prefix + `"state":null}`, "quiescent": prefix + `"state":"quiescent"}`, "nestedduplicate": prefix + `"state":"quiescent","terminal":` + strings.Replace(terminal.String(), `"v":1`, `"v":1,"v":1`, 1) + `}`, "mismatch": strings.Replace(prefix, hlsClientIdentity.ExecutionID, strings.Repeat("3", 32), 1) + `"state":"running"}`, "oversized": strings.Repeat("x", 4097), "terminalnonquiescent": prefix + `"state":"running","terminal":` + terminal.String() + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			s, err := c.LookupHLS(context.Background(), hlsClientIdentity)
			if err == nil || s.State != "unknown" || s.ConfirmedFor(hlsClientIdentity) {
				t.Fatal(s, err)
			}
		})
	}
	for _, status := range []int{404, 302, 500} {
		c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://127.0.0.1:1/")
			w.WriteHeader(status)
		}))
		s, err := c.CancelHLS(context.Background(), hlsClientIdentity)
		if err == nil || s.State != "unknown" {
			t.Fatal(s, err)
		}
	}
}

func TestHLSClientStreamUnknownAndDeadline(t *testing.T) {
	for _, kind := range []string{"eof", "header", "deadline", "callback"} {
		t.Run(kind, func(t *testing.T) {
			var posts atomic.Int32
			c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/capabilities" {
					json.NewEncoder(w).Encode(map[string]any{"v": 1, "isolation": HLSControllerRole, "controller_epoch": hlsClientIdentity.ControllerEpoch})
					return
				}
				posts.Add(1)
				w.Header().Set("X-Video-Controller-Epoch", hlsClientIdentity.ControllerEpoch)
				if kind != "header" {
					w.Header().Set("X-Video-Execution-ID", hlsClientIdentity.ExecutionID)
				}
				EncodeHLSEvent(w, HLSEvent{V: 1, Type: "heartbeat"})
				w.(http.Flusher).Flush()
				if kind == "deadline" {
					<-r.Context().Done()
				}
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := c.StreamHLS(ctx, hlsClientIdentity, "avi", 1024, func(HLSEvent) error {
				if kind == "callback" {
					return failure("invalid_media")
				}
				return nil
			})
			if err == nil || posts.Load() != 1 {
				t.Fatal(err, posts.Load())
			}
		})
	}
}

func TestHLSClientCapabilitiesStrict(t *testing.T) {
	for _, b := range []string{`{"v":1,"isolation":"` + HLSControllerRole + `","controller_epoch":"` + hlsClientIdentity.ControllerEpoch + `","v":1}`, `{"v":1,"isolation":"` + HLSControllerRole + `","controller_epoch":"UPPER"}`, `{"v":1,"isolation":"` + HLSControllerRole + `","controller_epoch":"` + hlsClientIdentity.ControllerEpoch + `","extra":true}`, strings.Repeat("x", 2049)} {
		c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(b)) }))
		if _, err := c.HLSReady(context.Background()); err == nil {
			t.Fatal("accepted", b)
		}
	}
}

func TestHLSClientRestartFencesBeforePOST(t *testing.T) {
	var posts atomic.Int32
	c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		json.NewEncoder(w).Encode(map[string]any{"v": 1, "isolation": HLSControllerRole, "controller_epoch": strings.Repeat("3", 32)})
	}))
	if _, err := c.StreamHLS(context.Background(), hlsClientIdentity, "mkv", 1024, nil); err == nil || posts.Load() != 0 {
		t.Fatal("restart was not fenced", err, posts.Load())
	}
}

func TestHLSClientControlIndependentFromStreamCancellation(t *testing.T) {
	c := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"v":1,"job_id":"` + testID + `","controller_epoch":"` + hlsClientIdentity.ControllerEpoch + `","execution_id":"` + hlsClientIdentity.ExecutionID + `","state":"stopping"}`))
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := c.CancelHLS(ctx, hlsClientIdentity)
	if err == nil || s.State != "unknown" {
		t.Fatal("cancelled caller qualified", s, err)
	}
	control, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	s, err = c.CancelHLS(control, hlsClientIdentity)
	if err != nil || s.State != "stopping" || s.ConfirmedFor(hlsClientIdentity) {
		t.Fatal("separate control failed", s, err)
	}
}
