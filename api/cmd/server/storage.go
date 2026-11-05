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

type runtimeOptions struct {
	Initialize bool
	LegacyHTTP bool
}

// Keep the initialization parser contract for existing callers/tests.
func parseOptions(args []string, output io.Writer) (bool, error) {
	options, err := parseRuntimeOptions(args, output)
	return options.Initialize, err
}

func parseRuntimeOptions(args []string, output io.Writer) (runtimeOptions, error) {
	flags := flag.NewFlagSet("drive-api", flag.ContinueOnError)
	flags.SetOutput(output)
	initialize := flags.Bool("init-filesystem", false, "initialize an empty, already-mounted NFS root and exit; requires filesystem backend and volume UUID")
	// Migration capability guard: older HTTP-only binaries reject this flag
	// before opening storage or starting durable queue workers.
	requireHTTPS := flags.Bool("require-https", false, "require native HTTPS serving; incompatible with explicit legacy HTTP")
	legacyHTTP := flags.Bool("legacy-http", false, "explicitly preserve the existing HTTP API/Auth transport; never a TLS failure fallback")
	if err := flags.Parse(args); err != nil {
		return runtimeOptions{}, err
	}
	if *initialize && (*requireHTTPS || *legacyHTTP) {
		return runtimeOptions{}, errors.New("serving and filesystem initialization are separate modes")
	}
	httpsSupplied := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "require-https" {
			httpsSupplied = true
		}
	})
	if httpsSupplied && *legacyHTTP {
		return runtimeOptions{}, errors.New("HTTPS and legacy HTTP are mutually exclusive")
	}
	if flags.NArg() != 0 {
		return runtimeOptions{}, errors.New("unexpected positional arguments")
	}
	return runtimeOptions{Initialize: *initialize, LegacyHTTP: *legacyHTTP}, nil
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
