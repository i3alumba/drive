//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-drive/api/internal/migration"
	"remote-drive/api/internal/storage"
)

type fixtureSource struct{ entry migration.Entry }

func (s fixtureSource) Ref() migration.SourceRef {
	return migration.SourceRef{Endpoint: "fixture.invalid:9000", Bucket: "drive"}
}
func (s fixtureSource) List(context.Context) ([]migration.Entry, error) {
	return []migration.Entry{s.entry}, nil
}
func (s fixtureSource) Open(context.Context, migration.Entry) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("fixture")), nil
}

type fixtureTarget struct {
	meta *storage.ObjectMetadata
	body string
}

func (d *fixtureTarget) Close() error { return nil }
func (d *fixtureTarget) SnapshotObjects(context.Context) ([]storage.ObjectMetadata, error) {
	if d.meta == nil {
		return nil, nil
	}
	return []storage.ObjectMetadata{*d.meta}, nil
}
func (d *fixtureTarget) GetObject(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error) {
	return io.NopCloser(strings.NewReader(d.body)), storage.ObjectInfo{Size: d.meta.Size, ContentType: d.meta.ContentType}, nil
}
func (d *fixtureTarget) ImportObject(_ context.Context, meta storage.ObjectMetadata, reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err == nil {
		d.meta, d.body = &meta, string(data)
	}
	return err
}
func (d *fixtureTarget) DeleteObject(context.Context, string) error { d.meta = nil; return nil }

func cliFixture() (connections, func(string) string) {
	hash := sha256.Sum256([]byte("fixture"))
	source := fixtureSource{entry: migration.Entry{ObjectMetadata: storage.ObjectMetadata{Key: "users/42/file", Size: 7, ContentType: "text/plain", Modified: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), SHA256: hex.EncodeToString(hash[:])}, ETag: "fixture-etag"}}
	destination := &fixtureTarget{}
	factory := connections{
		source: func(context.Context, string, string, string, string, bool) (migration.Source, error) {
			return source, nil
		},
		destination: func(string, string) (target, error) { return destination, nil },
	}
	values := map[string]string{"MINIO_ENDPOINT": "fixture.invalid:9000", "MINIO_BUCKET": "drive", "MINIO_ACCESS_KEY": "fixture-access", "MINIO_SECRET_KEY": "never-print-fixture-secret", "FILESYSTEM_ROOT": "/fixture/private-nfs", "FILESYSTEM_VOLUME_ID": "fixture-volume"}
	return factory, func(key string) string { return values[key] }
}

func TestCLIAllModesWithoutProductionAccessOrCredentialOutput(t *testing.T) {
	factory, env := cliFixture()
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	backup := filepath.Join(t.TempDir(), "backup.tar")
	commands := [][]string{
		{"--mode=inventory", "--manifest=" + manifest},
		{"--mode=backup", "--manifest=" + manifest, "--archive=" + backup},
		{"--mode=verify-backup", "--archive=" + backup},
		{"--mode=restore-backup", "--archive=" + backup},
		{"--mode=copy", "--manifest=" + manifest},
		{"--mode=verify", "--manifest=" + manifest},
		{"--mode=verify-source", "--manifest=" + manifest},
		{"--mode=sync", "--manifest=" + manifest, "--previous-manifest=" + manifest, "--writers-frozen"},
	}
	for _, args := range commands {
		var output, diagnostics bytes.Buffer
		if err := run(context.Background(), args, env, &output, &diagnostics, factory); err != nil {
			t.Fatalf("%s: %v", args[0], err)
		}
		if strings.Contains(output.String()+diagnostics.String(), "never-print") || strings.Contains(output.String(), "users/42") {
			t.Fatal("credentials or filenames leaked to report")
		}
	}
	for _, path := range []string{manifest, backup} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("artifact not private")
		}
	}
}

func TestCLIHelpInvalidArgumentsAndFenceFailBeforeConnections(t *testing.T) {
	factory, env := cliFixture()
	factory.source = func(context.Context, string, string, string, string, bool) (migration.Source, error) {
		t.Fatal("invalid command connected to source")
		return nil, nil
	}
	for _, args := range [][]string{nil, {"--mode=invalid"}, {"--unknown"}, {"--mode=sync", "--manifest=missing", "--previous-manifest=old"}, {"--mode=inventory", "--manifest=x", "unexpected"}, {"--mode=inventory", "--manifest=x", "--timeout=0"}} {
		if err := run(context.Background(), args, env, io.Discard, io.Discard, factory); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
	if err := run(context.Background(), []string{"--help"}, env, io.Discard, io.Discard, factory); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRequiresExplicitCredentialsAndSanitizesConnectionErrors(t *testing.T) {
	factory, env := cliFixture()
	args := []string{"--mode=inventory", "--manifest=" + filepath.Join(t.TempDir(), "manifest.json")}
	if err := run(context.Background(), args, func(string) string { return "" }, io.Discard, io.Discard, factory); err == nil {
		t.Fatal("default credentials silently used")
	}
	factory.source = func(context.Context, string, string, string, string, bool) (migration.Source, error) {
		return nil, errors.New("private raw native response fixture-secret")
	}
	err := run(context.Background(), args, env, io.Discard, io.Discard, factory)
	if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
		t.Fatal("native connection body leaked")
	}
}
