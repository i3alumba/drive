//go:build linux

package migration

import (
	"context"
	"errors"
	"io"
	"time"

	"remote-drive/api/internal/storage"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Source struct {
	client *minio.Client
	ref    SourceRef
}

// NewS3Source never creates a bucket. Historical S3 versions require a different
// archival plan: do not silently flatten an enabled/suspended versioned bucket.
func NewS3Source(ctx context.Context, endpoint, bucket, accessKey, secretKey string, secure bool) (*S3Source, error) {
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil, errors.New("explicit S3 endpoint/bucket/credentials required")
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure})
	if err != nil {
		return nil, errors.New("invalid S3 connection configuration")
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil || !exists {
		return nil, errors.New("existing S3 source bucket unavailable")
	}
	versioning, err := client.GetBucketVersioning(ctx, bucket)
	if err != nil {
		return nil, errors.New("cannot establish source bucket versioning state")
	}
	if versioning.Status != "" {
		return nil, errors.New("versioned/suspended bucket requires historical-version archival plan")
	}
	return &S3Source{client: client, ref: SourceRef{Endpoint: endpoint, Bucket: bucket, Secure: secure}}, nil
}

func (s *S3Source) Ref() SourceRef { return s.ref }

func sourceEntry(info minio.ObjectInfo) Entry {
	return Entry{ObjectMetadata: storage.ObjectMetadata{Key: info.Key, ContentType: info.ContentType, Size: info.Size, Modified: info.LastModified}, ETag: info.ETag, VersionID: info.VersionID}
}

func (s *S3Source) List(ctx context.Context) ([]Entry, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	versioning, err := s.client.GetBucketVersioning(ctx, s.ref.Bucket)
	if err != nil || versioning.Status != "" {
		return nil, errors.New("source versioning state changed or unavailable")
	}
	entries := make([]Entry, 0)
	for item := range s.client.ListObjects(ctx, s.ref.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if item.Err != nil {
			return nil, errors.New("source list failed")
		}
		if len(entries) >= 1_000_000 {
			return nil, errors.New("source exceeds inventory object limit")
		}
		info, err := s.client.StatObject(ctx, s.ref.Bucket, item.Key, minio.StatObjectOptions{})
		if err != nil || info.ETag != item.ETag || info.Size != item.Size || !info.LastModified.Truncate(time.Second).Equal(item.LastModified.Truncate(time.Second)) {
			return nil, ErrChanged
		}
		entry := sourceEntry(info)
		entry.Modified = item.LastModified // LIST retains subsecond precision lost in HTTP Last-Modified.
		entries = append(entries, entry)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *S3Source) Open(ctx context.Context, expected Entry) (io.ReadCloser, error) {
	options := minio.GetObjectOptions{VersionID: expected.VersionID}
	if err := options.SetMatchETag(expected.ETag); err != nil {
		return nil, errors.New("invalid source object precondition")
	}
	reader, err := s.client.GetObject(ctx, s.ref.Bucket, expected.Key, options)
	if err != nil {
		return nil, errors.New("source read failed")
	}
	info, err := reader.Stat()
	actual := sourceEntry(info)
	readExpected := expected
	actual.Modified = actual.Modified.Truncate(time.Second)
	readExpected.Modified = readExpected.Modified.Truncate(time.Second)
	if err != nil || !sameSource(actual, readExpected) {
		reader.Close()
		return nil, ErrChanged
	}
	return reader, nil
}
