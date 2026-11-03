package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const importRangeBytes int64 = 16 << 20
const importRangeAttempts = 5

var importContentRange = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)
var errImportRange = errors.New("remote byte-range validation or transfer failed")

func parseImportRange(response *http.Response, start, size, maximum int64) (int64, int64, error) {
	if response.StatusCode != http.StatusPartialContent {
		return 0, 0, errImportRange
	}
	parts := importContentRange.FindStringSubmatch(response.Header.Get("Content-Range"))
	if len(parts) != 4 {
		return 0, 0, errImportRange
	}
	values := [3]int64{}
	for i := range values {
		n, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return 0, 0, errImportRange
		}
		values[i] = n
	}
	total := values[2]
	if total <= 0 || total > maximum || start < 0 || start >= total || size > 0 && total != size {
		return 0, 0, errImportRange
	}
	length := min(importRangeBytes, total-start)
	if values[0] != start || values[1] != start+length-1 || response.ContentLength >= 0 && response.ContentLength != length {
		return 0, 0, errImportRange
	}
	return total, length, nil
}

func importRangePause(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Only checksum-bound imports may combine independent range responses. The
// ordinary importer still verifies the complete SHA-256 before publication.
func (m *importManager) openImportResponse(ctx context.Context, request *http.Request, checksumBound bool) (*http.Response, error) {
	if !checksumBound {
		return m.client.Do(request)
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=0-%d", importRangeBytes-1))
	var response *http.Response
	var err error
	attemptsUsed := 0
	for attempt := 0; attempt < importRangeAttempts; attempt++ {
		attemptsUsed = attempt + 1
		response, err = m.client.Do(request)
		if err == nil {
			break
		}
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt+1 < importRangeAttempts {
			if err := importRangePause(ctx, attempt); err != nil {
				return nil, err
			}
		}
	}
	if err != nil {
		return nil, errImportRange
	}
	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		// Empty objects and sources without usable range support keep the legacy
		// bounded full-GET path. No partial bytes have been accepted at this point.
		response.Body.Close()
		plain := request.Clone(ctx)
		plain.Header.Del("Range")
		return m.client.Do(plain)
	}
	if response.StatusCode != http.StatusPartialContent {
		return response, nil
	}
	total, _, err := parseImportRange(response, 0, 0, m.options.MaxFileBytes)
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	result := *response
	result.StatusCode = http.StatusOK
	result.ContentLength = total
	result.Body = &importRangeReader{ctx: ctx, client: m.client, request: request, first: response, firstAttempts: attemptsUsed, size: total, maximum: m.options.MaxFileBytes}
	return &result, nil
}

type importRangeReader struct {
	ctx                   context.Context
	client                *http.Client
	request               *http.Request
	first                 *http.Response
	firstAttempts         int
	size, maximum, offset int64
	buffer                []byte
	closed                bool
}

func (r *importRangeReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(r.buffer) == 0 {
		if r.offset == r.size {
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}
func (r *importRangeReader) fill() error {
	wanted := min(importRangeBytes, r.size-r.offset)
	initialAttempt := 0
	if r.first != nil {
		initialAttempt = max(0, r.firstAttempts-1)
	}
	for attempt := initialAttempt; attempt < importRangeAttempts; attempt++ {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		response := r.first
		r.first = nil
		if response == nil {
			next := r.request.Clone(r.ctx)
			next.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.offset, r.offset+wanted-1))
			var err error
			response, err = r.client.Do(next)
			if err != nil {
				if response != nil && response.Body != nil {
					response.Body.Close()
				}
				if attempt+1 < importRangeAttempts {
					if err := importRangePause(r.ctx, attempt); err != nil {
						return err
					}
				}
				continue
			}
		}
		_, length, err := parseImportRange(response, r.offset, r.size, r.maximum)
		if err != nil {
			response.Body.Close()
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, length+1))
		response.Body.Close()
		if int64(len(data)) > length {
			return errImportRange
		}
		if readErr == nil && int64(len(data)) == length {
			r.buffer = data
			r.offset += length
			return nil
		}
		// TLS/EOF failures discard this entire range, not the already verified
		// prefix. Fresh requests retain DNS pinning, redirects and certificate checks.
		if attempt+1 < importRangeAttempts {
			if err := importRangePause(r.ctx, attempt); err != nil {
				return err
			}
		}
	}
	return errImportRange
}
func (r *importRangeReader) Close() error {
	r.closed = true
	r.buffer = nil
	if r.first != nil {
		err := r.first.Body.Close()
		r.first = nil
		return err
	}
	return nil
}
