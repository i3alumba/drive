package torrent

import (
	"crypto/sha1"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRealAria2PauseResumeAndCancel(t *testing.T) {
	if os.Getenv("DRIVE_TORRENT_INTEGRATION") != "1" {
		t.Skip("enable real aria2 integration explicitly")
	}
	for _, action := range []string{"pause-resume", "cancel"} {
		t.Run(action, func(t *testing.T) {
			payload := "owned control fixture\n"
			gate := make(chan struct{})
			requested := make(chan struct{}, 1)
			var release sync.Once
			seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case requested <- struct{}{}:
				default:
				}
				select {
				case <-gate:
					http.ServeContent(w, r, "control.txt", time.Time{}, strings.NewReader(payload))
				case <-r.Context().Done():
				}
			}))
			defer seed.Close()
			defer release.Do(func() { close(gate) })
			sum := sha1.Sum([]byte(payload))
			metadata := bencode(map[string]any{"info": map[string]any{"name": "control.txt", "length": len(payload), "piece length": 16384, "pieces": string(sum[:])}, "url-list": []any{seed.URL + "/"}})
			file := filepath.Join(t.TempDir(), "control.torrent")
			if err := os.WriteFile(file, metadata, 0600); err != nil {
				t.Fatal(err)
			}
			uploader := &fixtureUploader{files: map[string][]byte{}}
			work := t.TempDir()
			m := NewManager(uploader, work, 25*time.Second)
			m.aria2Args = []string{"--no-conf=true", "--enable-dht=false", "--enable-dht6=false", "--bt-enable-lpd=false", "--enable-peer-exchange=false", "--disable-ipv6=true", "--bt-exclude-tracker=*", "--listen-port=49152-65535", "--interface=127.0.0.1"}
			job := m.Start(file, "control.torrent", "users/42/")
			select {
			case <-requested:
			case <-time.After(5 * time.Second):
				_, _ = m.Cancel(job.ID)
				t.Fatal("no real webseed request")
			}
			if action == "cancel" {
				if _, err := m.Cancel(job.ID); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 500; i++ {
					_, err := os.Stat(file)
					if os.IsNotExist(err) {
						uploader.mu.Lock()
						defer uploader.mu.Unlock()
						if len(uploader.files) != 0 {
							t.Fatal("cancelled torrent uploaded data")
						}
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("cancel did not clean own torrent metadata")
				return
			}
			if _, err := m.Pause(job.ID); err != nil {
				t.Fatal(err)
			}
			paused := false
			for i := 0; i < 500; i++ {
				snapshot, _ := m.Get(job.ID)
				if snapshot.Status == StatusPaused {
					paused = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !paused {
				_, _ = m.Cancel(job.ID)
				t.Fatal("pause did not settle")
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("pause removed resumable torrent metadata")
			}
			release.Do(func() { close(gate) })
			if _, err := m.Resume(job.ID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 1000; i++ {
				snapshot, _ := m.Get(job.ID)
				if snapshot.Status == StatusFailed {
					t.Fatal(snapshot.Error)
				}
				if snapshot.Status == StatusComplete {
					uploader.mu.Lock()
					defer uploader.mu.Unlock()
					if string(uploader.files["users/42/control.txt"]) != payload {
						t.Fatal("resume did not preserve bytes")
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			_, _ = m.Cancel(job.ID)
			t.Fatal("resumed torrent did not finish")
		})
	}
}
