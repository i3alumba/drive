package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"remote-drive/api/internal/config"
	"remote-drive/api/internal/server"
	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/torrent"
)

func main() {
	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := storage.New(ctx, cfg.MinioEndpoint, cfg.MinioAccessKey, cfg.MinioSecretKey, cfg.MinioBucket, cfg.MinioUseSSL)
	if err != nil {
		slog.Error("connect storage", "err", err)
		os.Exit(1)
	}

	torrentManager := torrent.NewManager(store, cfg.TorrentWorkDir, time.Duration(cfg.TorrentTimeoutS)*time.Second)
	handler := server.New(store, torrentManager).Routes()

	slog.Info("api listening", "addr", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, handler); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}
