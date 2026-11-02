//go:build linux

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"mime"
	"strings"

	"golang.org/x/sys/unix"
)

// ValidateImportMetadata refuses incompatible S3 keys before any destination
// write. In particular, canonical aliases must never silently overwrite keys.
func ValidateImportMetadata(meta ObjectMetadata) error {
	key, err := cleanFSKey(meta.Key)
	if err != nil || key != meta.Key {
		return errors.New("noncanonical or unsupported import key")
	}
	checksum, err := hex.DecodeString(meta.SHA256)
	if err != nil || len(checksum) != sha256.Size || hex.EncodeToString(checksum) != meta.SHA256 {
		return errors.New("invalid import checksum")
	}
	if meta.Size < 0 || meta.Size > math.MaxInt64-fsHeaderSize || meta.Modified.IsZero() || meta.Modified.Year() < 1 || meta.Modified.Year() > 9999 {
		return errors.New("invalid import size or timestamp")
	}
	if len(meta.ContentType) == 0 || len(meta.ContentType) > 1024 || strings.ContainsAny(meta.ContentType, "\r\n") {
		return errors.New("invalid import MIME")
	}
	if _, _, err := mime.ParseMediaType(meta.ContentType); err != nil {
		return errors.New("invalid import MIME")
	}
	return nil
}

// ImportObject preserves the source mtime/MIME and verifies SHA256 before the
// atomic publication. A wrong checksum/size leaves the old object untouched.
// Callers must authorize replacements against a previous verified manifest.
func (s *Filesystem) ImportObject(ctx context.Context, meta ObjectMetadata, reader io.Reader) error {
	if err := ValidateImportMetadata(meta); err != nil {
		return err
	}
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
	return s.putImported(ctx, meta.Key, reader, meta.Size, meta.ContentType, meta.Modified, meta.SHA256)
}

// SnapshotObjects reports stored metadata, not a new checksum proof. Consumers
// must read every payload before certifying a backup/migration. The writer lock
// prevents another process from using this volume while a migration is open.
func (s *Filesystem) SnapshotObjects(ctx context.Context) ([]ObjectMetadata, error) {
	s.lifetime.RLock()
	defer s.lifetime.RUnlock()
	s.mutations.Lock()
	defer s.mutations.Unlock()
	if err := s.guard(); err != nil {
		return nil, err
	}
	keys, err := s.keys(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]ObjectMetadata, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fd, err := s.node(key, false)
		if err != nil {
			return nil, err
		}
		file, meta, err := s.readEnvelope(fd, key)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		file.Close()
		entry := ObjectMetadata{Key: key, ContentType: meta.ContentType, Size: meta.Size, Modified: meta.Modified, SHA256: meta.SHA256}
		if err := ValidateImportMetadata(entry); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	if err := s.guard(); err != nil {
		return nil, err
	}
	return result, nil
}
