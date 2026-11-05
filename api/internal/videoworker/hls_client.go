package videoworker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

type HLSStatus struct {
	Identity HLSIdentity
	State    string
	Terminal *HLSEvent
}

func (s HLSStatus) ConfirmedFor(expected HLSIdentity) bool {
	if !expected.Valid() || s.Identity != expected || s.State != "quiescent" || s.Terminal == nil {
		return false
	}
	e := *s.Terminal
	_, err := e.fields()
	return err == nil && e.Type == "terminal" && e.Quiescence == "confirmed" && (HLSIdentity{JobID: e.JobID, ControllerEpoch: e.ControllerEpoch, ExecutionID: e.ExecutionID}) == expected
}

// hlsObject rejects duplicate, null, missing and unknown members before typed decoding.
func hlsObject(data []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	allowed := map[string]bool{}
	for _, k := range required {
		allowed[k] = true
	}
	for _, k := range optional {
		allowed[k] = true
	}
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, failure("protocol_error")
	}
	result := map[string]json.RawMessage{}
	for d.More() {
		t, err = d.Token()
		k, ok := t.(string)
		if err != nil || !ok || !allowed[k] || result[k] != nil {
			return nil, failure("protocol_error")
		}
		var v json.RawMessage
		if d.Decode(&v) != nil || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, failure("protocol_error")
		}
		result[k] = v
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return nil, failure("protocol_error")
	}
	for _, k := range required {
		if result[k] == nil {
			return nil, failure("protocol_error")
		}
	}
	return result, nil
}

func hlsBoundedResponse(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(b)) > limit || resp.StatusCode != http.StatusOK {
		return nil, failure("teardown_unknown")
	}
	return b, nil
}

func (c *Client) HLSReady(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://worker/v1/capabilities", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", failure("worker_unavailable")
	}
	b, err := hlsBoundedResponse(resp, 2048)
	if err != nil {
		return "", err
	}
	if _, err = hlsObject(b, []string{"v", "isolation", "controller_epoch"}); err != nil {
		return "", err
	}
	var caps struct {
		V         int    `json:"v"`
		Isolation string `json:"isolation"`
		Epoch     string `json:"controller_epoch"`
	}
	if json.Unmarshal(b, &caps) != nil || caps.V != HLSVersion || caps.Isolation != HLSControllerRole || !jobID.MatchString(caps.Epoch) {
		return "", errHLSSandboxUnqualified
	}
	return caps.Epoch, nil
}

func (c *Client) StreamHLS(ctx context.Context, identity HLSIdentity, format string, maxOutputBytes int64, accept func(HLSEvent) error) (HLSSequence, error) {
	zero := HLSSequence{Identity: identity, MaxOutputBytes: maxOutputBytes}
	if !identity.Valid() || !(request{format, maxOutputBytes}).valid() {
		return zero, failure("invalid_request")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	epoch, err := c.HLSReady(ctx)
	if err != nil {
		return zero, err
	}
	if epoch != identity.ControllerEpoch {
		return zero, failure("teardown_unknown")
	}
	body, _ := json.Marshal(hlsRequest{Format: format, MaxOutputBytes: maxOutputBytes, ControllerEpoch: identity.ControllerEpoch, ExecutionID: identity.ExecutionID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/v1/jobs/"+identity.JobID+"/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Bound response headers independently from the conversion deadline while
	// retaining the existing Unix-only transport and caller cancellation.
	headerTimer := time.AfterFunc(5*time.Second, cancel)
	resp, err := c.http.Do(req)
	headerTimer.Stop()
	if err != nil {
		return zero, failure("teardown_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Header.Values("X-Video-Controller-Epoch")) != 1 || len(resp.Header.Values("X-Video-Execution-ID")) != 1 || resp.Header.Get("X-Video-Controller-Epoch") != identity.ControllerEpoch || resp.Header.Get("X-Video-Execution-ID") != identity.ExecutionID {
		return zero, failure("teardown_unknown")
	}
	return ReadHLSEvents(resp.Body, maxOutputBytes, identity, accept)
}

type hlsRequest struct {
	Format          string `json:"format"`
	MaxOutputBytes  int64  `json:"max_output_bytes"`
	ControllerEpoch string `json:"controller_epoch"`
	ExecutionID     string `json:"execution_id"`
}

func (c *Client) LookupHLS(ctx context.Context, identity HLSIdentity) (HLSStatus, error) {
	return c.hlsControl(ctx, identity, http.MethodGet)
}
func (c *Client) CancelHLS(ctx context.Context, identity HLSIdentity) (HLSStatus, error) {
	return c.hlsControl(ctx, identity, http.MethodDelete)
}
func (c *Client) hlsControl(ctx context.Context, identity HLSIdentity, method string) (HLSStatus, error) {
	unknown := HLSStatus{Identity: identity, State: "unknown"}
	if !identity.Valid() {
		return unknown, failure("invalid_request")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := url.Values{"controller_epoch": {identity.ControllerEpoch}, "execution_id": {identity.ExecutionID}}
	req, _ := http.NewRequestWithContext(ctx, method, "http://worker/v1/jobs/"+identity.JobID+"?"+query.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return unknown, failure("teardown_unknown")
	}
	b, err := hlsBoundedResponse(resp, 4096)
	if err != nil {
		return unknown, err
	}
	fields, err := hlsObject(b, []string{"v", "job_id", "controller_epoch", "execution_id", "state"}, "terminal")
	if err != nil {
		return unknown, err
	}
	var wire struct {
		V int `json:"v"`
		HLSIdentity
		State string `json:"state"`
	}
	if json.Unmarshal(b, &wire) != nil || wire.V != HLSVersion || wire.HLSIdentity != identity {
		return unknown, failure("teardown_unknown")
	}
	status := HLSStatus{Identity: identity, State: wire.State}
	if raw, ok := fields["terminal"]; ok {
		e, err := DecodeHLSEvent(raw)
		if err != nil {
			return unknown, err
		}
		status.Terminal = &e
	}
	switch status.State {
	case "running", "stopping", "unknown":
		if status.Terminal != nil {
			return unknown, failure("protocol_error")
		}
	case "quiescent":
		if !status.ConfirmedFor(identity) {
			return unknown, failure("teardown_unknown")
		}
	default:
		return unknown, failure("protocol_error")
	}
	return status, nil
}
