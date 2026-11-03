//go:build linux

package server

import (
	"os"

	"golang.org/x/sys/unix"
)

func uploadOpenDir(parent *os.File, name string) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	var fd int
	var err error
	if parent == nil {
		fd, err = unix.Open(name, flags, 0)
	} else {
		fd, err = unix.Openat(int(parent.Fd()), name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func uploadOpenFile(parent *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		file.Close()
		return nil, os.ErrPermission
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = os.ErrInvalid
		}
		return nil, err
	}
	return file, nil
}

func uploadPrivateDir(dir *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o077 != 0 {
		return os.ErrPermission
	}
	return nil
}

func uploadMkdir(parent *os.File, name string) error {
	return unix.Mkdirat(int(parent.Fd()), name, 0o700)
}

func uploadRenameNewDir(parent *os.File, old, name string) error {
	return unix.Renameat2(int(parent.Fd()), old, int(parent.Fd()), name, unix.RENAME_NOREPLACE)
}

func uploadRename(parent *os.File, old, name string) error {
	return unix.Renameat(int(parent.Fd()), old, int(parent.Fd()), name)
}

func uploadUnlink(parent *os.File, name string, directory bool) error {
	flags := 0
	if directory {
		flags = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(int(parent.Fd()), name, flags)
}

func uploadTryLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func uploadFreeBytes(dir *os.File) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &stat); err != nil {
		return 0, err
	}
	// Saturate rather than overflow on unusually large filesystems.
	const maximum = uint64(1<<63 - 1)
	block := uint64(stat.Bsize)
	if block != 0 && stat.Bavail > maximum/block {
		return int64(maximum), nil
	}
	return int64(stat.Bavail * block), nil
}
