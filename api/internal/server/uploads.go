package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"remote-drive/api/internal/storage"
)

// EnableUploads is called once at startup with a private directory on the
// existing work filesystem. It never configures or initializes object storage.
func (s *Server) EnableUploads(options UploadOptions) error {
	if s.uploads != nil {
		return errors.New("uploads already enabled")
	}
	manager, err := newUploadManager(options)
	if err != nil {
		return err
	}
	s.uploads = manager
	return nil
}

func (s *Server) CloseUploads() {
	if s.uploads != nil {
		s.uploads.close()
	}
}

func (s *Server) uploadService(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if s.uploads == nil {
		writeJSON(w, 503, map[string]string{"error": "Resumable uploads are unavailable."})
		return false
	}
	return true
}

func writeUploadError(w http.ResponseWriter, err error) {
	var problem *uploadError
	if errors.As(err, &problem) {
		if problem.code == 429 {
			w.Header().Set("Retry-After", "2")
		}
		writeJSON(w, problem.code, map[string]string{"error": problem.message})
		return
	}
	writeJSON(w, 503, map[string]string{"error": "Upload service unavailable."})
}

func validUploadPath(path string) bool {
	if len(path) > 4096 || strings.Contains(path, "\\") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." || segment != strings.TrimSpace(segment) {
			return false
		}
	}
	return true
}

func (s *Server) uploadDestination(r *http.Request, path, name, space string) (resolvedPath, error) {
	// Keep the established personal/editable-share resolver, but do not let a
	// malformed filename or dot path silently normalize to another destination.
	if !validDownloadName(name) || !validUploadPath(path) {
		return resolvedPath{}, errors.New("invalid upload destination")
	}
	request := r.Clone(r.Context())
	copyURL := *r.URL
	request.URL = &copyURL
	query := url.Values{}
	query.Set("space", space)
	request.URL.RawQuery = query.Encode()
	resolved, err := s.resolvePath(request, storage.JoinObjectPath(path, name), true)
	if err != nil {
		return resolvedPath{}, err
	}
	prefix := personalPrefix(currentUser(r))
	if resolved.Share != nil {
		prefix = "users/" + resolved.Share.OwnerID + "/"
		if resolved.Share.IsDir {
			prefix = storage.DirPrefix(prefix + resolved.Share.Path)
		}
	}
	if !strings.HasPrefix(resolved.Key, prefix) {
		return resolvedPath{}, errors.New("upload destination is outside its space")
	}
	return resolved, nil
}

func (s *Server) createChunkedUpload(w http.ResponseWriter, r *http.Request) {
	if !s.uploadService(w) {
		return
	}
	var input createUploadRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Size == nil {
		writeJSON(w, 400, map[string]string{"error": "Invalid upload metadata."})
		return
	}
	if input.Space == "" {
		input.Space = "personal"
	}
	if input.ContentType == "" {
		input.ContentType = "application/octet-stream"
	}
	if len(input.ContentType) > 255 || strings.ContainsAny(input.ContentType, "\r\n") {
		writeJSON(w, 400, map[string]string{"error": "Invalid content type."})
		return
	}
	if _, _, err := mime.ParseMediaType(input.ContentType); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Invalid content type."})
		return
	}
	resolved, err := s.uploadDestination(r, input.Path, input.Name, input.Space)
	if err != nil {
		writeJSON(w, 403, map[string]string{"error": "Upload destination is invalid or not writable."})
		return
	}
	status, err := s.uploads.create(r.Context(), currentUser(r).ID, resolved.Key, input)
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, 201, status)
}

func (s *Server) listChunkedUploads(w http.ResponseWriter, r *http.Request) {
	if s.uploadService(w) {
		writeJSON(w, 200, s.uploads.list(currentUser(r).ID))
	}
}

func (s *Server) getChunkedUpload(w http.ResponseWriter, r *http.Request) {
	if !s.uploadService(w) {
		return
	}
	status, err := s.uploads.status(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, 200, status)
}

func (s *Server) putUploadPart(w http.ResponseWriter, r *http.Request) {
	if !s.uploadService(w) {
		return
	}
	value := r.PathValue("index")
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 || strconv.Itoa(index) != value || len(value) > 6 {
		writeJSON(w, 400, map[string]string{"error": "Invalid part index."})
		return
	}
	// A missing/slow body cannot occupy a session indefinitely. Reset the
	// connection deadline on return; Recorder writers may not support deadlines.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Minute))
	defer controller.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, uploadChunkSize+1)
	status, err := s.uploads.acceptPart(r.Context(), currentUser(r).ID, r.PathValue("id"), index, r.Header.Get("X-Chunk-SHA256"), r.Body)
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, 200, status)
}

func (s *Server) completeChunkedUpload(w http.ResponseWriter, r *http.Request) {
	if !s.uploadService(w) {
		return
	}
	user := currentUser(r)
	token := bearerToken(r) // ephemeral only; never part of durable session metadata
	id := r.PathValue("id")
	status, err := s.uploads.status(user.ID, id)
	if err != nil {
		writeUploadError(w, err)
		return
	}
	if _, err := s.uploadDestination(r, status.Path, status.Name, status.Space); err != nil {
		writeJSON(w, 403, map[string]string{"error": "Upload destination is no longer writable."})
		return
	}
	status, err = s.uploads.complete(user.ID, id, func(ctx context.Context, record uploadRecord, reader io.Reader) error {
		if s.auth == nil {
			return errors.New("authentication unavailable")
		}
		freshUser, ok := s.auth.Validate(ctx, token)
		if !ok || !freshUser.DriveAllowed || freshUser.ID != record.Owner {
			return errors.New("upload authorization changed")
		}
		request, err := http.NewRequestWithContext(context.WithValue(ctx, userContextKey, freshUser), http.MethodPost, "/api/uploads", nil)
		if err != nil {
			return err
		}
		resolved, err := s.uploadDestination(request, record.Status.Path, record.Status.Name, record.Status.Space)
		if err != nil || resolved.Key != record.Key {
			return errors.New("upload destination authorization changed")
		}
		return s.store.PutObject(ctx, record.Key, reader, record.Status.Size, record.ContentType)
	})
	if err != nil {
		writeUploadError(w, err)
		return
	}
	code := 202
	if status.State == "complete" {
		code = 200
	}
	writeJSON(w, code, status)
}

func (s *Server) cancelChunkedUpload(w http.ResponseWriter, r *http.Request) {
	if !s.uploadService(w) {
		return
	}
	status, err := s.uploads.cancelSession(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, 200, status)
}
