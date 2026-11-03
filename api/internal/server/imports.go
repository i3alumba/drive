package server

import (
	"context"
	"crypto/rand"
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
	"time"
	"unicode"

	"remote-drive/api/internal/storage"
)

const maxImportBytes int64 = 1 << 30

type ImportJob struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Status string `json:"status"`
	Bytes  int64  `json:"bytes"`
	Total  int64  `json:"total"`
	Error  string `json:"error,omitempty"`
	owner  string
	cancel context.CancelFunc
}

type importManager struct {
	mu     sync.Mutex
	jobs   map[string]*ImportJob
	slots  chan struct{}
	client *http.Client
}

func newImportManager() *importManager {
	return &importManager{jobs: map[string]*ImportJob{}, slots: make(chan struct{}, 4), client: remoteHTTPClient()}
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
		URL   string `json:"url"`
		Path  string `json:"path"`
		Name  string `json:"name"`
		Space string `json:"space"`
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	job := &ImportJob{ID: randomID(), Name: input.Name, Path: input.Path, Status: "downloading", owner: currentUser(r).ID, cancel: cancel}
	if job.Name == "" {
		job.Name = "URL download"
	}
	m.jobs[job.ID] = job
	copy := *job
	m.mu.Unlock()
	go s.runImport(ctx, job, u, input.Name, resolved.Key)
	writeJSON(w, http.StatusAccepted, copy)
}

func (s *Server) runImport(ctx context.Context, job *ImportJob, u *url.URL, name, target string) {
	m := s.imports
	defer func() { job.cancel(); <-m.slots }()
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
	response, err := m.client.Do(req)
	if err != nil {
		fail()
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > maxImportBytes {
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
	temp, err := os.CreateTemp("", "drive-url-*")
	if err != nil {
		fail()
		return
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	reader := io.LimitReader(response.Body, maxImportBytes+1)
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxImportBytes {
				fail()
				return
			}
			if _, err := temp.Write(buf[:n]); err != nil {
				fail()
				return
			}
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
