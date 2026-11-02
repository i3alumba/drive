//go:build linux

package migration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestS3SourcePreservesLISTTimestampUsesConditionalReadsAndNeverWrites(t *testing.T) {
	body := "fixture bytes"
	modified := time.Date(2020, 2, 3, 4, 5, 6, 123000000, time.UTC)
	var writes, conditionals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes.Add(1)
			w.WriteHeader(405)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Has("location"):
			io.WriteString(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
		case q.Has("versioning"):
			io.WriteString(w, `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`)
		case q.Get("list-type") == "2":
			fmt.Fprintf(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>drive</Name><IsTruncated>false</IsTruncated><KeyCount>1</KeyCount><Contents><Key>users/42/a</Key><LastModified>%s</LastModified><ETag>&quot;fixture-etag&quot;</ETag><Size>%d</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>`, modified.Format(time.RFC3339Nano), len(body))
		case r.URL.Path == "/drive/users/42/a":
			w.Header().Set("Content-Type", "text/x-fixture")
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", `"fixture-etag"`)
			w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
			if r.Method == http.MethodGet {
				if r.Header.Get("If-Match") != `"fixture-etag"` {
					t.Error("GET lacks exact ETag precondition")
				} else {
					conditionals.Add(1)
				}
				io.WriteString(w, body)
			}
		case r.Method == http.MethodHead && r.URL.Path == "/drive/":
			w.WriteHeader(200)
		default:
			t.Errorf("unexpected read: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	source, err := NewS3Source(context.Background(), strings.TrimPrefix(server.URL, "http://"), "drive", "fixture-access", "fixture-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	manifest := inventory(t, source)
	if len(manifest.Entries) != 1 || !manifest.Entries[0].Modified.Equal(modified) {
		t.Fatal("S3 subsecond timestamp lost")
	}
	if writes.Load() != 0 || conditionals.Load() == 0 {
		t.Fatal("read-only/conditional S3 contract violated")
	}
}
