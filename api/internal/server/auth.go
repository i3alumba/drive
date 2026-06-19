package server

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type AuthValidator struct {
	validateURL string
	client      *http.Client
}

func NewAuthValidator(validateURL string) *AuthValidator {
	if strings.TrimSpace(validateURL) == "" {
		return nil
	}
	return &AuthValidator{
		validateURL: validateURL,
		client:      &http.Client{Timeout: 5 * time.Second},
	}
}

func (v *AuthValidator) Middleware(next http.Handler) http.Handler {
	if v == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		if !v.Valid(r.Context(), token) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (v *AuthValidator) Valid(ctx context.Context, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.validateURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func bearerToken(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return strings.TrimSpace(value[len("Bearer "):])
	}
	if cookie, err := r.Cookie("access_token"); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}
