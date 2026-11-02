//go:build linux

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// Opt-in only: the operator must provide a newly created, token-labelled
// scratch parent on NFS. This test never initializes an arbitrary existing root.
func TestNASFilesystemAcceptance(t *testing.T) {
	parent, token := os.Getenv("DRIVE_NFS_ACCEPTANCE_ROOT"), os.Getenv("DRIVE_NFS_ACCEPTANCE_TOKEN")
	if parent == "" && token == "" {
		t.Skip("explicit owned NFS scratch fixture required")
	}
	parsed, err := uuid.Parse(token)
	if err != nil || parsed.String() != token || parsed == uuid.Nil || filepath.Base(parent) != ".drive-nfs-acceptance-"+token {
		t.Fatal("invalid scratch fixture identity")
	}
	marker, err := os.ReadFile(filepath.Join(parent, ".acceptance-token"))
	if err != nil || string(marker) != token+"\n" {
		t.Fatal("scratch fixture ownership marker mismatch")
	}
	parentFD, _, err := openVolume(parent, token, true)
	if err != nil {
		t.Fatalf("scratch parent is not suitable NFS: %v", err)
	}
	unix.Close(parentFD)
	root, err := os.MkdirTemp(parent, "volume-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("owned fixture cleanup: %v", err)
		}
	})
	volume := uuid.NewString()
	if err := InitializeFilesystem(root, volume); err != nil {
		t.Fatalf("NFS initialization/fsync: %v", err)
	}
	store, err := NewFilesystem(root, volume)
	if err != nil {
		t.Fatalf("NFS writer lock/startup: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	probeNASWriter(t, root, volume, "blocked")
	key := "users/42/Кириллица/документ.bin"
	payload := bytes.Repeat([]byte("synthetic-NAS-fixture\n"), 16384)
	if err := store.PutObject(ctx, key, bytes.NewReader(payload), int64(len(payload)), "application/x-drive-fixture"); err != nil {
		t.Fatal(err)
	}
	reader, info, err := store.GetObject(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, payload) || info.Size != int64(len(payload)) || info.ContentType != "application/x-drive-fixture" {
		t.Fatalf("NFS readback/MIME: %v", err)
	}
	for _, tc := range []struct {
		request  *ByteRange
		expected []byte
	}{
		{&ByteRange{Start: 3, End: 18}, payload[3:19]},
		{&ByteRange{SuffixLength: 17}, payload[len(payload)-17:]},
	} {
		reader, _, err := store.GetObjectRange(ctx, key, tc.request)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || !bytes.Equal(got, tc.expected) {
			t.Fatalf("NFS range: %v", err)
		}
	}
	// A failed overwrite must leave both the previous payload and MIME intact.
	if err := store.PutObject(ctx, key, strings.NewReader("short"), 100, "text/plain"); err == nil {
		t.Fatal("wrong-size overwrite committed")
	}
	gotText, gotInfo := readFS(t, store, key, nil)
	if gotText != string(payload) || gotInfo.ContentType != info.ContentType {
		t.Fatal("failed overwrite damaged old object")
	}
	exerciseAtomicReplacement(t, store)
	putFS(t, store, "users/42/pinned", "old", "text/plain")
	oldReader, _, err := store.GetObject(ctx, "users/42/pinned")
	if err != nil {
		t.Fatal(err)
	}
	defer oldReader.Close()
	putFS(t, store, "users/42/pinned", "new", "application/json")
	if _, err := store.List(ctx, "users/42"); err != nil {
		entries, _ := os.ReadDir(filepath.Join(root, fsObjects, "users", "42", "pinned"))
		for _, entry := range entries {
			t.Logf("owned pinned fixture entry: %q type=%v", entry.Name(), entry.Type())
		}
		t.Fatalf("list during pinned old generation: %v", err)
	}
	oldData, err := io.ReadAll(oldReader)
	oldReader.Close()
	if err != nil || string(oldData) != "old" {
		t.Fatalf("pinned generation lost: %v", err)
	}
	putFS(t, store, "users/42/delete-pinned", "open-after-delete", "text/plain")
	deletedReader, _, err := store.GetObject(ctx, "users/42/delete-pinned")
	if err != nil {
		t.Fatal(err)
	}
	defer deletedReader.Close()
	if err := store.DeleteObject(ctx, "users/42/delete-pinned"); err != nil {
		t.Fatal(err)
	}
	itemsWhileOpen, err := store.List(ctx, "users/42")
	if err != nil {
		t.Fatalf("list after deletion with active reader: %v", err)
	}
	for _, item := range itemsWhileOpen {
		if item.Name == "delete-pinned" {
			t.Fatal("deleted generation exposed as an object/prefix")
		}
	}
	deletedData, err := io.ReadAll(deletedReader)
	deletedReader.Close()
	if err != nil || string(deletedData) != "open-after-delete" {
		t.Fatalf("active reader damaged by deletion: %v", err)
	}
	if err := store.PutDirectory(ctx, "users/42/empty"); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveDirectory(ctx, "users/42/Кириллица", "users/42/archive"); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveObject(ctx, "users/42/archive/документ.bin", "users/42/renamed.bin"); err != nil {
		t.Fatal(err)
	}
	gotText, gotInfo = readFS(t, store, "users/42/renamed.bin", nil)
	if gotText != string(payload) || gotInfo.ContentType != info.ContentType {
		t.Fatal("NFS moves changed object")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	probeNASWriter(t, root, volume, "available")
	reopened, err := NewFilesystem(root, volume)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gotText, _ = readFS(t, reopened, "users/42/renamed.bin", nil)
	if gotText != string(payload) {
		t.Fatal("restart lost committed payload")
	}
	if err := reopened.DeleteDirectory(ctx, "users/42"); err != nil {
		t.Fatal(err)
	}
	items, err := reopened.List(ctx, "users/42")
	if err != nil || len(items) != 0 {
		t.Fatalf("NFS delete/list: %v, count=%d", err, len(items))
	}
	t.Log("NFS initialization, directory fsync, process writer exclusion/release, SHA256 readback, MIME, Range, moves, failed overwrite, reopen and deletion passed")
}

func probeNASWriter(t *testing.T, root, volume, expectation string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNASWriterLockProbe$", "-test.v")
	command.Env = append(os.Environ(), "DRIVE_NFS_LOCK_PROBE="+expectation, "DRIVE_NFS_PROBE_ROOT="+root, "DRIVE_NFS_PROBE_VOLUME="+volume)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("separate-process lock probe (%s): %v\n%s", expectation, err, output)
	}
}

func TestNASWriterLockProbe(t *testing.T) {
	expectation := os.Getenv("DRIVE_NFS_LOCK_PROBE")
	if expectation == "" {
		t.Skip("parent acceptance test subprocess only")
	}
	if expectation != "blocked" && expectation != "available" {
		t.Fatal("invalid lock probe mode")
	}
	store, err := NewFilesystem(os.Getenv("DRIVE_NFS_PROBE_ROOT"), os.Getenv("DRIVE_NFS_PROBE_VOLUME"))
	if store != nil {
		defer store.Close()
	}
	if expectation == "blocked" {
		if !errors.Is(err, ErrWriterLocked) {
			t.Fatalf("expected lock contention, got: %v", err)
		}
	} else if err != nil {
		t.Fatalf("released writer lock not available: %v", err)
	}
}
