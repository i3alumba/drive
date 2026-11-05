package videoworker

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const markerName = ".video-worker-v1"

func openDirectory(path string) (int, error) {
	return unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
}
func openJob(root, id string) (int, error) {
	fd, err := openDirectory(root)
	if err != nil {
		return -1, err
	}
	defer unix.Close(fd)
	job, err := unix.Openat(fd, id, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		err = privateDirectory(job)
		if err != nil {
			unix.Close(job)
		}
	}
	return job, err
}
func openInput(dir int, limit int64) (*os.File, error) {
	fd, err := unix.Openat(dir, InputName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), InputName)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 || st.Size <= 0 || st.Size > limit {
		f.Close()
		return nil, errors.New("invalid input")
	}
	return f, nil
}
func privateDirectory(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0077 != 0 {
		return os.ErrPermission
	}
	return nil
}

func absent(dir int, name string) bool {
	var st unix.Stat_t
	return errors.Is(unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW), unix.ENOENT)
}

// Initialize creates only a marker and two private directories in an existing empty base.
// It never repairs ownership or adopts an existing layout.
func Initialize(root string) error {
	fd, err := openDirectory(root)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), root)
	names, err := f.Readdirnames(1)
	f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) != 0 {
		return errors.New("scratch base must be empty")
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&0777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		return errors.New("scratch base must already have mode 0700")
	}
	m, err := unix.Openat(fd, markerName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	unix.Close(m)
	for _, name := range []string{"incoming", "ready"} {
		if err = unix.Mkdirat(fd, name, 0700); err != nil {
			return err
		}
	}
	return nil
}
