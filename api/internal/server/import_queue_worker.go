package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"

	"remote-drive/api/internal/storage"
)

func (s *Server) scheduleQueues() {
	defer close(s.queues.done)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.queues.stop:
			return
		case <-tick.C:
			s.dispatchQueueWork()
		}
	}
}
func (s *Server) dispatchQueueWork() {
	m := s.queues
	m.mu.Lock()
	qs := make([]*durableQueue, 0, len(m.queues))
	for _, q := range m.queues {
		qs = append(qs, q)
	}
	last := m.lastOwner
	m.mu.Unlock()
	sort.Slice(qs, func(i, j int) bool {
		a, b := qs[i].manifest, qs[j].manifest
		if a.Owner != b.Owner {
			ar, br := a.Owner > last, b.Owner > last
			if ar != br {
				return ar
			}
			return a.Owner < b.Owner
		}
		return a.AcceptedAt.Before(b.AcceptedAt)
	})
	demand := false
	for _, q := range qs {
		if !q.mu.TryLock() {
			continue
		}
		if q.frozen {
			q.mu.Unlock()
			continue
		}
		now := time.Now()
		id, target := "", ""
		// Saving always reconciles read-only, even after pause/cancel/expiry.
		for _, it := range q.state.Items {
			if it.State == "saving" && !q.busy[it.ID] {
				id = it.ID
				target = resolvePersonalPath(User{ID: q.manifest.Owner}, it.Path).Key
				break
			}
		}
		if id == "" && now.Before(q.manifest.ExpiresAt) && q.state.Control == "running" {
			for _, it := range q.state.Items {
				if q.busy[it.ID] || it.State != "queued" && it.State != "retry_wait" || it.NextAttemptAt != nil && now.Before(*it.NextAttemptAt) {
					continue
				}
				candidate := resolvePersonalPath(User{ID: q.manifest.Owner}, it.Path).Key
				s.imports.mu.Lock()
				locked := s.imports.destinations[candidate] != ""
				s.imports.mu.Unlock()
				if locked {
					continue
				}
				id = it.ID
				target = candidate
				break
			}
			if id == "" && !q.busy["inventory"] {
				for _, walk := range q.state.Walk {
					if !now.Before(walk.NextAttemptAt) {
						id = "inventory"
						break
					}
				}
			}
		}
		if id == "" && !now.Before(q.manifest.ExpiresAt) && q.state.Control != "expired" {
			id = "expiry"
		}
		if id == "" {
			q.mu.Unlock()
			continue
		}
		demand = true
		im := s.imports
		im.mu.Lock()
		if im.closed || im.ownerActive[q.manifest.Owner] >= 2 {
			im.mu.Unlock()
			q.mu.Unlock()
			continue
		}
		select {
		case im.slots <- struct{}{}:
		default:
			im.mu.Unlock()
			q.mu.Unlock()
			continue
		}
		if target != "" && im.destinations[target] != "" {
			<-im.slots
			im.mu.Unlock()
			q.mu.Unlock()
			continue
		}
		if target != "" {
			im.destinations[target] = id
		}
		im.ownerActive[q.manifest.Owner]++
		im.workers.Add(1)
		im.mu.Unlock()
		q.busy[id] = true
		ctx, cancel := context.WithDeadline(context.Background(), q.manifest.ExpiresAt)
		q.cancels[id] = cancel
		q.mu.Unlock()
		m.mu.Lock()
		m.lastOwner = q.manifest.Owner
		m.mu.Unlock()
		go func(q *durableQueue, id, target string) {
			defer func() {
				cancel()
				q.mu.Lock()
				delete(q.busy, id)
				delete(q.cancels, id)
				delete(q.progress, id)
				q.mu.Unlock()
				im.mu.Lock()
				if target != "" {
					delete(im.destinations, target)
				}
				im.ownerActive[q.manifest.Owner]--
				<-im.slots
				im.mu.Unlock()
				im.workers.Done()
			}()
			switch id {
			case "inventory":
				s.discoverQueue(ctx, q)
			case "expiry":
				q.mu.Lock()
				next := cloneQueueState(q.state)
				next.Control = "expired"
				for i := range next.Items {
					if !queueTerminal(next.Items[i].State) && next.Items[i].State != "saving" {
						next.Items[i].State = "expired"
					}
				}
				commitCtx, c := queueContext()
				_ = s.commitQueue(commitCtx, q, next)
				c()
				q.mu.Unlock()
			default:
				s.executeQueueItem(ctx, q, id, target)
			}
		}(q, id, target)
	}
	s.imports.mu.Lock()
	s.imports.queueDemand = demand
	s.imports.mu.Unlock()
}
func (s *Server) queueItemTransition(ctx context.Context, q *durableQueue, id string, change func(*queueRecord)) error {
	next := cloneQueueState(q.state)
	for i := range next.Items {
		if next.Items[i].ID == id {
			change(&next.Items[i])
			err := s.commitQueue(ctx, q, next)
			if errors.Is(err, errQueueCapacity) {
				q.frozen = true
				q.blockedCode = "queue_capacity"
			}
			return err
		}
	}
	return errQueueState
}
func (s *Server) failQueueItem(q *durableQueue, id, code string, retry bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	it := queueRecordByID(q.state, id)
	if q.frozen || queueTerminal(it.State) {
		return
	}
	ctx, cancel := queueContext()
	defer cancel()
	_ = s.queueItemTransition(ctx, q, id, func(it *queueRecord) {
		it.ErrorCode = code
		it.State = "failed"
		it.Bytes = 0
		if retry && it.Attempts < 3 && time.Now().Before(it.Deadline) && time.Now().Before(q.manifest.ExpiresAt) && q.state.Control != "cancelled" {
			it.State = "retry_wait"
			next := time.Now().Add(time.Duration(1<<it.Attempts) * time.Second)
			it.NextAttemptAt = &next
		}
	})
}
func (s *Server) executeQueueItem(parent context.Context, q *durableQueue, id, target string) {
	q.mu.Lock()
	it := queueRecordByID(q.state, id)
	if it.State == "saving" {
		q.mu.Unlock()
		s.reconcileQueueItem(q, it, target)
		return
	}
	if q.frozen || q.state.Control != "running" || queueTerminal(it.State) {
		q.mu.Unlock()
		return
	}
	deadline := it.Deadline
	if deadline.IsZero() {
		deadline = minQueueTime(time.Now().Add(s.imports.options.Timeout), q.manifest.ExpiresAt)
	}
	commitCtx, c := queueContext()
	err := s.queueItemTransition(commitCtx, q, id, func(it *queueRecord) {
		it.Deadline = deadline
		if !time.Now().Before(deadline) || it.Attempts >= 3 {
			it.State = "failed"
			it.ErrorCode = "deadline_or_attempts_exceeded"
			return
		}
		it.Attempts++
		it.TotalAttempts++
		it.State = "resolving"
		it.NextAttemptAt = nil
		it.ErrorCode = ""
		it.Bytes = 0
		it.Scratch = ".queue-" + q.manifest.ID + "-" + it.ID + "-" + strconv.Itoa(it.TotalAttempts)
	})
	c()
	it = queueRecordByID(q.state, id)
	q.mu.Unlock()
	if err != nil || it.State == "failed" {
		return
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	if it.Directory {
		s.publishQueueDirectory(ctx, q, it, target)
		return
	}
	if it.ExpectedSize > s.imports.options.MaxFileBytes {
		s.failQueueItem(q, id, "size_limit", false)
		return
	}
	source := q.manifest.Request.Sources[it.Source]
	raw := source.URL
	if source.Type == "yandex" {
		raw, err = s.resolveYandex(ctx, source, it)
		if err != nil {
			s.failQueueItem(q, id, queueSourceError(err), !errors.Is(err, errSourceChanged))
			return
		}
	}
	u, err := url.Parse(raw)
	if err != nil || validateRemoteURL(u) != nil {
		s.failQueueItem(q, id, "source_unavailable", false)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		s.failQueueItem(q, id, "source_unavailable", false)
		return
	}
	req.Header.Set("User-Agent", "Remote-Drive/1.0")
	response, err := s.imports.openImportResponse(ctx, req, it.ExpectedSHA != "")
	if err != nil {
		s.failQueueItem(q, id, "source_unavailable", true)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		s.failQueueItem(q, id, "source_unavailable", true)
		return
	}
	if response.ContentLength > s.imports.options.MaxFileBytes || it.ExpectedSize >= 0 && response.ContentLength >= 0 && response.ContentLength != it.ExpectedSize {
		s.failQueueItem(q, id, "source_changed_or_size_limit", false)
		return
	}
	expectedSize := it.ExpectedSize
	if expectedSize < 0 {
		expectedSize = response.ContentLength
	}
	if _, statErr := s.store.StatObject(ctx, target); statErr == nil {
		if q.manifest.Request.Existing == "skip_verified" && it.ExpectedSHA != "" && expectedSize >= 0 && s.queueFullHash(ctx, target, expectedSize, it.ExpectedSHA) {
			q.mu.Lock()
			cc, cl := queueContext()
			_ = s.queueItemTransition(cc, q, id, func(v *queueRecord) {
				if v.State == "cancelled" {
					return
				}
				v.State = "complete"
				v.Total = expectedSize
				v.Bytes = expectedSize
				v.SHA256 = it.ExpectedSHA
				v.Reused = true
			})
			cl()
			q.mu.Unlock()
			return
		}
		s.failQueueItem(q, id, "destination_conflict", false)
		return
	} else if !errors.Is(statErr, storage.ErrNotFound) {
		s.failQueueItem(q, id, "destination_unavailable", true)
		return
	}
	reservation := response.ContentLength
	if reservation < 0 {
		reservation = s.imports.options.MaxFileBytes
	}
	if !s.imports.reserveStaging(reservation) {
		s.failQueueItem(q, id, "staging_unavailable", true)
		return
	}
	defer s.imports.releaseStaging(reservation)
	q.mu.Lock()
	cc, cl := queueContext()
	err = s.queueItemTransition(cc, q, id, func(v *queueRecord) {
		if v.State != "cancelled" {
			v.State = "downloading"
			v.Total = max(0, response.ContentLength)
		}
	})
	cl()
	cancelled := queueRecordByID(q.state, id).State == "cancelled"
	q.mu.Unlock()
	if err != nil || cancelled {
		return
	}
	temp, err := uploadOpenFile(s.imports.root, it.Scratch, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		s.failQueueItem(q, id, "scratch_unavailable", true)
		return
	}
	defer func() { temp.Close(); _ = uploadUnlink(s.imports.root, it.Scratch, false) }()
	h := sha256.New()
	total := int64(0)
	buf := make([]byte, 64<<10)
	reader := io.LimitReader(response.Body, s.imports.options.MaxFileBytes+1)
	for {
		n, re := reader.Read(buf)
		if n > 0 {
			total += int64(n)
			free, fe := s.imports.freeBytes()
			if total > s.imports.options.MaxFileBytes || response.ContentLength >= 0 && total > response.ContentLength || fe != nil || free < s.imports.options.ReserveBytes+int64(n) {
				s.failQueueItem(q, id, "size_or_space_limit", false)
				return
			}
			if _, err = temp.Write(buf[:n]); err != nil {
				s.failQueueItem(q, id, "scratch_unavailable", true)
				return
			}
			_, _ = h.Write(buf[:n])
			q.mu.Lock()
			q.progress[id] = total
			q.mu.Unlock()
		}
		if re == io.EOF {
			break
		}
		if re != nil || ctx.Err() != nil {
			s.failQueueItem(q, id, "source_unavailable", true)
			return
		}
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if response.ContentLength >= 0 && total != response.ContentLength || it.ExpectedSize >= 0 && total != it.ExpectedSize || it.ExpectedSHA != "" && digest != it.ExpectedSHA {
		s.failQueueItem(q, id, "source_integrity", false)
		return
	}
	if _, err = temp.Seek(0, io.SeekStart); err != nil {
		s.failQueueItem(q, id, "scratch_unavailable", true)
		return
	}
	if ctx.Err() != nil {
		s.failQueueItem(q, id, "deadline_exceeded", false)
		return
	}
	if _, err = s.store.StatObject(ctx, target); !errors.Is(err, storage.ErrNotFound) {
		s.failQueueItem(q, id, "destination_conflict", false)
		return
	}
	q.mu.Lock()
	if q.frozen || queueRecordByID(q.state, id).State == "cancelled" {
		q.mu.Unlock()
		return
	}
	cc, cl = queueContext()
	err = s.queueItemTransition(cc, q, id, func(v *queueRecord) { v.State = "saving"; v.Bytes = total; v.Total = total; v.SHA256 = digest })
	cl()
	q.mu.Unlock()
	if err != nil {
		return
	} // no Put without a durable saving checkpoint
	err = ctx.Err()
	if err == nil {
		err = s.store.PutObject(ctx, target, temp, total, response.Header.Get("Content-Type"))
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	cc, cl = queueContext()
	defer cl()
	_ = s.queueItemTransition(cc, q, id, func(v *queueRecord) {
		if err != nil {
			v.State = "needs_verification"
			v.ErrorCode = "publication_uncertain"
		} else {
			v.State = "complete"
			v.ErrorCode = ""
		}
	})
}
func minQueueTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (s *Server) queueFullHash(ctx context.Context, target string, size int64, digest string) bool {
	f, info, err := s.store.GetObject(ctx, target)
	if err != nil {
		return false
	}
	defer f.Close()
	if info.Size != size || size < 0 || size > s.imports.options.MaxFileBytes {
		return false
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, size+1))
	return err == nil && n == size && hex.EncodeToString(h.Sum(nil)) == digest
}
func (s *Server) reconcileQueueItem(q *durableQueue, it queueRecord, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.imports.options.Timeout)
	defer cancel()
	match := false
	if !it.Directory {
		match = s.queueFullHash(ctx, target, it.Total, it.SHA256)
	} // directory publication is explicitly uncertain
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = s.queueItemTransition(ctx, q, it.ID, func(v *queueRecord) {
		if match {
			v.State = "complete"
			v.ErrorCode = ""
		} else {
			v.State = "needs_verification"
			v.ErrorCode = "publication_uncertain"
		}
	})
}
func (s *Server) publishQueueDirectory(ctx context.Context, q *durableQueue, it queueRecord, target string) {
	if obj, e := s.store.StatObject(ctx, target); e == nil && !obj.IsDir {
		s.failQueueItem(q, it.ID, "destination_conflict", false)
		return
	} else if e != nil && !errors.Is(e, storage.ErrNotFound) {
		s.failQueueItem(q, it.ID, "destination_unavailable", true)
		return
	}
	q.mu.Lock()
	if queueRecordByID(q.state, it.ID).State == "cancelled" {
		q.mu.Unlock()
		return
	}
	cc, c := queueContext()
	err := s.queueItemTransition(cc, q, it.ID, func(v *queueRecord) { v.State = "saving" })
	c()
	q.mu.Unlock()
	if err != nil {
		return
	}
	err = ctx.Err()
	if err == nil {
		err = s.store.PutDirectory(ctx, target)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	cc, c = queueContext()
	defer c()
	_ = s.queueItemTransition(cc, q, it.ID, func(v *queueRecord) {
		if err == nil {
			v.State = "complete"
		} else {
			v.State = "needs_verification"
			v.ErrorCode = "publication_uncertain"
		}
	})
}
