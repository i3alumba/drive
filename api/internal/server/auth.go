package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type contextKey string

const userContextKey contextKey = "driveUser"

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

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
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, User{ID: "anonymous", Username: "anonymous"})))
		})
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
		user, ok := v.Validate(r.Context(), token)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	})
}

func (v *AuthValidator) Validate(ctx context.Context, token string) (User, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.validateURL, nil)
	if err != nil {
		return User{}, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.client.Do(req)
	if err != nil {
		return User{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return User{}, false
	}
	user, err := decodeUser(resp)
	if err != nil || user.ID == "" || user.Username == "" {
		return User{}, false
	}
	return user, true
}

func (v *AuthValidator) Valid(ctx context.Context, token string) bool {
	_, ok := v.Validate(ctx, token)
	return ok
}

func decodeUser(resp *http.Response) (User, error) {
	var raw any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return User{}, err
	}
	if list, ok := raw.([]any); ok {
		if len(list) == 0 {
			return User{}, nil
		}
		raw = list[0]
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return User{}, nil
	}
	return User{ID: valueToString(obj["id"]), Username: valueToString(obj["username"])}, nil
}

func valueToString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func currentUser(r *http.Request) User {
	if user, ok := r.Context().Value(userContextKey).(User); ok {
		return user
	}
	return User{ID: "anonymous", Username: "anonymous"}
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
