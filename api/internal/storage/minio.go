package storage

import (
	"context"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const directoryMarker = ".keep"

type Object struct {
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
	IsDir        bool      `json:"isDir"`
}

type Store struct {
	client *minio.Client
	bucket string
}

func New(ctx context.Context, endpoint, accessKey, secretKey, bucket string, useSSL bool) (*Store, error) {
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: useSSL})
	if err != nil {
		return nil, err
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, err
		}
	}
	return &Store{client: client, bucket: bucket}, nil
}

func (s *Store) List(ctx context.Context, prefix string) ([]Object, error) {
	prefix = DirPrefix(prefix)
	objects := make([]Object, 0)
	for item := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: false}) {
		if item.Err != nil {
			return nil, item.Err
		}
		objectPath := item.Key
		isDir := strings.HasSuffix(objectPath, "/") || strings.HasSuffix(objectPath, "/"+directoryMarker)
		if strings.HasSuffix(objectPath, "/"+directoryMarker) {
			objectPath = strings.TrimSuffix(objectPath, directoryMarker)
		}
		objectPath = strings.TrimSuffix(objectPath, "/")
		if objectPath == strings.TrimSuffix(prefix, "/") {
			continue
		}
		objects = append(objects, Object{Name: path.Base(objectPath), Path: objectPath, Size: item.Size, LastModified: item.LastModified, IsDir: isDir})
	}
	return objects, nil
}

func (s *Store) PutDirectory(ctx context.Context, dir string) error {
	key := DirPrefix(dir) + directoryMarker
	if key == directoryMarker {
		return nil
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, strings.NewReader(""), 0, minio.PutObjectOptions{ContentType: "application/x-directory"})
	return err
}

func (s *Store) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	key = CleanObjectPath(key)
	if key == "" {
		return fmt.Errorf("object path is empty")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(key))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, reader, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *Store) GetObject(ctx context.Context, key string) (*minio.Object, minio.ObjectInfo, error) {
	return s.GetObjectRange(ctx, key, nil)
}

func (s *Store) GetObjectRange(ctx context.Context, key string, byteRange *ByteRange) (*minio.Object, minio.ObjectInfo, error) {
	key = CleanObjectPath(key)
	opts := minio.GetObjectOptions{}
	if byteRange != nil {
		if byteRange.SuffixLength > 0 {
			if err := opts.SetRange(0, -byteRange.SuffixLength); err != nil {
				return nil, minio.ObjectInfo{}, err
			}
		} else if byteRange.OpenEnded {
			opts.Set("Range", fmt.Sprintf("bytes=%d-", byteRange.Start))
		} else if err := opts.SetRange(byteRange.Start, byteRange.End); err != nil {
			return nil, minio.ObjectInfo{}, err
		}
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, minio.ObjectInfo{}, err
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, minio.ObjectInfo{}, err
	}
	return obj, info, nil
}

type ByteRange struct {
	Start        int64
	End          int64
	OpenEnded    bool
	SuffixLength int64
}

func (s *Store) MoveObject(ctx context.Context, source string, destination string) error {
	source = CleanObjectPath(source)
	destination = CleanObjectPath(destination)
	if source == "" || destination == "" {
		return fmt.Errorf("source and destination paths are required")
	}
	_, err := s.client.CopyObject(ctx, minio.CopyDestOptions{Bucket: s.bucket, Object: destination}, minio.CopySrcOptions{Bucket: s.bucket, Object: source})
	if err != nil {
		return err
	}
	return s.DeleteObject(ctx, source)
}

func (s *Store) MoveDirectory(ctx context.Context, source string, destination string) error {
	sourcePrefix := DirPrefix(source)
	destinationPrefix := DirPrefix(destination)
	if sourcePrefix == "" || destinationPrefix == "" {
		return fmt.Errorf("source and destination directories are required")
	}
	if strings.HasPrefix(destinationPrefix, sourcePrefix) {
		return fmt.Errorf("cannot move a directory into itself")
	}
	for item := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: sourcePrefix, Recursive: true}) {
		if item.Err != nil {
			return item.Err
		}
		relative := strings.TrimPrefix(item.Key, sourcePrefix)
		newKey := destinationPrefix + relative
		if _, err := s.client.CopyObject(ctx, minio.CopyDestOptions{Bucket: s.bucket, Object: newKey}, minio.CopySrcOptions{Bucket: s.bucket, Object: item.Key}); err != nil {
			return err
		}
	}
	return s.DeleteDirectory(ctx, sourcePrefix)
}

func (s *Store) DeleteObject(ctx context.Context, key string) error {
	key = CleanObjectPath(key)
	if key == "" {
		return fmt.Errorf("path is empty")
	}
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *Store) DeleteDirectory(ctx context.Context, dir string) error {
	dir = DirPrefix(dir)
	if dir == "" {
		return fmt.Errorf("directory path is empty")
	}
	return s.deletePrefix(ctx, dir)
}

func (s *Store) deletePrefix(ctx context.Context, prefix string) error {
	prefix = DirPrefix(prefix)
	objectsCh := make(chan minio.ObjectInfo)
	go func() {
		defer close(objectsCh)
		for item := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if item.Err == nil {
				objectsCh <- item
			}
		}
	}()
	for err := range s.client.RemoveObjects(ctx, s.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if err.Err != nil {
			return err.Err
		}
	}
	return nil
}
