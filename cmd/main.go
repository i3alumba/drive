package main

import (
	"log"
	"os"

	server "api/internal/adapters/framework/left/server/http"
	"api/internal/adapters/framework/right/auth"
	"api/internal/adapters/framework/right/storage"
)

func main() {
	minioEndpoint := os.Getenv("MINIO_ENDPOINT")
	minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
	minioSecretAccessKey := os.Getenv("MINIO_SECRET_ACCESS_KEY")
	storage := storage.NewMinioAdapter(minioEndpoint, minioAccessKey, minioSecretAccessKey)

	authEndpoint := os.Getenv("AUTH_ENDPOINT")
	auth := auth.NewJWTAdapter(authEndpoint)

	server := server.NewHTTPServeAdapter(storage, auth)
	if err := server.Serve("127.0.0.1", 8000); err != nil {
		log.Fatalf("Error running server: %v\n", err)
	}
}
