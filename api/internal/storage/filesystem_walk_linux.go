//go:build linux

package storage

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux NFS keeps open, replaced/unlinked files under .nfs + 24 hex digits
// until the final reader closes. Ignore only regular files of that exact shape;
// never follow/delete them, and never classify a symlink as harmless garbage.
func transientEntry(entry os.DirEntry) bool {
	if strings.HasPrefix(entry.Name(), fsTemporary) {
		return entry.Type().IsRegular()
	}
	name := entry.Name()
	if !entry.Type().IsRegular() || !strings.HasPrefix(name, fsNFSTemporary) || len(name) != len(fsNFSTemporary)+24 {
		return false
	}
	for _, char := range name[len(fsNFSTemporary):] {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// hasDescendant ignores empty structural directories left by interrupted writes.
// Stop at the first real object instead of reading an entire populated subtree.
func (s *Filesystem) hasDescendant(ctx context.Context, key string) (bool, error) {
	fd, err := s.node(key, false)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	return s.findObject(ctx, fd, key, false)
}

func (s *Filesystem) findObject(ctx context.Context, fd int, key string, includeSelf bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if includeSelf {
		file, _, err := s.readEnvelope(fd, key)
		if err == nil {
			file.Close()
			return true, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return false, err
		}
	}
	entries, err := readEntries(fd)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() == fsPayload || transientEntry(entry) {
			continue
		}
		if !entry.IsDir() {
			return false, errors.New("unexpected filesystem node")
		}
		name, err := logicalName(entry.Name())
		if err != nil {
			return false, err
		}
		child, err := unix.Openat(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return false, err
		}
		found, walkErr := s.findObject(ctx, child, path.Join(key, name), true)
		unix.Close(child)
		if found || walkErr != nil {
			return found, walkErr
		}
	}
	return false, nil
}

// Only empty structural nodes are removed. Payloads, unknown files and retained
// temporary files are never recursively deleted as "cleanup".
func (s *Filesystem) pruneEmpty(key string) error {
	for key != "" && key != "." {
		fd, err := s.node(key, false)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		entries, err := readEntries(fd)
		unix.Close(fd)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return nil
		}
		parentKey := path.Dir(key)
		if parentKey == "." {
			parentKey = ""
		}
		parent, err := s.node(parentKey, false)
		if err != nil {
			return err
		}
		err = unix.Unlinkat(parent, physicalName(path.Base(key)), unix.AT_REMOVEDIR)
		if err == nil {
			err = unix.Fsync(parent)
		}
		unix.Close(parent)
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return nil
		}
		if err != nil {
			return err
		}
		key = parentKey
	}
	return nil
}
