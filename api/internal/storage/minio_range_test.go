package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/minio/minio-go/v7"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestMinioPutObjectIfAbsentSendsConditionalCreate(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprint(collision), func(t *testing.T) {
			puts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(500)
					return
				}
				puts++
				if r.Header.Get("If-None-Match") != "*" {
					t.Errorf("missing conditional create header: %v", r.Header)
				}
				if collision {
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(412)
					_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
					return
				}
				w.Header().Set("ETag", `"fixture"`)
			}))
			defer server.Close()
			client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
			if err != nil {
				t.Fatal(err)
			}
			store := &Store{client: client, bucket: "fixture"}
			err = store.PutObjectIfAbsent(context.Background(), "movie.mp4", strings.NewReader("data"), 4, "video/mp4")
			if collision && !errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("expected collision: %v", err)
			}
			if !collision && err != nil {
				t.Fatal(err)
			}
			if puts != 1 {
				t.Fatalf("PUTs: %d", puts)
			}
		})
	}
}

func TestMinioConditionalCreateReachesMultipartCompletion(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprint(collision), func(t *testing.T) {
			var parts, completions atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				w.Header().Set("Content-Type", "application/xml")
				switch {
				case r.Method == http.MethodPost && query.Has("uploads"):
					io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>fixture</Bucket><Key>movie.mp4</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && query.Get("uploadId") == "fixture-upload":
					parts.Add(1)
					io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", `"part"`)
				case r.Method == http.MethodPost && query.Get("uploadId") == "fixture-upload":
					completions.Add(1)
					if r.Header.Get("If-None-Match") != "*" {
						t.Error("multipart completion omitted atomic create condition")
					}
					if collision {
						w.WriteHeader(412)
						io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
						return
					}
					io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>fixture</Bucket><Key>movie.mp4</Key><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
				case r.Method == http.MethodDelete && query.Get("uploadId") == "fixture-upload":
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
			if err != nil {
				t.Fatal(err)
			}
			store := &Store{client: client, bucket: "fixture"}
			data := bytes.NewReader(make([]byte, 17<<20))
			err = store.PutObjectIfAbsent(context.Background(), "movie.mp4", data, data.Size(), "video/mp4")
			if collision {
				if !errors.Is(err, ErrAlreadyExists) {
					t.Fatalf("expected collision: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if parts.Load() != 2 || completions.Load() != 1 {
				t.Fatalf("parts=%d completions=%d", parts.Load(), completions.Load())
			}
		})
	}
}

func TestMinioConditionalMultipartFailureJoinsAbort(t *testing.T) {
	for _, failure := range []string{"part", "complete", "cancel", "short", "extra", "abort"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var aborts, commits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				w.Header().Set("Content-Type", "application/xml")
				switch {
				case r.Method == http.MethodPost && q.Has("uploads"):
					io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>fixture</Bucket><Key>movie.mp4</Key><UploadId>owned-id</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && q.Get("uploadId") == "owned-id":
					io.Copy(io.Discard, r.Body)
					if failure == "cancel" {
						cancel()
						return
					}
					if failure == "part" || failure == "abort" {
						w.WriteHeader(400)
						io.WriteString(w, `<Error><Code>InvalidRequest</Code></Error>`)
						return
					}
					w.Header().Set("ETag", `"part"`)
				case r.Method == http.MethodPost && q.Get("uploadId") == "owned-id":
					commits.Add(1)
					if r.Header.Get("If-None-Match") != "*" {
						t.Error("unconditional completion")
					}
					w.WriteHeader(400)
					io.WriteString(w, `<Error><Code>InvalidRequest</Code></Error>`)
				case r.Method == http.MethodDelete:
					if q.Get("uploadId") != "owned-id" || r.URL.Path != "/fixture/movie.mp4" {
						t.Error("abort targeted unknown upload/object")
					}
					aborts.Add(1)
					if failure == "abort" {
						w.WriteHeader(400)
						io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
					} else {
						w.WriteHeader(204)
					}
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
			if err != nil {
				t.Fatal(err)
			}
			size := 17 << 20
			actual := size
			if failure == "short" {
				actual--
			}
			if failure == "extra" {
				actual++
			}
			store := &Store{client: client, bucket: "fixture"}
			err = store.PutObjectIfAbsent(ctx, "movie.mp4", bytes.NewReader(make([]byte, actual)), int64(size), "video/mp4")
			if err == nil {
				t.Fatal("publication falsely succeeded")
			}
			if aborts.Load() != 1 {
				t.Fatalf("abort not joined exactly once: %d", aborts.Load())
			}
			if failure == "complete" {
				if commits.Load() != 1 {
					t.Fatal("completion not exercised")
				}
			} else if commits.Load() != 0 {
				t.Fatal("invalid input reached completion")
			}
			if errors.Is(err, ErrMultipartCleanup) != (failure == "abort") {
				t.Fatalf("cleanup failure reporting: %v", err)
			}
		})
	}
}
