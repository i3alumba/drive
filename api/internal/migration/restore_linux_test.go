//go:build linux

package migration

import (
	"bytes"
	"context"
	"testing"
)

func TestBackupCanActuallyRestoreAndResumeWithoutSource(t *testing.T) {
	source := fixtureSource()
	source.objects["foo"] = object("foo", "file")
	source.objects["foo/bar"] = object("foo/bar", "prefix child")
	manifest := inventory(t, source)
	var archive bytes.Buffer
	if err := Backup(context.Background(), source, manifest, &archive); err != nil {
		t.Fatal(err)
	}
	destination := newDestination()
	parsed, result, err := RestoreBackup(context.Background(), bytes.NewReader(archive.Bytes()), destination)
	if err != nil || parsed.ID != manifest.ID || result.Copied != 4 || result.Verified != 4 {
		t.Fatalf("restore: %+v %v", result, err)
	}
	_, result, err = RestoreBackup(context.Background(), bytes.NewReader(archive.Bytes()), destination)
	if err != nil || result.Copied != 0 || result.Unchanged != 4 {
		t.Fatalf("restore replayed writes: %+v %v", result, err)
	}
	if destination.objects["foo"].body != "file" || destination.objects["foo/bar"].body != "prefix child" {
		t.Fatal("file/prefix collision lost during restore")
	}
}

func TestInvalidBackupAndConflictingDestinationNeverWrite(t *testing.T) {
	source := fixtureSource()
	manifest := inventory(t, source)
	var archive bytes.Buffer
	if err := Backup(context.Background(), source, manifest, &archive); err != nil {
		t.Fatal(err)
	}
	destination := newDestination()
	destination.objects["foreign"] = object("foreign", "preserve")
	if _, _, err := RestoreBackup(context.Background(), bytes.NewReader(archive.Bytes()), destination); err == nil {
		t.Fatal("unowned destination adopted")
	}
	if destination.writes != 0 || destination.removals != 0 {
		t.Fatal("conflicting destination changed")
	}
	destination = newDestination()
	if _, _, err := RestoreBackup(context.Background(), bytes.NewReader(archive.Bytes()[:len(archive.Bytes())/2]), destination); err == nil {
		t.Fatal("truncated archive restored")
	}
	if destination.writes != 0 {
		t.Fatal("archive not preverified before writes")
	}
}
