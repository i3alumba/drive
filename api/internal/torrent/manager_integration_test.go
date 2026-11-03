package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These are our own tiny legal torrents, generated at runtime. Real aria2
// downloads from a loopback BEP-19 web seed; no public swarm/tracker is needed.
func bencode(value any) []byte {
	switch v := value.(type) {
	case string:
		return append([]byte(strconv.Itoa(len(v))+":"), []byte(v)...)
	case int:
		return []byte(fmt.Sprintf("i%de", v))
	case []any:
		out := []byte("l")
		for _, item := range v {
			out = append(out, bencode(item)...)
		}
		return append(out, 'e')
	case map[string]any:
		keys := []string{}
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := []byte("d")
		for _, key := range keys {
			out = append(out, bencode(key)...)
			out = append(out, bencode(v[key])...)
		}
		return append(out, 'e')
	default:
		panic("unsupported fixture bencode value")
	}
}

type fixtureUploader struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (f *fixtureUploader) PutObject(ctx context.Context, key string, reader io.Reader, size int64, kind string) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[key] = body
	return nil
}

func TestRealAria2SimpleTorrents(t *testing.T) {
	if os.Getenv("DRIVE_TORRENT_INTEGRATION") != "1" {
		t.Skip("set DRIVE_TORRENT_INTEGRATION=1 with aria2c on PATH for real loopback downloads")
	}
	if _, err := exec.LookPath("aria2c"); err != nil {
		t.Fatal("aria2c is required for requested integration acceptance")
	}
	cases := []struct {
		name  string
		files map[string]string
		multi bool
	}{
		{"tiny.txt", map[string]string{"tiny.txt": "hello from a generated torrent\n"}, false},
		{"bundle", map[string]string{"bundle/readme.txt": "a multi-file torrent\n", "bundle/sub/notes.txt": "nested payload\n"}, true},
		{"unicode.txt", map[string]string{"unicode.txt": strings.Repeat("Тестовая раздача — owned fixture\n", 1400)}, false},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			seedDir := t.TempDir()
			keys := []string{}
			for name, body := range fixture.files {
				keys = append(keys, name)
				target := filepath.Join(seedDir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			sort.Strings(keys)
			seed := httptest.NewServer(http.FileServer(http.Dir(seedDir)))
			defer seed.Close()
			info := map[string]any{"name": fixture.name, "piece length": 16384}
			var combined []byte
			if fixture.multi {
				var entries []any
				for _, key := range keys {
					relative := strings.TrimPrefix(key, fixture.name+"/")
					parts := []any{}
					for _, part := range strings.Split(relative, "/") {
						parts = append(parts, part)
					}
					entries = append(entries, map[string]any{"length": len(fixture.files[key]), "path": parts})
					combined = append(combined, []byte(fixture.files[key])...)
				}
				info["files"] = entries
			} else {
				combined = []byte(fixture.files[fixture.name])
				info["length"] = len(combined)
			}
			var pieces []byte
			for offset := 0; offset < len(combined); offset += 16384 {
				end := offset + 16384
				if end > len(combined) {
					end = len(combined)
				}
				sum := sha1.Sum(combined[offset:end])
				pieces = append(pieces, sum[:]...)
			}
			info["pieces"] = string(pieces)
			metadata := bencode(map[string]any{"info": info, "url-list": []any{seed.URL + "/"}})
			file := filepath.Join(t.TempDir(), "fixture.torrent")
			if err := os.WriteFile(file, metadata, 0600); err != nil {
				t.Fatal(err)
			}
			uploader := &fixtureUploader{files: map[string][]byte{}}
			manager := NewManager(uploader, t.TempDir(), 25*time.Second)
			manager.aria2Args = []string{"--no-conf=true", "--enable-dht=false", "--enable-dht6=false", "--bt-enable-lpd=false", "--enable-peer-exchange=false", "--disable-ipv6=true", "--bt-exclude-tracker=*", "--listen-port=49152-65535", "--interface=127.0.0.1"}
			started := manager.Start(file, fixture.name+".torrent", "users/42/downloads/")
			deadline := time.Now().Add(28 * time.Second)
			for {
				job, _ := manager.Get(started.ID)
				if job.Status == StatusFailed {
					t.Fatal(job.Error)
				}
				if job.Status == StatusComplete {
					break
				}
				if time.Now().After(deadline) {
					_, _ = manager.Cancel(started.ID)
					t.Fatal("fixture torrent timed out")
				}
				time.Sleep(10 * time.Millisecond)
			}
			uploader.mu.Lock()
			defer uploader.mu.Unlock()
			if len(uploader.files) != len(fixture.files) {
				t.Fatalf("unexpected uploaded file count %d", len(uploader.files))
			}
			for key, wanted := range fixture.files {
				if !bytes.Equal(uploader.files["users/42/downloads/"+key], []byte(wanted)) {
					t.Fatal("payload/path mismatch", key)
				}
			}
			// Completion is published before deferred temporary cleanup finishes.
			for i := 0; i < 100; i++ {
				_, err := os.Stat(file)
				if os.IsNotExist(err) {
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("torrent metadata was not cleaned")
		})
	}
}
