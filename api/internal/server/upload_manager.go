package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maxUploadRecords = 1024

type uploadSession struct {
	record   uploadRecord
	inflight map[int]bool
}

type uploadManager struct {
	mu       sync.Mutex
	disk     *uploadDisk
	options  UploadOptions
	sessions map[string]*uploadSession
	now      func() time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	closed   bool
}

type uploadError struct {
	code    int
	message string
}

func (e *uploadError) Error() string { return e.message }

func uploadFail(code int, message string) error { return &uploadError{code, message} }

func newUploadManager(options UploadOptions) (*uploadManager, error) {
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = defaultUploadMax
	}
	if options.BudgetBytes == 0 {
		options.BudgetBytes = defaultUploadBudget
	}
	if options.MaxFileBytes < 1 || options.MaxFileBytes > 8192*uploadChunkSize || options.BudgetBytes < options.MaxFileBytes || options.ReserveBytes < 0 || options.Root == "" {
		return nil, errors.New("invalid upload staging configuration")
	}
	disk, err := openUploadDisk(options.Root)
	if err != nil {
		return nil, errors.New("upload staging unavailable or unsafe")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &uploadManager{disk: disk, options: options, sessions: make(map[string]*uploadSession), now: time.Now, ctx: ctx, cancel: cancel}
	ids, err := disk.ids()
	if err == nil {
		for _, id := range ids {
			var record uploadRecord
			record, err = disk.load(id)
			if errors.Is(err, os.ErrNotExist) {
				if err = disk.discardUninitialized(id); err == nil {
					continue
				}
			}
			if err != nil || !validUploadRecord(record) {
				err = errors.New("invalid upload recovery metadata")
				break
			}
			if record.Status.State == "committing" {
				record.Status.State = "uncertain"
				record.Status.Error = "Publication was interrupted. Verify the destination before starting another upload."
				if err = disk.save(record); err != nil {
					break
				}
			}
			m.sessions[id] = &uploadSession{record: record, inflight: make(map[int]bool)}
		}
	}
	if err != nil {
		cancel()
		disk.close()
		return nil, errors.New("upload staging recovery failed; existing data was preserved")
	}
	m.sweepLocked()
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.mu.Lock()
				m.sweepLocked()
				m.mu.Unlock()
			}
		}
	}()
	return m, nil
}

func (m *uploadManager) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.workers.Wait()
	m.disk.close()
}

func validUploadRecord(record uploadRecord) bool {
	id, err := strconv.ParseInt(record.Owner, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != record.Owner || !validUploadID(record.Status.ID) || record.Version != 1 {
		return false
	}
	s := record.Status
	if s.Size < 0 || s.Size > 8192*uploadChunkSize || s.ChunkSize != uploadChunkSize || s.ExpiresAt.IsZero() || s.Parts == nil {
		return false
	}
	switch s.State {
	case "uploading", "committing", "complete", "cancelled", "expired", "uncertain":
	default:
		return false
	}
	for index, part := range s.Parts {
		if index < 0 || int64(index) >= uploadPartCount(s.Size) || part.Size != uploadExpectedPart(s.Size, index) || !validUploadHash(part.SHA256) {
			return false
		}
	}
	if !validDownloadName(s.Name) || !validUploadPath(s.Path) || len(record.ContentType) > 255 || strings.ContainsAny(record.ContentType, "\r\n") {
		return false
	}
	if _, _, err := mime.ParseMediaType(record.ContentType); err != nil {
		return false
	}
	if s.Space != "personal" {
		if _, ok := shareIDFromSpace(s.Space); !ok {
			return false
		}
	}
	segments := strings.Split(record.Key, "/")
	if len(segments) < 3 || segments[0] != "users" || segments[len(segments)-1] == "" {
		return false
	}
	targetOwner, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil || targetOwner <= 0 || strconv.FormatInt(targetOwner, 10) != segments[1] || !validUploadPath(record.Key) {
		return false
	}
	return s.Space != "personal" || segments[1] == record.Owner
}

func validUploadHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func uploadPartCount(size int64) int64 { return (size + uploadChunkSize - 1) / uploadChunkSize }

func uploadExpectedPart(size int64, index int) int64 {
	remaining := size - int64(index)*uploadChunkSize
	if remaining > uploadChunkSize {
		return uploadChunkSize
	}
	return remaining
}

func copyUploadStatus(status UploadStatus) UploadStatus {
	parts := make(map[int]UploadPart, len(status.Parts))
	for index, part := range status.Parts {
		parts[index] = part
	}
	status.Parts = parts
	return status
}

func (m *uploadManager) ownedLocked(owner, id string) (*uploadSession, error) {
	if m.closed {
		return nil, uploadFail(http.StatusServiceUnavailable, "Upload service is stopping.")
	}
	session := m.sessions[id]
	if session == nil || session.record.Owner != owner {
		return nil, uploadFail(http.StatusNotFound, "Upload session not found.")
	}
	return session, nil
}

func (m *uploadManager) create(ctx context.Context, owner, key string, request createUploadRequest) (UploadStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return UploadStatus{}, uploadFail(408, "Upload creation was interrupted.")
	}
	if m.closed {
		return UploadStatus{}, uploadFail(503, "Upload service is stopping.")
	}
	m.sweepLocked()
	if request.ClientID != "" && !validUploadID(request.ClientID) {
		return UploadStatus{}, uploadFail(400, "Invalid upload client identifier.")
	}
	if request.ClientID != "" {
		if existing := m.sessions[request.ClientID]; existing != nil {
			r := existing.record
			if request.Size == nil || r.Owner != owner || r.Key != key || r.Status.Size != *request.Size || r.Status.Name != request.Name || r.Status.Path != request.Path || r.Status.Space != request.Space {
				return UploadStatus{}, uploadFail(409, "Upload identifier conflicts with another session.")
			}
			return copyUploadStatus(r.Status), nil
		}
	}
	if request.Size == nil || *request.Size < 0 || *request.Size > m.options.MaxFileBytes {
		return UploadStatus{}, uploadFail(413, "File exceeds the configured upload limit.")
	}
	var reserved, outstanding int64
	active, ownActive := 0, 0
	for _, session := range m.sessions {
		r := session.record
		if !r.Released {
			reserved += r.Status.Size
		}
		if r.Status.State == "uploading" {
			pending := r.Status.Size
			for _, part := range r.Status.Parts {
				pending -= part.Size
			}
			outstanding += pending
		}
		if r.Status.State == "uploading" || r.Status.State == "committing" || r.Status.State == "uncertain" {
			active++
			if r.Owner == owner {
				ownActive++
			}
		}
	}
	if ownActive >= 2 || active >= 4 || len(m.sessions) >= maxUploadRecords {
		return UploadStatus{}, uploadFail(429, "Too many upload sessions. Finish or cancel an existing upload.")
	}
	free, err := uploadFreeBytes(m.disk.root)
	if err != nil || *request.Size > m.options.BudgetBytes-reserved || free < m.options.ReserveBytes || outstanding > free-m.options.ReserveBytes || *request.Size > free-m.options.ReserveBytes-outstanding {
		return UploadStatus{}, uploadFail(507, "Insufficient upload staging space.")
	}
	id := request.ClientID
	if id == "" {
		id = uuid.NewString()
	}
	record := uploadRecord{Version: 1, Owner: owner, Key: key, ContentType: request.ContentType, Status: UploadStatus{ID: id, Name: request.Name, Path: request.Path, Space: request.Space, Size: *request.Size, ChunkSize: uploadChunkSize, Parts: make(map[int]UploadPart), State: "uploading", ExpiresAt: m.now().UTC().Add(uploadSessionTTL)}}
	if err := m.disk.create(record); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return UploadStatus{}, uploadFail(503, "Could not persist upload session; retry with the same identifier.")
		}
		// A prior create can have published its complete manifest but failed the
		// final directory sync. Reconcile only this exact immutable attempt and
		// sync again before acknowledging it; never replace an existing record.
		recovered, loadErr := m.disk.load(id)
		if loadErr != nil || !validUploadRecord(recovered) || recovered.Owner != owner || recovered.Key != key || recovered.Status.Size != *request.Size || recovered.Status.Name != request.Name || recovered.Status.Path != request.Path || recovered.Status.Space != request.Space || m.disk.syncCreation() != nil {
			return UploadStatus{}, uploadFail(503, "Could not reconcile upload creation; existing data was preserved.")
		}
		if recovered.Status.State == "committing" {
			recovered.Status.State = "uncertain"
			recovered.Status.Error = "Publication was interrupted. Verify the destination before starting another upload."
			if m.disk.save(recovered) != nil {
				return UploadStatus{}, uploadFail(503, "Could not persist upload recovery.")
			}
		}
		record = recovered
	}
	m.sessions[record.Status.ID] = &uploadSession{record: record, inflight: make(map[int]bool)}
	return copyUploadStatus(record.Status), nil
}

func (m *uploadManager) status(owner, id string) (UploadStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.ownedLocked(owner, id)
	if err != nil {
		return UploadStatus{}, err
	}
	return copyUploadStatus(session.record.Status), nil
}

func (m *uploadManager) list(owner string) []UploadStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []UploadStatus{}
	for _, session := range m.sessions {
		if session.record.Owner == owner {
			out = append(out, copyUploadStatus(session.record.Status))
		}
	}
	return out
}

func (m *uploadManager) acceptPart(ctx context.Context, owner, id string, index int, digest string, body io.Reader) (UploadStatus, error) {
	m.mu.Lock()
	session, err := m.ownedLocked(owner, id)
	if err != nil {
		m.mu.Unlock()
		return UploadStatus{}, err
	}
	r := session.record
	if r.Status.State != "uploading" || !m.now().Before(r.Status.ExpiresAt) {
		m.mu.Unlock()
		return UploadStatus{}, uploadFail(409, "Upload is not accepting parts.")
	}
	if index < 0 || int64(index) >= uploadPartCount(r.Status.Size) || !validUploadHash(digest) {
		m.mu.Unlock()
		return UploadStatus{}, uploadFail(400, "Invalid part index or checksum.")
	}
	if old, ok := r.Status.Parts[index]; ok && old.SHA256 != digest {
		m.mu.Unlock()
		return UploadStatus{}, uploadFail(409, "This part already has a different checksum.")
	}
	if len(session.inflight) >= 2 || session.inflight[index] {
		m.mu.Unlock()
		return UploadStatus{}, uploadFail(429, "Part is busy; retry later.")
	}
	free, err := uploadFreeBytes(m.disk.root)
	if err != nil || free < m.options.ReserveBytes+uploadExpectedPart(r.Status.Size, index) {
		m.mu.Unlock()
		return UploadStatus{}, uploadFail(507, "Insufficient upload staging space.")
	}
	session.inflight[index] = true
	m.workers.Add(1)
	m.mu.Unlock()
	defer m.workers.Done()

	part, writeErr := m.writePart(ctx, r, index, digest, body)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(session.inflight, index)
	if writeErr != nil {
		return UploadStatus{}, writeErr
	}
	if m.closed || session.record.Status.State != "uploading" {
		m.cleanupLocked(session)
		return UploadStatus{}, uploadFail(409, "Upload was stopped.")
	}
	next := session.record
	next.Status = copyUploadStatus(next.Status)
	next.Status.Parts[index] = part
	next.Status.ExpiresAt = m.now().UTC().Add(uploadSessionTTL)
	if err := m.disk.save(next); err != nil {
		return UploadStatus{}, uploadFail(503, "Could not persist part receipt; retry the part.")
	}
	session.record = next
	return copyUploadStatus(next.Status), nil
}

func (m *uploadManager) writePart(ctx context.Context, record uploadRecord, index int, digest string, body io.Reader) (UploadPart, error) {
	dir, err := m.disk.session(record.Status.ID)
	if err != nil {
		return UploadPart{}, uploadFail(503, "Upload staging unavailable.")
	}
	defer dir.Close()
	temporary := "tmp-" + uuid.NewString()
	file, err := uploadOpenFile(dir, temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return UploadPart{}, uploadFail(507, "Could not stage upload part.")
	}
	defer uploadUnlink(dir, temporary, false)
	hash := sha256.New()
	expected := uploadExpectedPart(record.Status.Size, index)
	n, err := io.Copy(io.MultiWriter(file, hash), &uploadContextReader{ctx: ctx, reader: io.LimitReader(body, expected+1)})
	if err != nil || ctx.Err() != nil || n != expected || hex.EncodeToString(hash.Sum(nil)) != digest {
		file.Close()
		return UploadPart{}, uploadFail(400, "Part length or checksum mismatch; retry this part.")
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return UploadPart{}, uploadFail(507, "Could not persist upload part.")
	}
	if err := file.Close(); err != nil {
		return UploadPart{}, uploadFail(507, "Could not persist upload part.")
	}
	if err := uploadRename(dir, temporary, uploadPartName(index)); err != nil {
		return UploadPart{}, uploadFail(503, "Could not publish part receipt.")
	}
	if err := dir.Sync(); err != nil {
		return UploadPart{}, uploadFail(503, "Could not persist upload part.")
	}
	return UploadPart{Size: expected, SHA256: digest}, nil
}

func (m *uploadManager) cancelSession(owner, id string) (UploadStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.ownedLocked(owner, id)
	if err != nil {
		return UploadStatus{}, err
	}
	if session.record.Status.State == "committing" || session.record.Status.State == "complete" {
		return UploadStatus{}, uploadFail(409, "File is saving or already saved; cancellation cannot undo publication.")
	}
	next := session.record
	next.Status.State = "cancelled"
	if err := m.disk.save(next); err != nil {
		return UploadStatus{}, uploadFail(503, "Could not persist cancellation.")
	}
	session.record = next
	m.cleanupLocked(session)
	return copyUploadStatus(session.record.Status), nil
}

func (m *uploadManager) cleanupLocked(session *uploadSession) {
	r := session.record
	if len(session.inflight) != 0 || r.Released || r.Status.State == "uploading" || r.Status.State == "committing" || r.Status.State == "uncertain" {
		return
	}
	if err := m.disk.removeFiles(r.Status.ID, false); err != nil {
		return // Keep the reservation charged until physical cleanup succeeds.
	}
	r.Released = true
	r.Status = copyUploadStatus(r.Status)
	r.Status.Parts = make(map[int]UploadPart)
	if m.disk.save(r) == nil {
		session.record = r
	}
}

func (m *uploadManager) sweepLocked() {
	for id, session := range m.sessions {
		r := session.record
		if r.Status.State != "committing" && len(session.inflight) == 0 && !m.now().Before(r.Status.ExpiresAt) {
			if r.Status.State == "uploading" || r.Status.State == "uncertain" {
				r.Status.State = "expired"
				if m.disk.save(r) != nil {
					continue
				}
				session.record = r
			}
			m.cleanupLocked(session)
			if session.record.Released && m.disk.removeFiles(id, true) == nil {
				delete(m.sessions, id)
			}
		} else {
			m.cleanupLocked(session)
		}
	}
}

type uploadContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *uploadContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (m *uploadManager) complete(owner, id string, publish func(context.Context, uploadRecord, io.Reader) error) (UploadStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.ownedLocked(owner, id)
	if err != nil {
		return UploadStatus{}, err
	}
	r := session.record
	if r.Status.State == "complete" || r.Status.State == "committing" {
		return copyUploadStatus(r.Status), nil
	}
	if r.Status.State != "uploading" || !m.now().Before(r.Status.ExpiresAt) || len(session.inflight) != 0 || int64(len(r.Status.Parts)) != uploadPartCount(r.Status.Size) {
		return UploadStatus{}, uploadFail(409, "Upload cannot finish: parts missing, busy, expired or publication uncertain.")
	}
	// Serialize this manager's final publication to the same resolved key. Other
	// existing writers retain their established replacement semantics.
	for _, other := range m.sessions {
		if other != session && other.record.Key == r.Key && other.record.Status.State == "committing" {
			return UploadStatus{}, uploadFail(409, "Another upload is saving to this destination.")
		}
	}
	r.Status.State = "committing"
	r.Status.ExpiresAt = m.now().UTC().Add(uploadSessionTTL)
	if err := m.disk.save(r); err != nil {
		return UploadStatus{}, uploadFail(503, "Could not persist completion request.")
	}
	session.record = r
	m.workers.Add(1)
	go m.commit(session, r, publish)
	return copyUploadStatus(r.Status), nil
}

func (m *uploadManager) commit(session *uploadSession, record uploadRecord, publish func(context.Context, uploadRecord, io.Reader) error) {
	defer m.workers.Done()
	ctx, cancel := context.WithTimeout(m.ctx, uploadCommitTimeout)
	defer cancel()
	hash := sha256.New()
	err := m.readParts(ctx, record, hash)
	if err == nil {
		record.Status.SHA256 = hex.EncodeToString(hash.Sum(nil))
		m.mu.Lock()
		err = m.disk.save(record)
		if err == nil {
			session.record = record
		}
		m.mu.Unlock()
	}
	if err == nil {
		// io.Pipe bounds memory and propagates integrity/IO failure into the
		// Store's existing atomic publication, preserving an old object on error.
		reader, writer := io.Pipe()
		written := make(chan error, 1)
		go func() {
			err := m.readParts(ctx, record, writer)
			_ = writer.CloseWithError(err)
			written <- err
		}()
		err = publish(ctx, record, reader)
		_ = reader.CloseWithError(err)
		readErr := <-written
		if err == nil {
			err = readErr
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		record.Status.State = "uncertain"
		record.Status.Error = "Saving failed or was interrupted. Verify the destination before starting another upload."
	} else {
		record.Status.State = "complete"
		record.Status.Error = ""
	}
	if m.disk.save(record) != nil {
		record.Status.State = "uncertain"
		record.Status.Error = "Publication receipt could not be persisted. Verify the destination before retrying."
	}
	session.record = record
	m.cleanupLocked(session)
}

func (m *uploadManager) readParts(ctx context.Context, record uploadRecord, target io.Writer) error {
	for index := 0; int64(index) < uploadPartCount(record.Status.Size); index++ {
		part := record.Status.Parts[index]
		file, err := m.disk.part(record.Status.ID, index)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || info.Size() != part.Size {
			file.Close()
			return errors.New("staged part length changed")
		}
		// Verify a bounded part BEFORE exposing its bytes to a sized Store reader:
		// some stores can commit after exactly Size bytes without asking for EOF.
		data, err := io.ReadAll(&uploadContextReader{ctx: ctx, reader: io.LimitReader(file, part.Size+1)})
		closeErr := file.Close()
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if closeErr != nil || int64(len(data)) != part.Size || hex.EncodeToString(digest[:]) != part.SHA256 {
			return errors.New("staged part integrity check failed")
		}
		if _, err := target.Write(data); err != nil {
			return err
		}
	}
	return ctx.Err()
}
