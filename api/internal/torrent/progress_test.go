package torrent

import "testing"

func TestParseAria2ProgressWithETA(t *testing.T) {
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

func TestParseAria2ProgressComputesETAWhenMissing(t *testing.T) {
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
	if eta != 1 {
		t.Fatalf("eta = %d", eta)
	}
}

func TestParseAria2ProgressWithDecimalMegabytes(t *testing.T) {
	progress, speed, eta, ok := parseAria2Progress("[#2089b0 10.5MiB/42.0MiB(25%) CN:2 DL:2.0MiB/s]")
	if !ok {
		t.Fatal("expected progress line to parse")
	}
	if progress != 0.25 {
		t.Fatalf("progress = %v", progress)
	}
	if speed != 2097152 {
		t.Fatalf("speed = %d", speed)
	}
	if eta != 16 {
		t.Fatalf("eta = %d", eta)
	}
}
