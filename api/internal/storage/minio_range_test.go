package storage

import (
	"context"
	"fmt"
	"github.com/minio/minio-go/v7"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMinioRangeAcquiresPinnedResponseMetadata(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			original := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modified := original
				if changed && r.Method == "GET" {
					modified = modified.Add(time.Minute)
				}
				w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
				w.Header().Set("ETag", `"fixture"`)
				w.Header().Set("Content-Type", "application/octet-stream")
				if r.Method == "HEAD" {
					w.Header().Set("Content-Length", "10")
					return
				}
				if r.Header.Get("Range") != "bytes=4-5" || r.Header.Get("If-Match") != `"fixture"` {
					t.Errorf("unbound/wrong range %v", r.Header)
				}
				w.Header().Set("Content-Range", "bytes 4-5/10")
				w.Header().Set("Content-Length", "2")
				w.WriteHeader(206)
				_, _ = w.Write([]byte("45"))
			}))
			defer server.Close()
			client, e := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
			if e != nil {
				t.Fatal(e)
			}
			store := &Store{client: client, bucket: "fixture"}
			body, info, e := store.GetObjectRange(context.Background(), "source", &ByteRange{Start: 4, End: 5})
			if changed {
				if e == nil {
					body.Close()
					t.Fatal("changed acquired metadata exposed")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer body.Close()
			data, e := io.ReadAll(body)
			if e != nil || string(data) != "45" || info.Size != 10 || !info.Modified.Equal(original) {
				t.Fatalf("%q %+v %v", data, info, e)
			}
		})
	}
}
