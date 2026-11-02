//go:build linux

package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
)

type fakeObject struct {
	meta storage.ObjectMetadata
	body string
	etag string
}
type fakeSource struct {
	objects map[string]fakeObject
	opens   int
	fail    string
}

func object(key, body string) fakeObject {
	hash := sha256.Sum256([]byte(body))
	return fakeObject{meta: storage.ObjectMetadata{Key: key, ContentType: "application/x-fixture", Size: int64(len(body)), Modified: time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC), SHA256: hex.EncodeToString(hash[:])}, body: body, etag: hex.EncodeToString(hash[:16])}
}
func fixtureSource() *fakeSource {
	return &fakeSource{objects: map[string]fakeObject{"users/42/a": object("users/42/a", "one"), "_drive/shares.json": object("_drive/shares.json", "[]")}}
}
func (s *fakeSource) Ref() SourceRef {
	return SourceRef{Endpoint: "source.invalid:9000", Bucket: "drive", Secure: true}
}
func (s *fakeSource) List(context.Context) ([]Entry, error) {
	var entries []Entry
	for _, obj := range s.objects {
		entries = append(entries, Entry{ObjectMetadata: obj.meta, ETag: obj.etag})
	}
	return entries, nil
}
func (s *fakeSource) Open(_ context.Context, entry Entry) (io.ReadCloser, error) {
	s.opens++
	obj, ok := s.objects[entry.Key]
	if !ok || entry.Key == s.fail || !sameSource(Entry{ObjectMetadata: obj.meta, ETag: obj.etag}, entry) {
		return nil, ErrChanged
	}
	return io.NopCloser(strings.NewReader(obj.body)), nil
}

type fakeDestination struct {
	objects  map[string]fakeObject
	writes   int
	removals int
}

func newDestination() *fakeDestination { return &fakeDestination{objects: make(map[string]fakeObject)} }
func (d *fakeDestination) SnapshotObjects(context.Context) ([]storage.ObjectMetadata, error) {
	var items []storage.ObjectMetadata
	for _, obj := range d.objects {
		items = append(items, obj.meta)
	}
	return items, nil
}
func (d *fakeDestination) GetObject(_ context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	obj, ok := d.objects[key]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(obj.body)), storage.ObjectInfo{Size: obj.meta.Size, ContentType: obj.meta.ContentType}, nil
}
func (d *fakeDestination) ImportObject(_ context.Context, meta storage.ObjectMetadata, reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	if meta.Size != int64(len(data)) || meta.SHA256 != hex.EncodeToString(hash[:]) {
		return storage.ErrIntegrity
	}
	d.objects[meta.Key] = fakeObject{meta: meta, body: string(data)}
	d.writes++
	return nil
}
func (d *fakeDestination) DeleteObject(_ context.Context, key string) error {
	delete(d.objects, key)
	d.removals++
	return nil
}

func inventory(t *testing.T, source Source) Manifest {
	t.Helper()
	m, err := Inventory(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInventoryManifestRoundtripAndCopyResume(t *testing.T) {
	source, destination := fixtureSource(), newDestination()
	m := inventory(t, source)
	var encoded bytes.Buffer
	if err := WriteManifest(&encoded, m); err != nil {
		t.Fatal(err)
	}
	parsed, err := ReadManifest(&encoded)
	if err != nil || parsed.ID != m.ID {
		t.Fatalf("manifest roundtrip: %v", err)
	}
	result, err := Copy(context.Background(), source, destination, parsed, nil, false)
	if err != nil || result.Copied != 2 || result.Verified != 2 {
		t.Fatalf("copy: %+v %v", result, err)
	}
	result, err = Copy(context.Background(), source, destination, parsed, nil, false)
	if err != nil || result.Copied != 0 || result.Unchanged != 2 || destination.writes != 2 {
		t.Fatalf("resume replayed writes: %+v %v", result, err)
	}
}

func TestSyncRequiresFenceAndProvesOldVersionsBeforeReplacingOrDeleting(t *testing.T) {
	source, destination := fixtureSource(), newDestination()
	previous := inventory(t, source)
	if _, err := Copy(context.Background(), source, destination, previous, nil, false); err != nil {
		t.Fatal(err)
	}
	delete(source.objects, "_drive/shares.json")
	source.objects["users/42/a"] = object("users/42/a", "new bytes")
	source.objects["users/43/new"] = object("users/43/new", "new identity prefix")
	final := inventory(t, source)
	if _, err := Copy(context.Background(), source, destination, final, &previous, false); err == nil {
		t.Fatal("unfenced final sync accepted")
	}
	if destination.writes != 2 || destination.removals != 0 {
		t.Fatal("unfenced sync mutated destination")
	}
	result, err := Copy(context.Background(), source, destination, final, &previous, true)
	if err != nil || result.Copied != 2 || result.Removed != 1 || result.Verified != 2 {
		t.Fatalf("sync: %+v %v", result, err)
	}
	if _, err := Copy(context.Background(), source, destination, final, &previous, true); err != nil {
		t.Fatalf("final sync cannot resume: %v", err)
	}
}

func TestUnknownOrCorruptDestinationIsNeverAdoptedOrCleared(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		source, destination := fixtureSource(), newDestination()
		m := inventory(t, source)
		if corrupt {
			obj := source.objects["users/42/a"]
			obj.body = "bad"
			destination.objects[obj.meta.Key] = obj
		} else {
			destination.objects["foreign"] = object("foreign", "preserve")
		}
		if _, err := Copy(context.Background(), source, destination, m, nil, false); err == nil {
			t.Fatal("foreign/corrupt destination accepted")
		}
		if destination.writes != 0 || destination.removals != 0 {
			t.Fatal("preflight changed existing objects")
		}
	}
}

func TestChangedSourceAndFailedImportCannotDeleteDestination(t *testing.T) {
	source, destination := fixtureSource(), newDestination()
	old := inventory(t, source)
	if _, err := Copy(context.Background(), source, destination, old, nil, false); err != nil {
		t.Fatal(err)
	}
	source.objects["users/42/a"] = object("users/42/a", "changed")
	if _, err := Copy(context.Background(), source, destination, old, nil, false); !errors.Is(err, ErrChanged) {
		t.Fatalf("drift accepted: %v", err)
	}
	delete(source.objects, "_drive/shares.json")
	final := inventory(t, source)
	source.fail = "users/42/a"
	if _, err := Copy(context.Background(), source, destination, final, &old, true); err == nil {
		t.Fatal("failed source read ignored")
	}
	if destination.removals != 0 || destination.objects["_drive/shares.json"].body != "[]" {
		t.Fatal("failed copy removed an old object")
	}
}

func TestInventoryRejectsAliasKeysBeforeReadingPayloads(t *testing.T) {
	source := fixtureSource()
	source.objects["/bad"] = object("/bad", "no")
	if _, err := Inventory(context.Background(), source); err == nil || source.opens != 0 {
		t.Fatal("alias key was normalized or read")
	}
}

func TestBackupRoundtripTruncationAndCorruption(t *testing.T) {
	source := fixtureSource()
	source.objects["foo"] = object("foo", "same-name file")
	source.objects["foo/bar"] = object("foo/bar", "same-name prefix child")
	m := inventory(t, source)
	var archive bytes.Buffer
	if err := Backup(context.Background(), source, m, &archive); err != nil {
		t.Fatal(err)
	}
	parsed, err := VerifyBackup(context.Background(), bytes.NewReader(archive.Bytes()))
	if err != nil || parsed.ID != m.ID {
		t.Fatalf("archive roundtrip: %v", err)
	}
	for _, data := range [][]byte{archive.Bytes()[:len(archive.Bytes())/2], append(append([]byte(nil), archive.Bytes()...), []byte("hidden extra bytes")...)} {
		if _, err := VerifyBackup(context.Background(), bytes.NewReader(data)); err == nil {
			t.Fatal("incomplete/trailing backup accepted")
		}
	}
	bad := append([]byte(nil), archive.Bytes()...)
	index := bytes.Index(bad, []byte("same-name prefix child"))
	if index < 0 {
		t.Fatal("fixture payload missing")
	}
	bad[index] ^= 1
	if _, err := VerifyBackup(context.Background(), bytes.NewReader(bad)); err == nil {
		t.Fatal("damaged backup accepted")
	}
}
