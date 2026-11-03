package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"

	"remote-drive/api/internal/storage"
)

const maxImportBytes int64 = 10 << 30

type ImportJob struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Path           string `json:"path"`
	Status         string `json:"status"`
	Bytes          int64  `json:"bytes"`
	Total          int64  `json:"total"`
	Error          string `json:"error,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
	expectedSHA256 string
	owner          string
	cancel         context.CancelFunc
}

type importManager struct {
	mu       sync.Mutex
	jobs     map[string]*ImportJob
	slots    chan struct{}
	client   *http.Client
	options  ImportOptions
	root     *os.File
	lock     *os.File
	reserved int64
	closed   bool
	workers  sync.WaitGroup
}

func newImportManager() *importManager {
	options := defaultImportOptions()
	client := remoteHTTPClient()
	client.Timeout = options.Timeout
	return &importManager{jobs: map[string]*ImportJob{}, slots: make(chan struct{}, 4), client: client, options: options}
}
func randomID() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("secure randomness unavailable")
	}
	return hex.EncodeToString(raw[:])
}
func validDownloadName(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsControl) < 0
}

func (s *Server) startImport(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL    string `json:"url"`
		Path   string `json:"path"`
		Name   string `json:"name"`
		Space  string `json:"space"`
		SHA256 string `json:"sha256"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid import request", 400)
		return
	}
	u, err := url.Parse(input.URL)
	if err != nil || validateRemoteURL(u) != nil {
		http.Error(w, "use a public HTTP(S) URL on port 80 or 443", 400)
		return
	}
	if input.SHA256 != "" {
		digest, err := hex.DecodeString(input.SHA256)
		if err != nil || len(digest) != sha256.Size || input.SHA256 != strings.ToLower(input.SHA256) {
			http.Error(w, "invalid SHA-256", http.StatusBadRequest)
			return
		}
	}
	if input.Name != "" && !validDownloadName(input.Name) {
		http.Error(w, "invalid file name", 400)
		return
	}
	if input.Space != "" {
		q := r.URL.Query()
		q.Set("space", input.Space)
		r.URL.RawQuery = q.Encode()
	}
	// Imports and their jobs stay in the authenticated owner's personal space.
	// Shared-space torrent/import job ownership must not be inferred from a target prefix.
	if space := r.URL.Query().Get("space"); space != "" && space != "personal" {
		http.Error(w, "URL imports require your personal space", 403)
		return
	}
	resolved, err := s.resolvePath(r, input.Path, true)
	if err != nil {
		writeError(w, err)
		return
	}
	m := s.imports
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		http.Error(w, "URL importer unavailable", http.StatusServiceUnavailable)
		return
	}
	active := 0
	for _, job := range m.jobs {
		if job.owner == currentUser(r).ID && (job.Status == "downloading" || job.Status == "saving") {
			active++
		}
	}
	if active >= 2 {
		m.mu.Unlock()
		http.Error(w, "two URL imports are already running", 429)
		return
	}
	select {
	case m.slots <- struct{}{}:
	default:
		m.mu.Unlock()
		http.Error(w, "URL import capacity reached; retry later", 429)
		return
	}
	// Terminal jobs are UI history, not a durable queue. Bound retained history.
	if len(m.jobs) >= 256 {
		for id, job := range m.jobs {
			if job.Status != "downloading" && job.Status != "saving" {
				delete(m.jobs, id)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.options.Timeout)
	job := &ImportJob{ID: randomID(), Name: input.Name, Path: input.Path, Status: "downloading", owner: currentUser(r).ID, cancel: cancel, expectedSHA256: input.SHA256}
	if job.Name == "" {
		job.Name = "URL download"
	}
	m.jobs[job.ID] = job
	copy := *job
	m.workers.Add(1)
	m.mu.Unlock()
	go s.runImport(ctx, job, u, input.Name, resolved.Key)
	writeJSON(w, http.StatusAccepted, copy)
}

func (s *Server) runImport(ctx context.Context, job *ImportJob, u *url.URL, name, target string) {
	m := s.imports
	defer func() { job.cancel(); <-m.slots; m.workers.Done() }()
	fail := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if job.Status != "cancelled" {
			job.Status = "failed"
			job.Error = "Remote download failed (unavailable, disallowed destination, limit or storage error)."
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		fail()
		return
	}
	req.Header.Set("User-Agent", "Remote-Drive/1.0")
	response, err := m.openImportResponse(ctx, req, job.expectedSHA256 != "")
	if err != nil {
		fail()
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > m.options.MaxFileBytes {
		fail()
		return
	}
	if name == "" {
		_, params, _ := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
		name = params["filename"]
		if !validDownloadName(name) {
			name = path.Base(response.Request.URL.Path)
		}
		if !validDownloadName(name) {
			name = "download"
		}
	}
	m.mu.Lock()
	job.Name = name
	job.Total = response.ContentLength
	m.mu.Unlock()
	reservation := response.ContentLength
	if reservation < 0 {
		reservation = m.options.MaxFileBytes
	}
	if !m.reserveStaging(reservation) {
		fail()
		return
	}
	defer m.releaseStaging(reservation)
	temp, cleanup, err := m.temporary()
	if err != nil {
		fail()
		return
	}
	defer cleanup()
	checksum := sha256.New()
	reader := io.LimitReader(response.Body, m.options.MaxFileBytes+1)
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > m.options.MaxFileBytes || response.ContentLength >= 0 && total > response.ContentLength {
				fail()
				return
			}
			free, err := m.freeBytes()
			if err != nil || free < m.options.ReserveBytes+int64(n) {
				fail()
				return
			}
			if _, err := temp.Write(buf[:n]); err != nil {
				fail()
				return
			}
			_, _ = checksum.Write(buf[:n])
			m.mu.Lock()
			job.Bytes = total
			m.mu.Unlock()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			fail()
			return
		}
		if ctx.Err() != nil {
			fail()
			return
		}
	}
	if response.ContentLength >= 0 && total != response.ContentLength {
		fail()
		return
	}
	digest := hex.EncodeToString(checksum.Sum(nil))
	if job.expectedSHA256 != "" && digest != job.expectedSHA256 {
		fail()
		return
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		fail()
		return
	}
	m.mu.Lock()
	if job.Status == "cancelled" {
		m.mu.Unlock()
		return
	}
	job.Status = "saving"
	m.mu.Unlock()
	key := storage.JoinObjectPath(target, name)
	if !strings.HasPrefix(key, storage.DirPrefix(target)) {
		fail()
		return
	}
	// Unlike an explicitly named computer upload, a remote server can choose
	// Content-Disposition. Do not knowingly replace an existing private file.
	if _, err := s.store.StatObject(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		fail()
		if err == nil {
			m.mu.Lock()
			job.Error = "Destination already exists. Choose a different Save as name."
			m.mu.Unlock()
		}
		return
	}
	if err := s.store.PutObject(ctx, key, temp, total, response.Header.Get("Content-Type")); err != nil {
		fail()
		return
	}
	m.mu.Lock()
	job.Status = "complete"
	job.Path = storage.JoinObjectPath(job.Path, name)
	job.Total = total
	job.SHA256 = digest
	job.Error = ""
	m.mu.Unlock()
}
func (s *Server) listImports(w http.ResponseWriter, r *http.Request) {
	m := s.imports
	m.mu.Lock()
	out := []ImportJob{}
	for _, job := range m.jobs {
		if job.owner == currentUser(r).ID {
			out = append(out, *job)
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, out)
}
func (s *Server) cancelImport(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/imports/")
	m := s.imports
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil || job.owner != currentUser(r).ID {
		m.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if job.Status == "saving" {
		m.mu.Unlock()
		http.Error(w, "file is being saved; wait for completion", 409)
		return
	}
	if job.Status == "downloading" {
		job.Status = "cancelled"
		job.cancel()
	}
	copy := *job
	m.mu.Unlock()
	writeJSON(w, 200, copy)
}
