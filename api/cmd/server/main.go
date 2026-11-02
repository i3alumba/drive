package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"remote-drive/api/internal/config"
	"remote-drive/api/internal/server"
	"remote-drive/api/internal/torrent"
)

func main() {
	initialize, err := parseOptions(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		slog.Error("invalid arguments", "err", err)
		os.Exit(2)
	}
	cfg := config.Load()
	if initialize {
		if cfg.StorageBackend != "filesystem" {
			slog.Error("initialization requires STORAGE_BACKEND=filesystem")
			os.Exit(1)
		}
		if err := initializeFilesystem(cfg.FilesystemRoot, cfg.FilesystemVolumeID); err != nil {
			slog.Error("initialize filesystem", "err", err)
			os.Exit(1)
		}
		slog.Info("filesystem initialized")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := newStore(ctx, cfg)
	if err != nil {
		slog.Error("connect storage", "err", err)
		os.Exit(1)
	}

	if closer, ok := store.(io.Closer); ok {
		defer closer.Close()
	}

	torrentManager := torrent.NewManager(store, cfg.TorrentWorkDir, time.Duration(cfg.TorrentTimeoutS)*time.Second)
	authValidator := server.NewAuthValidator(cfg.AuthValidateURL)
	handler := server.New(store, torrentManager, authValidator).Routes()

	slog.Info("api listening", "addr", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, handler); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}
