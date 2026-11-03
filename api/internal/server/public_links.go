package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"remote-drive/api/internal/storage"
)

type PublicLink struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	CleanupOnly bool      `json:"cleanupOnly,omitempty"`
}

func hexID(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}
func linkMetadata(owner, id string) string {
	return "_drive/public-links/" + owner + "/" + id + ".json"
}
func linkData(owner, id string) string { return "_drive/public-link-data/" + owner + "/" + id }

func (s *Server) createPublicLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	select {
	case s.publicCopies <- struct{}{}:
		defer func() { <-s.publicCopies }()
	default:
		http.Error(w, "public snapshot capacity reached; retry later", 429)
		return
	}
	var input struct {
		Path string `json:"path"`
		Days int    `json:"days"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		http.Error(w, "invalid link request", 400)
		return
	}
	if input.Days == 0 {
		input.Days = 7
	}
	if input.Days < 1 || input.Days > 30 || storage.CleanObjectPath(input.Path) == "" {
		http.Error(w, "a file and expiry of 1–30 days are required", 400)
		return
	}
	if space := r.URL.Query().Get("space"); space != "" && space != "personal" {
		http.Error(w, "only file owners may create public links", 403)
		return
	}
	owner := currentUser(r)
	links, err := s.store.List(r.Context(), "_drive/public-links/"+owner.ID+"/")
	if err != nil {
		writeError(w, err)
		return
	}
	snapshots, err := s.store.List(r.Context(), "_drive/public-link-data/"+owner.ID+"/")
	if err != nil {
		writeError(w, err)
		return
	}
	if len(links) >= 100 || len(snapshots) >= 100 {
		http.Error(w, "revoke old public links before creating more", 429)
		return
	}
	resolved := resolvePersonalPath(owner, input.Path)
	object, err := s.store.StatObject(r.Context(), resolved.Key)
	if err != nil {
		writeError(w, err)
		return
	}
	if object.IsDir {
		http.Error(w, "public links support files only", 400)
		return
	}
	reader, info, err := s.store.GetObject(r.Context(), resolved.Key)
	if err != nil {
		writeError(w, err)
		return
	}
	defer reader.Close()
	token := randomID()
	hash := sha256.Sum256([]byte(token))
	id := hex.EncodeToString(hash[:])
	link := PublicLink{ID: id, Path: storage.CleanObjectPath(input.Path), Name: path.Base(resolved.Key), ExpiresAt: time.Now().UTC().Add(time.Duration(input.Days) * 24 * time.Hour), Size: info.Size, ContentType: info.ContentType}
	// Public capabilities reference an immutable snapshot, never a mutable path
	// which could later hold a different private file. No original data is moved.
	if err := s.store.PutObject(r.Context(), linkData(owner.ID, id), reader, info.Size, info.ContentType); err != nil {
		s.cleanupPublicSnapshot(owner.ID, id)
		writeError(w, err)
		return
	}
	body, _ := json.Marshal(link)
	if err := s.store.PutObject(r.Context(), linkMetadata(owner.ID, id), strings.NewReader(string(body)), int64(len(body)), "application/json"); err != nil {
		s.cleanupPublicSnapshot(owner.ID, id)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"link": link, "downloadUrl": "/public/" + owner.ID + "/" + id + "#" + token})
}

func (s *Server) cleanupPublicSnapshot(owner, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.store.DeleteObject(ctx, linkData(owner, id))
}

func (s *Server) listPublicLinks(w http.ResponseWriter, r *http.Request) {
	owner := currentUser(r)
	prefix := "_drive/public-links/" + owner.ID + "/"
	objects, err := s.store.List(r.Context(), prefix)
	if err != nil {
		writeError(w, err)
		return
	}
	out := []PublicLink{}
	seen := map[string]bool{}
	for _, object := range objects {
		if object.IsDir || !strings.HasSuffix(object.Name, ".json") {
			continue
		}
		id := strings.TrimSuffix(object.Name, ".json")
		if !hexID(id) {
			continue
		}
		seen[id] = true
		link, err := s.readPublicLink(r, owner.ID, id)
		if err != nil {
			writeError(w, err)
			return
		}
		if file := r.URL.Query().Get("path"); file == "" || link.Path == storage.CleanObjectPath(file) {
			out = append(out, link)
		}
	}
	if r.URL.Query().Get("path") == "" {
		data, err := s.store.List(r.Context(), "_drive/public-link-data/"+owner.ID+"/")
		if err != nil {
			writeError(w, err)
			return
		}
		for _, object := range data {
			if !object.IsDir && hexID(object.Name) && !seen[object.Name] {
				out = append(out, PublicLink{ID: object.Name, Name: "Unlinked snapshot", Size: object.Size, CleanupOnly: true})
			}
		}
	}
	writeJSON(w, 200, out)
}
func (s *Server) readPublicLink(r *http.Request, owner, id string) (PublicLink, error) {
	reader, _, err := s.store.GetObject(r.Context(), linkMetadata(owner, id))
	if err != nil {
		return PublicLink{}, err
	}
	defer reader.Close()
	var link PublicLink
	err = json.NewDecoder(io.LimitReader(reader, 16385)).Decode(&link)
	if err == nil && (link.ID != id || link.Name == "" || link.Size < 0 || link.ExpiresAt.IsZero()) {
		err = errors.New("invalid public link metadata")
	}
	return link, err
}
func (s *Server) deletePublicLink(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/public-links/")
	if !hexID(id) {
		http.NotFound(w, r)
		return
	}
	owner := currentUser(r).ID
	// Both keys are validated-ID, authenticated-owner generated namespaces.
	// Retry orphan cleanup even after metadata was already successfully revoked.
	if err := s.store.DeleteObject(r.Context(), linkMetadata(owner, id)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteObject(r.Context(), linkData(owner, id)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) downloadPublicLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/public/"), "/")
	session := len(parts) == 3 && parts[2] == "session" && r.Method == http.MethodPost
	if (len(parts) != 2 && !session) || !hexID(parts[1]) {
		http.NotFound(w, r)
		return
	}
	owner, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || owner <= 0 || strconv.FormatInt(owner, 10) != parts[0] {
		http.NotFound(w, r)
		return
	}
	id := parts[1]
	link, err := s.readPublicLink(r, parts[0], id)
	if err != nil || !time.Now().Before(link.ExpiresAt) {
		http.NotFound(w, r)
		return
	}
	if session {
		s.exchangePublicSession(w, r, parts[0], id, link)
		return
	}
	if !s.validPublicSession(r, parts[0], id) {
		http.Error(w, "open the original shared link to download", http.StatusForbidden)
		return
	}
	// Range-free attachment download: never expose the metadata namespace or
	// permit this unauthenticated route to perform any mutation.
	reader, info, err := s.store.GetObject(r.Context(), linkData(parts[0], id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer reader.Close()
	if info.Size != link.Size {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": link.Name}))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, reader)
	}
}
