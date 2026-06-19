package torrent

import "testing"

func TestParseAria2Progress(t *testing.T) {
	progress, speed, eta, ok := parseAria2Progress("[#2089b0 400.0KiB/33.2MiB(12%) CN:1 DL:115.8KiB ETA:4m45s]")
	if !ok {
		t.Fatal("expected progress line to parse")
	}
	if progress != 0.12 {
		t.Fatalf("progress = %v", progress)
	}
	if speed != 118579 {
		t.Fatalf("speed = %d", speed)
	}
	if eta != 285 {
		t.Fatalf("eta = %d", eta)
	}
}

func TestParseAria2ProgressWithoutETA(t *testing.T) {
	progress, speed, eta, ok := parseAria2Progress("[#2089b0 1.0MiB/2.0MiB(50%) CN:1 DL:1.5MiB]")
	if !ok {
		t.Fatal("expected progress line to parse")
	}
	if progress != 0.5 {
		t.Fatalf("progress = %v", progress)
	}
	if speed != 1572864 {
		t.Fatalf("speed = %d", speed)
	}
	if eta != 0 {
		t.Fatalf("eta = %d", eta)
	}
}
