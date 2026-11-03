package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) queueAvailable(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if s.queues == nil {
		http.Error(w, "durable_queues_unavailable", 503)
		return false
	}
	return true
}
func (s *Server) queueLookup(w http.ResponseWriter, r *http.Request) *durableQueue {
	if !s.queueAvailable(w) {
		return nil
	}
	s.queues.mu.Lock()
	q := s.queues.queues[r.PathValue("id")]
	s.queues.mu.Unlock()
	if q == nil || q.manifest.Owner != currentUser(r).ID {
		http.NotFound(w, r)
		return nil
	}
	return q
}
func (s *Server) retainedQueueItems(owner string) (int, int) {
	s.queues.mu.Lock()
	defer s.queues.mu.Unlock()
	return s.queues.ownerItems[owner], s.queues.totalItems
}
func (s *Server) createImportQueue(w http.ResponseWriter, r *http.Request) {
	if !s.queueAvailable(w) {
		return
	}
	var in queueRequest
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 12<<20))
	if readErr != nil || !validQueueJSON(body) {
		http.Error(w, "invalid_queue_request", 400)
		return
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid_queue_request", 400)
		return
	}
	if r.URL.Query().Get("space") != "" && r.URL.Query().Get("space") != "personal" {
		http.Error(w, "personal_space_required", 403)
		return
	}
	if err := validateQueueRequest(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	owner := currentUser(r).ID
	if !validQueueOwner(owner) {
		http.Error(w, "invalid_owner", http.StatusUnauthorized)
		return
	}
	id := queueIdentity(owner, in.SubmissionID)
	fp := queueFingerprint(in)
	m := s.queues
	m.admission.Lock()
	defer m.admission.Unlock()
	m.mu.Lock()
	q := m.queues[id]
	m.mu.Unlock()
	if q != nil {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.manifest.Fingerprint != fp {
			http.Error(w, "submission_changed", 409)
			return
		}
		if !time.Now().Before(q.manifest.ExpiresAt) {
			http.Error(w, "submission_expired", 410)
			return
		}
		if q.frozen {
			http.Error(w, "queue_state_unavailable", 503)
			return
		}
		writeJSON(w, 202, q.summary())
		return
	}
	owned, total := s.retainedQueueItems(owner)
	if owned+len(in.Sources) > queueOwnerItems || total+len(in.Sources) > queueProcessItems {
		http.Error(w, "queue_capacity", 429)
		return
	}
	now := time.Now().UTC()
	manifest := queueManifest{Schema: 1, ID: id, Owner: owner, Fingerprint: fp, AcceptedAt: now, ExpiresAt: now.Add(queueLifetime), Request: in}
	st := queueState{Schema: 1, ID: id, Owner: owner, Fingerprint: fp, Revision: 1, Control: "running", Items: []queueRecord{}, Discovered: make([]bool, len(in.Sources)), Targets: map[string]string{}}
	for i, src := range in.Sources {
		if src.Type == "direct" {
			dest := path.Join(in.Path, src.Name)
			if !strictQueuePath(dest, false) {
				http.Error(w, "invalid_destination", http.StatusUnprocessableEntity)
				return
			}
			if _, exists := st.Targets[dest]; exists {
				http.Error(w, "destination_collision", http.StatusUnprocessableEntity)
				return
			}
			st.Targets[dest] = "f|" + strconv.Itoa(i) + ":"
			st.Items = append(st.Items, queueRecord{QueueItem: QueueItem{ID: queueIdentity(id, strconv.Itoa(i)), QueueID: id, Position: len(st.Items), Name: src.Name, Path: dest, State: "queued"}, Source: i, ExpectedSize: -1, ExpectedSHA: src.SHA256})
			st.Discovered[i] = true
		} else {
			st.Walk = append(st.Walk, queueWalk{Source: i, Destination: in.Path, Total: -1})
		}
	}
	mb, _ := json.Marshal(manifest)
	sb, _ := json.Marshal(st)
	if len(sb) > queueSnapshotLimit {
		http.Error(w, "queue_capacity", 429)
		return
	}
	q = &durableQueue{manifest: manifest, state: st, frozen: true, busy: map[string]bool{}, cancels: map[string]func(){}, metadataBytes: len(mb) + len(sb), stateBytes: len(sb), progress: map[string]int64{}}
	if err := m.registerQueue(q); err != nil {
		http.Error(w, "queue_capacity", 429)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// Client disconnect cannot turn an accepted execution intent into request work.
	ctx, cancel := queueContext()
	defer cancel()
	if _, err := s.queueWrite(ctx, queueBase(manifest)+"/state.json", st, queueSnapshotLimit); err != nil {
		http.Error(w, "queue_state_unavailable", 503)
		return
	}
	if _, err := s.queueWrite(ctx, queueBase(manifest)+"/manifest.json", manifest, 12<<20); err != nil {
		http.Error(w, "queue_state_unavailable", 503)
		return
	}
	q.frozen = false
	writeJSON(w, 202, q.summary())
}
func queuePagination(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	offset, limit := 0, 100
	var err error
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			http.Error(w, "invalid_pagination", 400)
			return 0, 0, false
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 500 {
			http.Error(w, "invalid_pagination", 400)
			return 0, 0, false
		}
	}
	return offset, limit, true
}
func (s *Server) listImportQueues(w http.ResponseWriter, r *http.Request) {
	if !s.queueAvailable(w) {
		return
	}
	offset, limit, ok := queuePagination(w, r)
	if !ok {
		return
	}
	s.queues.mu.Lock()
	qs := []*durableQueue{}
	for _, q := range s.queues.queues {
		if q.manifest.Owner == currentUser(r).ID {
			qs = append(qs, q)
		}
	}
	s.queues.mu.Unlock()
	sort.Slice(qs, func(i, j int) bool { return qs[i].manifest.AcceptedAt.Before(qs[j].manifest.AcceptedAt) })
	out := []QueueSummary{}
	for _, q := range qs[min(offset, len(qs)):min(min(offset, len(qs))+limit, len(qs))] {
		q.mu.Lock()
		out = append(out, q.summary())
		q.mu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"queues": out, "total": len(qs), "offset": offset, "limit": limit})
}
func (s *Server) getQueueSubmission(w http.ResponseWriter, r *http.Request) {
	if !validHex(r.PathValue("submissionId"), 32) {
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("id", queueIdentity(currentUser(r).ID, r.PathValue("submissionId")))
	q := s.queueLookup(w, r)
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// A frozen placeholder may have no acceptance manifest at all. Submission
	// reconciliation must not turn an uncertain write into a successful ACK.
	if q.frozen {
		http.Error(w, "queue_state_unavailable", http.StatusServiceUnavailable)
		return
	}
	if !time.Now().Before(q.manifest.ExpiresAt) {
		http.Error(w, "submission_expired", http.StatusGone)
		return
	}
	writeJSON(w, http.StatusOK, q.summary())
}
func (s *Server) getImportQueue(w http.ResponseWriter, r *http.Request) {
	q := s.queueLookup(w, r)
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !time.Now().Before(q.manifest.ExpiresAt) {
		http.Error(w, "submission_expired", 410)
		return
	}
	writeJSON(w, 200, q.summary())
}
func (s *Server) listQueueItems(w http.ResponseWriter, r *http.Request) {
	q := s.queueLookup(w, r)
	if q == nil {
		return
	}
	offset, limit, ok := queuePagination(w, r)
	if !ok {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []QueueItem{}
	for _, it := range q.state.Items[min(offset, len(q.state.Items)):min(min(offset, len(q.state.Items))+limit, len(q.state.Items))] {
		out = append(out, q.publicItem(it))
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": len(q.state.Items), "offset": offset, "limit": limit})
}
func (s *Server) controlImportQueue(w http.ResponseWriter, r *http.Request) {
	q := s.queueLookup(w, r)
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.frozen {
		http.Error(w, "queue_state_unavailable", 503)
		return
	}
	if !time.Now().Before(q.manifest.ExpiresAt) {
		http.Error(w, "submission_expired", 410)
		return
	}
	next := cloneQueueState(q.state)
	action := r.PathValue("action")
	switch action {
	case "pause":
		if next.Control != "cancelled" {
			next.Control = "paused"
		}
	case "resume":
		if next.Control != "cancelled" {
			next.Control = "running"
		} else {
			http.Error(w, "queue_cancelled", 409)
			return
		}
	case "cancel":
		next.Control = "cancelled"
		for i := range next.Items {
			it := &next.Items[i]
			if !queueTerminal(it.State) && it.State != "saving" {
				it.State = "cancelled"
				it.ErrorCode = ""
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	ctx, cancel := queueContext()
	defer cancel()
	if s.commitQueue(ctx, q, next) != nil {
		http.Error(w, "queue_state_unavailable", 503)
		return
	}
	if action == "cancel" {
		for id, cancel := range q.cancels {
			if id == "inventory" || queueRecordByID(q.state, id).State != "saving" {
				cancel()
			}
		}
	}
	writeJSON(w, 202, q.summary())
}
func queueRecordByID(st queueState, id string) queueRecord {
	for _, it := range st.Items {
		if it.ID == id {
			return it
		}
	}
	return queueRecord{}
}
func (s *Server) controlQueueItem(w http.ResponseWriter, r *http.Request) {
	q := s.queueLookup(w, r)
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.frozen {
		http.Error(w, "queue_state_unavailable", 503)
		return
	}
	if !time.Now().Before(q.manifest.ExpiresAt) {
		http.Error(w, "submission_expired", 410)
		return
	}
	next := cloneQueueState(q.state)
	index := -1
	for i := range next.Items {
		if next.Items[i].ID == r.PathValue("itemId") {
			index = i
			break
		}
	}
	if index < 0 {
		http.NotFound(w, r)
		return
	}
	it := &next.Items[index]
	status := 200
	if r.Method == http.MethodDelete {
		if it.State == "saving" {
			http.Error(w, "saving_may_finish", 409)
			return
		}
		if !queueTerminal(it.State) {
			it.State = "cancelled"
			it.ErrorCode = ""
		}
	} else {
		if it.State != "failed" || next.Control == "cancelled" || len(it.History) >= 64 {
			http.Error(w, "retry_not_allowed", 409)
			return
		}
		it.History = append(it.History, it.Attempts)
		it.Generation++
		it.Attempts = 0
		it.State = "queued"
		it.ErrorCode = ""
		it.NextAttemptAt = nil
		status = 202
		// Original execution deadline is deliberately retained.
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.imports.options.Timeout)
	defer cancel()
	if s.commitQueue(ctx, q, next) != nil {
		http.Error(w, "queue_state_unavailable", 503)
		return
	}
	if r.Method == http.MethodDelete {
		if cancel := q.cancels[it.ID]; cancel != nil {
			cancel()
		}
	}
	writeJSON(w, status, it.QueueItem)
}

func (s *Server) getQueueRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/import-queues/"), "/")
	if len(parts) == 2 && parts[0] == "submissions" {
		r.SetPathValue("submissionId", parts[1])
		s.getQueueSubmission(w, r)
		return
	}
	if len(parts) == 1 {
		r.SetPathValue("id", parts[0])
		s.getImportQueue(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "items" {
		r.SetPathValue("id", parts[0])
		s.listQueueItems(w, r)
		return
	}
	http.NotFound(w, r)
}
