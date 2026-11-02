//go:build linux

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestFilesystemImportPreservesSourceMetadataAndRejectsBadHashBeforePublish(t *testing.T) {
	store, _ := localFilesystem(t)
	body := "original S3 bytes"
	hash := sha256.Sum256([]byte(body))
	meta := ObjectMetadata{Key: "users/42/imported", ContentType: "text/x-source; charset=utf-8", Size: int64(len(body)), Modified: time.Date(2020, 2, 3, 4, 5, 6, 123456789, time.UTC), SHA256: hex.EncodeToString(hash[:])}
	if err := store.ImportObject(context.Background(), meta, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	entries, err := store.SnapshotObjects(context.Background())
	if err != nil || len(entries) != 1 || entries[0] != meta {
		t.Fatalf("import metadata changed: %+v %v", entries, err)
	}
	stat, err := store.StatObject(context.Background(), meta.Key)
	if err != nil || !stat.LastModified.Equal(meta.Modified) {
		t.Fatal("original mtime lost")
	}
	bad := meta
	bad.ContentType = "application/json"
	bad.SHA256 = strings.Repeat("0", 64)
	if err := store.ImportObject(context.Background(), bad, strings.NewReader(body)); err == nil {
		t.Fatal("wrong source checksum committed")
	}
	got, info := readFS(t, store, meta.Key, nil)
	if got != body || info.ContentType != meta.ContentType {
		t.Fatal("failed import changed old object")
	}
	for _, key := range []string{"/users/42/imported", "users/42/../imported", " users/42/imported", "users\\42\\imported"} {
		bad := meta
		bad.Key = key
		if err := store.ImportObject(context.Background(), bad, strings.NewReader(body)); err == nil {
			t.Fatalf("alias key accepted: %q", key)
		}
	}
}
