//go:build linux

package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestUploadRestartReclaimsPreRenameInitialization(t *testing.T) {
	for _, state := range []string{"empty", "complete_manifest", "partial_atomic_write"} {
		t.Run(state, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sessions")
			options := UploadOptions{Root: root, MaxFileBytes: 1024, BudgetBytes: 2048}
			manager, err := newUploadManager(options)
			if err != nil {
				t.Fatal(err)
			}
			name := "tmp-" + uuid.NewString()
			if err := uploadMkdir(manager.disk.root, name); err != nil {
				t.Fatal(err)
			}
			dir, err := uploadOpenDir(manager.disk.root, name)
			if err != nil {
				t.Fatal(err)
			}
			if state == "complete_manifest" {
				data, _ := json.Marshal(testUploadRecord())
				if err := uploadAtomicWrite(dir, "manifest.json", data); err != nil {
					t.Fatal(err)
				}
			} else if state == "partial_atomic_write" {
				file, err := uploadOpenFile(dir, "tmp-"+uuid.NewString(), os.O_CREATE|os.O_EXCL|os.O_WRONLY)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte(`{"version":1,"owner":`)); err != nil {
					t.Fatal(err)
				}
				file.Close()
			}
			dir.Close()
			// Construct actual pre-rename disk state rather than invoking a create
			// function whose returned-error path would execute deferred cleanup.
			manager.close()
			manager, err = newUploadManager(options)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.close()
			if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("pre-rename initialization survived restart recovery")
			}
		})
	}
}

func TestUploadRestartFullHistoryPlusInitializationDoesNotBlockStartup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	options := UploadOptions{Root: root, MaxFileBytes: 1024, BudgetBytes: 2048}
	manager, err := newUploadManager(options)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxUploadRecords; i++ {
		record := testUploadRecord()
		record.Released = true
		record.Status.State = "cancelled"
		if err := manager.disk.create(record); err != nil {
			t.Fatal(err)
		}
	}
	name := "tmp-" + uuid.NewString()
	if err := uploadMkdir(manager.disk.root, name); err != nil {
		t.Fatal(err)
	}
	manager.close()
	manager, err = newUploadManager(options)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.close()
	if len(manager.list("42")) != maxUploadRecords {
		t.Fatal("valid receipts were lost during initialization recovery")
	}
	if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stranded initializer was counted as retained history")
	}
}

func TestUploadInitializationRecoveryPreservesUnknownAndSymlinkContents(t *testing.T) {
	for _, state := range []string{"unknown_file", "symlink_directory", "symlink_leaf"} {
		t.Run(state, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sessions")
			options := UploadOptions{Root: root, MaxFileBytes: 1024, BudgetBytes: 2048}
			manager, err := newUploadManager(options)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "untouched")
			if err := os.WriteFile(outside, []byte("preserved"), 0o600); err != nil {
				t.Fatal(err)
			}
			name := "tmp-" + uuid.NewString()
			entry := filepath.Join(root, name)
			if state == "symlink_directory" {
				if err := os.Symlink(filepath.Dir(outside), entry); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := uploadMkdir(manager.disk.root, name); err != nil {
					t.Fatal(err)
				}
				if state == "unknown_file" {
					if err := os.WriteFile(filepath.Join(entry, "unknown-data"), []byte("preserved"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(outside, filepath.Join(entry, "tmp-"+uuid.NewString())); err != nil {
					t.Fatal(err)
				}
			}
			manager.close()
			manager, err = newUploadManager(options)
			if err == nil {
				manager.close()
				t.Fatal("unrecognized initializer was silently adopted or deleted")
			}
			if _, err := os.Lstat(entry); err != nil {
				t.Fatal("unrecognized entry was removed")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "preserved" {
				t.Fatal("recovery followed or modified an external target")
			}
		})
	}
}
