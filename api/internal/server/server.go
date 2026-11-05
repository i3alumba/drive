package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/torrent"
	"strconv"
	"strings"
)

// Store is the object contract used by Drive's HTTP handlers. Authentication
// and users/{id}/ prefixes stay above the selected storage implementation.
type Store interface {
	List(context.Context, string) ([]storage.Object, error)
	PutDirectory(context.Context, string) error
	PutObject(context.Context, string, io.Reader, int64, string) error
	GetObject(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error)
	GetObjectRange(context.Context, string, *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error)
	StatObject(context.Context, string) (storage.Object, error)
	MoveObject(context.Context, string, string) error
	MoveDirectory(context.Context, string, string) error
	DeleteObject(context.Context, string) error
	DeleteDirectory(context.Context, string) error
}

type Server struct {
	store           Store
	torrents        *torrent.Manager
	auth            *AuthValidator
	shares          *ShareStore
	probe           chan struct{}
	imports         *importManager
	queues          *queueManager
	uploads         *uploadManager
	video           *videoPreviewManager
	publicCopies    chan struct{}
	publicCookieKey []byte
}

func New(store Store, torrents *torrent.Manager, auth *AuthValidator) *Server {
	return &Server{store: store, torrents: torrents, auth: auth, shares: NewShareStore(store), probe: make(chan struct{}, 1), imports: newImportManager(), publicCopies: make(chan struct{}, 1), publicCookieKey: []byte(randomID())}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/spaces", s.listSpaces)
	mux.HandleFunc("GET /api/shares", s.listShares)
	mux.HandleFunc("POST /api/shares", s.createShare)
	mux.HandleFunc("DELETE /api/shares/", s.deleteShare)
	mux.HandleFunc("GET /api/files", s.listFiles)
	mux.HandleFunc("POST /api/upload", s.uploadFile)
	mux.HandleFunc("POST /api/uploads", s.createChunkedUpload)
	mux.HandleFunc("GET /api/uploads", s.listChunkedUploads)
	mux.HandleFunc("GET /api/uploads/{id}", s.getChunkedUpload)
	mux.HandleFunc("PUT /api/uploads/{id}/parts/{index}", s.putUploadPart)
	mux.HandleFunc("POST /api/uploads/{id}/complete", s.completeChunkedUpload)
	mux.HandleFunc("DELETE /api/uploads/{id}", s.cancelChunkedUpload)
	mux.HandleFunc("POST /api/imports", s.startImport)
	mux.HandleFunc("GET /api/imports", s.listImports)
	mux.HandleFunc("DELETE /api/imports/", s.cancelImport)
	mux.HandleFunc("POST /api/import-queues", s.createImportQueue)
	mux.HandleFunc("GET /api/import-queues", s.listImportQueues)
	mux.HandleFunc("GET /api/import-queues/", s.getQueueRoute)
	mux.HandleFunc("POST /api/import-queues/{id}/{action}", s.controlImportQueue)
	mux.HandleFunc("DELETE /api/import-queues/{id}/items/{itemId}", s.controlQueueItem)
	mux.HandleFunc("POST /api/import-queues/{id}/items/{itemId}/retry", s.controlQueueItem)
	mux.HandleFunc("POST /api/public-links", s.createPublicLink)
	mux.HandleFunc("GET /api/public-links", s.listPublicLinks)
	mux.HandleFunc("DELETE /api/public-links/", s.deletePublicLink)
	mux.HandleFunc("GET /api/download", s.downloadFile)
	mux.HandleFunc("GET /api/view", s.viewFile)
	mux.HandleFunc("GET /api/preview", s.previewFile)
	mux.HandleFunc("POST /api/video-previews", s.createVideoPreview)
	mux.HandleFunc("GET /api/video-previews/{id}", s.getVideoPreview)
	mux.HandleFunc("GET /api/video-previews/{id}/content", s.serveVideoPreview)
	mux.HandleFunc("DELETE /api/video-previews/{id}", s.cancelVideoPreview)
	mux.HandleFunc("DELETE /api/files", s.deletePath)
	mux.HandleFunc("POST /api/move", s.movePath)
	mux.HandleFunc("POST /api/directories", s.createDirectory)
	mux.HandleFunc("POST /api/torrents", s.uploadTorrent)
	mux.HandleFunc("GET /api/torrents", s.listTorrents)
	mux.HandleFunc("GET /api/torrents/", s.getTorrent)
	mux.HandleFunc("POST /api/torrents/", s.controlTorrent)
	protected := s.auth.Middleware(mux)
	return withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/public/") && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/session")) {
			s.downloadPublicLink(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/import-queues") || strings.HasPrefix(r.URL.Path, "/api/video-previews") {
			w.Header().Set("Cache-Control", "no-store")
		}
		protected.ServeHTTP(w, r)
	}))
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	resolved, err := s.resolvePath(r, r.URL.Query().Get("path"), false)
	if err != nil {
		writeError(w, err)
		return
	}
	if resolved.Share != nil && !resolved.Share.IsDir {
		item, err := s.store.StatObject(r.Context(), resolved.Key)
		if err != nil {
			writeError(w, err)
			return
		}
		item.Path = item.Name
		writeJSON(w, http.StatusOK, []storage.Object{item})
		return
	}
	items, err := s.store.List(r.Context(), resolved.Key)
	if err != nil {
		writeError(w, err)
		return
	}
	for i := range items {
		items[i] = virtualizeObject(items[i], resolved.DisplayPrefix)
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createDirectory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	resolved, err := s.resolvePath(r, req.Path, true)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.PutDirectory(r.Context(), resolved.Key); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": storage.DirPrefix(req.Path)})
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, err)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, err)
		return
	}
	defer file.Close()
	path := storage.JoinObjectPath(r.FormValue("path"), header.Filename)
	resolved, err := s.resolvePath(r, path, true)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.PutObject(r.Context(), resolved.Key, file, header.Size, header.Header.Get("Content-Type")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": path})
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "attachment")
}

func (s *Server) viewFile(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "inline")
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, disposition string) {
	resolved, err := s.resolvePath(r, r.URL.Query().Get("path"), false)
	if err != nil {
		writeError(w, err)
		return
	}
	key := resolved.Key
	byteRange, partial, err := parseRange(r.Header.Get("Range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	obj, info, err := s.store.GetObjectRange(r.Context(), key, byteRange)
	if err != nil {
		writeError(w, err)
		return
	}
	defer obj.Close()
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Disposition", disposition+`; filename="`+filepath.Base(key)+`"`)
	if partial && byteRange != nil {
		start, end, length, err := resolveRange(byteRange, info.Size)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(info.Size, 10))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	_, _ = io.Copy(w, obj)
}

func (s *Server) previewFile(w http.ResponseWriter, r *http.Request) {
	resolved, err := s.resolvePath(r, r.URL.Query().Get("path"), false)
	if err != nil {
		writeError(w, err)
		return
	}
	key := resolved.Key
	if !isOfficeDocument(key) {
		s.viewFile(w, r)
		return
	}
	obj, _, err := s.store.GetObject(r.Context(), key)
	if err != nil {
		writeError(w, err)
		return
	}
	defer obj.Close()
	dir, err := os.MkdirTemp("", "drive-preview-*")
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, filepath.Base(key))
	file, err := os.Create(input)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := io.Copy(file, obj); err != nil {
		_ = file.Close()
		writeError(w, err)
		return
	}
	if err := file.Close(); err != nil {
		writeError(w, err)
		return
	}
	profile := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "profile")}).String()
	cmd := exec.CommandContext(r.Context(), "soffice", "-env:UserInstallation="+profile, "--headless", "--convert-to", "pdf", "--outdir", dir, input)
	if output, err := cmd.CombinedOutput(); err != nil {
		writeError(w, errors.New("document preview conversion failed: "+strings.TrimSpace(string(output))))
		return
	}
	pdf := filepath.Join(dir, strings.TrimSuffix(filepath.Base(key), filepath.Ext(key))+".pdf")
	pdfFile, err := os.Open(pdf)
	if err != nil {
		writeError(w, err)
		return
	}
	defer pdfFile.Close()
	info, err := pdfFile.Stat()
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+strings.TrimSuffix(filepath.Base(key), filepath.Ext(key))+`.pdf"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, _ = io.Copy(w, pdfFile)
}

func isOfficeDocument(key string) bool {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".doc", ".docx", ".odt", ".ods", ".odp", ".ppt", ".pptx", ".xls", ".xlsx", ".rtf":
		return true
	default:
		return false
	}
}

func (s *Server) deletePath(w http.ResponseWriter, r *http.Request) {
	resolved, err := s.resolvePath(r, r.URL.Query().Get("path"), true)
	if err != nil {
		writeError(w, err)
		return
	}
	key := resolved.Key
	isDir := r.URL.Query().Get("dir") == "true"
	if isDir {
		err = s.store.DeleteDirectory(r.Context(), key)
	} else {
		err = s.store.DeleteObject(r.Context(), key)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) movePath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
		IsDir       bool   `json:"isDir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	source, err := s.resolvePath(r, req.Source, true)
	if err != nil {
		writeError(w, err)
		return
	}
	destination, err := s.resolvePath(r, req.Destination, true)
	if err != nil {
		writeError(w, err)
		return
	}
	if source.Share != nil && destination.Share != nil && source.Share.ID != destination.Share.ID {
		writeError(w, errors.New("cannot move files between different shared spaces"))
		return
	}
	if req.IsDir {
		err = s.store.MoveDirectory(r.Context(), source.Key, destination.Key)
	} else {
		err = s.store.MoveObject(r.Context(), source.Key, destination.Key)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": storage.CleanObjectPath(req.Destination)})
}

func (s *Server) uploadTorrent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, err)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("torrent")
	if err != nil {
		writeError(w, err)
		return
	}
	defer file.Close()
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".torrent") {
		writeError(w, errors.New("uploaded file must have .torrent extension"))
		return
	}
	tmp, err := os.CreateTemp("", "drive-*.torrent")
	if err != nil {
		writeError(w, err)
		return
	}
	defer tmp.Close()
	started := false
	defer func() {
		if !started {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, file); err != nil {
		writeError(w, err)
		return
	}
	resolved, err := s.resolvePath(r, r.FormValue("path"), true)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := tmp.Close(); err != nil {
		writeError(w, err)
		return
	}
	if s.torrents == nil {
		http.Error(w, "torrent service unavailable", 503)
		return
	}
	if !strings.HasPrefix(storage.DirPrefix(resolved.Key), personalPrefix(currentUser(r))) {
		http.Error(w, "torrent uploads require your personal space", 403)
		return
	}
	job := s.torrents.Start(tmp.Name(), header.Filename, resolved.Key)
	started = true
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) listTorrents(w http.ResponseWriter, r *http.Request) {
	prefix := personalPrefix(currentUser(r))
	jobs := s.torrents.List()
	filtered := jobs[:0]
	for _, job := range jobs {
		if strings.HasPrefix(job.TargetDir, prefix) {
			job.TargetDir = strings.TrimPrefix(job.TargetDir, prefix)
			filtered = append(filtered, job)
		}
	}
	writeJSON(w, http.StatusOK, filtered)
}

func (s *Server) getTorrent(w http.ResponseWriter, r *http.Request) {
	id, action := torrentIDAndAction(r.URL.Path)
	if action != "" {
		http.NotFound(w, r)
		return
	}
	job, ok := s.torrents.Get(id)
	if !ok || !strings.HasPrefix(job.TargetDir, personalPrefix(currentUser(r))) {
		http.NotFound(w, r)
		return
	}
	job.TargetDir = strings.TrimPrefix(job.TargetDir, personalPrefix(currentUser(r)))
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) controlTorrent(w http.ResponseWriter, r *http.Request) {
	id, action := torrentIDAndAction(r.URL.Path)
	if id == "" || action == "" {
		http.NotFound(w, r)
		return
	}
	existing, ok := s.torrents.Get(id)
	if !ok || !strings.HasPrefix(existing.TargetDir, personalPrefix(currentUser(r))) {
		http.NotFound(w, r)
		return
	}
	var (
		job torrent.Job
		err error
	)
	switch action {
	case "pause":
		job, err = s.torrents.Pause(id)
	case "resume":
		job, err = s.torrents.Resume(id)
	case "cancel":
		job, err = s.torrents.Cancel(id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if errors.Is(err, torrent.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeError(w, err)
		return
	}
	job.TargetDir = strings.TrimPrefix(job.TargetDir, personalPrefix(currentUser(r)))
	writeJSON(w, http.StatusOK, job)
}

type Space struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	OwnerUsername string `json:"ownerUsername,omitempty"`
	Permission    string `json:"permission"`
	Shared        bool   `json:"shared"`
	Path          string `json:"path,omitempty"`
	IsDir         bool   `json:"isDir,omitempty"`
}

func (s *Server) listSpaces(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	spaces := []Space{{ID: "personal", Name: "My files", Permission: permissionEdit}}
	shares, err := s.shares.Incoming(r.Context(), user)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, share := range shares {
		spaces = append(spaces, Space{
			ID:            shareSpaceID(share.ID),
			Name:          share.OwnerUsername + ": " + share.Path,
			OwnerUsername: share.OwnerUsername,
			Permission:    share.Permission,
			Shared:        true,
			Path:          share.Path,
			IsDir:         share.IsDir,
		})
	}
	writeJSON(w, http.StatusOK, spaces)
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	outgoing, err := s.shares.Outgoing(r.Context(), user)
	if err != nil {
		writeError(w, err)
		return
	}
	incoming, err := s.shares.Incoming(r.Context(), user)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Share{"outgoing": outgoing, "incoming": incoming})
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path           string `json:"path"`
		IsDir          bool   `json:"isDir"`
		TargetUsername string `json:"targetUsername"`
		Permission     string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	user := currentUser(r)
	resolved := resolvePersonalPath(user, req.Path)
	if err := validateShareOwnership(user, resolved.Key); err != nil {
		writeError(w, err)
		return
	}
	share, err := s.shares.Create(r.Context(), user, req.TargetUsername, req.Path, req.IsDir, req.Permission)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, share)
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/shares/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := s.shares.Delete(r.Context(), currentUser(r), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolvePath(r *http.Request, userPath string, write bool) (resolvedPath, error) {
	user := currentUser(r)
	space := r.URL.Query().Get("space")
	if space == "" && r.Method == http.MethodPost {
		space = r.FormValue("space")
	}
	if space == "" || space == "personal" {
		return resolvePersonalPath(user, userPath), nil
	}
	shareID, ok := shareIDFromSpace(space)
	if !ok {
		return resolvedPath{}, errors.New("unknown file space")
	}
	share, ok, err := s.shares.FindIncoming(r.Context(), user, shareID)
	if err != nil {
		return resolvedPath{}, err
	}
	if !ok {
		return resolvedPath{}, errors.New("shared space not found")
	}
	return resolveSharedPath(share, userPath, write)
}

func torrentIDAndAction(requestPath string) (id string, action string) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(requestPath, "/api/torrents/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func parseRange(value string) (*storage.ByteRange, bool, error) {
	if value == "" {
		return nil, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") {
		return nil, false, errors.New("only bytes ranges are supported")
	}
	spec := strings.TrimPrefix(value, "bytes=")
	if strings.Contains(spec, ",") {
		return nil, false, errors.New("multipart ranges are not supported")
	}
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return nil, false, errors.New("invalid range")
	}
	if parts[0] == "" {
		length, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || length <= 0 {
			return nil, false, errors.New("invalid suffix range")
		}
		return &storage.ByteRange{SuffixLength: length}, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 {
		return nil, false, errors.New("invalid range start")
	}
	end := int64(0)
	openEnded := parts[1] == ""
	if !openEnded {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return nil, false, errors.New("invalid range end")
		}
	}
	return &storage.ByteRange{Start: start, End: end, OpenEnded: openEnded}, true, nil
}

func resolveRange(byteRange *storage.ByteRange, size int64) (start int64, end int64, length int64, err error) {
	if size <= 0 {
		return 0, 0, 0, errors.New("empty object cannot satisfy range")
	}
	if byteRange.SuffixLength > 0 {
		length = byteRange.SuffixLength
		if length > size {
			length = size
		}
		start = size - length
		end = size - 1
		return start, end, length, nil
	}
	if byteRange.Start >= size {
		return 0, 0, 0, errors.New("range start is beyond object size")
	}
	start = byteRange.Start
	end = byteRange.End
	if byteRange.OpenEnded || end >= size {
		end = size - 1
	}
	length = end - start + 1
	return start, end, length, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, storage.ErrVolumeUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, storage.ErrIntegrity):
		status = http.StatusInternalServerError
	case errors.Is(err, storage.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, storage.ErrInvalidRange):
		status = http.StatusRequestedRangeNotSatisfiable
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Chunk-SHA256")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
