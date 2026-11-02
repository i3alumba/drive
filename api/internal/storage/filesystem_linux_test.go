//go:build linux

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func localFilesystem(t *testing.T) (*Filesystem, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	volume := uuid.NewString()
	if err := initializeFilesystem(root, volume, false); err != nil {
		t.Fatal(err)
	}
	store, err := newFilesystem(root, volume, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, root
}

func readFS(t *testing.T, store *Filesystem, key string, requested *ByteRange) (string, ObjectInfo) {
	t.Helper()
	reader, info, err := store.GetObjectRange(context.Background(), key, requested)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), info
}

func putFS(t *testing.T, store *Filesystem, key, data, mime string) {
	t.Helper()
	if err := store.PutObject(context.Background(), key, strings.NewReader(data), int64(len(data)), mime); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemRuntimeRequiresExistingIdentifiedNFSAndNeverCreatesRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := NewFilesystem(missing, uuid.NewString()); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("root was silently created")
	}
	local := t.TempDir()
	if err := InitializeFilesystem(local, uuid.NewString()); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("local FS accepted: %v", err)
	}
	entries, _ := os.ReadDir(local)
	if len(entries) != 0 {
		t.Fatal("local fallback was initialized")
	}
}

func TestFilesystemInitializationDoesNotAdoptNonemptyDirectoryOrReinitialize(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "original"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := initializeFilesystem(root, uuid.NewString(), false); err == nil {
		t.Fatal("nonempty directory accepted")
	}
	data, _ := os.ReadFile(filepath.Join(root, "original"))
	if string(data) != "preserve" {
		t.Fatal("original data changed")
	}
	store, root := localFilesystem(t)
	if err := initializeFilesystem(root, store.volume, false); err == nil {
		t.Fatal("reinitialization allowed")
	}
	if other, err := newFilesystem(root, store.volume, false); err == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
}

func TestFilesystemObjectMetadataRangesAndReservedNames(t *testing.T) {
	store, _ := localFilesystem(t)
	ctx := context.Background()
	for _, name := range []string{"notes.txt", ".drive-object", "~private", ".drive-tmp-name", "кириллица 🐟.txt", "literal%2Fname"} {
		key := "users/42/" + name
		putFS(t, store, key, "abcdef", "text/x-custom")
		data, info := readFS(t, store, key, nil)
		if data != "abcdef" || info.Size != 6 || info.ContentType != "text/x-custom" {
			t.Fatal("object metadata changed")
		}
		stat, err := store.StatObject(ctx, key)
		if err != nil || stat.Name != name || stat.LastModified.IsZero() {
			t.Fatalf("bad stat: %v", err)
		}
		for _, tc := range []struct {
			value *ByteRange
			want  string
		}{
			{&ByteRange{Start: 1, End: 3}, "bcd"},
			{&ByteRange{Start: 2, OpenEnded: true}, "cdef"},
			{&ByteRange{SuffixLength: 2}, "ef"},
			{&ByteRange{SuffixLength: 100}, "abcdef"},
		} {
			got, _ := readFS(t, store, key, tc.value)
			if got != tc.want {
				t.Fatalf("range = %q, want %q", got, tc.want)
			}
		}
	}
	items, err := store.List(ctx, "users/42")
	if err != nil || len(items) != 6 {
		t.Fatalf("list: %d %v", len(items), err)
	}
	for _, item := range items {
		if item.IsDir {
			t.Fatal("object exposed as directory")
		}
	}
}

func TestFilesystemPreservesS3FilePrefixCollisionAndDirectoryMarkers(t *testing.T) {
	store, _ := localFilesystem(t)
	putFS(t, store, "users/42/foo", "file", "text/plain")
	putFS(t, store, "users/42/foo/bar", "nested", "text/plain")
	if err := store.PutDirectory(context.Background(), "users/42/empty"); err != nil {
		t.Fatal(err)
	}
	items, err := store.List(context.Background(), "users/42")
	if err != nil {
		t.Fatal(err)
	}
	file, dir, empty := false, false, false
	for _, item := range items {
		if item.Name == "foo" {
			if item.IsDir {
				dir = true
			} else {
				file = true
			}
		}
		if item.Name == "empty" && item.IsDir {
			empty = true
		}
	}
	if !file || !dir || !empty {
		t.Fatalf("file/prefix/empty-dir lost: %+v", items)
	}
	if err := store.DeleteDirectory(context.Background(), "users/42/foo"); err != nil {
		t.Fatal(err)
	}
	data, _ := readFS(t, store, "users/42/foo", nil)
	if data != "file" {
		t.Fatal("directory deletion removed same-name object")
	}
	items, _ = store.List(context.Background(), "users/42")
	for _, item := range items {
		if item.Name == "foo" && item.IsDir {
			t.Fatal("deleted prefix remained visible")
		}
	}
}

type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("synthetic interrupted upload")
	}
	r.sent = true
	return copy(p, "partial"), nil
}

func TestFilesystemInterruptedOrWrongSizeUploadPreservesOldObjectAndMIME(t *testing.T) {
	store, _ := localFilesystem(t)
	key := "users/42/existing"
	putFS(t, store, key, "preserve", "text/plain")
	if err := store.PutObject(context.Background(), key, &failingReader{}, -1, "application/json"); err == nil {
		t.Fatal("reader failure ignored")
	}
	if err := store.PutObject(context.Background(), key, strings.NewReader("too-long"), 2, "application/json"); err == nil {
		t.Fatal("size mismatch ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.PutObject(ctx, key, strings.NewReader("overwrite"), 9, "application/json"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	data, info := readFS(t, store, key, nil)
	if data != "preserve" || info.ContentType != "text/plain" {
		t.Fatal("failed upload changed committed object")
	}
}

func TestFilesystemMountReplacementMarkerChangeAndCloseFailClosed(t *testing.T) {
	store, root := localFilesystem(t)
	putFS(t, store, "users/42/file", "preserve", "text/plain")
	if err := os.Rename(root, root+"-detached"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(context.Background(), "users/42/new", strings.NewReader("x"), 1, "text/plain"); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatalf("replacement accepted: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("local fallback received data")
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root+"-detached", root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, fsMarker), []byte(uuid.NewString()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(context.Background()); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatal("wrong volume marker accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetObject(context.Background(), "users/42/file"); !errors.Is(err, ErrVolumeUnavailable) {
		t.Fatal("closed store accepted IO")
	}
}

func TestFilesystemRejectsSymlinksAndForgedEscapingNodeNames(t *testing.T) {
	store, root := localFilesystem(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "original"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, fsObjects, "users")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(context.Background(), "users/42/file", strings.NewReader("x"), 1, "text/plain"); err == nil {
		t.Fatal("ancestor symlink followed")
	}
	if err := os.Remove(filepath.Join(root, fsObjects, "users")); err != nil {
		t.Fatal(err)
	}
	putFS(t, store, "users/42/file", "preserve", "text/plain")
	payload := filepath.Join(root, fsObjects, "users", "42", "file", fsPayload)
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "original"), payload); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetObject(context.Background(), "users/42/file"); err == nil {
		t.Fatal("payload symlink followed")
	}
	if err := store.DeleteObject(context.Background(), "users/42/file"); err == nil {
		t.Fatal("foreign symlink was deleted")
	}
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	putFS(t, store, "users/42/file", "preserve", "text/plain")
	if err := os.Mkdir(filepath.Join(root, fsObjects, "users", "42", "~.."), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background(), "users/42"); err == nil {
		t.Fatal("forged traversal node accepted")
	}
	data, _ := os.ReadFile(filepath.Join(outside, "original"))
	if string(data) != "preserve" {
		t.Fatal("outside data changed")
	}
}

func TestFilesystemChecksumCorruptionCannotBeMovedOrReadAsComplete(t *testing.T) {
	store, root := localFilesystem(t)
	key := "users/42/file"
	putFS(t, store, key, "preserve", "text/plain")
	payload := filepath.Join(root, fsObjects, "users", "42", "file", fsPayload)
	file, err := os.OpenFile(payload, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte("damaged!"), fsHeaderSize)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if !errors.Is(err, ErrIntegrity) || len(data) == len("preserve") {
		t.Fatal("complete damaged payload was emitted")
	}
	if err := store.MoveObject(context.Background(), key, "users/42/target"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("move ignored checksum: %v", err)
	}
	if _, _, err := store.GetObject(context.Background(), "users/42/target"); !errors.Is(err, ErrNotFound) {
		t.Fatal("corrupt copy was published")
	}
}

func TestFilesystemMovesPreserveContentAndRejectOverlappingTrees(t *testing.T) {
	store, _ := localFilesystem(t)
	putFS(t, store, "users/42/source/a", "one", "text/x-one")
	putFS(t, store, "users/42/source/sub/b", "two", "text/x-two")
	putFS(t, store, "users/43/untouched", "foreign", "text/plain")
	if err := store.MoveDirectory(context.Background(), "users/42/source", "users/42/source/sub/child"); err == nil {
		t.Fatal("descendant move accepted")
	}
	if err := store.MoveDirectory(context.Background(), "users/42/source/sub", "users/42/source"); err == nil {
		t.Fatal("ancestor move accepted")
	}
	if err := store.MoveDirectory(context.Background(), "users/42/source", "users/42/archive"); err != nil {
		t.Fatal(err)
	}
	data, info := readFS(t, store, "users/42/archive/sub/b", nil)
	if data != "two" || info.ContentType != "text/x-two" {
		t.Fatal("moved metadata changed")
	}
	if _, _, err := store.GetObject(context.Background(), "users/42/source/a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("source remains")
	}
	if err := store.MoveObject(context.Background(), "users/42/archive/a", "users/42/renamed"); err != nil {
		t.Fatal(err)
	}
	data, info = readFS(t, store, "users/42/renamed", nil)
	if data != "one" || info.ContentType != "text/x-one" {
		t.Fatal("file move changed payload")
	}
	data, _ = readFS(t, store, "users/43/untouched", nil)
	if data != "foreign" {
		t.Fatal("other prefix changed")
	}
}

func TestFilesystemAtomicReplacementNeverMixesMetadataAndPayload(t *testing.T) {
	store, _ := localFilesystem(t)
	exerciseAtomicReplacement(t, store)
}

func exerciseAtomicReplacement(t *testing.T, store *Filesystem) {
	t.Helper()
	key := "users/42/atomic"
	first, second := bytes.Repeat([]byte("a"), 32768), bytes.Repeat([]byte("b"), 32768)
	if err := store.PutObject(context.Background(), key, bytes.NewReader(first), int64(len(first)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	failures := make(chan error, 4)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				reader, info, err := store.GetObject(context.Background(), key)
				if err != nil {
					failures <- err
					return
				}
				data, err := io.ReadAll(reader)
				reader.Close()
				if err != nil {
					failures <- err
					return
				}
				if !(bytes.Equal(data, first) && info.ContentType == "text/plain") && !(bytes.Equal(data, second) && info.ContentType == "application/json") {
					failures <- errors.New("torn payload/metadata")
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		data, contentType := first, "text/plain"
		if i%2 == 0 {
			data, contentType = second, "application/json"
		}
		if err := store.PutObject(context.Background(), key, bytes.NewReader(data), int64(len(data)), contentType); err != nil {
			close(stop)
			workers.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}
