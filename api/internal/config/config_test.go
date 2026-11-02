package config

import "testing"

func TestStorageDefaultsRemainMinioAndFilesystemHasNoFallbackRoot(t *testing.T) {
	for _, key := range []string{"STORAGE_BACKEND", "FILESYSTEM_ROOT", "FILESYSTEM_VOLUME_ID"} {
		t.Setenv(key, "")
	}
	cfg := Load()
	if cfg.StorageBackend != "minio" || cfg.FilesystemRoot != "" || cfg.FilesystemVolumeID != "" {
		t.Fatal("unexpected storage defaults")
	}
}

func TestFilesystemSelectionIsExplicit(t *testing.T) {
	t.Setenv("STORAGE_BACKEND", "filesystem")
	t.Setenv("FILESYSTEM_ROOT", "/mounted/drive")
	t.Setenv("FILESYSTEM_VOLUME_ID", "fixture-volume")
	cfg := Load()
	if cfg.StorageBackend != "filesystem" || cfg.FilesystemRoot != "/mounted/drive" || cfg.FilesystemVolumeID != "fixture-volume" {
		t.Fatal("filesystem configuration was not retained")
	}
}
