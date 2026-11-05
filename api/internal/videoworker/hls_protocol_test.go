package videoworker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func hlsTestDigest() string {
	hash := sha256.Sum256([]byte("synthetic media"))
	return hex.EncodeToString(hash[:])
}

func hlsTestIdentity() HLSIdentity {
	return HLSIdentity{JobID: strings.Repeat("1", 32), ControllerEpoch: strings.Repeat("2", 32), ExecutionID: strings.Repeat("3", 32)}
}

func hlsTestEvents() []HLSEvent {
	identity := hlsTestIdentity()
	return []HLSEvent{
		{V: 1, Type: "heartbeat"},
		{V: 1, Type: "started", Mode: "transcode", ExpectedDurationUS: 4000000},
		{V: 1, Type: "init", Bytes: 64, SHA256: hlsTestDigest()},
		{V: 1, Type: "segment", Index: 0, Bytes: 256, SHA256: hlsTestDigest(), DurationUS: 4000000},
		{V: 1, Type: "conversion_end", Outcome: "complete", SegmentCount: 1, DurationUS: 4000000},
		{V: 1, Type: "terminal", Outcome: "complete", JobID: identity.JobID, ControllerEpoch: identity.ControllerEpoch, ExecutionID: identity.ExecutionID, Quiescence: "confirmed"},
	}
}

func hlsTestWire(t *testing.T, events []HLSEvent) []byte {
	t.Helper()
	var output bytes.Buffer
	for _, event := range events {
		if err := EncodeHLSEvent(&output, event); err != nil {
			t.Fatal(err)
		}
	}
	return output.Bytes()
}

func TestHLSEventCanonicalRoundTrip(t *testing.T) {
	for _, event := range hlsTestEvents() {
		data := hlsTestWire(t, []HLSEvent{event})
		decoded, err := DecodeHLSEvent(bytes.TrimSuffix(data, []byte{'\n'}))
		if err != nil || decoded != event {
			t.Fatalf("round trip: %+v => %+v %v", event, decoded, err)
		}
		if event.Type == "segment" && !bytes.Contains(data, []byte(`"index":0`)) {
			t.Fatal("zero segment index omitted")
		}
	}
}

func TestHLSEventStrictShape(t *testing.T) {
	for _, data := range []string{
		`{}`, `null`, `[]`,
		`{"v":1,"type":"heartbeat","url":"https://invalid.example"}`,
		`{"v":1,"type":"heartbeat","v":1}`,
		`{"v":1,"type":"heartbeat","mode":"transcode"}`,
		`{"v":1,"type":"terminal","outcome":"complete"}`,
		`{"v":1,"type":"terminal","outcome":"failed","code":"raw decoder path"}`,
		`{"v":1,"type":"terminal","outcome":"cancelled","code":"conversion_failed"}`,
		`{"v":1,"type":"terminal","outcome":"complete","code":"conversion_failed"}`,
		`{"v":1,"type":"started","mode":"transcode","expected_duration_us":1.5}`,
		`{"v":1,"type":"started","mode":"transcode","expected_duration_us":"100"}`,
		`{"v":1,"type":"started","mode":"transcode","expected_duration_us":null}`,
		`{"v":2,"type":"heartbeat"}`,
		`{"v":1,"type":"heartbeat"}{}`,
		strings.Repeat(" ", HLSEventMaxBytes),
	} {
		if _, err := DecodeHLSEvent([]byte(data)); err == nil {
			t.Fatal("invalid shape accepted", data)
		}
	}
}

func TestHLSEventArtifactLimits(t *testing.T) {
	base := HLSEvent{V: 1, Type: "segment", Index: 0, Bytes: 100, SHA256: hlsTestDigest(), DurationUS: 1000000}
	for _, change := range []func(*HLSEvent){
		func(e *HLSEvent) { e.Index = -1 },
		func(e *HLSEvent) { e.Index = HLSMaxSegments },
		func(e *HLSEvent) { e.Bytes = HLSSegmentMaxBytes + 1 },
		func(e *HLSEvent) { e.Bytes = 0 },
		func(e *HLSEvent) { e.DurationUS = HLSTargetSeconds*1000000 + 1 },
		func(e *HLSEvent) { e.DurationUS = -1 },
		func(e *HLSEvent) { e.SHA256 = strings.ToUpper(e.SHA256) },
		func(e *HLSEvent) { e.SHA256 = strings.Repeat("g", 64) },
	} {
		event := base
		change(&event)
		if err := EncodeHLSEvent(io.Discard, event); err == nil {
			t.Fatal("invalid artifact accepted", event)
		}
	}
	if err := EncodeHLSEvent(io.Discard, HLSEvent{V: 1, Type: "init", Bytes: HLSInitMaxBytes + 1, SHA256: hlsTestDigest()}); err == nil {
		t.Fatal("oversized initialization accepted")
	}
}

func TestHLSSequencePrefixAndTerminal(t *testing.T) {
	seen := 0
	ledger, err := ReadHLSEvents(bytes.NewReader(hlsTestWire(t, hlsTestEvents())), 320, hlsTestIdentity(), func(event HLSEvent) error { seen++; return nil })
	if err != nil || seen != 6 || !ledger.Terminal || ledger.Outcome != "complete" || ledger.Segments != 1 || ledger.Bytes != 320 || ledger.DurationUS != 4000000 {
		t.Fatal("valid sequence rejected", ledger, err)
	}
	for _, events := range [][]HLSEvent{
		hlsTestEvents()[:4],
		{hlsTestEvents()[2]},
		{hlsTestEvents()[1], hlsTestEvents()[1]},
		{hlsTestEvents()[1], hlsTestEvents()[3]},
		{hlsTestEvents()[1], hlsTestEvents()[2], hlsTestEvents()[3], hlsTestEvents()[3]},
		append(hlsTestEvents(), HLSEvent{V: 1, Type: "heartbeat"}),
	} {
		if _, err := ReadHLSEvents(bytes.NewReader(hlsTestWire(t, events)), 320, hlsTestIdentity(), nil); err == nil {
			t.Fatal("invalid sequence accepted", events)
		}
	}
	if _, err := ReadHLSEvents(bytes.NewReader(hlsTestWire(t, hlsTestEvents())), 319, hlsTestIdentity(), nil); err == nil {
		t.Fatal("aggregate output limit bypassed")
	}
}

func TestHLSSequenceDurationAndIndexFences(t *testing.T) {
	for _, kind := range []string{"skip", "too_long", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			events := hlsTestEvents()
			switch kind {
			case "skip":
				events[3].Index = 1
			case "too_long":
				events[3].DurationUS = 7000000
			case "truncated":
				events[3].DurationUS = 500000
			}
			if _, err := ReadHLSEvents(bytes.NewReader(hlsTestWire(t, events)), 320, hlsTestIdentity(), nil); err == nil {
				t.Fatal("duration/index fence bypassed", kind)
			}
		})
	}
	sequence := HLSSequence{MaxOutputBytes: 1024, Events: HLSMaxEvents, Identity: hlsTestIdentity()}
	if sequence.Accept(HLSEvent{V: 1, Type: "heartbeat"}) == nil {
		t.Fatal("unbounded heartbeat/event count")
	}
}

func TestHLSTruncatedTransportIsUnknownNotSuccess(t *testing.T) {
	_, err := ReadHLSEvents(bytes.NewReader(hlsTestWire(t, hlsTestEvents()[:5])), 320, hlsTestIdentity(), nil)
	var problem *Error
	if !errors.As(err, &problem) || problem.Code != "teardown_unknown" {
		t.Fatal("EOF falsely acknowledged completion", err)
	}
	_, err = ReadHLSEvents(strings.NewReader(strings.Repeat("x", HLSEventMaxBytes+1)), 320, hlsTestIdentity(), nil)
	if err == nil {
		t.Fatal("oversized event accepted")
	}
}

func TestHLSConversionEndCannotForgeQuiescence(t *testing.T) {
	for _, kind := range []string{"wrong_job", "wrong_epoch", "wrong_execution", "no_conversion_end", "artifact_after_conversion"} {
		t.Run(kind, func(t *testing.T) {
			events := hlsTestEvents()
			switch kind {
			case "wrong_job":
				events[5].JobID = strings.Repeat("4", 32)
			case "wrong_epoch":
				events[5].ControllerEpoch = strings.Repeat("4", 32)
			case "wrong_execution":
				events[5].ExecutionID = strings.Repeat("4", 32)
			case "no_conversion_end":
				events = append(events[:4], events[5])
			case "artifact_after_conversion":
				events = append(events[:5], events[3])
			}
			if _, err := ReadHLSEvents(bytes.NewReader(hlsTestWire(t, events)), 1024, hlsTestIdentity(), nil); err == nil {
				t.Fatal("unbound or premature terminal/artifact accepted", kind)
			}
		})
	}
	for _, kind := range []string{"unconfirmed", "unknown_teardown"} {
		event := hlsTestEvents()[5]
		if kind == "unconfirmed" {
			event.Quiescence = "unknown"
		} else {
			event.Outcome, event.Code = "failed", "teardown_unknown"
		}
		if EncodeHLSEvent(io.Discard, event) == nil {
			t.Fatal("development/unknown execution forged a confirmed receipt", kind)
		}
	}
}

func TestHLSManifestUsesOnlyFixedPrefix(t *testing.T) {
	first, err := HLSManifest([]int64{4000000}, false)
	if err != nil || strings.Contains(first, "ENDLIST") || !strings.Contains(first, "#EXT-X-TARGETDURATION:8\n") || !strings.Contains(first, "segment-000000.m4s") {
		t.Fatal(first, err)
	}
	second, err := HLSManifest([]int64{4000000, 3500000}, true)
	if err != nil || !strings.HasSuffix(second, "#EXT-X-ENDLIST\n") || strings.Contains(second, "http") || strings.Contains(second, "EXT-X-KEY") || !strings.Contains(second, "#EXTINF:3.500000,") {
		t.Fatal(second, err)
	}
	for _, duration := range []int64{0, -1, 8000001} {
		if _, err = HLSManifest([]int64{duration}, false); err == nil {
			t.Fatal("bad manifest duration accepted")
		}
	}
	if _, err = HLSSegmentName(-1); err == nil {
		t.Fatal("negative index accepted")
	}
	if _, err = HLSSegmentName(HLSMaxSegments); err == nil {
		t.Fatal("unbounded index accepted")
	}
}
