//go:build linux

package migration

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateArtifactsNeverOverwriteOrPublishFailedWrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "manifest")
	if err := WritePrivateFile(target, func(w io.Writer) error { _, err := io.WriteString(w, "preserve"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateFile(target, func(w io.Writer) error { _, err := io.WriteString(w, "overwrite"); return err }); err == nil {
		t.Fatal("existing artifact overwritten")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "preserve" {
		t.Fatal("original output changed")
	}
	failed := filepath.Join(dir, "failed")
	if err := WritePrivateFile(failed, func(w io.Writer) error { io.WriteString(w, "partial"); return errors.New("synthetic failure") }); err == nil {
		t.Fatal("failure ignored")
	}
	if _, err := os.Stat(failed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial final output published")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("unpublished temporary output not removed")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPrivateFile(link); err == nil {
		t.Fatal("input symlink followed")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPrivateFile(target); err == nil {
		t.Fatal("public-readable private input accepted")
	}
}
