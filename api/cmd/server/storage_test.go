package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"remote-drive/api/internal/config"
)

func TestStorageOptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		initialize bool
		wantErr    bool
	}{
		{"server", nil, false, false},
		{"HTTPS capability", []string{"--require-https"}, false, false},
		{"HTTPS capability false is still server mode", []string{"--require-https=false"}, false, false},
		{"HTTPS cannot initialize storage", []string{"--require-https", "--init-filesystem"}, false, true},
		{"initialize", []string{"--init-filesystem"}, true, false},
		{"disabled initialize", []string{"--init-filesystem=false"}, false, false},
		{"unknown flag", []string{"--unknown"}, false, true},
		{"extra arguments", []string{"unexpected"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initialize, err := parseOptions(tc.args, io.Discard)
			if initialize != tc.initialize || (err != nil) != tc.wantErr {
				t.Fatalf("option result: %v %v", initialize, err)
			}
		})
	}
	if _, err := parseOptions([]string{"--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatal("help mode not recognized")
	}
}

func TestStorageSelectionFailsClosedWithoutMinioFallback(t *testing.T) {
	if _, err := newStore(context.Background(), config.Config{StorageBackend: "typo"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := newStore(context.Background(), config.Config{StorageBackend: "filesystem", FilesystemRoot: missing, FilesystemVolumeID: "00000000-0000-0000-0000-000000000001"}); err == nil {
		t.Fatal("unmounted filesystem accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("filesystem mode created a local root")
	}
}

func TestVideoInitializationModeIsSeparate(t *testing.T) {
	options, e := parseRuntimeOptions([]string{"--init-video-root"}, io.Discard)
	if e != nil || !options.InitializeVideo {
		t.Fatalf("%+v %v", options, e)
	}
	for _, args := range [][]string{{"--init-video-root", "--init-filesystem"}, {"--init-video-root", "--legacy-http"}, {"--init-video-root", "--require-https"}} {
		if _, e := parseRuntimeOptions(args, io.Discard); e == nil {
			t.Fatal("mixed modes accepted")
		}
	}
}
