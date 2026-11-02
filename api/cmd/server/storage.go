package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"remote-drive/api/internal/config"
	"remote-drive/api/internal/server"
	"remote-drive/api/internal/storage"
)

func parseOptions(args []string, output io.Writer) (bool, error) {
	flags := flag.NewFlagSet("drive-api", flag.ContinueOnError)
	flags.SetOutput(output)
	initialize := flags.Bool("init-filesystem", false, "initialize an empty, already-mounted NFS root and exit; requires filesystem backend and volume UUID")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if flags.NArg() != 0 {
		return false, errors.New("unexpected positional arguments")
	}
	return *initialize, nil
}

func newStore(ctx context.Context, cfg config.Config) (server.Store, error) {
	switch cfg.StorageBackend {
	case "minio":
		return storage.New(ctx, cfg.MinioEndpoint, cfg.MinioAccessKey, cfg.MinioSecretKey, cfg.MinioBucket, cfg.MinioUseSSL)
	case "filesystem":
		return newFilesystem(cfg.FilesystemRoot, cfg.FilesystemVolumeID)
	default:
		return nil, errors.New("unknown STORAGE_BACKEND (expected minio or filesystem)")
	}
}
