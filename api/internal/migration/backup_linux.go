//go:build linux

package migration

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

const archiveManifest = "drive-manifest.json"

// Backup creates an ordinary tar/PAX archive, not a directory extraction.
// File/prefix collisions remain representable; restoring with plain tar -x may
// not be. The archive contains private filenames/data and must stay protected.
func Backup(ctx context.Context, source Source, manifest Manifest, output io.Writer) error {
	if err := Validate(manifest); err != nil {
		return err
	}
	if err := checkSource(ctx, source, manifest); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > maxManifestBytes {
		return errors.New("manifest too large for backup")
	}
	archive := tar.NewWriter(output)
	if err := archive.WriteHeader(&tar.Header{Name: archiveManifest, Mode: 0600, Size: int64(len(encoded)), ModTime: manifest.Created, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := archive.Write(encoded); err != nil {
		return err
	}
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		reader, err := source.Open(ctx, entry)
		if err != nil {
			return errors.New("backup source changed or unavailable")
		}
		header := &tar.Header{Name: "objects/" + entry.Key, Mode: 0600, Size: entry.Size, ModTime: entry.Modified, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"DRIVE.contentType": entry.ContentType, "DRIVE.sha256": entry.SHA256}}
		if err := archive.WriteHeader(header); err != nil {
			reader.Close()
			return err
		}
		hash := sha256.New()
		size, readErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(reader, entry.Size+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || size != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return errors.New("backup payload checksum/size mismatch")
		}
	}
	if err := checkSource(ctx, source, manifest); err != nil {
		return err
	}
	return archive.Close()
}

// VerifyBackup reads all bytes and metadata without extracting files or
// contacting S3/NAS. It rejects extra/missing entries and unsafe tar types.
func VerifyBackup(ctx context.Context, input io.Reader) (Manifest, error) {
	archive := tar.NewReader(input)
	header, err := archive.Next()
	if err != nil || header.Name != archiveManifest || header.Typeflag != tar.TypeReg || header.Size > maxManifestBytes {
		return Manifest{}, errors.New("invalid backup manifest entry")
	}
	manifest, err := ReadManifest(archive)
	if err != nil {
		return Manifest{}, err
	}
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		header, err := archive.Next()
		if err != nil || header.Typeflag != tar.TypeReg || header.Name != "objects/"+entry.Key || header.Size != entry.Size || !header.ModTime.Equal(entry.Modified) || header.PAXRecords["DRIVE.contentType"] != entry.ContentType || header.PAXRecords["DRIVE.sha256"] != entry.SHA256 {
			return Manifest{}, errors.New("backup object metadata mismatch")
		}
		hash := sha256.New()
		size, err := io.Copy(hash, archive)
		if err != nil || size != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return Manifest{}, errors.New("backup payload verification failed")
		}
	}
	if _, err := archive.Next(); err != io.EOF {
		return Manifest{}, errors.New("unexpected backup entries")
	}
	// Consume trailing data: tar.Reader otherwise accepts bytes hidden after
	// end-of-archive. Only zero block padding is allowed by this format.
	padding := make([]byte, 32768)
	for {
		n, err := input.Read(padding)
		if !bytes.Equal(padding[:n], make([]byte, n)) {
			return Manifest{}, errors.New("unexpected trailing backup data")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, errors.New("backup trailing read failed")
		}
	}
	return manifest, nil
}
