//go:build linux

package server

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestUploadCreationFaultSameIDRetryWithoutRestart(t *testing.T) {
	for _, stage := range []string{"before_manifest", "after_publish_before_sync"} {
		t.Run(stage, func(t *testing.T) {
			manager := testUploadManager(t, 1024)
			size := int64(3)
			input := createUploadRequest{ClientID: uuid.NewString(), Name: "file", Space: "personal", Size: &size, ContentType: "text/plain"}
			failed := false
			fault := func() error {
				if !failed {
					failed = true
					return errors.New("injected transient creation IO failure")
				}
				return nil
			}
			if stage == "before_manifest" {
				manager.disk.beforeCreateManifest = fault
			} else {
				manager.disk.createSync = func() error {
					if err := fault(); err != nil {
						return err
					}
					return manager.disk.root.Sync()
				}
			}
			_, err := manager.create(context.Background(), "42", "users/42/file", input)
			uploadErrorCode(t, err, 503)
			if stage == "before_manifest" {
				if _, err := manager.disk.session(input.ClientID); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed pre-publication creation left a poisoned final ID")
				}
			}
			status, err := manager.create(context.Background(), "42", "users/42/file", input)
			if err != nil || status.ID != input.ClientID || len(manager.list("42")) != 1 {
				t.Fatalf("same-ID creation did not recover without restart: %v", err)
			}
			if _, err := manager.disk.load(status.ID); err != nil {
				t.Fatal(err)
			}
			directory, err := uploadOpenDir(manager.disk.root, ".")
			if err != nil {
				t.Fatal(err)
			}
			names, err := directory.Readdirnames(-1)
			directory.Close()
			if err != nil || len(names) != 3 {
				t.Fatal("failed creation/reconciliation leaked temporary directories")
			}
		})
	}
}
