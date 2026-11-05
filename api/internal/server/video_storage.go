package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"remote-drive/api/internal/videoworker"
)

const videoFenceName = ".active-preview"

func (m *videoPreviewManager) createFence(identity videoworker.HLSIdentity) error {
	if !identity.Valid() {
		return os.ErrInvalid
	}
	file, err := uploadOpenFile(m.root, videoFenceName, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = json.NewEncoder(file).Encode(identity); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return m.root.Sync()
}

func (m *videoPreviewManager) removeFence(identity videoworker.HLSIdentity) error {
	file, err := uploadOpenFile(m.root, videoFenceName, os.O_RDONLY)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 513))
	file.Close()
	expected, marshalErr := json.Marshal(identity)
	if err != nil || marshalErr != nil || len(data) > 512 || !bytes.Equal(data, append(expected, '\n')) {
		return errors.New("video execution fence does not match")
	}
	if err = uploadUnlink(m.root, videoFenceName, false); err != nil {
		return err
	}
	return m.root.Sync()
}

// copyVideoBytes retains F1's cooperative, bounded I/O contract. A kernel-
// blocked operation cannot be interrupted here. Callers retain admission until
// it actually returns, rather than abandoning an I/O goroutine.
func copyVideoBytes(ctx context.Context, writer io.Writer, reader io.Reader) (int64, error) {
	buffer := make([]byte, 32<<10)
	var copied int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		n, readErr := reader.Read(buffer)
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if n > 0 {
			emptyReads = 0
			written, err := writer.Write(buffer[:n])
			copied += int64(written)
			if err != nil {
				return copied, err
			}
			if written != n {
				return copied, io.ErrShortWrite
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return copied, io.ErrNoProgress
			}
		}
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if readErr == io.EOF {
			return copied, nil
		}
		if readErr != nil {
			return copied, readErr
		}
	}
}

func (s *Server) stageVideoSource(ctx context.Context, m *videoPreviewManager, job *videoPreviewJob, dir *os.File) error {
	source, info, err := s.store.GetObject(ctx, job.sourceKey)
	if err != nil {
		return err
	}
	defer source.Close()
	if info.Size != job.source.Size || info.Size > m.options.MaxInputBytes {
		return os.ErrInvalid
	}
	file, err := uploadOpenFile(dir, videoworker.InputName, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	count, err := copyVideoBytes(ctx, io.MultiWriter(file, digest), io.LimitReader(source, info.Size+1))
	if err != nil {
		return err
	}
	if count != info.Size {
		return errors.New("invalid video source read")
	}
	current, err := s.store.StatObject(ctx, job.sourceKey)
	if err != nil || current.Size != job.source.Size || !current.LastModified.Equal(job.source.LastModified) {
		return errors.New("video source changed")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	job.sourceSHA = hex.EncodeToString(digest.Sum(nil))
	m.mu.Unlock()
	return file.Close()
}

// copyVideoAsset validates the exact fresh private bytes, never the mutable
// worker pathname. Nothing is advertised until the caller commits its ledger.
func (m *videoPreviewManager) copyVideoAsset(ctx context.Context, id, name string, source io.Reader, size int64, expectedDigest string, validate func([]byte) error) (err error) {
	limit := int64(videoworker.HLSSegmentMaxBytes)
	if name == videoworker.HLSInitName {
		limit = videoworker.HLSInitMaxBytes
	}
	if !validVideoID(id) || !videoAssetName(name) || size <= 0 || size > limit || size > m.options.MaxOutputBytes || len(expectedDigest) != 64 || validate == nil {
		return os.ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = uploadMkdir(m.ready, id); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dir, err := uploadOpenDir(m.ready, id)
	if err != nil {
		return err
	}
	defer dir.Close()
	if uploadPrivateDir(dir) != nil {
		return os.ErrPermission
	}
	partial := name + ".part"
	file, err := uploadOpenFile(dir, partial, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		return err
	}
	defer file.Close()
	defer func() {
		if err != nil {
			_ = uploadUnlink(dir, partial, false)
			_ = uploadUnlink(m.ready, id, true) // Unknown/other artifacts prevent removal.
		}
	}()
	digest := sha256.New()
	count, err := copyVideoBytes(ctx, io.MultiWriter(file, digest), io.LimitReader(source, size+1))
	if err != nil {
		return err
	}
	if count != size || hex.EncodeToString(digest.Sum(nil)) != expectedDigest {
		return errors.New("private video artifact integrity mismatch")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	private := bytes.NewBuffer(make([]byte, 0, int(size)))
	if _, err = copyVideoBytes(ctx, private, io.LimitReader(file, size+1)); err != nil {
		return err
	}
	if int64(private.Len()) != size {
		return errors.New("private video artifact changed")
	}
	if err = validate(private.Bytes()); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	// The helper uses renameat2/RENAME_NOREPLACE for files as well as dirs.
	return uploadRenameNewDir(dir, partial, name)
}

func videoKnownScratchName(name string) bool {
	if name == videoworker.InputName || name == videoworker.HLSPlaylistName || name == videoworker.HLSPlaylistName+".tmp" {
		return true
	}
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".tmp"), ".part")
	return videoAssetName(base)
}

// Cleanup is exact-name, nonrecursive and fail-closed. Unknown entries survive.
func (m *videoPreviewManager) cleanVideoDirectory(parent *os.File, id string) error {
	dir, err := uploadOpenDir(parent, id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(videoworker.HLSMaxSegments*2 + 16)
	if err != nil && err != io.EOF || len(entries) >= videoworker.HLSMaxSegments*2+16 {
		return errors.New("video cleanup inventory unavailable")
	}
	for _, entry := range entries {
		if parent == m.incoming && entry.Name() == videoworker.HLSDirectoryName && entry.IsDir() {
			// Exactly one fixed output child, never arbitrary recursive cleanup.
			if err := m.cleanVideoDirectory(dir, videoworker.HLSDirectoryName); err != nil {
				return err
			}
			continue
		}
		if videoKnownScratchName(entry.Name()) {
			if err := uploadUnlink(dir, entry.Name(), false); err != nil {
				return err
			}
		}
	}
	return uploadUnlink(parent, id, true)
}

func (m *videoPreviewManager) scratchUsageLocked() (int64, error) {
	var total int64
	for _, name := range []string{"incoming", "ready"} {
		parent, err := uploadOpenDir(m.root, name)
		if err != nil {
			return 0, err
		}
		defer parent.Close()
		entries, err := parent.ReadDir(257)
		if err != nil && err != io.EOF || len(entries) > 256 {
			return 0, errors.New("video scratch inventory unavailable")
		}
		for _, entry := range entries {
			if entry.Name() == videoworker.SocketName && entry.Type()&os.ModeSocket != 0 {
				continue
			}
			if name == "incoming" || !validVideoID(entry.Name()) || !entry.IsDir() {
				return 0, os.ErrInvalid
			}
			dir, err := uploadOpenDir(parent, entry.Name())
			if err != nil {
				return 0, err
			}
			files, err := dir.ReadDir(videoworker.HLSMaxSegments + 4)
			if err != nil && err != io.EOF || len(files) > videoworker.HLSMaxSegments+3 {
				dir.Close()
				return 0, os.ErrInvalid
			}
			for _, entry := range files {
				file, err := uploadOpenFile(dir, entry.Name(), os.O_RDONLY)
				if err != nil {
					dir.Close()
					return 0, err
				}
				info, err := file.Stat()
				file.Close()
				if err != nil || info.Size() > m.options.BudgetBytes-total {
					dir.Close()
					return 0, os.ErrInvalid
				}
				total += info.Size()
			}
			dir.Close()
		}
	}
	return total, nil
}
