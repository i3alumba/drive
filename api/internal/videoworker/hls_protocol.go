package videoworker

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const (
	HLSVersion         = 1
	HLSInitName        = "init.mp4"
	HLSPlaylistName    = "stream.m3u8"
	HLSInitMaxBytes    = 1 << 20
	HLSSegmentMaxBytes = 64 << 20
	HLSMaxSegments     = 8192
	HLSTargetSeconds   = 8
	HLSMaxDurationUS   = 6 * 60 * 60 * 1000000
	HLSEventMaxBytes   = 2048
	HLSMaxEvents       = 16384
	HLSControllerRole  = "isolated-job-controller-v1"
)

// HLSEvent never transports a filename, URL, playlist, credential or diagnostic.
// Its exact allowed fields depend on Type; decoding rejects duplicate/extra keys.
type HLSIdentity struct {
	JobID           string `json:"job_id"`
	ControllerEpoch string `json:"controller_epoch"`
	ExecutionID     string `json:"execution_id"`
}

func (identity HLSIdentity) Valid() bool {
	return jobID.MatchString(identity.JobID) && jobID.MatchString(identity.ControllerEpoch) && jobID.MatchString(identity.ExecutionID)
}

type HLSEvent struct {
	V                  int    `json:"v"`
	Type               string `json:"type"`
	Mode               string `json:"mode,omitempty"`
	ExpectedDurationUS int64  `json:"expected_duration_us,omitempty"`
	Bytes              int64  `json:"bytes,omitempty"`
	SHA256             string `json:"sha256,omitempty"`
	Index              int    `json:"index,omitempty"`
	DurationUS         int64  `json:"duration_us,omitempty"`
	Outcome            string `json:"outcome,omitempty"`
	Code               string `json:"code,omitempty"`
	SegmentCount       int    `json:"segment_count,omitempty"`
	JobID              string `json:"job_id,omitempty"`
	ControllerEpoch    string `json:"controller_epoch,omitempty"`
	ExecutionID        string `json:"execution_id,omitempty"`
	Quiescence         string `json:"quiescence,omitempty"`
}

func HLSSegmentName(index int) (string, error) {
	if index < 0 || index >= HLSMaxSegments {
		return "", failure("protocol_error")
	}
	return fmt.Sprintf("segment-%06d.m4s", index), nil
}

func hlsDigestValid(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func hlsSafeCode(code string) bool {
	return safeCode(code) || code == "cancelled" || code == "quota_exceeded" || code == "protocol_error" || code == "teardown_unknown" || code == "sandbox_unqualified"
}

func (e HLSEvent) fields() (map[string]any, error) {
	if e.V != HLSVersion {
		return nil, failure("protocol_error")
	}
	fields := map[string]any{"v": e.V, "type": e.Type}
	switch e.Type {
	case "started":
		if (e.Mode != "remux" && e.Mode != "transcode") || e.ExpectedDurationUS <= 0 || e.ExpectedDurationUS > HLSMaxDurationUS {
			return nil, failure("protocol_error")
		}
		fields["mode"], fields["expected_duration_us"] = e.Mode, e.ExpectedDurationUS
	case "init":
		if e.Bytes <= 0 || e.Bytes > HLSInitMaxBytes || !hlsDigestValid(e.SHA256) {
			return nil, failure("protocol_error")
		}
		fields["bytes"], fields["sha256"] = e.Bytes, e.SHA256
	case "segment":
		if e.Index < 0 || e.Index >= HLSMaxSegments || e.Bytes <= 0 || e.Bytes > HLSSegmentMaxBytes || !hlsDigestValid(e.SHA256) || e.DurationUS <= 0 || e.DurationUS > HLSTargetSeconds*1000000 {
			return nil, failure("protocol_error")
		}
		fields["index"], fields["bytes"], fields["sha256"], fields["duration_us"] = e.Index, e.Bytes, e.SHA256, e.DurationUS
	case "heartbeat":
	case "conversion_end", "terminal":
		if e.Outcome != "complete" && e.Outcome != "failed" && e.Outcome != "cancelled" {
			return nil, failure("protocol_error")
		}
		if (e.Outcome == "complete" && e.Code != "") || (e.Outcome != "complete" && !hlsSafeCode(e.Code)) || (e.Outcome == "cancelled" && e.Code != "cancelled") {
			return nil, failure("protocol_error")
		}
		fields["outcome"], fields["code"] = e.Outcome, e.Code
		if e.Type == "conversion_end" {
			if e.SegmentCount < 0 || e.SegmentCount > HLSMaxSegments || e.DurationUS < 0 || e.DurationUS > HLSMaxDurationUS {
				return nil, failure("protocol_error")
			}
			fields["segment_count"], fields["duration_us"] = e.SegmentCount, e.DurationUS
		} else {
			identity := HLSIdentity{JobID: e.JobID, ControllerEpoch: e.ControllerEpoch, ExecutionID: e.ExecutionID}
			if !identity.Valid() || e.Quiescence != "confirmed" || e.Code == "teardown_unknown" || e.Code == "sandbox_unqualified" {
				return nil, failure("protocol_error")
			}
			fields["job_id"], fields["controller_epoch"], fields["execution_id"], fields["quiescence"] = e.JobID, e.ControllerEpoch, e.ExecutionID, e.Quiescence
		}
	default:
		return nil, failure("protocol_error")
	}
	return fields, nil
}

func EncodeHLSEvent(writer io.Writer, event HLSEvent) error {
	fields, err := event.fields()
	if err != nil {
		return err
	}
	data, err := json.Marshal(fields)
	if err != nil || len(data)+1 > HLSEventMaxBytes {
		return failure("protocol_error")
	}
	data = append(data, '\n')
	n, err := writer.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func DecodeHLSEvent(data []byte) (HLSEvent, error) {
	var event HLSEvent
	if len(data) == 0 || len(data)+1 > HLSEventMaxBytes {
		return event, failure("protocol_error")
	}
	// Decode keys separately: encoding/json otherwise accepts duplicate keys.
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return event, failure("protocol_error")
	}
	keys := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || keys[key] {
			return event, failure("protocol_error")
		}
		keys[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return event, failure("protocol_error")
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return event, failure("protocol_error")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&event) != nil {
		return event, failure("protocol_error")
	}
	fields, err := event.fields()
	if err != nil || len(fields) != len(keys) {
		return event, failure("protocol_error")
	}
	for key := range fields {
		if !keys[key] {
			return event, failure("protocol_error")
		}
	}
	return event, nil
}

// HLSSequence enforces one contiguous bounded prefix. Heartbeats do not extend
// deadlines or prove teardown. Only a trusted controller may emit terminal.
type HLSSequence struct {
	Identity                                        HLSIdentity
	Started, Initialized, ConversionEnded, Terminal bool
	ConversionOutcome                               string
	Mode                                            string
	ExpectedDurationUS                              int64
	DurationUS, Bytes                               int64
	Segments, Events                                int
	MaxOutputBytes                                  int64
	Outcome, Code                                   string
}

func (s *HLSSequence) Accept(e HLSEvent) error {
	if _, err := e.fields(); err != nil || s.Terminal || s.Events >= HLSMaxEvents || s.MaxOutputBytes <= 0 || !s.Identity.Valid() {
		return failure("protocol_error")
	}
	// Validate before mutating the ledger; callers may inspect it after errors.
	if s.ConversionEnded && e.Type != "heartbeat" && e.Type != "terminal" {
		return failure("protocol_error")
	}
	switch e.Type {
	case "started":
		if s.Started {
			return failure("protocol_error")
		}
	case "init":
		if !s.Started || s.Initialized || e.Bytes > s.MaxOutputBytes-s.Bytes {
			return failure("protocol_error")
		}
	case "segment":
		if !s.Initialized || e.Index != s.Segments || e.Bytes > s.MaxOutputBytes-s.Bytes || e.DurationUS > HLSMaxDurationUS-s.DurationUS || e.DurationUS > s.ExpectedDurationUS+2000000-s.DurationUS {
			return failure("protocol_error")
		}
	case "conversion_end":
		if e.SegmentCount != s.Segments || e.DurationUS != s.DurationUS || (e.Outcome == "complete" && (!s.Initialized || s.Segments == 0 || s.DurationUS < s.ExpectedDurationUS-min(int64(2000000), max(int64(100000), s.ExpectedDurationUS/100)))) {
			return failure("protocol_error")
		}
	case "terminal":
		identity := HLSIdentity{JobID: e.JobID, ControllerEpoch: e.ControllerEpoch, ExecutionID: e.ExecutionID}
		if !s.ConversionEnded || identity != s.Identity || (e.Outcome == "complete" && s.ConversionOutcome != "complete") {
			return failure("protocol_error")
		}
	}
	s.Events++
	switch e.Type {
	case "started":
		s.Started, s.Mode, s.ExpectedDurationUS = true, e.Mode, e.ExpectedDurationUS
	case "init":
		s.Initialized = true
		s.Bytes += e.Bytes
	case "segment":
		s.Segments++
		s.Bytes += e.Bytes
		s.DurationUS += e.DurationUS
	case "conversion_end":
		s.ConversionEnded, s.ConversionOutcome = true, e.Outcome
	case "terminal":
		s.Terminal, s.Outcome, s.Code = true, e.Outcome, e.Code
	}
	return nil
}

func ReadHLSEvents(reader io.Reader, maxOutputBytes int64, identity HLSIdentity, accept func(HLSEvent) error) (HLSSequence, error) {
	sequence := HLSSequence{MaxOutputBytes: maxOutputBytes, Identity: identity}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, HLSEventMaxBytes), HLSEventMaxBytes)
	for scanner.Scan() {
		event, err := DecodeHLSEvent(scanner.Bytes())
		if err != nil {
			return sequence, err
		}
		if err = sequence.Accept(event); err != nil {
			return sequence, err
		}
		if accept != nil {
			if err = accept(event); err != nil {
				return sequence, err
			}
		}
	}
	if scanner.Err() != nil || !sequence.Terminal {
		return sequence, failure("teardown_unknown")
	}
	return sequence, nil
}

// HLSManifest uses only accepted, independently validated private artifacts.
// Durations and filenames never come from an untrusted playlist or URL.
func HLSManifest(durationsUS []int64, complete bool) (string, error) {
	if len(durationsUS) == 0 || len(durationsUS) > HLSMaxSegments {
		return "", failure("protocol_error")
	}
	var buffer bytes.Buffer
	buffer.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:8\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init.mp4\"\n")
	var total int64
	for index, duration := range durationsUS {
		if duration <= 0 || duration > HLSTargetSeconds*1000000 || duration > HLSMaxDurationUS-total {
			return "", failure("protocol_error")
		}
		total += duration
		buffer.WriteString("#EXTINF:")
		buffer.WriteString(strconv.FormatInt(duration/1000000, 10))
		buffer.WriteByte('.')
		buffer.WriteString(fmt.Sprintf("%06d", duration%1000000))
		buffer.WriteString(",\n")
		name, _ := HLSSegmentName(index)
		buffer.WriteString(name)
		buffer.WriteByte('\n')
	}
	if complete {
		buffer.WriteString("#EXT-X-ENDLIST\n")
	}
	return buffer.String(), nil
}

var errHLSSandboxUnqualified = errors.New("HLS controller cannot prove sandbox quiescence")
