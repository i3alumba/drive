//go:build linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
)

func TestImportQueueInitialDestinationRejectionIsCorrectable(t *testing.T) {
	s := queueTestServer(t, newLocalQueueStore(t), "")
	defer s.CloseImports()
	queueStopScheduler(s)
	id := strings.Repeat("a", 32)
	input := queueRequest{SubmissionID: id, Path: "target", Sources: []queueSource{{Type: "direct", URL: "https://one.example/file.bin"}, {Type: "direct", URL: "https://two.example/file.bin"}}}
	submit := func() *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.createImportQueue(w, ownerRequest("POST", "/api/import-queues", string(body), "42"))
		return w
	}
	rejected := submit()
	if rejected.Code != http.StatusUnprocessableEntity || !strings.Contains(rejected.Body.String(), "destination_collision") {
		t.Fatal("collision was not definitively rejected", rejected.Code, rejected.Body.String())
	}
	if receipt := queueSubmissionResponse(s, id, "42"); receipt.Code != http.StatusNotFound {
		t.Fatal("rejected input registered a receipt", receipt.Code)
	}
	assertNoQueueReservation(t, s, "42", id)
	// A corrected draft may reuse the ID: the rejected draft registered no intent.
	input.Sources[1].Name = "different.bin"
	if accepted := submit(); accepted.Code != http.StatusAccepted {
		t.Fatal("corrected draft not accepted", accepted.Code, accepted.Body.String())
	}
	if again := submit(); again.Code != http.StatusAccepted {
		t.Fatal("identical receipt lost", again.Code)
	}
	input.Sources[1].Name = ""
	if conflict := submit(); conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "submission_changed") {
		t.Fatal("existing submission conflict must remain 409", conflict.Code, conflict.Body.String())
	}
}

func TestImportQueueInvalidInitialDestinationDoesNotRegisterIntent(t *testing.T) {
	s := queueTestServer(t, newLocalQueueStore(t), "")
	defer s.CloseImports()
	queueStopScheduler(s)
	id := strings.Repeat("b", 32)
	input := queueRequest{SubmissionID: id, Path: strings.Join([]string{strings.Repeat("a", 250), strings.Repeat("b", 250), strings.Repeat("c", 250), strings.Repeat("d", 100)}, "/"), Sources: queueDirect(strings.Repeat("f", 64))}
	body, _ := json.Marshal(input)
	w := httptest.NewRecorder()
	s.createImportQueue(w, ownerRequest("POST", "/api/import-queues", string(body), "42"))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "invalid_destination") {
		t.Fatal(w.Code, w.Body.String())
	}
	assertNoQueueReservation(t, s, "42", id)
}

func assertNoQueueReservation(t *testing.T, s *Server, owner, submission string) {
	t.Helper()
	id := queueIdentity(owner, submission)
	s.queues.mu.Lock()
	registered := s.queues.queues[id] != nil
	metadata := s.queues.metadataBytes
	owned, total := s.queues.ownerItems[owner], s.queues.totalItems
	s.queues.mu.Unlock()
	if registered || metadata != 0 || owned != 0 || total != 0 {
		t.Fatal("rejection changed catalog/reservations", registered, metadata, owned, total)
	}
	for _, name := range []string{"state.json", "manifest.json"} {
		if _, err := s.store.StatObject(context.Background(), queueNamespace+"/"+owner+"/"+id+"/"+name); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal("rejection persisted intent", name, err)
		}
	}
	s.dispatchQueueWork()
	if len(s.imports.slots) != 0 {
		t.Fatal("rejected intent executed")
	}
}

// Test-only construction of a normal one-item acceptance candidate. Registration
// uses the production helper, with no production interleaving/test hook.
func queueReservationCandidate(owner string, number int) *durableQueue {
	submission := fmt.Sprintf("%032x", number+1)
	request := queueRequest{SubmissionID: submission, Space: "personal", Path: "target", Existing: "error", Sources: queueDirect(fmt.Sprintf("file-%d", number))}
	id := queueIdentity(owner, submission)
	now := time.Now().UTC()
	manifest := queueManifest{Schema: 1, ID: id, Owner: owner, Fingerprint: queueFingerprint(request), AcceptedAt: now, ExpiresAt: now.Add(queueLifetime), Request: request}
	dest := path.Join(request.Path, request.Sources[0].Name)
	state := queueState{Schema: 1, ID: id, Owner: owner, Fingerprint: manifest.Fingerprint, Revision: 1, Control: "paused", Discovered: []bool{true}, Targets: map[string]string{dest: "f|0:"}, Items: []queueRecord{{QueueItem: QueueItem{ID: queueIdentity(id, "0"), QueueID: id, Position: 0, Name: request.Sources[0].Name, Path: dest, State: "queued"}, Source: 0, ExpectedSize: -1}}}
	mb, _ := json.Marshal(manifest)
	sb, _ := json.Marshal(state)
	return &durableQueue{manifest: manifest, state: state, frozen: true, busy: map[string]bool{}, cancels: map[string]func(){}, metadataBytes: len(mb) + len(sb), stateBytes: len(sb), progress: map[string]int64{}}
}

func persistQueueReservation(s *Server, q *durableQueue) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	ctx, cancel := queueContext()
	defer cancel()
	if _, err := s.queueWrite(ctx, queueBase(q.manifest)+"/state.json", q.state, queueSnapshotLimit); err != nil {
		return err
	}
	if _, err := s.queueWrite(ctx, queueBase(q.manifest)+"/manifest.json", q.manifest, 12<<20); err != nil {
		return err
	}
	q.frozen = false
	return nil
}

func appendQueueInventoryRecords(st *queueState, manifest queueManifest, count int) {
	for i := 0; i < count; i++ {
		position := len(st.Items)
		name := fmt.Sprintf("child-%05d", position)
		identity := "f|0:" + name
		destination := path.Join(manifest.Request.Path, name)
		st.Targets[destination] = identity
		st.Items = append(st.Items, queueRecord{QueueItem: QueueItem{ID: queueIdentity(manifest.ID, identity), QueueID: manifest.ID, Position: position, Name: name, Path: destination, State: "queued"}, Source: 0, ProviderPath: name, ExpectedSize: 0})
	}
}

func queueInventoryAtCapacity(t *testing.T, s *Server, owner string, number, retained int) *durableQueue {
	t.Helper()
	q, code := queueSubmit(t, s, fmt.Sprintf("%032x", number+1000), owner, []queueSource{{Type: "yandex", URL: "https://disk.yandex.ru/d/fixture"}})
	if code != http.StatusAccepted {
		t.Fatal(code)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	next := cloneQueueState(q.state)
	next.Control = "paused"
	appendQueueInventoryRecords(&next, q.manifest, retained-1) // pending discovery reserves the remaining record
	if err := s.commitQueue(context.Background(), q, next); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestImportQueueRegistrationRechecksCapacityAfterDiscovery(t *testing.T) {
	for _, scope := range []string{"owner", "process"} {
		t.Run(scope, func(t *testing.T) {
			disk := newLocalQueueStore(t)
			root := filepath.Join(t.TempDir(), "imports")
			s := queueTestServer(t, disk, root)
			defer s.CloseImports()
			queueStopScheduler(s)
			candidateOwner, discoveryOwner := "42", "42"
			if scope == "process" {
				for i := 0; i < 4; i++ {
					queueInventoryAtCapacity(t, s, fmt.Sprint(100+i), i, queueOwnerItems)
				}
				candidateOwner, discoveryOwner = "47", "46"
			}
			inventory := queueInventoryAtCapacity(t, s, discoveryOwner, 10, queueOwnerItems-1)
			candidate := queueReservationCandidate(candidateOwner, 100)
			owned, total := s.retainedQueueItems(candidateOwner)
			if owned+queueRetainedItems(candidate.state) > queueOwnerItems || total+queueRetainedItems(candidate.state) > queueProcessItems {
				t.Fatal("preliminary check should pass", owned, total)
			}
			// Deterministically complete a discovery commit AFTER the stale precheck and
			// BEFORE production's atomic reservation, without sleeps or a public hook.
			inventory.mu.Lock()
			next := cloneQueueState(inventory.state)
			appendQueueInventoryRecords(&next, inventory.manifest, 1)
			err := s.commitQueue(context.Background(), inventory, next)
			inventory.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err = s.queues.registerQueue(candidate); !errors.Is(err, errQueueCapacity) {
				t.Fatal("stale precheck bypassed capacity", err)
			}
			owned, total = s.retainedQueueItems(discoveryOwner)
			wantTotal := queueOwnerItems
			if scope == "process" {
				wantTotal = queueProcessItems
			}
			if owned != queueOwnerItems || total != wantTotal {
				t.Fatal("incorrect final reservations", owned, total)
			}
			s.queues.mu.Lock()
			registered := s.queues.queues[candidate.manifest.ID] != nil
			s.queues.mu.Unlock()
			if registered {
				t.Fatal("rejected candidate registered")
			}
			for _, name := range []string{"state.json", "manifest.json"} {
				if _, err = s.store.StatObject(context.Background(), queueBase(candidate.manifest)+"/"+name); !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("rejected reservation persisted", name, err)
				}
			}
			if err = s.CloseImports(); err != nil {
				t.Fatal(err)
			}
			reopened := queueTestServer(t, disk, root)
			defer reopened.CloseImports()
			queueStopScheduler(reopened)
			gotOwner, gotTotal := reopened.retainedQueueItems(discoveryOwner)
			if gotOwner != owned || gotTotal != total || reopened.queues.capacityUnknown {
				t.Fatal("bounded catalog could not reopen", gotOwner, gotTotal)
			}
			for _, q := range reopened.queues.queues {
				if q.frozen {
					t.Fatal("reopened bounded queue blocked")
				}
			}
		})
	}
}

func TestImportQueueConcurrentRegistrationAndDiscoveryRespectCapacity(t *testing.T) {
	for _, scope := range []string{"owner", "process"} {
		t.Run(scope, func(t *testing.T) {
			s := queueTestServer(t, newLocalQueueStore(t), "")
			defer s.CloseImports()
			queueStopScheduler(s)
			inventory := queueInventoryAtCapacity(t, s, "42", 0, 1)
			const headroom = 12
			m := s.queues
			m.mu.Lock()
			// Counter-only unit fixture represents pre-existing retained catalogs; the
			// deterministic test above additionally writes and reopens a real full catalog.
			baseline := queueOwnerItems - headroom
			if scope == "owner" {
				m.ownerItems["42"] = baseline
				m.totalItems = baseline
			} else {
				baseline = queueProcessItems - headroom
				for i := 0; i < 4; i++ {
					m.ownerItems[fmt.Sprint(100+i)] = queueOwnerItems
				}
				m.ownerItems["104"] = baseline - 4*queueOwnerItems - 1
				m.totalItems = baseline
			}
			m.mu.Unlock()
			const workers = 64
			start := make(chan struct{})
			failures := make(chan error, workers*2)
			var ready, done sync.WaitGroup
			var successful atomic.Int32
			ready.Add(workers)
			done.Add(workers)
			for i := 0; i < workers; i++ {
				go func(i int) {
					defer done.Done()
					ready.Done()
					<-start
					var err error
					if i%2 == 0 {
						owner := "42"
						if scope == "process" {
							owner = fmt.Sprint(42 + i%3)
						}
						candidate := queueReservationCandidate(owner, i+2000)
						err = m.registerQueue(candidate)
						if err == nil {
							err = persistQueueReservation(s, candidate)
						}
					} else {
						inventory.mu.Lock()
						next := cloneQueueState(inventory.state)
						appendQueueInventoryRecords(&next, inventory.manifest, 1)
						err = s.commitQueue(context.Background(), inventory, next)
						inventory.mu.Unlock()
					}
					if err == nil {
						successful.Add(1)
					} else if !errors.Is(err, errQueueCapacity) {
						failures <- err
					}
					m.mu.Lock()
					bounded := m.totalItems <= queueProcessItems
					for _, count := range m.ownerItems {
						bounded = bounded && count <= queueOwnerItems
					}
					m.mu.Unlock()
					if !bounded {
						failures <- errors.New("retained item cap exceeded")
					}
				}(i)
			}
			ready.Wait()
			close(start)
			done.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			if got := int(successful.Load()); got != headroom {
				t.Fatal("capacity not reserved exactly once per successful operation", got)
			}
			m.mu.Lock()
			total := m.totalItems
			owned := m.ownerItems["42"]
			m.mu.Unlock()
			if total != baseline+headroom || scope == "owner" && owned != queueOwnerItems {
				t.Fatal("incorrect final counts", owned, total)
			}
		})
	}
}

func TestImportQueueAmbiguousDiscoveryKeepsCapacityReserved(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			disk := newLocalQueueStore(t)
			fault := &queueFaultStore{Store: disk}
			s := queueTestServer(t, fault, "")
			defer s.CloseImports()
			queueStopScheduler(s)
			inventory := queueInventoryAtCapacity(t, s, "42", 0, 1)
			s.queues.mu.Lock()
			s.queues.ownerItems["42"] = queueOwnerItems - 1
			s.queues.totalItems = queueOwnerItems - 1
			s.queues.mu.Unlock()
			fault.setFault(func(key string, body []byte) (bool, bool) { return !after, after })
			inventory.mu.Lock()
			next := cloneQueueState(inventory.state)
			appendQueueInventoryRecords(&next, inventory.manifest, 1)
			err := s.commitQueue(context.Background(), inventory, next)
			frozen := inventory.frozen
			inventory.mu.Unlock()
			if !errors.Is(err, errQueueState) || !frozen {
				t.Fatal("ambiguous discovery did not freeze", err)
			}
			fault.setFault(nil)
			if owned, total := s.retainedQueueItems("42"); owned != queueOwnerItems || total != queueOwnerItems {
				t.Fatal("ambiguous write released reservation", owned, total)
			}
			if err = s.queues.registerQueue(queueReservationCandidate("42", 3000)); !errors.Is(err, errQueueCapacity) {
				t.Fatal("uncertain reservation reused", err)
			}
			s.dispatchQueueWork()
			if len(s.imports.slots) != 0 {
				t.Fatal("ambiguous inventory executed")
			}
		})
	}
}
