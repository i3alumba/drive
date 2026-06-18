package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/torrent"
)

type Server struct {
	store    *storage.Store
	torrents *torrent.Manager
}

func New(store *storage.Store, torrents *torrent.Manager) *Server {
	return &Server{store: store, torrents: torrents}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/files", s.listFiles)
	mux.HandleFunc("POST /api/upload", s.uploadFile)
	mux.HandleFunc("GET /api/download", s.downloadFile)
	mux.HandleFunc("GET /api/view", s.viewFile)
	mux.HandleFunc("GET /api/preview", s.previewFile)
	mux.HandleFunc("DELETE /api/files", s.deletePath)
	mux.HandleFunc("POST /api/move", s.movePath)
	mux.HandleFunc("POST /api/directories", s.createDirectory)
	mux.HandleFunc("POST /api/torrents", s.uploadTorrent)
	mux.HandleFunc("GET /api/torrents", s.listTorrents)
	mux.HandleFunc("GET /api/torrents/", s.getTorrent)
	mux.HandleFunc("POST /api/torrents/", s.controlTorrent)
	return withCORS(mux)
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.List(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
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
	if err := s.store.PutDirectory(r.Context(), req.Path); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": storage.DirPrefix(req.Path)})
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(512 << 20); err != nil {
		writeError(w, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, err)
		return
	}
	defer file.Close()
	key := storage.JoinObjectPath(r.FormValue("path"), header.Filename)
	if err := s.store.PutObject(r.Context(), key, file, header.Size, header.Header.Get("Content-Type")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": key})
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "attachment")
}

func (s *Server) viewFile(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "inline")
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, disposition string) {
	key := storage.CleanObjectPath(r.URL.Query().Get("path"))
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
	key := storage.CleanObjectPath(r.URL.Query().Get("path"))
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
	cmd := exec.CommandContext(r.Context(), "soffice", "--headless", "--convert-to", "pdf", "--outdir", dir, input)
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
	key := storage.CleanObjectPath(r.URL.Query().Get("path"))
	isDir := r.URL.Query().Get("dir") == "true"
	var err error
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
	var err error
	if req.IsDir {
		err = s.store.MoveDirectory(r.Context(), req.Source, req.Destination)
	} else {
		err = s.store.MoveObject(r.Context(), req.Source, req.Destination)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": storage.CleanObjectPath(req.Destination)})
}

func (s *Server) uploadTorrent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, err)
		return
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
	if _, err := io.Copy(tmp, file); err != nil {
		writeError(w, err)
		return
	}
	job := s.torrents.Start(tmp.Name(), header.Filename, r.FormValue("path"))
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) listTorrents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.torrents.List())
}

func (s *Server) getTorrent(w http.ResponseWriter, r *http.Request) {
	id, action := torrentIDAndAction(r.URL.Path)
	if action != "" {
		http.NotFound(w, r)
		return
	}
	job, ok := s.torrents.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) controlTorrent(w http.ResponseWriter, r *http.Request) {
	id, action := torrentIDAndAction(r.URL.Path)
	if id == "" || action == "" {
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
	writeJSON(w, http.StatusOK, job)
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
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
