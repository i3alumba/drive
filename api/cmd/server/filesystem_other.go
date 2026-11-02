//go:build !linux

package main

import (
	"errors"

	"remote-drive/api/internal/server"
)

func newFilesystem(_, _ string) (server.Store, error) {
	return nil, errors.New("filesystem backend requires Linux")
}

func initializeFilesystem(_, _ string) error {
	return errors.New("filesystem initialization requires Linux")
}
