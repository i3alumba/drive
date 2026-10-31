package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type contextKey string

const userContextKey contextKey = "driveUser"

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DriveAllowed bool   `json:"-"`
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
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (v *AuthValidator) Middleware(next http.Handler) http.Handler {
	if v == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authentication is not configured"})
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
		if !user.DriveAllowed {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Drive access is not granted"})
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
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return User{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return User{}, errors.New("unexpected authentication response")
	}
	if list, ok := raw.([]any); ok {
		if len(list) != 1 {
			return User{}, nil
		}
		raw = list[0]
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return User{}, nil
	}
	id := valueToString(obj["id"])
	parsedID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || parsedID <= 0 || strconv.FormatInt(parsedID, 10) != id {
		return User{}, errors.New("invalid storage owner ID")
	}
	username, ok := obj["username"].(string)
	if !ok || username == "" {
		return User{}, errors.New("invalid authentication username")
	}
	services, _ := obj["services"].(map[string]any)
	allowed, _ := services["drive"].(bool)
	return User{ID: id, Username: username, DriveAllowed: allowed}, nil
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
	// Media elements need cookie authentication, but mutation requests must carry
	// an explicit header so another same-site application cannot forge form posts.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return ""
	}
	if cookie, err := r.Cookie("access_token"); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}
