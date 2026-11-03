package torrent

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestParseAria2ConnectedPeers(t *testing.T) {
	cases := []struct {
		line  string
		peers int
	}{
		{"[#123 100MiB/756MiB(13%) CN:53 SD:35 DL:8MiB ETA:1m]", 53},
		{"[#123 0B/756MiB(0%) CN:0 SD:0 DL:0B]", 0},
		{"CN:7]", 7},
		{"[#123 1MiB/2MiB(50%) DL:1MiB]", 0},
		{"CN:-3 DL:1MiB", 0},
		{"CN:999999999999999999999 DL:1MiB", 0},
		{"CN:3invalid DL:1MiB", 0},
	}
	for _, input := range cases {
		if got := parseAria2Peers(input.line); got != input.peers {
			t.Errorf("connected peers: got %d, want %d", got, input.peers)
		}
	}
}

func TestAria2TelemetryReachesPublicJobAndIsIsolated(t *testing.T) {
	manager := NewManager(nil, t.TempDir(), 0)
	manager.jobs["first"] = &Job{ID: "first", Status: StatusDownloading}
	manager.jobs["second"] = &Job{ID: "second", Status: StatusDownloading}
	// Controlled aria2-format stdout: no network or torrent file is involved.
	command := exec.CommandContext(context.Background(), "sh", "-c", "printf '%s\\n' '[#123 100MiB/756MiB(13%) CN:53 SD:35 DL:8MiB ETA:1m]'")
	if _, err := manager.runAria2(context.Background(), "first", command); err != nil {
		t.Fatal(err)
	}
	first, _ := manager.Get("first")
	second, _ := manager.Get("second")
	if first.ConnectedPeers != 53 || first.DownloadSpeedBytesPerSecond != 8<<20 || first.ETASeconds != 60 || second.ConnectedPeers != 0 || second.DownloadSpeedBytesPerSecond != 0 {
		t.Fatal("per-job telemetry was lost or mixed")
	}
	data, err := json.Marshal(first)
	if err != nil || !strings.Contains(string(data), `"connectedPeers":53`) {
		t.Fatal("connected peers absent from API snapshot")
	}
}

func TestTorrentRatesResetOnStateChanges(t *testing.T) {
	for _, state := range []Status{StatusUploading, StatusComplete, StatusFailed} {
		manager := NewManager(nil, t.TempDir(), 0)
		manager.jobs["job"] = &Job{ID: "job", Status: StatusDownloading}
		manager.update("job", StatusDownloading, .5, 1024, 20, 7, "")
		manager.update("job", state, .5, 1024, 20, 7, "")
		job, _ := manager.Get("job")
		if job.ConnectedPeers != 0 || job.DownloadSpeedBytesPerSecond != 0 || job.ETASeconds != 0 {
			t.Fatal("stale rates after download state")
		}
	}
	for _, action := range []string{"pause", "cancel"} {
		manager := NewManager(nil, t.TempDir(), 0)
		manager.jobs["job"] = &Job{ID: "job", Status: StatusDownloading}
		manager.update("job", StatusDownloading, .5, 1024, 20, 7, "")
		if action == "pause" {
			_, _ = manager.Pause("job")
		} else {
			_, _ = manager.Cancel("job")
		}
		job, _ := manager.Get("job")
		if job.ConnectedPeers != 0 || job.DownloadSpeedBytesPerSecond != 0 || job.ETASeconds != 0 {
			t.Fatal("stale rates after control action")
		}
		manager.update("job", StatusDownloading, .9, 2048, 10, 9, "")
		job, _ = manager.Get("job")
		if job.ConnectedPeers != 0 || job.DownloadSpeedBytesPerSecond != 0 {
			t.Fatal("late progress revived stopped metrics")
		}
	}
}
