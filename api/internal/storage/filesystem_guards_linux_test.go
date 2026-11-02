//go:build linux

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteLockMountRejectsUnknownOrLocalOnlyLocks(t *testing.T) {
	for _, tc := range []struct {
		options  string
		accepted bool
	}{
		{"rw,vers=3,local_lock=none", true},
		{"rw,vers=3,local_lock=all", false},
		{"rw,vers=3,local_lock=flock", false},
		{"rw,vers=3,local_lock=posix", false},
		{"rw,vers=3", false},
		{"rw,vers=3,local_lock=none,nolock", false},
	} {
		mountinfo := "42 1 0:99 / /volume rw - nfs 192.0.2.2:/export " + tc.options + "\n"
		if remoteLockMount(strings.NewReader(mountinfo), 42) != tc.accepted {
			t.Fatalf("wrong acceptance for %q", tc.options)
		}
		if remoteLockMount(strings.NewReader(mountinfo), 43) {
			t.Fatal("different mount accepted")
		}
	}
}

func TestPhysicalObjectNameRoundTripsAndCannotEncodeTraversal(t *testing.T) {
	for _, name := range []string{"normal", "~normal", "~~normal", fsPayload, fsTemporary + "file", "кириллица"} {
		decoded, err := logicalName(physicalName(name))
		if err != nil || decoded != name {
			t.Fatalf("roundtrip %q: %q %v", name, decoded, err)
		}
	}
	for _, name := range []string{"~..", "~.", "~", "~ordinary", "back\\slash"} {
		if _, err := logicalName(name); err == nil {
			t.Fatalf("malformed name %q accepted", name)
		}
	}
}

func TestFilesystemRejectsChangedLockMarkerSymlinkAndRootSymlink(t *testing.T) {
	store, root := localFilesystem(t)
	if err := os.Rename(filepath.Join(root, fsLock), filepath.Join(root, "original-lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, fsLock), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(context.Background()); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatal("writer authority change ignored")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if other, err := newFilesystem(link, store.volume, false); err == nil {
		other.Close()
		t.Fatal("symlink root accepted")
	}

	other, otherRoot := localFilesystem(t)
	if err := os.Rename(filepath.Join(otherRoot, fsMarker), filepath.Join(otherRoot, "original-marker")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(otherRoot, "original-marker"), filepath.Join(otherRoot, fsMarker)); err != nil {
		t.Fatal(err)
	}
	if err := other.Ready(context.Background()); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatal("symlink volume identity accepted")
	}
}

func TestFilesystemDoesNotAdoptEnvelopeFromAnotherVolume(t *testing.T) {
	first, firstRoot := localFilesystem(t)
	second, secondRoot := localFilesystem(t)
	key := "users/42/file"
	putFS(t, first, key, "first", "text/plain")
	putFS(t, second, key, "second", "text/plain")
	data, err := os.ReadFile(filepath.Join(firstRoot, fsObjects, "users", "42", "file", fsPayload))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, fsObjects, "users", "42", "file", fsPayload), data, 0600); err != nil {
		t.Fatal(err)
	}
	if reader, _, err := second.GetObject(context.Background(), key); err == nil {
		reader.Close()
		t.Fatal("foreign volume envelope adopted")
	}
}
