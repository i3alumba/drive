//go:build linux

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"remote-drive/api/internal/torrent"
)

// This fixture uses actual browser JS and HTTP handlers with an owned loopback
// Auth fixture and temporary storage, never production credentials or NAS data.
func TestChunkedUploadBrowserActualAPI(t *testing.T) {
	if os.Getenv("DRIVE_UPLOAD_BROWSER_INTEGRATION") != "1" {
		t.Skip("set DRIVE_UPLOAD_BROWSER_INTEGRATION=1 after frontend build; requires sandbox-capable Chrome and Node")
	}
	frontend, err := filepath.Abs("../../../frontend")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(frontend, "dist", "index.html")); err != nil {
		t.Fatal("build frontend before enabling actual API browser fixture")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-browser" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"id":42,"username":"owned-browser-fixture","services":{"drive":true}}`)
	}))
	defer auth.Close()
	store := testTransferStore()
	work := t.TempDir()
	torrents := torrent.NewManager(store, filepath.Join(work, "torrents"), time.Minute)
	app := New(store, torrents, NewAuthValidator(auth.URL))
	if err := app.EnableUploads(UploadOptions{Root: filepath.Join(work, "sessions"), MaxFileBytes: 128 << 20, BudgetBytes: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	defer app.CloseUploads()
	mux := http.NewServeMux()
	mux.Handle("/api/", app.Routes())
	mux.Handle("/", http.FileServer(http.Dir(filepath.Join(frontend, "dist"))))
	api := httptest.NewServer(mux)
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, filepath.Join(frontend, "tests", "helpers", "chunked-live.mjs"), api.URL)
	// Chromium's Unix singleton socket is limited to 108 bytes. Go test's
	// descriptive TempDir name plus a browser profile can exceed that limit.
	browserTemp, err := os.MkdirTemp("/tmp", "duc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(browserTemp)
	command.Env = append(os.Environ(), "TMPDIR="+browserTemp)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("owned loopback browser failed: %v; sanitized evidence: %s", err, output)
	}
	var proof struct {
		OK      bool  `json:"ok"`
		Sandbox bool  `json:"sandbox"`
		Bytes   int64 `json:"bytes"`
		Resume  bool  `json:"pause_reload_resume"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &proof); err != nil || !proof.OK || !proof.Sandbox || !proof.Resume || proof.Bytes != 101<<20+3 {
		t.Fatal("actual browser evidence missing or invalid")
	}
	store.mu.Lock()
	data := store.data["users/42/large-fixture.bin"]
	actual := sha256.Sum256(data)
	length := len(data)
	store.mu.Unlock()
	if int64(length) != proof.Bytes {
		t.Fatal("actual browser upload published wrong length")
	}
	expected := sha256.New()
	block := bytes.Repeat([]byte{0x61}, 1<<20)
	for remaining := proof.Bytes; remaining > 0; {
		n := int64(len(block))
		if n > remaining {
			n = remaining
		}
		expected.Write(block[:n])
		remaining -= n
	}
	if hex.EncodeToString(actual[:]) != hex.EncodeToString(expected.Sum(nil)) {
		t.Fatal("actual browser upload failed whole-file digest check")
	}
	t.Logf("actual browser + API: %d bytes, bounded parts, pause/reload/resume and exact SHA-256; sandbox enabled", proof.Bytes)
}
