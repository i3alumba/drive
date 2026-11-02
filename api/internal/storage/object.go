package storage

import (
	"errors"
	"time"
)

// ErrNotFound identifies a missing object, not an unavailable storage backend.
var (
	ErrNotFound          = errors.New("object not found")
	ErrVolumeUnavailable = errors.New("filesystem volume unavailable")
	ErrWriterLocked      = errors.New("filesystem writer already active")
	ErrInvalidRange      = errors.New("invalid object range")
	ErrIntegrity         = errors.New("filesystem object checksum mismatch")
)

// ObjectMetadata is the portable, checksum-bound import/export record. Keys
// retain their exact logical spelling; migration must not normalize aliases.
type ObjectMetadata struct {
	Key         string    `json:"key"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	Modified    time.Time `json:"modified"`
	SHA256      string    `json:"sha256"`
}

// ObjectInfo contains the download metadata needed by HTTP consumers, without
// exposing a storage vendor's response types.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

type Object struct {
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
	IsDir        bool      `json:"isDir"`
}

type ByteRange struct {
	Start        int64
	End          int64
	OpenEnded    bool
	SuffixLength int64
}
