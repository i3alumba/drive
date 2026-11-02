//go:build linux

// Package migration implements read-only S3 inventory and manifest-bound NAS
// copying. It cannot delete S3 data, deploy services, or assert a writer fence.
package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"remote-drive/api/internal/storage"

	"github.com/google/uuid"
)

const maxManifestBytes = 512 << 20

var ErrChanged = errors.New("source changed since inventory")

type SourceRef struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Secure   bool   `json:"secure"`
}

type Entry struct {
	storage.ObjectMetadata
	ETag      string `json:"etag"`
	VersionID string `json:"versionId,omitempty"`
}

type Manifest struct {
	Schema  int       `json:"schema"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Source  SourceRef `json:"source"`
	Entries []Entry   `json:"entries"`
}

// Source intentionally has no write/delete operations. List must include HEAD
// metadata, not infer MIME or checksums from a filename/ETag.
type Source interface {
	Ref() SourceRef
	List(context.Context) ([]Entry, error)
	Open(context.Context, Entry) (io.ReadCloser, error)
}

func Validate(m Manifest) error {
	id, err := uuid.Parse(m.ID)
	if m.Schema != 1 || err != nil || id == uuid.Nil || id.String() != m.ID || m.Created.IsZero() {
		return errors.New("invalid manifest identity")
	}
	if m.Source.Endpoint == "" || strings.ContainsAny(m.Source.Endpoint, "@/\r\n") || m.Source.Bucket == "" || strings.ContainsAny(m.Source.Bucket, "/\r\n") {
		return errors.New("invalid manifest source reference")
	}
	if len(m.Entries) > 1_000_000 {
		return errors.New("manifest exceeds object limit")
	}
	seen := make(map[string]bool, len(m.Entries))
	var total int64
	for _, entry := range m.Entries {
		if err := storage.ValidateImportMetadata(entry.ObjectMetadata); err != nil {
			return errors.New("manifest contains unsupported object metadata")
		}
		if seen[entry.Key] || entry.ETag == "" || len(entry.ETag) > 1024 || len(entry.VersionID) > 1024 || strings.ContainsAny(entry.ETag+entry.VersionID, "\r\n") {
			return errors.New("duplicate key or invalid object identity")
		}
		if entry.Size > math.MaxInt64-total {
			return errors.New("manifest total size overflow")
		}
		total += entry.Size
		seen[entry.Key] = true
	}
	return nil
}

func ReadManifest(reader io.Reader) (Manifest, error) {
	var m Manifest
	limited := &io.LimitedReader{R: reader, N: maxManifestBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, errors.New("invalid manifest JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return m, errors.New("trailing manifest data")
	}
	if limited.N <= 0 {
		return m, errors.New("manifest exceeds byte limit")
	}
	if err := Validate(m); err != nil {
		return m, err
	}
	return m, nil
}

func WriteManifest(writer io.Writer, m Manifest) error {
	if err := Validate(m); err != nil {
		return err
	}
	return json.NewEncoder(writer).Encode(m)
}

func sameObject(a, b storage.ObjectMetadata) bool {
	return a.Key == b.Key && a.Size == b.Size && a.ContentType == b.ContentType && a.Modified.Equal(b.Modified) && a.SHA256 == b.SHA256
}

func sameSource(a, b Entry) bool {
	return a.Key == b.Key && a.Size == b.Size && a.ContentType == b.ContentType && a.Modified.Equal(b.Modified) && a.ETag == b.ETag && a.VersionID == b.VersionID
}

func checkSource(ctx context.Context, source Source, m Manifest) error {
	if source.Ref() != m.Source {
		return errors.New("manifest/source binding mismatch")
	}
	current, err := source.List(ctx)
	if err != nil {
		return errors.New("source inventory unavailable")
	}
	if len(current) != len(m.Entries) {
		return ErrChanged
	}
	index := make(map[string]Entry, len(current))
	for _, entry := range current {
		if _, duplicate := index[entry.Key]; duplicate {
			return ErrChanged
		}
		index[entry.Key] = entry
	}
	for _, expected := range m.Entries {
		if !sameSource(index[expected.Key], expected) {
			return ErrChanged
		}
	}
	return nil
}

func Inventory(ctx context.Context, source Source) (Manifest, error) {
	entries, err := source.List(ctx)
	if err != nil {
		return Manifest{}, errors.New("source inventory unavailable")
	}
	entries = append([]Entry(nil), entries...)
	m := Manifest{Schema: 1, ID: uuid.NewString(), Created: time.Now().UTC(), Source: source.Ref(), Entries: entries}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Key < m.Entries[j].Key })
	for i := range m.Entries {
		entry := &m.Entries[i]
		// Validate the entire key/metadata set before reading any payloads.
		entry.SHA256 = strings.Repeat("0", 64)
	}
	if err := Validate(m); err != nil {
		return Manifest{}, err
	}
	for i := range m.Entries {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		entry := &m.Entries[i]
		reader, err := source.Open(ctx, *entry)
		if err != nil {
			return Manifest{}, errors.New("source object unavailable or changed")
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, io.LimitReader(reader, entry.Size+1))
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil || size != entry.Size {
			return Manifest{}, errors.New("source payload read failed")
		}
		entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
	}
	if err := checkSource(ctx, source, m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
