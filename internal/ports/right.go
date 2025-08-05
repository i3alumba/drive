package ports

import (
	"io"
	"io/fs"
	"time"
)

type ObjectInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
}

type StoragePort interface {
	PutObject(f fs.File, user string, path string) error

	GetObject(home string, path string) (io.ReadSeeker, error)

	StatObject(home string, path string) (ObjectInfo, error)

	ListObjects(home string, prefix string) (<-chan ObjectInfo, error)
}

type Metadata interface {
	Get(key string) string
	Set(key string, value string)
}

type AuthPort interface {
	GetUsername(credentials Metadata) (string, error)
}
