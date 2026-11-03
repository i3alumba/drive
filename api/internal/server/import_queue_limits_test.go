//go:build linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestImportQueueSnapshotAndAggregateBoundsBeforeWrite(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	q, _ := queueSubmit(t, s, strings.Repeat("3", 32), "42", queueDirect("a"))
	q.mu.Lock()
	revision := q.state.Revision
	next := cloneQueueState(q.state)
	next.Targets["large"] = strings.Repeat("x", queueSnapshotLimit)
	if err := s.commitQueue(context.Background(), q, next); !errors.Is(err, errQueueCapacity) || q.frozen || q.state.Revision != revision {
		t.Fatal("oversized snapshot replaced committed state", err)
	}
	next = cloneQueueState(q.state)
	next.Items[0].ErrorCode = strings.Repeat("x", 100)
	s.queues.mu.Lock()
	s.queues.metadataBytes = queueMetadataLimit
	s.queues.mu.Unlock()
	if err := s.commitQueue(context.Background(), q, next); !errors.Is(err, errQueueCapacity) {
		t.Fatal(err)
	}
	q.mu.Unlock()
	var persisted queueState
	_, err := s.queueRead(context.Background(), queueBase(q.manifest)+"/state.json", &persisted, queueSnapshotLimit)
	if err != nil || persisted.Revision != revision {
		t.Fatal("capacity failure wrote state")
	}
}
func TestImportQueueSkipVerifiedAndCompletedReceiptNeverRedownloads(t *testing.T) {
	for _, expected := range []string{queueDigest("old"), queueDigest("bad"), ""} {
		t.Run(expected, func(t *testing.T) {
			disk := newLocalQueueStore(t)
			s := queueTestServer(t, disk, "")
			defer s.CloseImports()
			queueStopScheduler(s)
			queueFakeBytes(s, "old")
			_ = disk.PutObject(context.Background(), "users/42/target/a", strings.NewReader("old"), 3, "")
			src := queueDirect("a")
			src[0].SHA256 = expected
			body, _ := json.Marshal(queueRequest{SubmissionID: strings.Repeat("4", 32), Path: "target", Existing: "skip_verified", Sources: src})
			w := httptest.NewRecorder()
			s.createImportQueue(w, ownerRequest("POST", "/api/import-queues", string(body), "42"))
			if w.Code != 202 {
				t.Fatal(w.Code)
			}
			q := s.queues.queues[queueIdentity("42", strings.Repeat("4", 32))]
			s.dispatchQueueWork()
			s.imports.workers.Wait()
			if expected == queueDigest("old") {
				it := queueWait(t, q, "complete")
				if !it.Reused {
					t.Fatal("not verified reuse")
				}
				_ = disk.DeleteObject(context.Background(), "users/42/target/a")
				s.dispatchQueueWork()
				if len(s.imports.slots) != 0 {
					t.Fatal("completed receipt redownloaded deleted output")
				}
			} else {
				it := queueWait(t, q, "failed")
				if it.ErrorCode != "destination_conflict" {
					t.Fatal(it)
				}
			}
		})
	}
}
func TestImportQueueProviderChangedSHAAnd503RetryRetainDeadline(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	var calls atomic.Int32
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"name":"file","type":"file","size":4,"sha256":"` + queueDigest("data") + `"}`
		status := 200
		if calls.Add(1) == 1 {
			status = 503
			body = "unavailable"
		} else if calls.Load() > 2 {
			body = `{"name":"file","type":"file","size":4,"sha256":"` + queueDigest("evil") + `"}`
		}
		return &http.Response{StatusCode: status, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	q, _ := queueSubmit(t, s, strings.Repeat("5", 32), "42", []queueSource{{Type: "yandex", URL: "https://disk.yandex.ru/d/public"}})
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	q.mu.Lock()
	if len(q.state.Walk) != 1 || q.state.Walk[0].Attempts != 1 || !q.state.Walk[0].NextAttemptAt.After(time.Now()) {
		t.Fatal("provider retry was not persisted")
	}
	deadline := q.state.Walk[0].Deadline
	next := cloneQueueState(q.state)
	next.Walk[0].NextAttemptAt = time.Time{}
	_ = s.commitQueue(context.Background(), q, next)
	q.mu.Unlock()
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	it := queueWait(t, q, "failed")
	if it.ErrorCode != "source_changed" || it.Attempts != 1 {
		t.Fatal(it)
	}
	if deadline.After(q.manifest.ExpiresAt) || deadline.After(q.manifest.AcceptedAt.Add(s.imports.options.Timeout+time.Second)) {
		t.Fatal("inventory deadline not capped")
	}
}
func TestImportQueueFailureBackoffDoesNotBlockOtherSources(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		code := 200
		if strings.Contains(r.URL.Path, "bad") {
			code = 503
		}
		return &http.Response{StatusCode: code, ContentLength: 4, Body: io.NopCloser(strings.NewReader("data")), Header: make(http.Header), Request: r}, nil
	})
	sources := append(queueDirect("bad"), queueDirect("good")...)
	q, _ := queueSubmit(t, s, strings.Repeat("6", 32), "42", sources)
	for attempt := 0; attempt < 3; attempt++ {
		s.dispatchQueueWork()
		s.imports.workers.Wait()
		s.dispatchQueueWork()
		s.imports.workers.Wait()
		q.mu.Lock()
		first, second := q.state.Items[0], q.state.Items[1]
		if second.State != "complete" {
			t.Fatal("backoff blocked independent item", second.State)
		}
		if attempt < 2 {
			if first.State != "retry_wait" || first.NextAttemptAt == nil || !first.NextAttemptAt.After(time.Now()) {
				t.Fatal(first)
			}
			next := cloneQueueState(q.state)
			next.Items[0].NextAttemptAt = nil
			_ = s.commitQueue(context.Background(), q, next)
		}
		q.mu.Unlock()
	}
	it := queueWait(t, q, "failed")
	if it.Attempts != 3 || it.TotalAttempts != 3 {
		t.Fatal(it)
	}
}

type queueBlockingBody struct {
	ctx      context.Context
	entered  chan<- struct{}
	released <-chan struct{}
	once     bool
}

func (b *queueBlockingBody) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		b.entered <- struct{}{}
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.released:
		return 0, io.EOF
	}
}
func (b *queueBlockingBody) Close() error { return nil }
func TestImportQueueLegacySharedSlotsBytesAndOwnerFairness(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	entered := make(chan struct{}, 8)
	released := make(chan struct{})
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 1 << 20, Body: &queueBlockingBody{ctx: r.Context(), entered: entered, released: released}, Header: make(http.Header), Request: r}, nil
	})
	for _, name := range []string{"legacy-a", "legacy-b"} {
		w := httptest.NewRecorder()
		s.startImport(w, ownerRequest("POST", "/api/imports", `{"url":"http://source.example/`+name+`","name":"`+name+`"}`, "42"))
		if w.Code != 202 {
			t.Fatal(w.Code)
		}
	}
	q42, _ := queueSubmit(t, s, strings.Repeat("7", 32), "42", queueDirect("queue42"))
	q43, _ := queueSubmit(t, s, strings.Repeat("8", 32), "43", queueDirect("queue43"))
	q44, _ := queueSubmit(t, s, strings.Repeat("9", 32), "44", queueDirect("queue44"))
	s.dispatchQueueWork()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("shared slots did not start")
		}
	}
	s.imports.mu.Lock()
	if len(s.imports.slots) != 4 || s.imports.ownerActive["42"] != 2 || s.imports.reserved != 4<<20 {
		t.Fatal("legacy and queues not sharing capacity", s.imports.ownerActive, s.imports.reserved)
	}
	s.imports.mu.Unlock()
	q42.mu.Lock()
	state := q42.state.Items[0].State
	q42.mu.Unlock()
	if state != "queued" {
		t.Fatal("owner limit bypassed", state)
	}
	for i := 0; i < 20; i++ {
		s.dispatchQueueWork()
	}
	if len(s.imports.slots) != 4 {
		t.Fatal("replacement workers spawned")
	}
	// Cancel legacy to free slots; the previously unavailable owner becomes eligible.
	s.imports.mu.Lock()
	jobs := []string{}
	for id := range s.imports.jobs {
		jobs = append(jobs, id)
	}
	s.imports.mu.Unlock()
	for _, id := range jobs {
		w := httptest.NewRecorder()
		s.cancelImport(w, ownerRequest("DELETE", "/api/imports/"+id, "", "42"))
	}
	deadline := time.Now().Add(time.Second)
	for len(s.imports.slots) > 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s.dispatchQueueWork()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("eligible owner starved")
	}
	q43.mu.Lock()
	a := q43.state.Items[0].State
	q43.mu.Unlock()
	q44.mu.Lock()
	b := q44.state.Items[0].State
	q44.mu.Unlock()
	if a != "downloading" || b != "downloading" {
		t.Fatal("unavailable owner blocked others", a, b)
	}
	close(released)
	s.imports.workers.Wait()
}
func TestImportQueuePauseActiveFinishCancelSavingAndDestinationSerialization(t *testing.T) {
	disk := newLocalQueueStore(t)
	fault := &queueFaultStore{Store: disk}
	s := queueTestServer(t, fault, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	saving := make(chan struct{})
	release := make(chan struct{})
	// Block only publication, not metadata: cancellation observes the durable saving state.
	blocked := &queueBlockingPutStore{Store: fault, entered: saving, release: release}
	s.store = blocked
	queueFakeBytes(s, "data")
	q, _ := queueSubmit(t, s, strings.Repeat("a", 32), "42", queueDirect("a"))
	duplicate, _ := queueSubmit(t, s, strings.Repeat("b", 32), "42", queueDirect("a"))
	s.dispatchQueueWork()
	select {
	case <-saving:
	case <-time.After(time.Second):
		t.Fatal("save did not start")
	}
	s.dispatchQueueWork()
	duplicate.mu.Lock()
	state := duplicate.state.Items[0].State
	duplicate.mu.Unlock()
	if state != "queued" {
		t.Fatal("destination was not serialized", state)
	}
	w := httptest.NewRecorder()
	r := ownerRequest("POST", "/pause", "", "42")
	r.SetPathValue("id", q.manifest.ID)
	r.SetPathValue("action", "pause")
	s.controlImportQueue(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	r = ownerRequest("DELETE", "/item", "", "42")
	r.SetPathValue("id", q.manifest.ID)
	r.SetPathValue("itemId", q.state.Items[0].ID)
	s.controlQueueItem(w, r)
	if w.Code != 409 {
		t.Fatal("saving item cancelled", w.Code)
	}
	w = httptest.NewRecorder()
	r = ownerRequest("POST", "/cancel", "", "42")
	r.SetPathValue("id", q.manifest.ID)
	r.SetPathValue("action", "cancel")
	s.controlImportQueue(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var summary QueueSummary
	_ = json.Unmarshal(w.Body.Bytes(), &summary)
	if summary.SavingItems != 1 || summary.State != "cancelling" {
		t.Fatal(summary)
	}
	close(release)
	s.imports.workers.Wait()
	queueWait(t, q, "complete")
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	queueWait(t, duplicate, "failed")
}

type queueBlockingPutStore struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (s *queueBlockingPutStore) PutObject(ctx context.Context, key string, r io.Reader, size int64, kind string) error {
	if strings.HasPrefix(key, "users/") {
		close(s.entered)
		<-s.release
	}
	return s.Store.PutObject(ctx, key, r, size, kind)
}
