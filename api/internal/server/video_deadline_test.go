//go:build linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"remote-drive/api/internal/videoworker"
)

func TestVideoPublicationDeadlineDeniesPrefixWithoutAbandoningIO(t *testing.T) {
	reader := newGatedVideoReader()
	t.Cleanup(reader.unblock)
	var fixture *videoFixture
	fixture = newVideoFixture(t, func(root string) videoProcessor {
		return videoProcessFunc(func(ctx context.Context, identity videoworker.HLSIdentity, _ string, limit int64, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
			return emitVideoFixture(ctx, root, identity, limit, emit, func() error {
				reader.data = videoSegmentFixtures[1]
				return fixture.server.video.copyVideoAsset(ctx, identity.JobID, "segment-000001.m4s", reader, int64(len(reader.data)), videoTestDigest(reader.data), func([]byte) error { return errors.New("expired publication reached validator") })
			})
		})
	})
	created := fixture.create(t)
	waitVideoSignal(t, reader.blocked)
	manager := fixture.server.video
	manager.mu.Lock()
	execution := manager.jobs[created.ID].execution
	manager.mu.Unlock()
	waitVideoSignal(t, execution.Done())
	base := "/api/video-previews/" + created.ID
	status := fixture.request("GET", base, "owner", "", "")
	var snapshot videoPreviewStatus
	if status.Code != 200 || json.Unmarshal(status.Body.Bytes(), &snapshot) != nil || snapshot.Status != "stopping" {
		t.Fatal("deadline not visible while IO blocked", status.Code, status.Body)
	}
	for _, asset := range []string{videoworker.HLSPlaylistName, videoworker.HLSInitName, "segment-000000.m4s"} {
		for _, method := range []string{"GET", "HEAD"} {
			if response := fixture.request(method, base+"/"+asset, "owner", "", "bytes=0-3"); response.Code != 409 || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("expired prefix still playable", asset, method, response.Code)
			}
		}
	}
	manager.mu.Lock()
	busy, running := manager.busy, manager.jobs[created.ID].running
	manager.mu.Unlock()
	if !busy || !running || reader.reads.Load() != 2 {
		t.Fatal("blocked IO abandoned or admission released")
	}
	if _, err := os.Stat(filepath.Join(fixture.root, videoFenceName)); err != nil {
		t.Fatal("active execution fence removed", err)
	}
	if response := fixture.request("POST", "/api/video-previews", "owner", `{"path":"movie.mkv"}`, ""); response.Code != 429 {
		t.Fatal("timed-out blocked publication admitted another job", response.Code)
	}
	reader.unblock()
	waitVideoState(t, manager, created.ID, "failed")
	manager.mu.Lock()
	busy = manager.busy
	manager.mu.Unlock()
	if busy || reader.reads.Load() != 2 {
		t.Fatal("ordinary reconciliation failed or source drained")
	}
	if _, err := os.Stat(filepath.Join(fixture.root, videoFenceName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed reconciliation left fence", err)
	}
}
