//go:build linux

package migration

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// WritePrivateFile publishes only a complete fsynced artifact, never replaces
// existing paths/symlinks, and removes only its own unpublished temporary file.
func WritePrivateFile(name string, write func(io.Writer) error) error {
	if name == "" {
		return errors.New("output path required")
	}
	if _, err := os.Lstat(name); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("output already exists or is inaccessible")
	}
	dir := filepath.Dir(name)
	file, err := os.CreateTemp(dir, ".drive-migration-partial-")
	if err != nil {
		return errors.New("private output creation failed")
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if err := write(file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return errors.New("output fsync failed")
	}
	if err := file.Close(); err != nil {
		return errors.New("output close failed")
	}
	// link is atomic and cannot overwrite a target created during the copy.
	if err := os.Link(temporary, name); err != nil {
		return errors.New("output publication failed (no overwrite)")
	}
	if err := os.Remove(temporary); err != nil {
		return errors.New("output published but temporary unlink failed")
	}
	parent, err := os.Open(dir)
	if err != nil {
		return errors.New("output published but directory open failed")
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return errors.New("output published but directory fsync failed")
	}
	return nil
}

func OpenPrivateFile(name string) (*os.File, error) {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private regular input file (0600) required")
	}
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("input open failed")
	}
	file := os.NewFile(uintptr(fd), "private migration input")
	actual, err := file.Stat()
	if err != nil || !actual.Mode().IsRegular() || actual.Mode().Perm()&0077 != 0 || !os.SameFile(info, actual) {
		file.Close()
		return nil, errors.New("input changed during open")
	}
	return file, nil
}
