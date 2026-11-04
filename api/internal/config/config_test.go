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

func TestHTTPSConfigurationDoesNotGuessServerKeysOrAnAuthIdentity(t *testing.T) {
	for _, name := range []string{"API_ADDR", "API_TLS_CERT_FILE", "API_TLS_KEY_FILE", "AUTH_VALIDATE_URL", "MINIO_USE_SSL"} {
		t.Setenv(name, "")
	}
	cfg := Load()
	if cfg.Addr != ":8443" || cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" || cfg.AuthValidateURL != "" || !cfg.MinioUseSSL {
		t.Fatal("HTTPS defaults or unconfigured Auth behavior changed")
	}
	t.Setenv("API_TLS_CERT_FILE", "/run/internal-tls/tls.crt")
	t.Setenv("API_TLS_KEY_FILE", "/run/internal-tls/tls.key")
	t.Setenv("API_ADDR", "127.0.0.1:8443")
	cfg = Load()
	if cfg.TLSCertFile != "/run/internal-tls/tls.crt" || cfg.TLSKeyFile != "/run/internal-tls/tls.key" || cfg.Addr != "127.0.0.1:8443" {
		t.Fatal("configured server identity/address ignored")
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
