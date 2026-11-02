//go:build linux

package main

import (
	"remote-drive/api/internal/server"
	"remote-drive/api/internal/storage"
)

func newFilesystem(root, volume string) (server.Store, error) {
	store, err := storage.NewFilesystem(root, volume)
	if err != nil {
		return nil, err
	}
	return store, nil
}

func initializeFilesystem(root, volume string) error {
	return storage.InitializeFilesystem(root, volume)
}
