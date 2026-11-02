//go:build linux

package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"remote-drive/api/internal/storage"
)

type Destination interface {
	SnapshotObjects(context.Context) ([]storage.ObjectMetadata, error)
	GetObject(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error)
	ImportObject(context.Context, storage.ObjectMetadata, io.Reader) error
	DeleteObject(context.Context, string) error
}

type Result struct {
	Copied    int `json:"copied"`
	Unchanged int `json:"unchanged"`
	Removed   int `json:"removed"`
	Verified  int `json:"verified"`
}

func verifyPayload(ctx context.Context, destination Destination, expected storage.ObjectMetadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reader, info, err := destination.GetObject(ctx, expected.Key)
	if err != nil {
		return errors.New("destination object unavailable")
	}
	hash := sha256.New()
	size, readErr := io.Copy(hash, io.LimitReader(reader, expected.Size+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || size != expected.Size || info.Size != expected.Size || info.ContentType != expected.ContentType || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return errors.New("destination payload verification failed")
	}
	return nil
}

func Verify(ctx context.Context, destination Destination, m Manifest) (Result, error) {
	var result Result
	if err := Validate(m); err != nil {
		return result, err
	}
	current, err := destination.SnapshotObjects(ctx)
	if err != nil {
		return result, errors.New("destination inventory unavailable")
	}
	if len(current) != len(m.Entries) {
		return result, errors.New("destination key set differs from manifest")
	}
	index := make(map[string]storage.ObjectMetadata, len(current))
	for _, entry := range current {
		if _, duplicate := index[entry.Key]; duplicate {
			return result, errors.New("duplicate destination key")
		}
		index[entry.Key] = entry
	}
	for _, entry := range m.Entries {
		if !sameObject(index[entry.Key], entry.ObjectMetadata) {
			return result, errors.New("destination metadata differs from manifest")
		}
		if err := verifyPayload(ctx, destination, entry.ObjectMetadata); err != nil {
			return result, err
		}
		result.Verified++
	}
	return result, nil
}

// Copy preflights every existing destination payload before making any changes.
// With previous=nil it only creates missing objects or skips exact verified
// matches. A final sync may replace/remove only versions proven by previous;
// it requires an explicit operator acknowledgement of a real source writer fence.
// Partial failure retains S3 originals. No source mutation exists in this API.
func Copy(ctx context.Context, source Source, destination Destination, m Manifest, previous *Manifest, writersFrozen bool) (Result, error) {
	var result Result
	if err := Validate(m); err != nil {
		return result, err
	}
	if previous != nil {
		if !writersFrozen {
			return result, errors.New("final sync requires acknowledged writer fence")
		}
		if err := Validate(*previous); err != nil {
			return result, err
		}
		if previous.Source != m.Source || previous.Created.After(m.Created) {
			return result, errors.New("previous manifest binding mismatch")
		}
	}
	if err := checkSource(ctx, source, m); err != nil {
		return result, err
	}
	current, err := destination.SnapshotObjects(ctx)
	if err != nil {
		return result, errors.New("destination inventory unavailable")
	}
	wanted, prior := make(map[string]Entry), make(map[string]Entry)
	for _, entry := range m.Entries {
		wanted[entry.Key] = entry
	}
	if previous != nil {
		for _, entry := range previous.Entries {
			prior[entry.Key] = entry
		}
	}
	matched := make(map[string]bool)
	seen := make(map[string]bool)
	var removals []string
	for _, existing := range current {
		if seen[existing.Key] {
			return result, errors.New("duplicate destination key")
		}
		seen[existing.Key] = true
		next, present := wanted[existing.Key]
		old, knownPrevious := prior[existing.Key]
		if present && sameObject(existing, next.ObjectMetadata) {
			matched[existing.Key] = true
		} else if !knownPrevious || !sameObject(existing, old.ObjectMetadata) {
			return result, errors.New("destination contains an unowned or conflicting version")
		}
		if err := verifyPayload(ctx, destination, existing); err != nil {
			return result, err
		}
		if !present {
			removals = append(removals, existing.Key)
		}
	}
	for _, entry := range m.Entries {
		if matched[entry.Key] {
			result.Unchanged++
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		reader, err := source.Open(ctx, entry)
		if err != nil {
			return result, errors.New("source object unavailable or changed")
		}
		copyErr := destination.ImportObject(ctx, entry.ObjectMetadata, reader)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			return result, errors.New("object import failed; original source retained")
		}
		result.Copied++
	}
	// Never delete obsolete destination versions until all new data is copied
	// and the source still matches the final fenced manifest.
	if err := checkSource(ctx, source, m); err != nil {
		return result, err
	}
	for _, key := range removals {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := destination.DeleteObject(ctx, key); err != nil {
			return result, errors.New("obsolete destination removal failed")
		}
		result.Removed++
	}
	verified, err := Verify(ctx, destination, m)
	if err != nil {
		return result, err
	}
	result.Verified = verified.Verified
	return result, nil
}

// CheckSource is a read-only metadata/key-set check. It is not a writer fence
// and cannot prove a coherent global snapshot while applications still write.
func CheckSource(ctx context.Context, source Source, m Manifest) error {
	if err := Validate(m); err != nil {
		return err
	}
	return checkSource(ctx, source, m)
}
