//go:build linux

package storage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	fsMarker       = ".drive-volume-id"
	fsLock         = ".drive-lock"
	fsObjects      = "objects"
	fsPayload      = ".drive-object"
	fsTemporary    = ".drive-tmp-"
	fsNFSTemporary = ".nfs"
	fsHeaderSize   = 8192
	fsMagic        = "DRIVEFS1"
)

// Filesystem stores one atomic envelope per object. Physical key nodes are
// directories, so both keys "foo" and "foo/bar" remain representable. Each
// envelope contains MIME/size/checksum together with the original payload.
// This Linux backend deliberately requires an existing identified NFS volume.
// It never creates its configured root or falls back to local storage.
type Filesystem struct {
	lifetime   sync.RWMutex
	mutations  sync.Mutex
	root       string
	volume     string
	fd         int
	lockFD     int
	mountID    uint64
	inode      uint64
	requireNFS bool
	closed     bool
}

type envelope struct {
	Volume      string    `json:"volume"`
	Key         string    `json:"key"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	Modified    time.Time `json:"modified"`
	SHA256      string    `json:"sha256"`
}

// InitializeFilesystem is an explicit operator action, not server startup. Only
// an empty, already-mounted NFS directory may be initialized. Partial failures
// are not cleaned by deleting an unknown directory tree.
func InitializeFilesystem(root, volume string) error {
	return initializeFilesystem(root, volume, true)
}

func initializeFilesystem(root, volume string, requireNFS bool) error {
	fd, _, err := openVolume(root, volume, requireNFS)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	entries, err := readEntries(fd)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("filesystem initialization requires an empty directory")
	}
	if err := unix.Mkdirat(fd, fsObjects, 0700); err != nil {
		return err
	}
	lock, err := unix.Openat(fd, fsLock, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if err := unix.Close(lock); err != nil {
		return err
	}
	marker, err := unix.Openat(fd, fsTemporary+"volume", unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(marker), "volume marker")
	_, err = io.WriteString(file, volume+"\n")
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := unix.Renameat(fd, fsTemporary+"volume", fd, fsMarker); err != nil {
		return err
	}
	return unix.Fsync(fd)
}

func NewFilesystem(root, volume string) (*Filesystem, error) {
	return newFilesystem(root, volume, true)
}

// The local-filesystem variant is deliberately unexported and used only by
// unit fixtures. Runtime configuration cannot disable the NFS requirement.
func newFilesystem(root, volume string, requireNFS bool) (*Filesystem, error) {
	fd, st, err := openVolume(root, volume, requireNFS)
	if err != nil {
		return nil, err
	}
	s := &Filesystem{root: filepath.Clean(root), volume: volume, fd: fd, lockFD: -1,
		mountID: st.Mnt_id, inode: st.Ino, requireNFS: requireNFS}
	if err := s.guard(); err != nil {
		unix.Close(fd)
		return nil, err
	}
	objects, err := unix.Openat(fd, fsObjects, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		unix.Close(fd)
		return nil, ErrVolumeUnavailable
	}
	unix.Close(objects)
	lock, err := unix.Openat(fd, fsLock, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		unix.Close(fd)
		return nil, ErrVolumeUnavailable
	}
	var lockInfo unix.Stat_t
	if err := unix.Fstat(lock, &lockInfo); err != nil || lockInfo.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(lock)
		unix.Close(fd)
		return nil, ErrVolumeUnavailable
	}
	if err := unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(lock)
		unix.Close(fd)
		return nil, writerLockError(err)
	}
	s.lockFD = lock
	if err := s.Ready(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func writerLockError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrWriterLocked
	}
	return errors.Join(ErrVolumeUnavailable, err)
}

func openVolume(root, volume string, requireNFS bool) (int, unix.Statx_t, error) {
	var st unix.Statx_t
	id, err := uuid.Parse(volume)
	if err != nil || id.String() != volume || id == uuid.Nil || !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return -1, st, errors.New("absolute dedicated root and canonical volume UUID required")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, st, err
	}
	for _, component := range strings.Split(strings.Trim(filepath.Clean(root), "/"), "/") {
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if openErr != nil {
			return -1, st, ErrVolumeUnavailable
		}
		fd = next
	}
	if err := unix.Statx(unix.AT_FDCWD, filepath.Clean(root), unix.AT_SYMLINK_NOFOLLOW, unix.STATX_INO|unix.STATX_MNT_ID|unix.STATX_TYPE, &st); err != nil {
		unix.Close(fd)
		return -1, st, ErrVolumeUnavailable
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil || (requireNFS && fs.Type != unix.NFS_SUPER_MAGIC) {
		unix.Close(fd)
		return -1, st, ErrVolumeUnavailable
	}
	var actual unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_INO|unix.STATX_MNT_ID, &actual); err != nil || actual.Ino != st.Ino || actual.Mnt_id != st.Mnt_id || st.Mask&unix.STATX_MNT_ID == 0 || actual.Mask&unix.STATX_MNT_ID == 0 {
		unix.Close(fd)
		return -1, st, ErrVolumeUnavailable
	}
	if requireNFS && !remoteLocks(st.Mnt_id) {
		unix.Close(fd)
		return -1, st, ErrVolumeUnavailable
	}
	return fd, st, nil
}

// NFS nolock/local_lock mounts cannot provide the required writer exclusion.
// Unknown mount options fail closed rather than assuming a working lock service.
func remoteLocks(mountID uint64) bool {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer file.Close()
	return remoteLockMount(file, mountID)
}

func remoteLockMount(info io.Reader, mountID uint64) bool {
	scanner := bufio.NewScanner(info)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 || fields[0] != strconv.FormatUint(mountID, 10) {
			continue
		}
		for i, field := range fields {
			if field != "-" || i+3 >= len(fields) {
				continue
			}
			if fields[i+1] != "nfs" && fields[i+1] != "nfs4" {
				return false
			}
			remote := false
			for _, option := range strings.Split(fields[i+3], ",") {
				if option == "nolock" || strings.HasPrefix(option, "local_lock=") && option != "local_lock=none" {
					return false
				}
				if option == "local_lock=none" {
					remote = true
				}
			}
			return remote
		}
	}
	return false
}

// guard checks the current mount namespace as well as the pinned descriptor.
// All writes subsequently use directory fds, never a re-resolved absolute path.
func (s *Filesystem) guard() error {
	if s.closed {
		return ErrVolumeUnavailable
	}
	var current unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, s.root, unix.AT_SYMLINK_NOFOLLOW,
		unix.STATX_INO|unix.STATX_MNT_ID|unix.STATX_TYPE, &current); err != nil ||
		current.Mnt_id != s.mountID || current.Ino != s.inode || current.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrVolumeUnavailable
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(s.fd, &fs); err != nil || (s.requireNFS && fs.Type != unix.NFS_SUPER_MAGIC) {
		return ErrVolumeUnavailable
	}
	if s.requireNFS && !remoteLocks(s.mountID) {
		return ErrVolumeUnavailable
	}
	if s.lockFD >= 0 {
		var held, current unix.Stat_t
		if unix.Fstat(s.lockFD, &held) != nil || unix.Fstatat(s.fd, fsLock, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || held.Ino != current.Ino || held.Dev != current.Dev || current.Mode&unix.S_IFMT != unix.S_IFREG {
			return ErrVolumeUnavailable
		}
	}
	fd, err := unix.Openat(s.fd, fsMarker, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return ErrVolumeUnavailable
	}
	file := os.NewFile(uintptr(fd), "volume marker")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128 {
		return ErrVolumeUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil || string(data) != s.volume+"\n" {
		return ErrVolumeUnavailable
	}
	return nil
}

func (s *Filesystem) Ready(ctx context.Context) error {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.guard(); err != nil {
		return err
	}
	fd, err := s.node("", false)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func (s *Filesystem) Close() error {
	s.lifetime.Lock()
	defer s.lifetime.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	lockErr := unix.Close(s.lockFD)
	rootErr := unix.Close(s.fd)
	return errors.Join(lockErr, rootErr)
}

func cleanFSKey(key string) (string, error) {
	key = CleanObjectPath(key)
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.IndexByte(key, 0) >= 0 {
		return "", errors.New("invalid object key")
	}
	for _, component := range strings.Split(key, "/") {
		if len(physicalName(component)) > 255 {
			return "", errors.New("object component exceeds filesystem limit")
		}
	}
	return key, nil
}

func physicalName(name string) string {
	if strings.HasPrefix(name, "~") || name == fsPayload || strings.HasPrefix(name, fsTemporary) || strings.HasPrefix(name, fsNFSTemporary) {
		return "~" + name
	}
	return name
}

func logicalName(name string) (string, error) {
	logical := name
	if strings.HasPrefix(logical, "~") {
		logical = strings.TrimPrefix(logical, "~")
	}
	if logical == "" || logical == "." || logical == ".." || strings.ContainsAny(logical, "/\\\\\x00") || !utf8.ValidString(logical) || physicalName(logical) != name {
		return "", errors.New("invalid physical object name")
	}
	return logical, nil
}

func (s *Filesystem) node(key string, create bool) (int, error) {
	fd, err := unix.Openat(s.fd, fsObjects, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, ErrVolumeUnavailable
	}
	if key == "" {
		return fd, nil
	}
	for _, name := range strings.Split(key, "/") {
		component := physicalName(name)
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if err := unix.Mkdirat(fd, component, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return -1, err
			}
			if err := unix.Fsync(fd); err != nil {
				unix.Close(fd)
				return -1, err
			}
			next, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		unix.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, unix.ENOENT) {
				return -1, ErrNotFound
			}
			return -1, openErr
		}
		fd = next
	}
	return fd, nil
}

func readEntries(fd int) ([]os.DirEntry, error) {
	copyFD, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(copyFD), "object directory")
	defer file.Close()
	return file.ReadDir(-1)
}

func (s *Filesystem) readEnvelope(fd int, key string) (*os.File, envelope, error) {
	var meta envelope
	payload, err := unix.Openat(fd, fsPayload, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, meta, ErrNotFound
	}
	if err != nil {
		return nil, meta, err
	}
	file := os.NewFile(uintptr(payload), "object payload")
	fail := func(err error) (*os.File, envelope, error) { file.Close(); return nil, envelope{}, err }
	stat, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !stat.Mode().IsRegular() {
		return fail(errors.New("non-regular object payload"))
	}
	block := make([]byte, fsHeaderSize)
	if _, err := io.ReadFull(file, block); err != nil {
		return fail(errors.New("incomplete filesystem object"))
	}
	n := int(binary.BigEndian.Uint32(block[len(fsMagic) : len(fsMagic)+4]))
	if string(block[:len(fsMagic)]) != fsMagic || n <= 0 || n > fsHeaderSize-len(fsMagic)-4 {
		return fail(errors.New("invalid filesystem object header"))
	}
	if err := json.Unmarshal(block[len(fsMagic)+4:len(fsMagic)+4+n], &meta); err != nil {
		return fail(errors.New("invalid filesystem object metadata"))
	}
	if len(meta.ContentType) == 0 || len(meta.ContentType) > 1024 {
		return fail(errors.New("invalid object content type"))
	}
	if _, _, err := mime.ParseMediaType(meta.ContentType); err != nil {
		return fail(errors.New("invalid object content type"))
	}
	checksum, err := hex.DecodeString(meta.SHA256)
	if err != nil || len(checksum) != sha256.Size || meta.Volume != s.volume || meta.Key != key || meta.Size < 0 || meta.Size != stat.Size()-fsHeaderSize || meta.Modified.IsZero() || strings.ContainsAny(meta.ContentType, "\r\n") {
		return fail(errors.New("filesystem object metadata mismatch"))
	}
	return file, meta, nil
}

func (s *Filesystem) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	s.mutations.Lock()
	defer s.mutations.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.guard(); err != nil {
		return err
	}
	return s.put(ctx, key, reader, size, contentType)
}

func (s *Filesystem) put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	return s.putImported(ctx, key, reader, size, contentType, time.Now().UTC(), "")
}

func (s *Filesystem) putImported(ctx context.Context, key string, reader io.Reader, size int64, contentType string, modified time.Time, expectedSHA256 string) error {
	key, err := cleanFSKey(key)
	if err != nil {
		return err
	}
	if size < -1 {
		return errors.New("invalid upload size")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(key))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(contentType) > 1024 || strings.ContainsAny(contentType, "\r\n") {
		return errors.New("invalid content type")
	}
	if _, _, err := mime.ParseMediaType(contentType); err != nil {
		return errors.New("invalid content type")
	}
	fd, err := s.node(key, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	temporary := fsTemporary + uuid.NewString()
	tmpFD, err := unix.Openat(fd, temporary, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(tmpFD), "temporary object")
	defer file.Close()
	defer unix.Unlinkat(fd, temporary, 0)
	if _, err := file.Write(make([]byte, fsHeaderSize)); err != nil {
		return err
	}
	hash := sha256.New()
	input := io.Reader(contextReader{ctx: ctx, reader: reader})
	if size >= 0 && size < int64(^uint64(0)>>1) {
		input = io.LimitReader(input, size+1)
	}
	written, err := io.Copy(io.MultiWriter(file, hash), input)
	if err != nil {
		return err
	}
	if size >= 0 && written != size {
		return errors.New("upload size mismatch")
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	if expectedSHA256 != "" && checksum != expectedSHA256 {
		return ErrIntegrity
	}
	meta := envelope{Volume: s.volume, Key: key, ContentType: contentType, Size: written, Modified: modified.UTC(), SHA256: checksum}
	encoded, err := json.Marshal(meta)
	if err != nil || len(encoded) > fsHeaderSize-len(fsMagic)-4 {
		return errors.New("object metadata exceeds format limit")
	}
	block := make([]byte, fsHeaderSize)
	copy(block, fsMagic)
	binary.BigEndian.PutUint32(block[len(fsMagic):], uint32(len(encoded)))
	copy(block[len(fsMagic)+4:], encoded)
	if _, err := file.WriteAt(block, 0); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.guard(); err != nil {
		return err
	}
	// Reject foreign leaf types. rename replaces the link itself, not a target,
	// but silently destroying a foreign symlink/directory is not acceptable.
	var previous unix.Stat_t
	if err := unix.Fstatat(fd, fsPayload, &previous, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if previous.Mode&unix.S_IFMT != unix.S_IFREG {
			return errors.New("invalid object payload type")
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := unix.Renameat(fd, temporary, fd, fsPayload); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	return s.guard()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type sectionCloser struct {
	section *io.SectionReader
	file    *os.File
	meta    envelope
	hash    hash.Hash
	read    int64
}

func (r *sectionCloser) Read(p []byte) (int, error) {
	n, err := r.section.Read(p)
	if r.hash != nil {
		_, _ = r.hash.Write(p[:n])
		r.read += int64(n)
		if r.read == r.meta.Size && hex.EncodeToString(r.hash.Sum(nil)) != r.meta.SHA256 {
			// Withhold the final chunk on corruption: callers cannot receive a
			// complete Content-Length and mistake damaged bytes for success.
			return 0, ErrIntegrity
		}
	}
	return n, err
}

func (r *sectionCloser) Close() error { return r.file.Close() }

func objectReader(file *os.File, meta envelope, start, length int64) *sectionCloser {
	r := &sectionCloser{section: io.NewSectionReader(file, fsHeaderSize+start, length), file: file, meta: meta}
	if start == 0 && length == meta.Size {
		r.hash = sha256.New()
	}
	return r
}

func (s *Filesystem) GetObject(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	return s.GetObjectRange(ctx, key, nil)
}

func (s *Filesystem) GetObjectRange(ctx context.Context, key string, requested *ByteRange) (io.ReadCloser, ObjectInfo, error) {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}
	if err := s.guard(); err != nil {
		return nil, ObjectInfo{}, err
	}
	key, err := cleanFSKey(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	fd, err := s.node(key, false)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	defer unix.Close(fd)
	file, meta, err := s.readEnvelope(fd, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	start, length := int64(0), meta.Size
	if requested != nil {
		if requested.SuffixLength > 0 {
			length = requested.SuffixLength
			if length > meta.Size {
				length = meta.Size
			}
			start = meta.Size - length
		} else {
			start = requested.Start
			end := requested.End
			if requested.OpenEnded || end >= meta.Size {
				end = meta.Size - 1
			}
			length = end - start + 1
		}
		if meta.Size == 0 || start < 0 || start >= meta.Size || length <= 0 {
			file.Close()
			return nil, ObjectInfo{}, ErrInvalidRange
		}
	}
	return objectReader(file, meta, start, length), ObjectInfo{Size: meta.Size, ContentType: meta.ContentType}, nil
}

func (s *Filesystem) StatObject(ctx context.Context, key string) (Object, error) {
	reader, info, err := s.GetObject(ctx, key)
	if err != nil {
		return Object{}, err
	}
	defer reader.Close()
	// Use the already verified immutable metadata, not another header read.
	meta := reader.(*sectionCloser).meta
	key = CleanObjectPath(key)
	return Object{Name: path.Base(key), Path: key, Size: info.Size, LastModified: meta.Modified}, nil
}

func (s *Filesystem) PutDirectory(ctx context.Context, key string) error {
	key = CleanObjectPath(key)
	if key == "" {
		return s.Ready(ctx)
	}
	return s.PutObject(ctx, DirPrefix(key)+directoryMarker, strings.NewReader(""), 0, "application/x-directory")
}

func (s *Filesystem) List(ctx context.Context, prefix string) ([]Object, error) {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.guard(); err != nil {
		return nil, err
	}
	prefix = CleanObjectPath(prefix)
	fd, err := s.node(prefix, false)
	if errors.Is(err, ErrNotFound) {
		return []Object{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	entries, err := readEntries(fd)
	if err != nil {
		return nil, err
	}
	objects := make([]Object, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Name() == fsPayload || transientEntry(entry) {
			continue
		}
		if !entry.IsDir() {
			return nil, errors.New("unexpected filesystem node")
		}
		name, err := logicalName(entry.Name())
		if err != nil {
			return nil, err
		}
		key := path.Join(prefix, name)
		child, err := s.node(key, false)
		if err != nil {
			return nil, err
		}
		file, meta, objectErr := s.readEnvelope(child, key)
		if objectErr == nil {
			file.Close()
			if name != directoryMarker {
				objects = append(objects, Object{Name: name, Path: key, Size: meta.Size, LastModified: meta.Modified})
			}
		} else if !errors.Is(objectErr, ErrNotFound) {
			unix.Close(child)
			return nil, objectErr
		}
		unix.Close(child)
		hasChildren, err := s.hasDescendant(ctx, key)
		if err != nil {
			return nil, err
		}
		if hasChildren {
			objects = append(objects, Object{Name: name, Path: key, IsDir: true})
		}
	}
	return objects, nil
}

func (s *Filesystem) DeleteObject(ctx context.Context, key string) error {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	s.mutations.Lock()
	defer s.mutations.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.guard(); err != nil {
		return err
	}
	key, err := cleanFSKey(key)
	if err != nil {
		return err
	}
	if err := s.deleteObject(key); err != nil {
		return err
	}
	return s.pruneEmpty(key)
}

func (s *Filesystem) deleteObject(key string) error {
	fd, err := s.node(key, false)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	file, _, err := s.readEnvelope(fd, key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	file.Close()
	if err := unix.Unlinkat(fd, fsPayload, 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}

func (s *Filesystem) keys(ctx context.Context, prefix string) ([]string, error) {
	fd, err := s.node(prefix, false)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var keys []string
	var walk func(int, string) error
	walk = func(dir int, key string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := readEntries(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if transientEntry(entry) {
				continue
			}
			if entry.Name() == fsPayload {
				file, _, err := s.readEnvelope(dir, key)
				if err != nil {
					return err
				}
				file.Close()
				keys = append(keys, key)
				continue
			}
			if !entry.IsDir() {
				return errors.New("unexpected filesystem node")
			}
			child, err := unix.Openat(dir, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			name, nameErr := logicalName(entry.Name())
			if nameErr != nil {
				unix.Close(child)
				return nameErr
			}
			err = walk(child, path.Join(key, name))
			unix.Close(child)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(fd, prefix); err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *Filesystem) DeleteDirectory(ctx context.Context, prefix string) error {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	s.mutations.Lock()
	defer s.mutations.Unlock()
	if err := s.guard(); err != nil {
		return err
	}
	prefix, err := cleanFSKey(prefix)
	if err != nil {
		return err
	}
	keys, err := s.keys(ctx, prefix)
	if err != nil {
		return err
	}
	// Match S3 prefix semantics: deleting "foo/" does not delete key "foo".
	for _, key := range keys {
		if key == prefix {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.guard(); err != nil {
			return err
		}
		if err := s.deleteObject(key); err != nil {
			return err
		}
		if err := s.pruneEmpty(key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Filesystem) copyObject(ctx context.Context, source, destination string) error {
	fd, err := s.node(source, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	file, meta, err := s.readEnvelope(fd, source)
	if err != nil {
		return err
	}
	defer file.Close()
	return s.put(ctx, destination, objectReader(file, meta, 0, meta.Size), meta.Size, meta.ContentType)
}

func (s *Filesystem) MoveObject(ctx context.Context, source, destination string) error {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	s.mutations.Lock()
	defer s.mutations.Unlock()
	if err := s.guard(); err != nil {
		return err
	}
	source, err := cleanFSKey(source)
	if err != nil {
		return err
	}
	destination, err = cleanFSKey(destination)
	if err != nil {
		return err
	}
	if source == destination {
		return nil
	}
	if err := s.copyObject(ctx, source, destination); err != nil {
		return err
	}
	if err := s.deleteObject(source); err != nil {
		return err
	}
	return s.pruneEmpty(source)
}

func (s *Filesystem) MoveDirectory(ctx context.Context, source, destination string) error {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	s.mutations.Lock()
	defer s.mutations.Unlock()
	if err := s.guard(); err != nil {
		return err
	}
	source, err := cleanFSKey(source)
	if err != nil {
		return err
	}
	destination, err = cleanFSKey(destination)
	if err != nil {
		return err
	}
	if source == destination || strings.HasPrefix(destination+"/", source+"/") || strings.HasPrefix(source+"/", destination+"/") {
		return errors.New("cannot move a directory into itself")
	}
	keys, err := s.keys(ctx, source)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key == source {
			continue
		}
		relative := strings.TrimPrefix(key, source+"/")
		if err := s.copyObject(ctx, key, destination+"/"+relative); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if key == source {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.guard(); err != nil {
			return err
		}
		if err := s.deleteObject(key); err != nil {
			return err
		}
		if err := s.pruneEmpty(key); err != nil {
			return err
		}
	}
	return nil
}
