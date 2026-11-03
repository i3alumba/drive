//go:build linux

package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testUploadDisk(t *testing.T) (*uploadDisk, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions")
	disk, err := openUploadDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(disk.close)
	return disk, root
}

func testUploadRecord() uploadRecord {
	return uploadRecord{Version: 1, Owner: "42", Key: "users/42/file", ContentType: "text/plain", Status: UploadStatus{ID: uuid.NewString(), Name: "file", Space: "personal", Size: 3, ChunkSize: uploadChunkSize, Parts: map[int]UploadPart{}, State: "uploading", ExpiresAt: time.Now().UTC().Add(time.Hour)}}
}

func TestUploadDiskDurableManifestAndExclusiveLock(t *testing.T) {
	disk, root := testUploadDisk(t)
	record := testUploadRecord()
	if err := disk.create(record); err != nil {
		t.Fatal(err)
	}
	if other, err := openUploadDisk(root); err == nil {
		other.close()
		t.Fatal("another writer acquired the staging root")
	}
	loaded, err := disk.load(record.Status.ID)
	if err != nil || loaded.Owner != record.Owner || loaded.Status.ID != record.Status.ID {
		t.Fatalf("manifest recovery failed: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, record.Status.ID, "manifest.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("manifest is not private")
	}
	ids, err := disk.ids()
	if err != nil || len(ids) != 1 || ids[0] != record.Status.ID {
		t.Fatalf("session inventory failed: %v", err)
	}
	disk.close()
	reopened, err := openUploadDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if _, err := reopened.load(record.Status.ID); err != nil {
		t.Fatal(err)
	}
}

func TestUploadDiskRejectsSymlinksAndTraversal(t *testing.T) {
	disk, root := testUploadDisk(t)
	record := testUploadRecord()
	if err := disk.create(record); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, record.Status.ID, uploadPartName(0))); err != nil {
		t.Fatal(err)
	}
	if file, err := disk.part(record.Status.ID, 0); err == nil {
		file.Close()
		t.Fatal("part symlink was followed")
	}
	if dir, err := disk.session("../outside"); err == nil {
		dir.Close()
		t.Fatal("traversal accepted")
	}
	if err := disk.removeFiles(record.Status.ID, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "original" {
		t.Fatal("cleanup touched a symlink target")
	}
}

func TestUploadDiskNeverAdoptsUnmarkedDirectory(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "unrelated")
	if err := os.WriteFile(original, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if disk, err := openUploadDisk(root); err == nil {
		disk.close()
		t.Fatal("unmarked directory adopted")
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatal("unrelated data changed")
	}
	if _, err := os.Stat(filepath.Join(root, ".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unmarked directory was modified before validation")
	}
}

func TestUploadDiskCleanupStopsAtUnknownEntry(t *testing.T) {
	disk, root := testUploadDisk(t)
	record := testUploadRecord()
	if err := disk.create(record); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, record.Status.ID, "not-a-managed-file")
	if err := os.WriteFile(unknown, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := disk.removeFiles(record.Status.ID, true); err == nil {
		t.Fatal("unknown entry did not stop cleanup")
	}
	if _, err := os.Stat(unknown); errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown entry deleted")
	}
}
