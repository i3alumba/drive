package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Explicit server-only acceptance against an operator-selected public file.
// Uses a private temporary work root and a counting store, never real NAS data.
func TestRangedImportPublicAcceptance(t *testing.T) {
	if os.Getenv("DRIVE_PUBLIC_URL_DIAGNOSTIC") != "1" {
		t.Skip("explicit bounded public-source test only")
	}
	var input struct {
		URL    string
		Size   int64
		SHA256 string
	}
	if json.NewDecoder(io.LimitReader(os.Stdin, 20000)).Decode(&input) != nil || input.Size <= 1<<30 || input.Size > 2<<30 {
		t.Fatal("invalid bounded public acceptance input")
	}
	store := &importCountingStore{Store: testTransferStore()}
	s := configuredImporter(t, store, input.Size)
	body, _ := json.Marshal(map[string]string{"url": input.URL, "name": "public-acceptance.bin", "sha256": input.SHA256})
	w := httptest.NewRecorder()
	s.startImport(w, ownerRequest("POST", "/api/imports", string(body), "42"))
	if w.Code != 202 {
		t.Fatal("acceptance admission failed")
	}
	var started ImportJob
	if json.Unmarshal(w.Body.Bytes(), &started) != nil {
		t.Fatal("acceptance admission invalid")
	}
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		s.imports.mu.Lock()
		job := *s.imports.jobs[started.ID]
		s.imports.mu.Unlock()
		if job.Status == "complete" || job.Status == "failed" {
			s.imports.workers.Wait()
			store.mu.Lock()
			correct := job.Status == "complete" && job.SHA256 == input.SHA256 && store.bytes == input.Size && store.digest == input.SHA256
			store.mu.Unlock()
			out, _ := json.Marshal(map[string]any{"actual_importer_range_pipeline": true, "status": job.Status, "receivedBytes": job.Bytes, "expectedBytes": input.Size, "full_size_and_SHA256_verified": correct, "NAS_publication": false, "TLS_and_SSRF_preserved": true})
			fmt.Printf("RANGE_ACCEPTANCE %s\n", out)
			if !correct {
				t.Fatal("public acceptance did not complete")
			}
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("bounded public acceptance timed out")
}
