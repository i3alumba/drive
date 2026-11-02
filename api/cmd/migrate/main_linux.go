//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"remote-drive/api/internal/migration"
	"remote-drive/api/internal/storage"
)

type target interface {
	migration.Destination
	Close() error
}
type connections struct {
	source      func(context.Context, string, string, string, string, bool) (migration.Source, error)
	destination func(string, string) (target, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	factory := connections{
		source: func(ctx context.Context, endpoint, bucket, access, secret string, secure bool) (migration.Source, error) {
			return migration.NewS3Source(ctx, endpoint, bucket, access, secret, secure)
		},
		destination: func(root, volume string) (target, error) { return storage.NewFilesystem(root, volume) },
	}
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, factory); err != nil {
		// All boundary errors are sanitized by run/adapters; never log native
		// response bodies, credentials, filenames or whole manifests.
		fmt.Fprintln(os.Stderr, "drive-migrate:", err)
		os.Exit(1)
	}
}

func loadManifest(path string) (migration.Manifest, error) {
	file, err := migration.OpenPrivateFile(path)
	if err != nil {
		return migration.Manifest{}, err
	}
	defer file.Close()
	return migration.ReadManifest(file)
}

func run(parent context.Context, args []string, env func(string) string, output, diagnostics io.Writer, factory connections) error {
	flags := flag.NewFlagSet("drive-migrate", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	mode := flags.String("mode", "", "inventory|backup|verify-backup|restore-backup|copy|sync|verify|verify-source")
	manifestPath := flags.String("manifest", "", "private manifest path; new output for inventory, existing input otherwise")
	archivePath := flags.String("archive", "", "new backup tar output or existing verify-backup input")
	previousPath := flags.String("previous-manifest", "", "previous manifest authorizing only proven old destination versions during sync")
	root := flags.String("destination-root", env("FILESYSTEM_ROOT"), "existing initialized private NFS root (never auto-created)")
	volume := flags.String("volume-id", env("FILESYSTEM_VOLUME_ID"), "expected canonical NFS volume UUID")
	endpoint := flags.String("endpoint", env("MINIO_ENDPOINT"), "explicit S3 endpoint without scheme/userinfo")
	bucket := flags.String("bucket", env("MINIO_BUCKET"), "explicit existing S3 bucket")
	frozen := flags.Bool("writers-frozen", false, "operator acknowledgement of actual source write fence; required for sync, not an automatic fence")
	timeout := flags.Duration("timeout", 24*time.Hour, "overall Go context deadline; cannot interrupt blocked hard-NFS kernel I/O")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid command options")
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return errors.New("unexpected arguments or invalid timeout")
	}
	switch *mode {
	case "inventory", "backup", "verify-backup", "restore-backup", "copy", "sync", "verify", "verify-source":
	default:
		return errors.New("explicit valid --mode required")
	}
	if *mode == "verify-backup" || *mode == "restore-backup" {
		if *archivePath == "" {
			return errors.New("--archive required")
		}
	} else if *manifestPath == "" {
		return errors.New("--manifest required")
	}
	if *mode == "backup" && *archivePath == "" {
		return errors.New("--archive required")
	}
	if *mode == "sync" && (!*frozen || *previousPath == "") {
		return errors.New("sync requires --previous-manifest and --writers-frozen")
	}
	if *mode != "sync" && *previousPath != "" {
		return errors.New("--previous-manifest is only valid for sync")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	var manifest migration.Manifest
	var err error
	var archiveInput *os.File
	if *mode == "verify-backup" || *mode == "restore-backup" {
		file, err := migration.OpenPrivateFile(*archivePath)
		if err != nil {
			return err
		}
		defer file.Close()
		archiveInput = file
		if *mode == "verify-backup" {
			manifest, err = migration.VerifyBackup(ctx, file)
			if err != nil {
				return err
			}
		}
	} else if *mode != "inventory" {
		manifest, err = loadManifest(*manifestPath)
		if err != nil {
			return err
		}
	}
	var source migration.Source
	if *mode == "inventory" || *mode == "backup" || *mode == "copy" || *mode == "sync" || *mode == "verify-source" {
		access, secret := env("MINIO_ACCESS_KEY"), env("MINIO_SECRET_KEY")
		if *endpoint == "" || *bucket == "" || access == "" || secret == "" {
			return errors.New("explicit S3 endpoint/bucket and credential environment required")
		}
		secure := false
		if value := env("MINIO_USE_SSL"); value != "" {
			secure, err = strconv.ParseBool(value)
			if err != nil {
				return errors.New("invalid MINIO_USE_SSL")
			}
		}
		source, err = factory.source(ctx, *endpoint, *bucket, access, secret, secure)
		if err != nil {
			return errors.New("read-only S3 connection unavailable")
		}
	}
	result := migration.Result{}
	switch *mode {
	case "inventory":
		manifest, err = migration.Inventory(ctx, source)
		if err == nil {
			err = migration.WritePrivateFile(*manifestPath, func(w io.Writer) error { return migration.WriteManifest(w, manifest) })
		}
	case "backup":
		err = migration.WritePrivateFile(*archivePath, func(w io.Writer) error { return migration.Backup(ctx, source, manifest, w) })
	case "verify-source":
		err = migration.CheckSource(ctx, source, manifest)
	case "copy", "sync", "verify", "restore-backup":
		if *root == "" || *volume == "" {
			return errors.New("explicit initialized destination root and volume UUID required")
		}
		var previous *migration.Manifest
		if *mode == "sync" {
			old, readErr := loadManifest(*previousPath)
			if readErr != nil {
				return readErr
			}
			previous = &old
		}
		destination, openErr := factory.destination(*root, *volume)
		if openErr != nil {
			return errors.New("identified NFS destination unavailable or writer lock held")
		}
		defer destination.Close()
		if *mode == "verify" {
			result, err = migration.Verify(ctx, destination, manifest)
		} else if *mode == "restore-backup" {
			manifest, result, err = migration.RestoreBackup(ctx, archiveInput, destination)
		} else {
			result, err = migration.Copy(ctx, source, destination, manifest, previous, *frozen)
		}
	}
	if err != nil {
		return err
	}
	var total int64
	for _, entry := range manifest.Entries {
		total += entry.Size
	}
	return json.NewEncoder(output).Encode(struct {
		Mode    string           `json:"mode"`
		Objects int              `json:"objects"`
		Bytes   int64            `json:"bytes"`
		Result  migration.Result `json:"result"`
	}{Mode: *mode, Objects: len(manifest.Entries), Bytes: total, Result: result})
}
