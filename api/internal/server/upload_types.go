package server

import "time"

const (
	uploadChunkSize      int64 = 8 << 20
	defaultUploadMax     int64 = 10 << 30
	defaultUploadBudget  int64 = 20 << 30
	defaultUploadReserve int64 = 2 << 30
	uploadSessionTTL           = 24 * time.Hour
	uploadCommitTimeout        = 30 * time.Minute
)

// UploadOptions bounds private staging on an existing work filesystem. Root is
// separate from object storage; enabling uploads never initializes a NAS volume.
type UploadOptions struct {
	Root         string
	MaxFileBytes int64
	BudgetBytes  int64
	ReserveBytes int64
}

// UploadPart describes bytes durably accepted by the API, not browser progress.
type UploadPart struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// UploadStatus deliberately excludes resolved storage keys and authentication.
// A session ID is an identifier, never a capability: every request checks Auth.
type UploadStatus struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Path      string             `json:"path"`
	Space     string             `json:"space"`
	Size      int64              `json:"size"`
	ChunkSize int64              `json:"chunkSize"`
	Parts     map[int]UploadPart `json:"parts"`
	State     string             `json:"state"`
	Error     string             `json:"error,omitempty"`
	SHA256    string             `json:"sha256,omitempty"`
	ExpiresAt time.Time          `json:"expiresAt"`
}

// uploadRecord is private, restart-recoverable metadata. Bearers and cookies
// must never be written here. Version scopes cleanup to this manager's format.
type uploadRecord struct {
	Version     int          `json:"version"`
	Owner       string       `json:"owner"`
	Key         string       `json:"key"`
	ContentType string       `json:"contentType"`
	Released    bool         `json:"released"`
	Status      UploadStatus `json:"status"`
}

type createUploadRequest struct {
	ClientID    string `json:"clientId,omitempty"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Space       string `json:"space"`
	Size        *int64 `json:"size"`
	ContentType string `json:"contentType"`
}
