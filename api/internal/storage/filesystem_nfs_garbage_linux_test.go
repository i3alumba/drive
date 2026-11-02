//go:build linux

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const fixtureSillyName = ".nfs000000000000000100000002"

func TestFilesystemIgnoresButNeverDeletesNFSRegularSillyFiles(t *testing.T) {
	store, root := localFilesystem(t)
	putFS(t, store, "users/42/pinned", "current", "text/plain")
	garbage := filepath.Join(root, fsObjects, "users", "42", "pinned", fixtureSillyName)
	if err := os.WriteFile(garbage, []byte("NFS-owned previous generation"), 0600); err != nil {
		t.Fatal(err)
	}
	items, err := store.List(context.Background(), "users/42")
	if err != nil || len(items) != 1 || items[0].Name != "pinned" || items[0].IsDir {
		t.Fatalf("NFS garbage broke listing: %+v %v", items, err)
	}
	if err := store.DeleteDirectory(context.Background(), "users/42"); err != nil {
		t.Fatal(err)
	}
	items, err = store.List(context.Background(), "users/42")
	if err != nil || len(items) != 0 {
		t.Fatalf("NFS garbage exposed an empty prefix: %+v %v", items, err)
	}
	data, err := os.ReadFile(garbage)
	if err != nil || string(data) != "NFS-owned previous generation" {
		t.Fatal("application deleted or changed kernel-owned file")
	}
}

func TestFilesystemRejectsNFSTemporaryLookalikesAndSymlinks(t *testing.T) {
	for _, name := range []string{fixtureSillyName, ".nfsnot-a-silly-file", ".nfs00000000000000010000000g", fsTemporary + "symlink"} {
		t.Run(name, func(t *testing.T) {
			store, root := localFilesystem(t)
			putFS(t, store, "users/42/pinned", "current", "text/plain")
			entry := filepath.Join(root, fsObjects, "users", "42", "pinned", name)
			if name == fixtureSillyName || name == fsTemporary+"symlink" {
				if err := os.Symlink(filepath.Join(root, fsMarker), entry); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(entry, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.List(context.Background(), "users/42"); err == nil {
				t.Fatal("unknown file/symlink was treated as kernel garbage")
			}
		})
	}
}

func TestFilesystemNFSPrefixRemainsAvailableAsLogicalObjectName(t *testing.T) {
	store, _ := localFilesystem(t)
	key := "users/42/" + fixtureSillyName
	putFS(t, store, key, "ordinary logical object", "text/plain")
	data, _ := readFS(t, store, key, nil)
	items, err := store.List(context.Background(), "users/42")
	if data != "ordinary logical object" || err != nil || len(items) != 1 || items[0].Name != fixtureSillyName {
		t.Fatal("kernel name reservation hid a user object")
	}
}
