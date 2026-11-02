//go:build linux

package migration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"remote-drive/api/internal/storage"

	"github.com/google/uuid"
)

func TestNASMigrationAcceptance(t *testing.T) {
	parent, token := os.Getenv("DRIVE_NFS_ACCEPTANCE_ROOT"), os.Getenv("DRIVE_NFS_ACCEPTANCE_TOKEN")
	if parent == "" && token == "" {
		t.Skip("explicit owned NFS scratch fixture required")
	}
	id, err := uuid.Parse(token)
	if err != nil || id == uuid.Nil || id.String() != token || filepath.Base(parent) != ".drive-nfs-acceptance-"+token {
		t.Fatal("invalid fixture identity")
	}
	marker, err := os.ReadFile(filepath.Join(parent, ".acceptance-token"))
	if err != nil || string(marker) != token+"\n" {
		t.Fatal("fixture ownership marker mismatch")
	}
	newTarget := func() *storage.Filesystem {
		root, err := os.MkdirTemp(parent, "migration-volume-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("owned cleanup: %v", err)
			}
		})
		volume := uuid.NewString()
		if err := storage.InitializeFilesystem(root, volume); err != nil {
			t.Fatal(err)
		}
		target, err := storage.NewFilesystem(root, volume)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = target.Close() })
		return target
	}
	ctx := context.Background()
	source, destination := fixtureSource(), newTarget()
	source.objects["foo"] = object("foo", "same-name file")
	source.objects["foo/bar"] = object("foo/bar", "same-name child")
	initial := inventory(t, source)
	first, err := Copy(ctx, source, destination, initial, nil, false)
	if err != nil || first.Copied != 4 || first.Verified != 4 {
		t.Fatalf("NFS copy: %+v %v", first, err)
	}
	resumed, err := Copy(ctx, source, destination, initial, nil, false)
	if err != nil || resumed.Unchanged != 4 || resumed.Copied != 0 {
		t.Fatalf("NFS resume: %+v %v", resumed, err)
	}
	var archive bytes.Buffer
	if err := Backup(ctx, source, initial, &archive); err != nil {
		t.Fatal(err)
	}
	restored := newTarget()
	_, result, err := RestoreBackup(ctx, bytes.NewReader(archive.Bytes()), restored)
	if err != nil || result.Verified != 4 {
		t.Fatalf("NFS restore metadata/hash: %+v %v", result, err)
	}
	delete(source.objects, "_drive/shares.json")
	source.objects["users/42/a"] = object("users/42/a", "final bytes")
	source.objects["users/43/new"] = object("users/43/new", "new prefix")
	final := inventory(t, source)
	result, err = Copy(ctx, source, destination, final, &initial, true)
	if err != nil || result.Copied != 2 || result.Removed != 1 || result.Verified != 4 {
		t.Fatalf("NFS final sync: %+v %v", result, err)
	}
	t.Log("NFS inventory/copy/resume, exact SHA256/MIME/subsecond mtime, backup/restore, file-prefix collisions, shares and fenced final sync passed (synthetic source)")
}
