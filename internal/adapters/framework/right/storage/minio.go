package storage

import (
	"io"
	"io/fs"
	"log"
	"strings"

	"github.com/minio/minio-go"

	"api/internal/ports"
)

type MinioAdapter struct {
	mc *minio.Client
}

func NewMinioAdapter(endpoint string, accessKey string, secretAccessKey string) *MinioAdapter {
	mc, err := minio.New(endpoint, accessKey, secretAccessKey, false)
	if err != nil {
		log.Fatalf("Unable to create Minio client: %v", err)
	}
	return &MinioAdapter{mc: mc}
}

func (a *MinioAdapter) PutObject(f fs.File, home string, path string) error {
	exists, err := a.mc.BucketExists(home)
	if err != nil {
		return err
	}

	if !exists {
		a.mc.MakeBucket(home, "")
	}

	stat, err := f.Stat()
	if err != nil {
		return err
	}

	filename := stat.Name()
	info, err := a.mc.PutObject(home, filename, f, -1, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return err
	}
	log.Printf("Uploaded %s (%.2f MB)\n", filename, float64(info)/(1<<20))

	return nil
}

func (a *MinioAdapter) GetObject(user string, path string) (io.ReadSeeker, error) {
	file, err := a.mc.GetObject(user, path, minio.GetObjectOptions{})
	return file, err
}

func toObjectInfo(minioObjectInfo minio.ObjectInfo) ports.ObjectInfo {
	return ports.ObjectInfo{
		Name:    minioObjectInfo.Key,
		Size:    minioObjectInfo.Size,
		ModTime: minioObjectInfo.LastModified,
		IsDir:   strings.HasSuffix(minioObjectInfo.Key, "/"),
	}
}

func (a *MinioAdapter) ListObjects(home string, prefix string) (<-chan ports.ObjectInfo, error) {
	doneCh := make(chan struct{})
	minioObjects := a.mc.ListObjectsV2(home, prefix, false, doneCh)
	objects := make(chan ports.ObjectInfo)
	go func() {
		for minioObject := range minioObjects {
			objects <- toObjectInfo(minioObject)
		}
		close(objects)
	}()
	return objects, nil
}

func (a *MinioAdapter) StatObject(home string, path string) (ports.ObjectInfo, error) {
	stat, err := a.mc.StatObject(home, path, minio.StatObjectOptions{})
	if err != nil {
		return ports.ObjectInfo{}, err
	}
	return toObjectInfo(stat), nil
}
