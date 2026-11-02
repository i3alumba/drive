//go:build linux

package migration

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// RestoreBackup verifies the whole archive before touching the destination.
// Resume accepts only exact, payload-verified matches; never overwrites or
// removes an unrelated version. No tar paths are extracted to the host.
func RestoreBackup(ctx context.Context, input io.ReadSeeker, destination Destination) (Manifest, Result, error) {
	var result Result
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return Manifest{}, result, err
	}
	manifest, err := VerifyBackup(ctx, input)
	if err != nil {
		return manifest, result, err
	}
	current, err := destination.SnapshotObjects(ctx)
	if err != nil {
		return manifest, result, errors.New("restore destination inventory unavailable")
	}
	wanted := make(map[string]Entry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		wanted[entry.Key] = entry
	}
	matched := make(map[string]bool)
	for _, existing := range current {
		expected, found := wanted[existing.Key]
		if !found || matched[existing.Key] || !sameObject(existing, expected.ObjectMetadata) {
			return manifest, result, errors.New("restore refuses unknown/conflicting destination versions")
		}
		if err := verifyPayload(ctx, destination, existing); err != nil {
			return manifest, result, err
		}
		matched[existing.Key] = true
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return manifest, result, err
	}
	archive := tar.NewReader(input)
	header, err := archive.Next()
	if err != nil || header.Name != archiveManifest {
		return manifest, result, errors.New("backup changed before restore")
	}
	repeated, err := ReadManifest(archive)
	if err != nil {
		return manifest, result, err
	}
	firstJSON, _ := json.Marshal(manifest)
	secondJSON, _ := json.Marshal(repeated)
	if !bytes.Equal(firstJSON, secondJSON) {
		return manifest, result, errors.New("backup manifest changed before restore")
	}
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return manifest, result, err
		}
		header, err := archive.Next()
		if err != nil || header.Typeflag != tar.TypeReg || header.Name != "objects/"+entry.Key || header.Size != entry.Size || !header.ModTime.Equal(entry.Modified) || header.PAXRecords["DRIVE.contentType"] != entry.ContentType || header.PAXRecords["DRIVE.sha256"] != entry.SHA256 {
			return manifest, result, errors.New("backup object changed before restore")
		}
		if matched[entry.Key] {
			result.Unchanged++
			continue
		}
		if err := destination.ImportObject(ctx, entry.ObjectMetadata, archive); err != nil {
			return manifest, result, errors.New("restore object import failed")
		}
		result.Copied++
	}
	if _, err := archive.Next(); err != io.EOF {
		return manifest, result, errors.New("unexpected restore archive entries")
	}
	verified, err := Verify(ctx, destination, manifest)
	result.Verified = verified.Verified
	return manifest, result, err
}
