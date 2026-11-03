package config

import (
	"os"
	"strconv"
)

type Config struct {
	StorageBackend     string
	FilesystemRoot     string
	FilesystemVolumeID string
	Addr               string
	MinioEndpoint      string
	MinioAccessKey     string
	MinioSecretKey     string
	MinioBucket        string
	MinioUseSSL        bool
	TorrentWorkDir     string
	TorrentTimeoutS    int
	AuthValidateURL    string
	UploadMaxFileBytes int64
	UploadBudgetBytes  int64
	UploadReserveBytes int64
}

func Load() Config {
	return Config{
		StorageBackend:     getenv("STORAGE_BACKEND", "minio"),
		FilesystemRoot:     os.Getenv("FILESYSTEM_ROOT"),
		FilesystemVolumeID: os.Getenv("FILESYSTEM_VOLUME_ID"),
		Addr:               getenv("API_ADDR", ":8080"),
		MinioEndpoint:      getenv("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccessKey:     getenv("MINIO_ACCESS_KEY", "minioadmin"),
		MinioSecretKey:     getenv("MINIO_SECRET_KEY", "minioadmin"),
		MinioBucket:        getenv("MINIO_BUCKET", "drive"),
		MinioUseSSL:        getenvBool("MINIO_USE_SSL", false),
		TorrentWorkDir:     getenv("TORRENT_WORK_DIR", "/tmp/drive-torrents"),
		TorrentTimeoutS:    getenvInt("TORRENT_TIMEOUT_SECONDS", 3600),
		AuthValidateURL:    getenv("AUTH_VALIDATE_URL", ""),
		UploadMaxFileBytes: getenvBytes("UPLOAD_MAX_FILE_BYTES", 10<<30, false),
		UploadBudgetBytes:  getenvBytes("UPLOAD_STAGING_BUDGET_BYTES", 20<<30, false),
		UploadReserveBytes: getenvBytes("UPLOAD_FREE_RESERVE_BYTES", 2<<30, true),
	}
}

// Invalid upload limits must fail startup validation, not silently relax a
// safety bound. Unlike byte counts, other legacy configuration keeps its API.
func getenvBytes(key string, fallback int64, allowZero bool) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || parsed == 0 && !allowZero {
		return -1
	}
	return parsed
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
