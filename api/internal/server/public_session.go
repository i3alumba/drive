package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const publicCookieName = "__Secure-drive-download"
const publicSessionLifetime = 5 * time.Minute

func publicDownloadPath(owner, id string) string { return "/api/public/" + owner + "/" + id }
func (s *Server) publicGrant(owner, id, expiry string) string {
	mac := hmac.New(sha256.New, s.publicCookieKey)
	_, _ = mac.Write([]byte(owner + "\n" + id + "\n" + expiry))
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Server) validPublicSession(r *http.Request, owner, id string) bool {
	cookie, err := r.Cookie(publicCookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 || !hexID(parts[1]) {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	now := time.Now().Unix()
	if err != nil || expires <= now || expires > now+int64(publicSessionLifetime/time.Second) {
		return false
	}
	return hmac.Equal([]byte(parts[1]), []byte(s.publicGrant(owner, id, parts[0])))
}

// The long-lived capability is sent only in a JSON body, never in a request URI.
// This yields a short-lived, single-snapshot cookie, not a portal login session.
func (s *Server) exchangePublicSession(w http.ResponseWriter, r *http.Request, owner, id string, link PublicLink) {
	if kind, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(kind) != "application/json" {
		http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		host := r.Host
		if value, _, err := net.SplitHostPort(host); err == nil {
			host = value
		}
		loopback := host == "localhost"
		if ip := net.ParseIP(host); ip != nil {
			loopback = ip.IsLoopback()
		}
		if err != nil || parsed.Host != r.Host || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback)) {
			http.Error(w, "same origin required", 403)
			return
		}
	}
	var input struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || !hexID(input.Token) {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256([]byte(input.Token))
	if !hmac.Equal([]byte(hex.EncodeToString(sum[:])), []byte(id)) {
		http.NotFound(w, r)
		return
	}
	expires := time.Now().Add(publicSessionLifetime)
	if link.ExpiresAt.Before(expires) {
		expires = link.ExpiresAt
	}
	stamp := strconv.FormatInt(expires.Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: publicCookieName, Value: stamp + "." + s.publicGrant(owner, id, stamp), Path: publicDownloadPath(owner, id), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
	w.WriteHeader(http.StatusNoContent)
}
