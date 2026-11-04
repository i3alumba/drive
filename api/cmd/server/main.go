package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	transportTLS, err := apiTLS(cfg)
	if err != nil {
		slog.Error("invalid API HTTPS identity", "err", err)
		os.Exit(1)
	}
	if cfg.StorageBackend == "minio" && !cfg.MinioUseSSL {
		slog.Error("MinIO HTTPS is required")
		os.Exit(1)
	}
	authValidator := server.NewAuthValidator(cfg.AuthValidateURL)
	if cfg.AuthValidateURL != "" && authValidator == nil {
		slog.Error("invalid Auth HTTPS endpoint")
		os.Exit(1)
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
	app := server.New(store, torrentManager, authValidator)
	if runtime.GOOS == "linux" {
		if err := app.EnableUploads(server.UploadOptions{
			Root:         filepath.Join(cfg.TorrentWorkDir, ".upload-sessions-v1"),
			MaxFileBytes: cfg.UploadMaxFileBytes,
			BudgetBytes:  cfg.UploadBudgetBytes,
			ReserveBytes: cfg.UploadReserveBytes,
		}); err != nil {
			slog.Error("enable resumable uploads", "err", err)
			os.Exit(1)
		}
		defer app.CloseUploads()
		if err := app.EnableImports(server.ImportOptions{
			Root:         filepath.Join(cfg.TorrentWorkDir, ".url-imports-v1"),
			MaxFileBytes: cfg.ImportMaxFileBytes,
			BudgetBytes:  cfg.ImportBudgetBytes,
			ReserveBytes: cfg.ImportReserveBytes,
			Timeout:      time.Duration(cfg.ImportTimeoutS) * time.Second,
		}); err != nil {
			slog.Error("enable server-side URL imports", "err", err)
			os.Exit(1)
		}
		defer app.CloseImports()
		if err := app.EnableImportQueues(); err != nil {
			slog.Error("enable durable URL queues", "err", err)
			os.Exit(1)
		}
	} else {
		// Preserve legacy MinIO/API use on other hosts; Linux supplies the
		// no-follow/flock primitives required by resumable staging.
		slog.Warn("resumable uploads require a Linux API instance")
	}
	handler := app.Routes()

	api := &http.Server{
		Addr: cfg.Addr, Handler: handler, TLSConfig: transportTLS,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}
	// No body/write deadline: large uploads and native streaming keep their contract.
	slog.Info("api HTTPS listening", "addr", cfg.Addr)
	if err := api.ListenAndServeTLS("", ""); err != nil {
		slog.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

// Validate before any storage/torrent construction; no plaintext fallback.
func apiTLS(cfg config.Config) (*tls.Config, error) {
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return nil, errors.New("API HTTPS certificate and key required")
	}
	identity, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, errors.New("invalid API HTTPS certificate/key")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{identity}}, nil
}
