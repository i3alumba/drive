//go:build linux

package videoworker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDevelopmentHLSHandlerPermanentFence(t *testing.T) {
	root, _ := newJob(t)
	raw, err := NewDevelopmentHLSHandler(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := raw.(*developmentHLSHandler)
	body := fmtHLSDevelopmentRequest(h.epoch)
	for _, bad := range []string{body + "{}", strings.Replace(body, `"format":"mkv"`, `"format":"mkv","format":"avi"`, 1), strings.Replace(body, h.epoch, strings.Repeat("0", 32), 1), strings.Replace(body, `"format":"mkv"`, `"format":"mp4"`, 1)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/jobs/"+testID+"/events", strings.NewReader(bad)))
		if w.Code != 400 || h.accepted {
			t.Fatal(w.Code, h.accepted, w.Body)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/jobs/"+testID+"/events", strings.NewReader(body)))
	if w.Code != 200 || !h.accepted {
		t.Fatal(w.Code, w.Body)
	}
	var events []HLSEvent
	seq, err := ReadHLSEvents(w.Body, 1<<20, HLSIdentity{JobID: testID, ControllerEpoch: h.epoch, ExecutionID: hlsClientIdentity.ExecutionID}, func(e HLSEvent) error { events = append(events, e); return nil })
	if err == nil || seq.Terminal || len(events) != 1 || events[0].Type != "conversion_end" || events[0].Outcome != "failed" {
		t.Fatal(events, seq, err)
	}
	for _, method := range []string{"GET", "DELETE", "DELETE"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/v1/jobs/"+testID+"?controller_epoch="+h.epoch+"&execution_id="+hlsClientIdentity.ExecutionID, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "terminal") || !strings.Contains(w.Body.String(), `"state":"unknown"`) {
			t.Fatal(w.Code, w.Body)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/jobs/"+strings.Repeat("3", 32)+"/events", strings.NewReader(body)))
	if w.Code != 429 {
		t.Fatal("admission reopened", w.Code, w.Body)
	}
}

func fmtHLSDevelopmentRequest(epoch string) string {
	b, _ := json.Marshal(hlsRequest{Format: "mkv", MaxOutputBytes: 1 << 20, ControllerEpoch: epoch, ExecutionID: hlsClientIdentity.ExecutionID})
	return string(b)
}

func TestDevelopmentHLSHandlerRawProgressiveCancel(t *testing.T) {
	hlsRequireIntegration(t)
	root, dir := newJob(t)
	hlsFixture(t, dir, "remux", 9)
	wrapper := filepath.Join(t.TempDir(), "realtime-ffmpeg")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /usr/bin/ffmpeg -re \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := NewDevelopmentHLSHandler(root, Options{FFmpeg: wrapper})
	if err != nil {
		t.Fatal(err)
	}
	h := raw.(*developmentHLSHandler)
	c := hlsUnixClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://worker/v1/jobs/"+testID+"/events", bytes.NewBufferString(fmtHLSDevelopmentRequest(h.epoch)))
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	identity := HLSIdentity{JobID: testID, ControllerEpoch: h.epoch, ExecutionID: hlsClientIdentity.ExecutionID}
	gotPrefix := false
	gotEnd := false
	seq, err := ReadHLSEvents(resp.Body, 1<<20, identity, func(e HLSEvent) error {
		if e.Type == "segment" && !gotPrefix {
			gotPrefix = true
			playlist, err := os.ReadFile(filepath.Join(dir, HLSDirectoryName, HLSPlaylistName))
			if err != nil {
				return err
			}
			if strings.Contains(string(playlist), "ENDLIST") {
				t.Error("prefix arrived after completion")
			}
			control, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s, err := c.LookupHLS(control, identity)
			if err != nil || s.State != "unknown" || s.ConfirmedFor(identity) {
				t.Error(s, err)
			}
			for range 2 {
				s, err = c.CancelHLS(control, identity)
				if err != nil || s.State != "unknown" || s.ConfirmedFor(identity) {
					t.Error(s, err)
				}
			}
		}
		if e.Type == "conversion_end" {
			gotEnd = true
			if e.Outcome != "cancelled" {
				t.Error("cancel outcome", e)
			}
		}
		return nil
	})
	if err == nil || seq.Terminal || !gotPrefix || !gotEnd {
		t.Fatal(seq, err, gotPrefix, gotEnd)
	}
	req, _ = http.NewRequestWithContext(ctx, "POST", "http://worker/v1/jobs/"+testID+"/events", strings.NewReader(fmtHLSDevelopmentRequest(h.epoch)))
	resp2, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 429 {
		t.Fatal("fence reopened", resp2.StatusCode)
	}
}
