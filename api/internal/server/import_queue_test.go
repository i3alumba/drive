//go:build linux

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
)

// localQueueStore is a real durable LOCAL fixture, not the production NAS
// backend: payload fsync, rename, directory fsync, real reopen/streaming reads.
// It intentionally has no production constructor or NFS guard escape hatch.
type localQueueStore struct {
	Store
	root string
	mu   sync.Mutex
}

func newLocalQueueStore(t *testing.T) *localQueueStore {
	t.Helper()
	return &localQueueStore{root: t.TempDir()}
}
func (s *localQueueStore) location(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(key))
}
func (s *localQueueStore) PutObject(ctx context.Context, key string, r io.Reader, size int64, kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dir := s.location(key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	n, err := io.Copy(f, r)
	if err != nil {
		return err
	}
	if n != size {
		return io.ErrUnexpectedEOF
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "payload")); err != nil {
		return err
	}
	for current := dir; ; current = filepath.Dir(current) {
		d, err := os.Open(current)
		if err != nil {
			return err
		}
		err = d.Sync()
		closeErr := d.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if current == s.root {
			break
		}
	}
	return nil
}
func (s *localQueueStore) GetObject(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	f, err := os.Open(filepath.Join(s.location(key), "payload"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, storage.ObjectInfo{}, err
	}
	return f, storage.ObjectInfo{Size: info.Size()}, nil
}
func (s *localQueueStore) StatObject(ctx context.Context, key string) (storage.Object, error) {
	f, info, err := s.GetObject(ctx, key)
	if err != nil {
		return storage.Object{}, err
	}
	f.Close()
	return storage.Object{Name: path.Base(key), Path: key, Size: info.Size}, nil
}
func (s *localQueueStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.location(prefix))
	if errors.Is(err, os.ErrNotExist) {
		return []storage.Object{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []storage.Object{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		key := path.Join(prefix, e.Name())
		_, err := os.Stat(filepath.Join(s.location(key), "payload"))
		out = append(out, storage.Object{Name: e.Name(), Path: key, IsDir: errors.Is(err, os.ErrNotExist)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (s *localQueueStore) PutDirectory(ctx context.Context, key string) error {
	return os.MkdirAll(s.location(key), 0700)
}
func (s *localQueueStore) DeleteObject(ctx context.Context, key string) error {
	return os.Remove(filepath.Join(s.location(key), "payload"))
}

type queueFaultStore struct {
	Store
	mu    sync.Mutex
	fault func(string, []byte) (bool, bool)
	puts  int
}

func (s *queueFaultStore) PutObject(ctx context.Context, key string, r io.Reader, size int64, kind string) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	hook := s.fault
	before, after := false, false
	if hook != nil {
		before, after = hook(key, data)
	}
	if strings.HasPrefix(key, "users/") {
		s.puts++
	}
	s.mu.Unlock()
	if before {
		return errors.New("synthetic private failure")
	}
	if err = s.Store.PutObject(ctx, key, bytes.NewReader(data), size, kind); err != nil {
		return err
	}
	if after {
		return errors.New("synthetic directory fsync failure after rename")
	}
	return nil
}
func (s *queueFaultStore) setFault(hook func(string, []byte) (bool, bool)) {
	s.mu.Lock()
	s.fault = hook
	s.mu.Unlock()
}
func queueTestServer(t *testing.T, store Store, root string) *Server {
	t.Helper()
	if root == "" {
		root = filepath.Join(t.TempDir(), "imports")
	}
	s := New(store, nil, nil)
	if err := s.EnableImports(ImportOptions{Root: root, MaxFileBytes: 1 << 20, BudgetBytes: 4 << 20, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableImportQueues(); err != nil {
		s.CloseImports()
		t.Fatal(err)
	}
	return s
}
func queueStopScheduler(s *Server) {
	s.queues.stopOnce.Do(func() { close(s.queues.stop) })
	<-s.queues.done
}
func queueSubmit(t *testing.T, s *Server, submission, owner string, sources []queueSource) (*durableQueue, int) {
	t.Helper()
	body, _ := json.Marshal(queueRequest{SubmissionID: submission, Path: "target", Existing: "error", Sources: sources})
	w := httptest.NewRecorder()
	s.createImportQueue(w, ownerRequest("POST", "/api/import-queues", string(body), owner))
	s.queues.mu.Lock()
	q := s.queues.queues[queueIdentity(owner, submission)]
	s.queues.mu.Unlock()
	return q, w.Code
}
func queueSubmissionResponse(s *Server, submission, owner string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.getQueueRoute(w, ownerRequest("GET", "/api/import-queues/submissions/"+submission, "", owner))
	return w
}
func queueDirect(name string) []queueSource {
	return []queueSource{{Type: "direct", URL: "http://source.example/" + name + "?private=capability", Name: name}}
}
func queueFakeBytes(s *Server, data string) {
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Body: io.NopCloser(strings.NewReader(data)), Header: make(http.Header), Request: r}, nil
	})
}
func queueWait(t *testing.T, q *durableQueue, want string) queueRecord {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		if len(q.state.Items) > 0 && q.state.Items[0].State == want {
			it := q.state.Items[0]
			q.mu.Unlock()
			return it
		}
		frozen := q.frozen
		q.mu.Unlock()
		if frozen {
			t.Fatal("queue unexpectedly frozen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t.Fatalf("wanted %s, state=%+v", want, q.state)
	return queueRecord{}
}
func queueDigest(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}

func TestImportQueueDurableAcceptanceLostACKAndChangedSubmission(t *testing.T) {
	store := newLocalQueueStore(t)
	s := queueTestServer(t, store, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	id := strings.Repeat("a", 32)
	q, code := queueSubmit(t, s, id, "42", queueDirect("a.bin"))
	if code != 202 {
		t.Fatal(code)
	}
	q2, code := queueSubmit(t, s, id, "42", queueDirect("a.bin"))
	if code != 202 || q2 != q {
		t.Fatal("lost ACK duplicated queue", code)
	}
	_, code = queueSubmit(t, s, id, "42", queueDirect("different.bin"))
	if code != 409 {
		t.Fatal(code)
	}
	_, code = queueSubmit(t, s, id, "43", queueDirect("a.bin"))
	if code != 202 {
		t.Fatal("submission should be owner-scoped", code)
	}
	w := httptest.NewRecorder()
	r := ownerRequest("GET", "/api/import-queues/"+q.manifest.ID, "", "43")
	r.SetPathValue("id", q.manifest.ID)
	s.getImportQueue(w, r)
	if w.Code != 404 {
		t.Fatal("cross-owner lookup", w.Code)
	}
	for _, route := range []string{"/api/import-queues", "/api/import-queues/" + q.manifest.ID, "/api/import-queues/" + q.manifest.ID + "/items"} {
		w = httptest.NewRecorder()
		r = ownerRequest("GET", route, "", "42")
		if route == "/api/import-queues" {
			s.listImportQueues(w, r)
		} else {
			s.getQueueRoute(w, r)
		}
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code)
		}
		for _, secret := range []string{"source.example", "capability", "_drive/", "public_key", "fingerprint"} {
			if strings.Contains(strings.ToLower(w.Body.String()), secret) {
				t.Fatal("private source leaked", secret, w.Body.String())
			}
		}
	}
}
func TestImportQueueAcceptanceFaultsFreezeAndRecommit(t *testing.T) {
	for _, boundary := range []string{"state_before", "state_after", "manifest_before", "manifest_after"} {
		t.Run(boundary, func(t *testing.T) {
			disk := newLocalQueueStore(t)
			fault := &queueFaultStore{Store: disk}
			root := filepath.Join(t.TempDir(), "imports")
			s := queueTestServer(t, fault, root)
			queueStopScheduler(s)
			fault.setFault(func(key string, data []byte) (bool, bool) {
				hit := strings.Contains(key, strings.Split(boundary, "_")[0]+".json")
				return hit && strings.HasSuffix(boundary, "before"), hit && strings.HasSuffix(boundary, "after")
			})
			id := strings.Repeat("b", 32)
			if receipt := queueSubmissionResponse(s, id, "42"); receipt.Code != http.StatusNotFound {
				t.Fatal("unknown submission reported acceptance", receipt.Code)
			}
			q, code := queueSubmit(t, s, id, "42", queueDirect("a"))
			if code != 503 || !q.frozen {
				t.Fatal("ambiguous acceptance acknowledged", code)
			}
			if receipt := queueSubmissionResponse(s, id, "42"); receipt.Code != http.StatusServiceUnavailable || receipt.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("uncertain POST received successful submission receipt", receipt.Code, receipt.Body.String())
			}
			if _, retryCode := queueSubmit(t, s, id, "42", queueDirect("a")); retryCode != http.StatusServiceUnavailable {
				t.Fatal("uncertain POST retry received acceptance", retryCode)
			}
			if owned, total := s.retainedQueueItems("42"); owned != 1 || total != 1 {
				t.Fatal("uncertain acceptance released or duplicated its reservation", owned, total)
			}
			s.dispatchQueueWork()
			if len(s.imports.slots) != 0 {
				t.Fatal("uncertain state executed")
			}
			fault.setFault(nil)
			if err := s.CloseImports(); err != nil {
				t.Fatal(err)
			}
			next := queueTestServer(t, fault, root)
			defer next.CloseImports()
			queueStopScheduler(next)
			next.queues.mu.Lock()
			recovered := next.queues.queues[q.manifest.ID]
			next.queues.mu.Unlock()
			committed := boundary == "manifest_after"
			if committed && (recovered == nil || recovered.frozen) {
				t.Fatal("committed acceptance not successfully recommitted")
			}
			if !committed && recovered != nil {
				t.Fatal("orphan state became executable")
			}
			receipt := queueSubmissionResponse(next, id, "42")
			want := http.StatusNotFound
			if committed {
				want = http.StatusOK
			}
			if receipt.Code != want {
				t.Fatal("restart receipt disagrees with durable acceptance", receipt.Code, want)
			}
			if !committed {
				next.dispatchQueueWork()
				if len(next.imports.slots) != 0 {
					t.Fatal("restart dispatched an orphan acceptance")
				}
			}
			_, code = queueSubmit(t, next, id, "42", queueDirect("a"))
			if code != 202 {
				t.Fatal("safe identical receipt/create failed", code)
			}
		})
	}
}
func TestImportQueueMissingAndCorruptCommittedStateFailClosed(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			disk := newLocalQueueStore(t)
			root := filepath.Join(t.TempDir(), "imports")
			s := queueTestServer(t, disk, root)
			queueStopScheduler(s)
			q, code := queueSubmit(t, s, strings.Repeat("c", 32), "42", queueDirect("a"))
			if code != 202 {
				t.Fatal(code)
			}
			s.CloseImports()
			key := queueBase(q.manifest) + "/state.json"
			if corrupt {
				_ = disk.PutObject(context.Background(), key, strings.NewReader("{"), 1, "application/json")
			} else {
				_ = disk.DeleteObject(context.Background(), key)
			}
			next := queueTestServer(t, disk, root)
			defer next.CloseImports()
			queueStopScheduler(next)
			recovered := next.queues.queues[q.manifest.ID]
			if recovered == nil || !recovered.frozen {
				t.Fatal("committed corrupt state reconstructed")
			}
			next.dispatchQueueWork()
			if len(next.imports.slots) != 0 {
				t.Fatal("corruption dispatched")
			}
		})
	}
}
func TestImportQueuePublicationCheckpointsAndRecoveryFullSHA(t *testing.T) {
	for _, failure := range []string{"saving_before", "saving_after", "put_after", "completion_before", "completion_after"} {
		t.Run(failure, func(t *testing.T) {
			disk := newLocalQueueStore(t)
			fault := &queueFaultStore{Store: disk}
			root := filepath.Join(t.TempDir(), "imports")
			s := queueTestServer(t, fault, root)
			queueStopScheduler(s)
			queueFakeBytes(s, "payload")
			q, code := queueSubmit(t, s, strings.Repeat("d", 32), "42", queueDirect("a"))
			if code != 202 {
				t.Fatal(code)
			}
			fault.setFault(func(key string, b []byte) (bool, bool) {
				if strings.HasPrefix(key, "users/") {
					return false, failure == "put_after"
				}
				var st queueState
				if strings.HasSuffix(key, "state.json") {
					_ = json.Unmarshal(b, &st)
				}
				hit := len(st.Items) > 0 && (strings.HasPrefix(failure, "saving") && st.Items[0].State == "saving" || strings.HasPrefix(failure, "completion") && st.Items[0].State == "complete")
				return hit && strings.HasSuffix(failure, "before"), hit && strings.HasSuffix(failure, "after")
			})
			s.dispatchQueueWork()
			s.imports.workers.Wait()
			fault.setFault(nil)
			s.CloseImports()
			fault.mu.Lock()
			before := fault.puts
			fault.mu.Unlock()
			if strings.HasPrefix(failure, "saving") && before != 0 {
				t.Fatal("Put after uncertain saving checkpoint")
			}
			next := queueTestServer(t, fault, root)
			defer next.CloseImports()
			queueStopScheduler(next)
			queueFakeBytes(next, "payload")
			recovered := next.queues.queues[q.manifest.ID]
			if failure == "saving_before" {
				recovered.mu.Lock()
				if recovered.state.Items[0].NextAttemptAt == nil {
					t.Fatal("restart backoff missing")
				}
				retry := cloneQueueState(recovered.state)
				retry.Items[0].NextAttemptAt = nil
				_ = next.commitQueue(context.Background(), recovered, retry)
				recovered.mu.Unlock()
			}
			next.dispatchQueueWork()
			next.imports.workers.Wait()
			recovered.mu.Lock()
			state := recovered.state.Items[0].State
			recovered.mu.Unlock()
			if failure == "saving_before" { // Durable downloading proves Put impossible: safe retry from zero.
				if state != "complete" {
					t.Fatal(state)
				}
			} else if failure == "saving_after" {
				if state != "needs_verification" {
					t.Fatal(state)
				}
			} else if state != "complete" && failure != "put_after" {
				t.Fatal(state)
			}
			if failure == "put_after" && state != "needs_verification" {
				t.Fatal("Put error retried", state)
			}
			fault.mu.Lock()
			after := fault.puts
			fault.mu.Unlock()
			if failure != "saving_before" && after != before {
				t.Fatal("publication replayed", before, after)
			}
		})
	}
}
func TestImportQueueReadOnlyReconcileRejectsSameSizeWrongSHA(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	q, _ := queueSubmit(t, s, strings.Repeat("e", 32), "42", queueDirect("a"))
	q.mu.Lock()
	next := cloneQueueState(q.state)
	it := &next.Items[0]
	it.State = "saving"
	it.Total = 3
	it.Bytes = 3
	it.SHA256 = queueDigest("one")
	_ = s.commitQueue(context.Background(), q, next)
	q.mu.Unlock()
	_ = disk.PutObject(context.Background(), "users/42/target/a", strings.NewReader("two"), 3, "")
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	queueWait(t, q, "needs_verification")
	f, _, _ := disk.GetObject(context.Background(), "users/42/target/a")
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "two" {
		t.Fatal("reconcile changed destination")
	}
}
func TestImportQueuePauseCancelSavingAndExplicitRetryDeadline(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	queueFakeBytes(s, "payload")
	q, _ := queueSubmit(t, s, strings.Repeat("f", 32), "42", queueDirect("a"))
	control := func(action string) int {
		w := httptest.NewRecorder()
		r := ownerRequest("POST", "/api/import-queues/"+q.manifest.ID+"/"+action, "", "42")
		r.SetPathValue("id", q.manifest.ID)
		r.SetPathValue("action", action)
		s.controlImportQueue(w, r)
		return w.Code
	}
	if control("pause") != 202 {
		t.Fatal("pause")
	}
	s.dispatchQueueWork()
	if len(s.imports.slots) != 0 {
		t.Fatal("pause admitted work")
	}
	if control("resume") != 202 {
		t.Fatal("resume")
	}
	q.mu.Lock()
	next := cloneQueueState(q.state)
	next.Items[0].State = "failed"
	next.Items[0].Attempts = 3
	next.Items[0].TotalAttempts = 3
	next.Items[0].Deadline = time.Now().Add(-time.Second)
	original := next.Items[0].Deadline
	_ = s.commitQueue(context.Background(), q, next)
	q.mu.Unlock()
	w := httptest.NewRecorder()
	r := ownerRequest("POST", "/retry", "", "42")
	r.SetPathValue("id", q.manifest.ID)
	r.SetPathValue("itemId", next.Items[0].ID)
	s.controlQueueItem(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	s.dispatchQueueWork()
	s.imports.workers.Wait()
	it := queueWait(t, q, "failed")
	if it.TotalAttempts != 3 || it.Attempts != 0 || len(it.History) != 1 || !it.Deadline.Equal(original) {
		t.Fatal("retry reset deadline/history", it)
	}
	if control("cancel") != 202 {
		t.Fatal("cancel")
	}
	if control("resume") != 409 {
		t.Fatal("cancel resumed")
	}
}
func TestImportQueueProviderPathsPaginationNormalizationAndFreshResolution(t *testing.T) {
	disk := newLocalQueueStore(t)
	s := queueTestServer(t, disk, "")
	defer s.CloseImports()
	queueStopScheduler(s)
	var resolutions atomic.Int32
	sha := queueDigest("data")
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := ""
		status := 200
		if r.URL.Host == "cloud-api.yandex.net" {
			pp := r.URL.Query().Get("path")
			if strings.HasSuffix(r.URL.Path, "/download") {
				resolutions.Add(1)
				body = `{"href":"https://download.example/file?private=signed","method":"GET"}`
			} else if pp == "/ space.bin " {
				body = fmt.Sprintf(`{"name":" space.bin ","type":"file","size":4,"sha256":%q}`, sha)
			} else if pp == "/empty" {
				body = `{"name":"empty","type":"dir","_embedded":{"items":[],"total":0,"offset":0,"limit":200}}`
			} else if r.URL.Query().Get("offset") == "0" {
				body = fmt.Sprintf(`{"name":"root","type":"dir","_embedded":{"items":[{"name":" space.bin ","type":"file","size":4,"sha256":%q}],"total":2,"offset":0,"limit":1}}`, sha)
			} else {
				body = `{"name":"root","type":"dir","_embedded":{"items":[{"name":"empty","type":"dir"}],"total":2,"offset":1,"limit":1}}`
			}
		} else {
			body = "data"
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header), Request: r}, nil
	})
	q, code := queueSubmit(t, s, strings.Repeat("1", 32), "42", []queueSource{{Type: "yandex", URL: "https://disk.yandex.ru/d/public"}})
	if code != 202 {
		t.Fatal(code)
	}
	for i := 0; i < 12; i++ {
		s.dispatchQueueWork()
		s.imports.workers.Wait()
	}
	q.mu.Lock()
	summary := q.summary()
	records := append([]queueRecord(nil), q.state.Items...)
	q.mu.Unlock()
	if summary.State != "complete" || summary.TotalItems != 2 || summary.NormalizedNames != 1 || resolutions.Load() != 1 {
		t.Fatal(summary, resolutions.Load())
	}
	if records[0].ProviderPath != " space.bin " || records[0].Path != "target/space.bin" {
		t.Fatal("provider locator normalized", records)
	}
	f, _, err := disk.GetObject(context.Background(), queueBase(q.manifest)+"/state.json")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if strings.Contains(string(b), "private=signed") {
		t.Fatal("signed URL persisted")
	}
}
func TestImportQueueStrictProviderPathsAndNormalizedCollision(t *testing.T) {
	for _, name := range []string{"..", "/abs", "a/b", "a\\b", "a\x00b", string([]byte{0xff}), " . "} {
		if _, _, err := normalizedProviderName(name); err == nil {
			t.Fatalf("unsafe provider name accepted %q", name)
		}
	}
	st := queueState{Targets: map[string]string{}}
	m := queueManifest{ID: strings.Repeat("a", 64)}
	size := int64(1)
	resource := yandexResource{Type: "file", Size: &size}
	if addYandexRecord(&st, m, 0, " a ", "a", resource) != nil || addYandexRecord(&st, m, 0, "a", "a", resource) == nil {
		t.Fatal("normalized sources merged")
	}
	for _, p := range []string{"/a", "a/../b", "a//b", "a\\b", ".", "a/./b"} {
		in := queueRequest{SubmissionID: strings.Repeat("a", 32), Path: p, Sources: queueDirect("x")}
		if validateQueueRequest(&in) == nil {
			t.Fatal("unsafe personal path", p)
		}
	}
}

// Real loopback HTTPS Auth, HTTP source/API and durable disk fixture. No external
// provider, production NAS, browser profile, or live acceptance is involved.
func TestImportQueueLocalHTTPDisconnectAuthExpiryAndRestart(t *testing.T) {
	disk := newLocalQueueStore(t)
	root := filepath.Join(t.TempDir(), "imports")
	var expired atomic.Bool
	auth := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/" {
			t.Error("Auth fixture received a noncanonical profile path")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if expired.Load() {
			w.WriteHeader(401)
			return
		}
		id := 42
		if r.Header.Get("Authorization") == "Bearer other" {
			id = 43
		}
		fmt.Fprintf(w, `{"id":%d,"username":"local","services":{"drive":true}}`, id)
	}))
	defer auth.Close()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			fmt.Fprint(w, "local durable bytes")
		case <-r.Context().Done():
			return
		}
	}))
	defer source.Close()
	configure := func(s *Server) {
		s.auth = NewAuthValidator(auth.URL + "/api/user/")
		trustAuthFixture(t, s.auth, auth)
		transport := remoteHTTPClient().Transport.(*http.Transport).Clone()
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, source.Listener.Addr().String())
		}
		s.imports.client.Transport = transport
	}
	s := queueTestServer(t, disk, root)
	configure(s)
	api := httptest.NewServer(s.Routes())
	body, _ := json.Marshal(queueRequest{SubmissionID: strings.Repeat("2", 32), Sources: queueDirect("real.bin")})
	req, _ := http.NewRequest("POST", api.URL+"/api/import-queues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer short-lived")
	response, err := api.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 202 {
		t.Fatal(response.StatusCode)
	}
	var receipt QueueSummary
	_ = json.NewDecoder(response.Body).Decode(&receipt)
	response.Body.Close()
	api.CloseClientConnections() // original HTTP client is gone
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not continue after acceptance")
	}
	expired.Store(true)
	api.Close()
	s.CloseImports() // interrupted download, release execution fence
	// New server genuinely reloads durable files; no original JWT is supplied.
	next := queueTestServer(t, disk, root)
	configure(next)
	defer next.CloseImports()
	nextAPI := httptest.NewServer(next.Routes())
	defer nextAPI.Close()
	close(release)
	q := next.queues.queues[receipt.ID]
	queueWait(t, q, "complete")
	f, _, err := disk.GetObject(context.Background(), "users/42/real.bin")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "local durable bytes" {
		t.Fatal(string(data))
	}
	req, _ = http.NewRequest("GET", nextAPI.URL+"/api/import-queues/"+receipt.ID, nil)
	req.Header.Set("Authorization", "Bearer short-lived")
	res, err := nextAPI.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("expired auth management", res.StatusCode)
	}
	expired.Store(false)
	req, _ = http.NewRequest("GET", nextAPI.URL+"/api/import-queues/"+receipt.ID, nil)
	req.Header.Set("Authorization", "Bearer other")
	res, err = nextAPI.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("other owner", res.StatusCode)
	}
	req, _ = http.NewRequest("POST", nextAPI.URL+"/api/import-queues/"+receipt.ID+"/cancel", nil)
	req.AddCookie(&http.Cookie{Name: "access_token", Value: "short-lived"})
	res, err = nextAPI.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("cookie mutation", res.StatusCode)
	}
}
