package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"remote-drive/api/internal/storage"
)

func queueBase(m queueManifest) string { return queueNamespace + "/" + m.Owner + "/" + m.ID }
func queueFingerprint(r queueRequest) string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *Server) queueWrite(ctx context.Context, key string, v any, limit int) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	if len(b) > limit {
		return 0, errQueueCapacity
	}
	return len(b), s.store.PutObject(ctx, key, bytes.NewReader(b), int64(len(b)), "application/json")
}
func (s *Server) queueRead(ctx context.Context, key string, v any, limit int) (int, error) {
	f, info, err := s.store.GetObject(ctx, key)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if info.Size < 0 || info.Size > int64(limit) {
		return 0, errQueueState
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(b) > limit || !validQueueJSON(b) {
		return 0, errQueueState
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return 0, errQueueState
	}
	return len(b), nil
}

// registerQueue rechecks and reserves current capacity atomically with catalog
// registration: discovery may have advanced since creation's preliminary check.
// Reservations remain conservative if a subsequent acceptance write is uncertain.
func (m *queueManager) registerQueue(q *durableQueue) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	retained := queueRetainedItems(q.state)
	owner := q.manifest.Owner
	if m.capacityUnknown || m.queues[q.manifest.ID] != nil || len(m.queues) >= queueProcessItems ||
		m.metadataBytes > queueMetadataLimit-q.metadataBytes ||
		m.ownerItems[owner]+retained > queueOwnerItems || m.totalItems+retained > queueProcessItems {
		return errQueueCapacity
	}
	m.queues[q.manifest.ID] = q
	m.metadataBytes += q.metadataBytes
	m.ownerItems[owner] += retained
	m.totalItems += retained
	return nil
}

// commitQueue is called with q.mu held. An ambiguous Store error permanently
// freezes this in-process instance. Only fenced recovery can recommit readback.
func (s *Server) commitQueue(ctx context.Context, q *durableQueue, next queueState) error {
	if q.frozen {
		return errQueueState
	}
	next.Revision = q.state.Revision + 1
	b, err := json.Marshal(next)
	if err != nil || len(b) > queueSnapshotLimit {
		return errQueueCapacity
	}
	delta := len(b) - q.stateBytes
	m := s.queues
	m.mu.Lock()
	if delta > 0 && m.metadataBytes > queueMetadataLimit-delta {
		m.mu.Unlock()
		return errQueueCapacity
	}
	added := queueRetainedItems(next) - queueRetainedItems(q.state)
	if added > 0 && m.capacityUnknown || m.ownerItems[q.manifest.Owner]+added > queueOwnerItems || m.totalItems+added > queueProcessItems {
		m.mu.Unlock()
		return errQueueCapacity
	}
	m.ownerItems[q.manifest.Owner] += max(0, added)
	m.totalItems += max(0, added)
	m.metadataBytes += max(0, delta)
	m.mu.Unlock()
	err = s.store.PutObject(ctx, queueBase(q.manifest)+"/state.json", bytes.NewReader(b), int64(len(b)), "application/json")
	if err != nil {
		q.frozen = true
		return errQueueState
	}
	if delta < 0 {
		m.mu.Lock()
		m.metadataBytes += delta
		m.mu.Unlock()
	}
	if added < 0 {
		m.mu.Lock()
		m.ownerItems[q.manifest.Owner] += added
		m.totalItems += added
		m.mu.Unlock()
	}
	q.stateBytes = len(b)
	q.state = next
	return nil
}

// cloneQueueState avoids exposing a partially mutated transition when writes fail.
func cloneQueueState(st queueState) queueState {
	b, _ := json.Marshal(st)
	var out queueState
	_ = json.Unmarshal(b, &out)
	return out
}
func validQueueOwner(owner string) bool {
	n, e := strconv.ParseInt(owner, 10, 64)
	return e == nil && n > 0 && strconv.FormatInt(n, 10) == owner
}
func validateQueueState(m queueManifest, st queueState) error {
	if st.Schema != 1 || st.ID != m.ID || st.Owner != m.Owner || st.Fingerprint != m.Fingerprint || st.Revision == 0 || len(st.Items) > queueOwnerItems || len(st.Discovered) != len(m.Request.Sources) || st.Targets == nil || st.Pages < 0 || st.Pages > 10000 || len(st.Walk) > queueOwnerItems+1000 || len(st.Targets) != len(st.Items) {
		return errQueueState
	}
	switch st.Control {
	case "running", "paused", "cancelled", "expired":
	default:
		return errQueueState
	}
	ids := map[string]bool{}
	for i, it := range st.Items {
		if !validHex(it.ID, 64) || ids[it.ID] || it.QueueID != m.ID || it.Position != i || it.Source < 0 || it.Source >= len(m.Request.Sources) || !strictQueuePath(it.Path, false) || it.Name != path.Base(it.Path) || !validDownloadName(it.Name) || it.Path != strings.TrimSpace(it.Path) || !strictQueuePath(it.ProviderPath, true) || it.Total < 0 || it.Bytes < 0 || it.ExpectedSize < -1 || it.Attempts < 0 || it.Attempts > 3 || it.TotalAttempts < it.Attempts || it.Generation < 0 || len(it.History) > 64 {
			return errQueueState
		}
		source := m.Request.Sources[it.Source]
		expectedID := queueIdentity(m.ID, strconv.Itoa(it.Source))
		identity := "f|" + strconv.Itoa(it.Source) + ":" + it.ProviderPath
		if it.Directory {
			identity = "d|" + strconv.Itoa(it.Source) + ":" + it.ProviderPath
		}
		if source.Type == "yandex" {
			expectedID = queueIdentity(m.ID, identity)
			if it.ExpectedSize < 0 {
				return errQueueState
			}
		} else if it.Directory || it.ProviderPath != "" || it.Path != path.Join(m.Request.Path, source.Name) || it.ExpectedSHA != source.SHA256 || it.ExpectedSize != -1 {
			return errQueueState
		}
		if it.ID != expectedID || st.Targets[it.Path] != identity || m.Request.Path != "" && !strings.HasPrefix(it.Path, m.Request.Path+"/") {
			return errQueueState
		}
		if (it.State == "resolving" || it.State == "downloading" || it.State == "saving" || it.State == "complete" || it.State == "needs_verification") && it.TotalAttempts == 0 {
			return errQueueState
		}
		if (it.State == "saving" || it.State == "complete" || it.State == "needs_verification") && it.Bytes != it.Total {
			return errQueueState
		}
		if it.Directory && (it.ExpectedSize != 0 || it.ExpectedSHA != "" || it.Total != 0 || it.Bytes != 0) {
			return errQueueState
		}
		historyAttempts := 0
		for _, attempts := range it.History {
			if attempts < 0 || attempts > 3 {
				return errQueueState
			}
			historyAttempts += attempts
		}
		if it.Generation != len(it.History) || it.TotalAttempts != historyAttempts+it.Attempts {
			return errQueueState
		}
		ids[it.ID] = true
		switch it.State {
		case "queued", "resolving", "downloading", "saving", "retry_wait", "complete", "failed", "cancelled", "expired", "needs_verification":
		default:
			return errQueueState
		}
		if (it.State == "saving" || it.State == "needs_verification" || it.State == "complete") && !it.Directory && !validHex(it.SHA256, 64) {
			return errQueueState
		}
		if it.ExpectedSHA != "" && !validHex(it.ExpectedSHA, 64) {
			return errQueueState
		}
		if it.TotalAttempts > 0 && it.Deadline.IsZero() || !it.Deadline.IsZero() && it.Deadline.After(m.ExpiresAt) {
			return errQueueState
		}
		if it.Scratch != "" && it.Scratch != ".queue-"+m.ID+"-"+it.ID+"-"+strconv.Itoa(it.TotalAttempts) {
			return errQueueState
		}
	}
	for _, w := range st.Walk {
		if w.Source < 0 || w.Source >= len(m.Request.Sources) || !strictQueuePath(w.ProviderPath, true) || !strictQueuePath(w.Destination, true) || w.Offset < 0 || w.Offset > 10000 || w.Total < -1 || w.Attempts < 0 || w.Attempts > 3 || !w.Deadline.IsZero() && w.Deadline.After(m.ExpiresAt) {
			return errQueueState
		}
	}
	return nil
}

// EnableImportQueues must follow EnableImports: its lock is the execution fence.
// It does not provide distributed exclusion for separate work volumes.
func (s *Server) EnableImportQueues() error {
	s.imports.mu.Lock()
	fenced := runtime.GOOS == "linux" && s.imports.root != nil && s.imports.lock != nil && !s.imports.closed
	s.imports.mu.Unlock()
	if !fenced {
		return errors.New("durable queues require fenced Linux URL imports")
	}
	if s.queues != nil {
		return errors.New("durable queues already enabled")
	}
	m := &queueManager{queues: map[string]*durableQueue{}, ownerItems: map[string]int{}, stop: make(chan struct{}), done: make(chan struct{})}
	s.queues = m
	enabled := false
	defer func() {
		if !enabled {
			s.queues = nil
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), s.imports.options.Timeout)
	defer cancel()
	owners, err := s.store.List(ctx, queueNamespace)
	if errors.Is(err, storage.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return errQueueState
	}
	retained := 0
	for _, owner := range owners {
		if !owner.IsDir || !validQueueOwner(owner.Name) || owner.Path != queueNamespace+"/"+owner.Name {
			return errQueueState
		}
		dirs, err := s.store.List(ctx, owner.Path)
		if err != nil {
			return errQueueState
		}
		for _, dir := range dirs {
			if !dir.IsDir || !validHex(dir.Name, 64) || dir.Path != owner.Path+"/"+dir.Name {
				return errQueueState
			}
			var manifest queueManifest
			n, err := s.queueRead(ctx, dir.Path+"/manifest.json", &manifest, 12<<20)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			} // orphan state is never executable
			if err != nil {
				return errQueueState
			}
			req := manifest.Request
			if manifest.Schema != 1 || manifest.Owner != owner.Name || manifest.ID != dir.Name || manifest.ID != queueIdentity(manifest.Owner, req.SubmissionID) || validateQueueRequest(&req) != nil || queueFingerprint(req) != manifest.Fingerprint || queueFingerprint(manifest.Request) != manifest.Fingerprint || !manifest.ExpiresAt.Equal(manifest.AcceptedAt.Add(queueLifetime)) {
				return errQueueState
			}
			q := &durableQueue{manifest: manifest, busy: map[string]bool{}, cancels: map[string]func(){}, progress: map[string]int64{}}
			size, readErr := s.queueRead(ctx, dir.Path+"/state.json", &q.state, queueSnapshotLimit)
			q.stateBytes = size
			q.metadataBytes = n + size
			m.metadataBytes += q.metadataBytes
			if m.metadataBytes > queueMetadataLimit || len(m.queues) >= queueProcessItems {
				return errQueueCapacity
			}
			m.queues[manifest.ID] = q
			if readErr != nil || validateQueueState(manifest, q.state) != nil {
				q.frozen = true
				q.state = queueState{} // Never expose an unvalidated record through safe DTOs.
				m.capacityUnknown = true
				continue
			}
			retained += queueRetainedItems(q.state)
			m.ownerItems[manifest.Owner] += queueRetainedItems(q.state)
			m.totalItems += queueRetainedItems(q.state)
			if m.ownerItems[manifest.Owner] > queueOwnerItems {
				return errQueueCapacity
			}
			if retained > queueProcessItems {
				return errQueueCapacity
			}
			// Recommit both readbacks: a rename/readback alone never proves fsync.
			if _, err := s.queueWrite(ctx, dir.Path+"/manifest.json", manifest, 12<<20); err != nil {
				q.frozen = true
				continue
			}
			next := cloneQueueState(q.state)
			for i := range next.Items {
				it := &next.Items[i]
				if it.Scratch != "" {
					f, e := uploadOpenFile(s.imports.root, it.Scratch, 0)
					if e == nil {
						f.Close()
						if uploadUnlink(s.imports.root, it.Scratch, false) != nil {
							q.frozen = true
						}
					} else if !errors.Is(e, os.ErrNotExist) {
						q.frozen = true
					}
				}
				if it.State == "resolving" || it.State == "downloading" {
					it.State = "retry_wait"
					it.ErrorCode = "interrupted"
					backoff := time.Now().Add(time.Duration(1<<it.Attempts) * time.Second)
					it.NextAttemptAt = &backoff
					if it.Attempts >= 3 {
						it.State = "failed"
					}
				}
			}
			if err := s.commitQueue(ctx, q, next); err != nil {
				q.frozen = true
			}
		}
	}
	enabled = true
	go s.scheduleQueues()
	return nil
}
func (s *Server) closeImportQueues() {
	if s.queues == nil {
		return
	}
	s.queues.stopOnce.Do(func() { close(s.queues.stop) })
	<-s.queues.done
	s.queues.mu.Lock()
	qs := make([]*durableQueue, 0, len(s.queues.queues))
	for _, q := range s.queues.queues {
		qs = append(qs, q)
	}
	s.queues.mu.Unlock()
	for _, q := range qs {
		q.mu.Lock()
		for _, cancel := range q.cancels {
			cancel()
		}
		q.mu.Unlock()
	}
}
func queueContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Hour)
}
