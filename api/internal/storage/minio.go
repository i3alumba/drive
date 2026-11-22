package storage

import (
	"bytes"
	"context"
	"errors"
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

func (s *Store) Ready(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("object bucket unavailable")
	}
	return nil
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

// PutObjectIfAbsent requires backend conditional-write support; it never falls
// back to a racy StatObject followed by PutObject.
func (s *Store) PutObjectIfAbsent(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	key = CleanObjectPath(key)
	if key == "" {
		return fmt.Errorf("object path is empty")
	}
	if size < 0 {
		return fmt.Errorf("conditional upload requires known size")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opts := minio.PutObjectOptions{ContentType: contentType}
	opts.SetMatchETagExcept("*")
	err := s.putConditional(ctx, key, reader, size, opts)
	var response minio.ErrorResponse
	if errors.As(err, &response) && response.StatusCode == 412 {
		if errors.Is(err, ErrMultipartCleanup) {
			return errors.Join(ErrAlreadyExists, ErrMultipartCleanup)
		}
		return ErrAlreadyExists
	}
	return err
}

func (s *Store) GetObject(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	return s.GetObjectRange(ctx, key, nil)
}

func (s *Store) StatObject(ctx context.Context, key string) (Object, error) {
	key = CleanObjectPath(key)
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return Object{}, objectError(err)
	}
	objectPath := strings.TrimSuffix(key, "/")
	return Object{Name: path.Base(objectPath), Path: objectPath, Size: info.Size, LastModified: info.LastModified, IsDir: false}, nil
}

func (s *Store) GetObjectRange(ctx context.Context, key string, byteRange *ByteRange) (io.ReadCloser, ObjectInfo, error) {
	key = CleanObjectPath(key)
	opts := minio.GetObjectOptions{}
	if byteRange != nil {
		if byteRange.SuffixLength > 0 {
			if err := opts.SetRange(0, -byteRange.SuffixLength); err != nil {
				return nil, ObjectInfo{}, err
			}
		} else if byteRange.OpenEnded {
			opts.Set("Range", fmt.Sprintf("bytes=%d-", byteRange.Start))
		} else if err := opts.SetRange(byteRange.Start, byteRange.End); err != nil {
			return nil, ObjectInfo{}, err
		}
	}
	// MinIO Object.Stat before the first Read performs HEAD and removes Range.
	// Pin HEAD identity, then acquire the actual ranged GET before reading its
	// metadata. Only the one-byte prefix is retained, never staged input.
	pinned, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, objectError(err)
	}
	if err = opts.SetMatchETag(pinned.ETag); err != nil {
		return nil, ObjectInfo{}, err
	}
	opts.VersionID = pinned.VersionID
	obj, err := s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, ObjectInfo{}, objectError(err)
	}
	var prefix [1]byte
	n, readErr := obj.Read(prefix[:])
	if readErr != nil && readErr != io.EOF {
		obj.Close()
		return nil, ObjectInfo{}, objectError(readErr)
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, ObjectInfo{}, objectError(err)
	}
	if info.ETag != pinned.ETag || !info.LastModified.Equal(pinned.LastModified) {
		obj.Close()
		return nil, ObjectInfo{}, ErrIntegrity
	}
	return &prefixedObject{Reader: io.MultiReader(bytes.NewReader(prefix[:n]), obj), Closer: obj}, ObjectInfo{Size: pinned.Size, ContentType: info.ContentType, Modified: info.LastModified}, nil
}

func objectError(err error) error {
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return ErrNotFound
	}
	return err
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

type prefixedObject struct {
	io.Reader
	io.Closer
}

// The pinned SDK's high-level multipart paths reset options before completion,
// discarding conditional headers. Keep the original options at every commit.
func (s *Store) putConditional(ctx context.Context, key string, reader io.Reader, size int64, opts minio.PutObjectOptions) (resultErr error) {
	const partBytes int64 = 16 << 20
	if size > 32<<30 {
		return errors.New("conditional upload exceeds output limit")
	}
	core := minio.Core{Client: s.client}
	if size <= partBytes {
		data, err := io.ReadAll(io.LimitReader(reader, size+1))
		if err != nil {
			return err
		}
		if int64(len(data)) != size {
			return errors.New("conditional upload size mismatch")
		}
		_, err = core.PutObject(ctx, s.bucket, key, bytes.NewReader(data), size, "", "", opts)
		return err
	}
	uploadID, err := core.NewMultipartUpload(ctx, s.bucket, key, opts)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := core.AbortMultipartUpload(cleanupCtx, s.bucket, key, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			resultErr = errors.Join(resultErr, ErrMultipartCleanup)
		}
	}()
	buffer := make([]byte, partBytes)
	parts := make([]minio.CompletePart, 0, (size+partBytes-1)/partBytes)
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(remaining, partBytes)
		if _, err := io.ReadFull(reader, buffer[:count]); err != nil {
			return errors.New("conditional upload size mismatch")
		}
		number := len(parts) + 1
		part, err := core.PutObjectPart(ctx, s.bucket, key, uploadID, number, bytes.NewReader(buffer[:count]), count, minio.PutObjectPartOptions{})
		if err != nil {
			return err
		}
		parts = append(parts, minio.CompletePart{PartNumber: number, ETag: part.ETag})
		remaining -= count
	}
	var extra [1]byte
	if n, err := io.ReadFull(reader, extra[:]); n != 0 || err != io.EOF {
		return errors.New("conditional upload size mismatch")
	}
	_, err = core.CompleteMultipartUpload(ctx, s.bucket, key, uploadID, parts, opts)
	if err != nil {
		return err
	}
	committed = true
	return nil
}
