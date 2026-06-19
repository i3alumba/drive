package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"

	"remote-drive/api/internal/storage"
)

const sharesObjectKey = "_drive/shares.json"

const (
	permissionRead = "read"
	permissionEdit = "edit"
)

type Share struct {
	ID             string    `json:"id"`
	OwnerID        string    `json:"ownerId"`
	OwnerUsername  string    `json:"ownerUsername"`
	TargetUsername string    `json:"targetUsername"`
	Path           string    `json:"path"`
	IsDir          bool      `json:"isDir"`
	Permission     string    `json:"permission"`
	CreatedAt      time.Time `json:"createdAt"`
}

type ShareStore struct {
	store *storage.Store
	mu    sync.Mutex
}

func NewShareStore(store *storage.Store) *ShareStore {
	return &ShareStore{store: store}
}

func (s *ShareStore) Create(ctx context.Context, user User, targetUsername, objectPath string, isDir bool, permission string) (Share, error) {
	targetUsername = strings.TrimSpace(targetUsername)
	if targetUsername == "" {
		return Share{}, errors.New("targetUsername is required")
	}
	if strings.EqualFold(targetUsername, user.Username) {
		return Share{}, errors.New("cannot share with yourself")
	}
	permission = strings.TrimSpace(strings.ToLower(permission))
	if permission != permissionRead && permission != permissionEdit {
		return Share{}, errors.New("permission must be read or edit")
	}
	objectPath = storage.CleanObjectPath(objectPath)
	if objectPath == "" {
		return Share{}, errors.New("path is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	shares, err := s.readLocked(ctx)
	if err != nil {
		return Share{}, err
	}
	share := Share{
		ID:             uuid.NewString(),
		OwnerID:        user.ID,
		OwnerUsername:  user.Username,
		TargetUsername: targetUsername,
		Path:           objectPath,
		IsDir:          isDir,
		Permission:     permission,
		CreatedAt:      time.Now().UTC(),
	}
	shares = append(shares, share)
	if err := s.writeLocked(ctx, shares); err != nil {
		return Share{}, err
	}
	return share, nil
}

func (s *ShareStore) Delete(ctx context.Context, user User, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	shares, err := s.readLocked(ctx)
	if err != nil {
		return err
	}
	filtered := shares[:0]
	removed := false
	for _, share := range shares {
		if share.ID == id && share.OwnerID == user.ID {
			removed = true
			continue
		}
		filtered = append(filtered, share)
	}
	if !removed {
		return errors.New("share not found")
	}
	return s.writeLocked(ctx, filtered)
}

func (s *ShareStore) Incoming(ctx context.Context, user User) ([]Share, error) {
	shares, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Share, 0)
	for _, share := range shares {
		if strings.EqualFold(share.TargetUsername, user.Username) {
			result = append(result, share)
		}
	}
	return result, nil
}

func (s *ShareStore) Outgoing(ctx context.Context, user User) ([]Share, error) {
	shares, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Share, 0)
	for _, share := range shares {
		if share.OwnerID == user.ID {
			result = append(result, share)
		}
	}
	return result, nil
}

func (s *ShareStore) FindIncoming(ctx context.Context, user User, id string) (Share, bool, error) {
	shares, err := s.Incoming(ctx, user)
	if err != nil {
		return Share{}, false, err
	}
	for _, share := range shares {
		if share.ID == id {
			return share, true, nil
		}
	}
	return Share{}, false, nil
}

func (s *ShareStore) read(ctx context.Context) ([]Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(ctx)
}

func (s *ShareStore) readLocked(ctx context.Context) ([]Share, error) {
	obj, _, err := s.store.GetObject(ctx, sharesObjectKey)
	if err != nil {
		var minioErr minio.ErrorResponse
		if errors.As(err, &minioErr) && minioErr.Code == "NoSuchKey" {
			return []Share{}, nil
		}
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "The specified key does not exist") {
			return []Share{}, nil
		}
		return nil, err
	}
	defer obj.Close()
	body, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return []Share{}, nil
	}
	var shares []Share
	if err := json.Unmarshal(body, &shares); err != nil {
		return nil, err
	}
	return shares, nil
}

func (s *ShareStore) writeLocked(ctx context.Context, shares []Share) error {
	body, err := json.MarshalIndent(shares, "", "  ")
	if err != nil {
		return err
	}
	return s.store.PutObject(ctx, sharesObjectKey, strings.NewReader(string(body)), int64(len(body)), "application/json")
}

type resolvedPath struct {
	Key           string
	DisplayPrefix string
	Share         *Share
}

func personalPrefix(user User) string {
	return "users/" + storage.CleanObjectPath(user.ID) + "/"
}

func resolvePersonalPath(user User, userPath string) resolvedPath {
	clean := storage.CleanObjectPath(userPath)
	return resolvedPath{Key: storage.CleanObjectPath(personalPrefix(user) + clean), DisplayPrefix: personalPrefix(user)}
}

func resolveSharedPath(share Share, userPath string, write bool) (resolvedPath, error) {
	if write && share.Permission != permissionEdit {
		return resolvedPath{}, errors.New("share is read-only")
	}
	clean := storage.CleanObjectPath(userPath)
	ownerPrefix := "users/" + storage.CleanObjectPath(share.OwnerID) + "/"
	shareRoot := storage.CleanObjectPath(ownerPrefix + share.Path)
	if !share.IsDir {
		if clean != "" && clean != path.Base(share.Path) {
			return resolvedPath{}, errors.New("path is outside shared file")
		}
		return resolvedPath{Key: shareRoot, DisplayPrefix: strings.TrimSuffix(shareRoot, path.Base(shareRoot)), Share: &share}, nil
	}
	key := storage.CleanObjectPath(shareRoot + "/" + clean)
	return resolvedPath{Key: key, DisplayPrefix: storage.DirPrefix(shareRoot), Share: &share}, nil
}

func shareSpaceID(id string) string { return "share:" + id }

func shareIDFromSpace(space string) (string, bool) {
	id, ok := strings.CutPrefix(space, "share:")
	return id, ok && id != ""
}

func virtualizeObject(item storage.Object, displayPrefix string) storage.Object {
	item.Path = storage.CleanObjectPath(strings.TrimPrefix(item.Path, displayPrefix))
	return item
}

func validateShareOwnership(user User, objectPath string) error {
	if !strings.HasPrefix(storage.CleanObjectPath(objectPath), strings.TrimSuffix(personalPrefix(user), "/")+"/") {
		return fmt.Errorf("path is outside user's file space")
	}
	return nil
}
