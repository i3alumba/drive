//go:build linux

package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportQueueRecoveryRetainsIntentAttemptsDeadlineAndExactScratchCleanup(t *testing.T) {
	for _, control := range []string{"running", "paused", "cancelled"} {
		t.Run(control, func(t *testing.T) {
			disk := newLocalQueueStore(t)
			root := filepath.Join(t.TempDir(), "imports")
			s := queueTestServer(t, disk, root)
			queueStopScheduler(s)
			q, _ := queueSubmit(t, s, strings.Repeat("c", 32), "42", queueDirect("a"))
			q.mu.Lock()
			next := cloneQueueState(q.state)
			next.Control = control
			it := &next.Items[0]
			it.Attempts = 1
			it.TotalAttempts = 1
			it.State = "downloading"
			if control == "cancelled" {
				it.State = "cancelled"
			}
			it.Deadline = time.Now().Add(20 * time.Second)
			deadline := it.Deadline
			it.Scratch = ".queue-" + q.manifest.ID + "-" + it.ID + "-1"
			scratch := it.Scratch
			if err := s.commitQueue(context.Background(), q, next); err != nil {
				t.Fatal(err)
			}
			q.mu.Unlock()
			for _, name := range []string{scratch, ".url-anonymous", "upload-foreign", "torrent-foreign"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("owned-local-fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s.CloseImports()
			recovered := queueTestServer(t, disk, root)
			defer recovered.CloseImports()
			queueStopScheduler(recovered)
			q = recovered.queues.queues[q.manifest.ID]
			q.mu.Lock()
			got := q.state.Items[0]
			gotControl := q.state.Control
			q.mu.Unlock()
			if gotControl != control || got.Attempts != 1 || got.TotalAttempts != 1 || !got.Deadline.Equal(deadline) {
				t.Fatal("recovery reset execution intent", gotControl, got)
			}
			if control != "cancelled" && got.State != "retry_wait" || control == "cancelled" && got.State != "cancelled" {
				t.Fatal(got.State)
			}
			if _, err := os.Stat(filepath.Join(root, scratch)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("exact owned scratch not reclaimed", err)
			}
			for _, name := range []string{".url-anonymous", "upload-foreign", "torrent-foreign"} {
				b, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || string(b) != "owned-local-fixture" {
					t.Fatal("foreign scratch touched", name, err)
				}
			}
			if control != "running" {
				recovered.dispatchQueueWork()
				if len(recovered.imports.slots) != 0 {
					t.Fatal("recovery resumed stopped intent")
				}
			}
		})
	}
}
func TestImportQueueExpiryReceiptAndPaginationAreBounded(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	id := strings.Repeat("d", 32)
	q, _ := queueSubmit(t, s, id, "42", queueDirect("a"))
	for _, query := range []string{"?limit=501", "?limit=0", "?offset=-1", "?offset=overflow"} {
		w := httptest.NewRecorder()
		s.listImportQueues(w, ownerRequest("GET", "/api/import-queues"+query, "", "42"))
		if w.Code != 400 {
			t.Fatal(query, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.listImportQueues(w, ownerRequest("GET", "/api/import-queues?offset=999&limit=500", "", "42"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"queues":[]`) {
		t.Fatal(w.Body.String())
	}
	q.mu.Lock()
	q.manifest.ExpiresAt = time.Now().Add(-time.Second)
	q.mu.Unlock()
	_, code := queueSubmit(t, s, id, "42", queueDirect("a"))
	if code != 410 {
		t.Fatal("expired idempotency receipt recreated", code)
	}
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	q.mu.Lock()
	summary := q.summary()
	state := q.state.Items[0].State
	q.mu.Unlock()
	if summary.State != "expired" || state != "expired" {
		t.Fatal(summary, state)
	}
}
func TestImportQueueEmptyFileKnownCompletedSize(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	queueFakeBytes(s, "")
	q, _ := queueSubmit(t, s, strings.Repeat("e", 32), "42", queueDirect("empty"))
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	queueWait(t, q, "complete")
	q.mu.Lock()
	summary := q.summary()
	q.mu.Unlock()
	if summary.UnknownSizeItems != 0 || summary.TotalBytes != 0 || summary.CompletedItems != 1 {
		t.Fatal(summary)
	}
}
