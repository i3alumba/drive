package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestObjectErrorDistinguishesMissingKeyFromUnavailableStorage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		missing bool
	}{
		{"missing key", minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}, true},
		{"missing bucket", minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: 404}, false},
		{"permission denied", minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, false},
		{"cancelled", context.Canceled, false},
		{"untrusted error text", errors.New("storage unavailable: NoSuchKey"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := objectError(tc.err)
			if errors.Is(got, ErrNotFound) != tc.missing {
				t.Fatalf("missing = %v, want %v", errors.Is(got, ErrNotFound), tc.missing)
			}
			if !tc.missing && got != tc.err {
				t.Fatal("non-missing failure was replaced")
			}
		})
	}
}
