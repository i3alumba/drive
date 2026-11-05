// Package videoworker implements private, Unix-only video preview conversion.
package videoworker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"
)

const (
	InputName  = "input"
	OutputName = "output.mp4"
	SocketName = "worker.sock"
)

type Options struct {
	FFmpeg, FFprobe               string
	MaxInputBytes, MaxOutputBytes int64
	Timeout                       time.Duration
}

type Result struct {
	Mode     string  `json:"mode"`
	Size     int64   `json:"size"`
	Duration float64 `json:"duration"`
}

type Error struct {
	Code string `json:"code"`
}

func (e *Error) Error() string  { return e.Code }
func failure(code string) error { return &Error{Code: code} }
func safeCode(code string) bool {
	switch code {
	case "busy", "invalid_request", "invalid_media", "unsupported_media", "conversion_failed", "worker_unavailable":
		return true
	}
	return false
}

var jobID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type request struct {
	Format         string `json:"format"`
	MaxOutputBytes int64  `json:"max_output_bytes"`
}

func (r request) valid() bool {
	return (r.Format == "mkv" || r.Format == "avi") && r.MaxOutputBytes > 0
}

type Client struct{ http *http.Client }

func NewClient(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout:  65 * time.Minute,
		MaxResponseHeaderBytes: 4096,
		DisableKeepAlives:      true,
	}
	return &Client{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Process(ctx context.Context, id, format string, maxOutputBytes int64) (Result, error) {
	var result Result
	body := request{format, maxOutputBytes}
	if !jobID.MatchString(id) || !body.valid() {
		return result, failure("invalid_request")
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/jobs/"+id, bytes.NewReader(data))
	if err != nil {
		return result, failure("invalid_request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return result, failure("worker_unavailable")
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(data) > 4096 {
		return result, failure("worker_unavailable")
	}
	if resp.StatusCode != http.StatusOK {
		var e Error
		if json.Unmarshal(data, &e) == nil && safeCode(e.Code) {
			return result, &e
		}
		return result, failure("worker_unavailable")
	}
	if json.Unmarshal(data, &result) != nil || (result.Mode != "remux" && result.Mode != "transcode") || result.Size <= 0 || result.Size > maxOutputBytes || !validDuration(result.Duration) {
		return Result{}, failure("worker_unavailable")
	}
	return result, nil
}
