package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type importBrokenBody struct{ io.Reader }

func (b importBrokenBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		return 0, errors.New("simulated TLS read failure")
	}
	return n, err
}
func rangeResponse(r *http.Request, data []byte, start, end, total int64) *http.Response {
	return &http.Response{StatusCode: 206, ContentLength: end - start + 1, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", start, end, total)}, "Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(bytes.NewReader(data)), Request: r}
}
func startRangeTest(t *testing.T, s *Server, data []byte) ImportJob {
	t.Helper()
	hash := sha256.Sum256(data)
	body, _ := json.Marshal(map[string]string{"url": "https://example.com/source?signature=private", "name": "ranged.bin", "sha256": hex.EncodeToString(hash[:])})
	return awaitImport(t, s, string(body))
}
func assertNoRangePublication(t *testing.T, s *Server) {
	t.Helper()
	store := s.store.(*transferStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.data["users/42/ranged.bin"]; ok {
		t.Fatal("failed range was published")
	}
}

func TestImportRangesRetryDiscardAndWholeSHA(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, int(importRangeBytes)+3)
	store := testTransferStore()
	s := configuredImporter(t, store, int64(len(data)))
	var calls atomic.Int64
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			t.Fatal("Range missing")
		}
		end = min(end, int64(len(data))-1)
		resp := rangeResponse(r, data[start:end+1], start, end, int64(len(data)))
		if call == 1 {
			resp.Body = io.NopCloser(importBrokenBody{bytes.NewReader([]byte("untrusted partial"))})
		}
		return resp, nil
	})
	job := startRangeTest(t, s, data)
	if job.Status != "complete" || calls.Load() != 3 || job.Total != int64(len(data)) || job.Bytes != job.Total {
		t.Fatalf("status %s requests %d", job.Status, calls.Load())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !bytes.Equal(store.data["users/42/ranged.bin"], data) {
		t.Fatal("range concatenation differs")
	}
}
func TestImportRangesRejectInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		length       int64
		data         string
	}{
		{"missing", "", 3, "abc"}, {"wrong_start", "bytes 1-2/3", 3, "abc"}, {"wrong_end", "bytes 0-1/3", 3, "abc"}, {"overflow", "bytes 0-2/9223372036854775808", 3, "abc"}, {"zero", "bytes 0-0/0", 1, "a"}, {"over_limit", "bytes 0-4/5", 5, "abcde"}, {"extra_header_text", "bytes 0-2/3 trailing", 3, "abc"}, {"wrong_length", "bytes 0-2/3", 2, "abc"}, {"overlong_body", "bytes 0-2/3", 3, "abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := configuredImporter(t, testTransferStore(), 3)
			s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 206, ContentLength: tc.length, Header: http.Header{"Content-Range": {tc.header}}, Body: io.NopCloser(strings.NewReader(tc.data)), Request: r}, nil
			})
			if startRangeTest(t, s, []byte("abc")).Status != "failed" {
				t.Fatal("invalid range accepted")
			}
			assertNoRangePublication(t, s)
		})
	}
}
func TestImportRangesBoundedRetries(t *testing.T) {
	s := configuredImporter(t, testTransferStore(), 3)
	var calls atomic.Int64
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := rangeResponse(r, nil, 0, 2, 3)
		resp.Body = io.NopCloser(importBrokenBody{strings.NewReader("ab")})
		return resp, nil
	})
	if startRangeTest(t, s, []byte("abc")).Status != "failed" || calls.Load() != importRangeAttempts {
		t.Fatal("range retries unbounded or published")
	}
	assertNoRangePublication(t, s)
}
func TestImportRangesChangedTotalOrIgnoredLaterRange(t *testing.T) {
	for _, status := range []int{200, 206} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			data := bytes.Repeat([]byte{'a'}, int(importRangeBytes)+3)
			s := configuredImporter(t, testTransferStore(), int64(len(data)))
			var calls atomic.Int64
			s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return rangeResponse(r, data[:importRangeBytes], 0, importRangeBytes-1, int64(len(data))), nil
				}
				resp := rangeResponse(r, []byte("aaa"), importRangeBytes, importRangeBytes+2, int64(len(data))+1)
				resp.StatusCode = status
				return resp, nil
			})
			if startRangeTest(t, s, data).Status != "failed" {
				t.Fatal("changed or ignored range accepted")
			}
			assertNoRangePublication(t, s)
		})
	}
}
func TestImportRangesSourceChangeStillFailsWholeSHA(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, int(importRangeBytes)+3)
	s := configuredImporter(t, testTransferStore(), int64(len(data)))
	var calls atomic.Int64
	s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return rangeResponse(r, data[:importRangeBytes], 0, importRangeBytes-1, int64(len(data))), nil
		}
		return rangeResponse(r, []byte("bbb"), importRangeBytes, importRangeBytes+2, int64(len(data))), nil
	})
	if startRangeTest(t, s, data).Status != "failed" {
		t.Fatal("mixed source bytes published")
	}
	assertNoRangePublication(t, s)
}
func TestImportRangesLegacyAndEmptyFallback(t *testing.T) {
	for _, data := range []string{"abc", ""} {
		t.Run(fmt.Sprint(len(data)), func(t *testing.T) {
			s := configuredImporter(t, testTransferStore(), 3)
			var calls atomic.Int64
			s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if data == "" && r.Header.Get("Range") != "" {
					return &http.Response{StatusCode: 416, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), Request: r}, nil
			})
			if startRangeTest(t, s, []byte(data)).Status != "complete" {
				t.Fatal("legacy full GET fallback broke")
			}
			if data == "" && calls.Load() != 2 {
				t.Fatal("empty fallback not bounded")
			}
		})
	}
}
func TestImportRangeCancellationClosesUnreadFirstBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := &trackedRangeBody{Reader: strings.NewReader("abc")}
	r := &importRangeReader{ctx: ctx, first: &http.Response{Body: body}, size: 3}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored")
	}
	r.Close()
	if !body.closed {
		t.Fatal("initial response leaked")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if !errors.Is(importRangePause(ctx, 4), context.DeadlineExceeded) {
		t.Fatal("retry ignored deadline")
	}
}

func TestImportRangeSharedHeaderAndBodyAttemptBudget(t *testing.T) {
	for _, headers := range []int64{2, 4} {
		t.Run(fmt.Sprint(headers), func(t *testing.T) {
			s := configuredImporter(t, testTransferStore(), 3)
			var calls atomic.Int64
			s.imports.client.Transport = remoteRoundTrip(func(r *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				if call <= headers {
					return nil, errors.New("simulated connection failure")
				}
				response := rangeResponse(r, []byte("abc"), 0, 2, 3)
				if call == headers+1 {
					response.Body = io.NopCloser(importBrokenBody{strings.NewReader("ab")})
				}
				return response, nil
			})
			job := startRangeTest(t, s, []byte("abc"))
			if headers == 4 {
				if job.Status != "failed" || calls.Load() != importRangeAttempts {
					t.Fatal("first range exceeded shared five-attempt budget")
				}
				assertNoRangePublication(t, s)
			} else if job.Status != "complete" || calls.Load() != 4 {
				t.Fatal("remaining first-range retry budget was lost")
			}
		})
	}
}

type trackedRangeBody struct {
	io.Reader
	closed bool
}

func (b *trackedRangeBody) Close() error { b.closed = true; return nil }
