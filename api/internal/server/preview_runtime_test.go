package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
)

type officeFixtureStore struct{ Store }

func (officeFixtureStore) GetObject(_ context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if key != "users/42/document.rtf" {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	body := `{\rtf1\ansi Synthetic private Drive preview fixture}`
	return io.NopCloser(strings.NewReader(body)), storage.ObjectInfo{Size: int64(len(body)), ContentType: "application/rtf"}, nil
}

// Run inside the real API runtime image; no Auth/S3/production service required.
func TestOfficePreviewRuntimeAcceptance(t *testing.T) {
	if os.Getenv("DRIVE_OFFICE_ACCEPTANCE") != "1" {
		t.Skip("explicit API-image runtime acceptance required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, userContextKey, User{ID: "42", Username: "fixture"})
	request := httptest.NewRequest(http.MethodGet, "/api/preview?path=document.rtf", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	New(officeFixtureStore{}, nil, nil).previewFile(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(response.Body.String(), "%PDF-") {
		t.Fatalf("office preview failed: HTTP %d", response.Code)
	}
}
