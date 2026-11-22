package videoworker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const RemoteSourceVersion = 3
const RemoteControllerRole = "isolated-range-source-egress-controller-v3"

// RemoteSource is an execution secret sent only to the trusted Unix controller.
// Source identity/authorization stay at the API; no storage key leaves the API.
type RemoteSource struct {
	StartTime      float64     `json:"start_time"`
	ForceTranscode bool        `json:"force_transcode"`
	V              int         `json:"v"`
	Identity       HLSIdentity `json:"identity"`
	Endpoint       string      `json:"endpoint"`
	Size           int64       `json:"size"`
	Deadline       time.Time   `json:"deadline"`
	MaxReadBytes   int64       `json:"max_read_bytes"`
	ChunkBytes     int64       `json:"chunk_bytes"`
	Concurrency    int         `json:"concurrency"`
}

func (s RemoteSource) ValidFor(identity HLSIdentity) bool {
	u, e := url.Parse(s.Endpoint)
	if e != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	port, pe := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || pe != nil || port < 1 || port > 65535 || len(u.Path) != 71 {
		return false
	}
	token, te := hex.DecodeString(u.Path[7:])
	if te != nil || len(token) != 32 {
		return false
	}
	return !math.IsNaN(s.StartTime) && !math.IsInf(s.StartTime, 0) && s.StartTime >= 0 && s.StartTime < 21600 && u.Scheme == "http" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(u.Path) == 71 && u.Path[:7] == "/input/" && s.V == RemoteSourceVersion && identity.Valid() && s.Identity == identity && s.Size > 0 && s.Deadline.After(time.Now()) && s.MaxReadBytes > 0 && s.ChunkBytes > 0 && s.Concurrency > 0
}

type RemoteHLSRequest struct {
	V              int          `json:"v"`
	Identity       HLSIdentity  `json:"identity"`
	Format         string       `json:"format"`
	MaxOutputBytes int64        `json:"max_output_bytes"`
	Source         RemoteSource `json:"source"`
}

func (c *Client) HLSRemoteReady(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://worker/v3/capabilities", nil)
	resp, e := c.http.Do(req)
	if e != nil {
		return "", failure("worker_unavailable")
	}
	b, e := hlsBoundedResponse(resp, 2048)
	if e != nil {
		return "", e
	}
	if _, e = hlsObject(b, []string{"v", "isolation", "controller_epoch", "source", "egress"}); e != nil {
		return "", e
	}
	var caps struct {
		V         int    `json:"v"`
		Isolation string `json:"isolation"`
		Epoch     string `json:"controller_epoch"`
		Source    string `json:"source"`
		Egress    string `json:"egress"`
	}
	if json.Unmarshal(b, &caps) != nil || caps.V != RemoteSourceVersion || caps.Isolation != RemoteControllerRole || !jobID.MatchString(caps.Epoch) || caps.Source != "bounded-http-range-timeline-v3" || caps.Egress != "selected-broker-only-no-redirect-no-proxy" {
		return "", errHLSSandboxUnqualified
	}
	return caps.Epoch, nil
}
func (c *Client) StreamRemoteHLS(ctx context.Context, identity HLSIdentity, format string, maxOutputBytes int64, source RemoteSource, accept func(HLSEvent) error) (HLSSequence, error) {
	return c.streamHLS(ctx, identity, format, maxOutputBytes, &source, accept)
}
