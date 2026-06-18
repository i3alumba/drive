package config

import (
	"os"
	"strconv"
)

type Config struct {
	Addr            string
	MinioEndpoint   string
	MinioAccessKey  string
	MinioSecretKey  string
	MinioBucket     string
	MinioUseSSL     bool
	TorrentWorkDir  string
	TorrentTimeoutS int
}

func Load() Config {
	return Config{
		Addr:            getenv("API_ADDR", ":8080"),
		MinioEndpoint:   getenv("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccessKey:  getenv("MINIO_ACCESS_KEY", "minioadmin"),
		MinioSecretKey:  getenv("MINIO_SECRET_KEY", "minioadmin"),
		MinioBucket:     getenv("MINIO_BUCKET", "drive"),
		MinioUseSSL:     getenvBool("MINIO_USE_SSL", false),
		TorrentWorkDir:  getenv("TORRENT_WORK_DIR", "/tmp/drive-torrents"),
		TorrentTimeoutS: getenvInt("TORRENT_TIMEOUT_SECONDS", 3600),
	}
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
