package server

import (
	"testing"

	"remote-drive/api/internal/storage"
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantStart  int64
		wantEnd    int64
		openEnded  bool
		suffix     int64
		wantErr    bool
		wantParsed bool
	}{
		{name: "empty", header: "", wantParsed: false},
		{name: "bounded", header: "bytes=10-99", wantParsed: true, wantStart: 10, wantEnd: 99},
		{name: "open ended", header: "bytes=10-", wantParsed: true, wantStart: 10, openEnded: true},
		{name: "suffix", header: "bytes=-2048", wantParsed: true, suffix: 2048},
		{name: "multipart rejected", header: "bytes=0-1,3-4", wantErr: true},
		{name: "bad suffix", header: "bytes=-0", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, parsed, err := parseRange(tt.header)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parsed != tt.wantParsed {
				t.Fatalf("parsed=%v want %v", parsed, tt.wantParsed)
			}
			if !parsed {
				return
			}
			if got.Start != tt.wantStart || got.End != tt.wantEnd || got.OpenEnded != tt.openEnded || got.SuffixLength != tt.suffix {
				t.Fatalf("range=%+v", got)
			}
		})
	}
}

func TestResolveRange(t *testing.T) {
	tests := []struct {
		name       string
		start      int64
		end        int64
		openEnded  bool
		suffix     int64
		size       int64
		wantStart  int64
		wantEnd    int64
		wantLength int64
	}{
		{name: "bounded", start: 10, end: 19, size: 100, wantStart: 10, wantEnd: 19, wantLength: 10},
		{name: "bounded clipped", start: 90, end: 200, size: 100, wantStart: 90, wantEnd: 99, wantLength: 10},
		{name: "open ended", start: 90, openEnded: true, size: 100, wantStart: 90, wantEnd: 99, wantLength: 10},
		{name: "suffix", suffix: 20, size: 100, wantStart: 80, wantEnd: 99, wantLength: 20},
		{name: "suffix larger than file", suffix: 200, size: 100, wantStart: 0, wantEnd: 99, wantLength: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, length, err := resolveRange(&storage.ByteRange{Start: tt.start, End: tt.end, OpenEnded: tt.openEnded, SuffixLength: tt.suffix}, tt.size)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if start != tt.wantStart || end != tt.wantEnd || length != tt.wantLength {
				t.Fatalf("got %d-%d len %d", start, end, length)
			}
		})
	}
}
