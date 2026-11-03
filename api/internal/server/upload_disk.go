package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const uploadRootMarker = "drive-upload-sessions-v1\n"

// uploadDisk uses directory-relative, no-follow operations. User filenames are
// metadata only and are never used as names in the staging filesystem.
type uploadDisk struct {
	root                 *os.File
	lock                 *os.File
	createSync           func() error // optional IO boundaries for fault-injection tests
	beforeCreateManifest func() error
}

func openUploadDisk(path string) (_ *uploadDisk, err error) {
	root, err := uploadOpenDir(nil, path)
	if errors.Is(err, os.ErrNotExist) {
		return createUploadDisk(path)
	}
	if err != nil {
		return nil, err
	}
	disk := &uploadDisk{root: root}
	defer func() {
		if err != nil {
			disk.close()
		}
	}()
	info, err := root.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("upload root must be private")
	}
	// A misconfigured path must not create even a lock file in an unmarked
	// directory (in particular, never touch an existing object/NAS root).
	marker, readErr := uploadReadFile(root, ".manager-version", 128)
	if readErr != nil {
		return nil, readErr
	}
	if string(marker) != uploadRootMarker {
		return nil, errors.New("unrecognized upload staging root")
	}
	disk.lock, err = uploadOpenFile(root, ".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err = uploadTryLock(disk.lock); err != nil {
		return nil, errors.New("upload staging is already locked")
	}
	return disk, nil
}

// Publish a fully marked root atomically. A crash before publication cannot
// leave an unmarked destination that would prevent all subsequent restarts.
func createUploadDisk(path string) (*uploadDisk, error) {
	parent, err := uploadOpenDir(nil, filepath.Dir(path))
	if err != nil {
		return nil, err // Never create a missing parent/mount as a fallback.
	}
	defer parent.Close()
	temporary := filepath.Base(path) + ".init-" + uuid.NewString()
	if err := uploadMkdir(parent, temporary); err != nil {
		return nil, err
	}
	defer uploadUnlink(parent, temporary, true)
	dir, err := uploadOpenDir(parent, temporary)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err := uploadAtomicWrite(dir, ".manager-version", []byte(uploadRootMarker)); err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			_ = uploadUnlink(dir, ".manager-version", false)
		}
	}()
	if err := uploadRenameNewDir(parent, temporary, filepath.Base(path)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return openUploadDisk(path) // A concurrent initializer won; still lock exclusively.
		}
		return nil, err
	}
	published = true
	// The directory handle now references the published root: its marker must
	// NOT be removed by temporary-directory cleanup.
	if err := parent.Sync(); err != nil {
		return nil, err
	}
	return openUploadDisk(path)
}

func (d *uploadDisk) close() {
	if d.lock != nil {
		_ = d.lock.Close()
	}
	if d.root != nil {
		_ = d.root.Close()
	}
}

func validUploadID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id && parsed.Version() == 4
}

func (d *uploadDisk) session(id string) (*os.File, error) {
	if !validUploadID(id) {
		return nil, os.ErrInvalid
	}
	return uploadOpenDir(d.root, id)
}

func (d *uploadDisk) create(record uploadRecord) error {
	if !validUploadID(record.Status.ID) {
		return os.ErrInvalid
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporary := "tmp-" + uuid.NewString()
	if err := uploadMkdir(d.root, temporary); err != nil {
		return err
	}
	defer uploadUnlink(d.root, temporary, true)
	dir, err := uploadOpenDir(d.root, temporary)
	if err != nil {
		return err
	}
	defer dir.Close()
	published := false
	defer func() {
		if !published {
			_ = uploadUnlink(dir, "manifest.json", false)
		}
	}()
	if d.beforeCreateManifest != nil {
		if err := d.beforeCreateManifest(); err != nil {
			return err
		}
	}
	if err := uploadAtomicWrite(dir, "manifest.json", data); err != nil {
		return err
	}
	// A public session ID is never visible without a complete manifest. Errors
	// before rename roll back only our temporary directory; after rename a
	// same-ID retry can reconcile the intact manifest instead of hitting EEXIST.
	if err := uploadRenameNewDir(d.root, temporary, record.Status.ID); err != nil {
		return err
	}
	published = true
	return d.syncCreation()
}

func (d *uploadDisk) syncCreation() error {
	if d.createSync != nil {
		return d.createSync()
	}
	return d.root.Sync()
}

func (d *uploadDisk) save(record uploadRecord) error {
	dir, err := d.session(record.Status.ID)
	if err != nil {
		return err
	}
	defer dir.Close()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return uploadAtomicWrite(dir, "manifest.json", data)
}

// discardUninitialized handles a crash before the first manifest publication,
// or after the final manifest removal. Only empty/known-temporary session dirs
// are eligible; orphan parts or unknown entries are preserved for inspection.
func (d *uploadDisk) discardUninitialized(id string) error {
	dir, err := d.session(id)
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !validUploadTemporary(name) {
			return errors.New("unrecognized uninitialized upload session")
		}
	}
	return d.removeFiles(id, true)
}

func (d *uploadDisk) load(id string) (uploadRecord, error) {
	var record uploadRecord
	dir, err := d.session(id)
	if err != nil {
		return record, err
	}
	defer dir.Close()
	data, err := uploadReadFile(dir, "manifest.json", 1<<20)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return record, errors.New("invalid upload manifest suffix")
	}
	if record.Version != 1 || record.Status.ID != id {
		return record, errors.New("unrecognized upload manifest")
	}
	return record, nil
}

func (d *uploadDisk) ids() ([]string, error) {
	dir, err := uploadOpenDir(d.root, ".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	ids := []string{}
	initializations := []string{}
	scanned := 0
	for {
		names, readErr := dir.Readdirnames(128)
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		scanned += len(names)
		if scanned > 65536 {
			return nil, errors.New("upload recovery scan limit exceeded")
		}
		for _, name := range names {
			switch {
			case validUploadID(name):
				ids = append(ids, name)
			case validUploadTemporary(name):
				// The exclusive manager lock is held. Reclaim interrupted creation
				// before counting live receipts, not by ignoring its temporary name.
				initializations = append(initializations, name)
			case name == ".lock", name == ".manager-version":
			default:
				return nil, errors.New("unrecognized upload staging entry")
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	// Do not mutate directory entries while enumerating them. Temporary
	// initialization does not consume retained-session inventory slots.
	for _, name := range initializations {
		if err := d.reclaimInitialization(name); err != nil {
			return nil, err
		}
	}
	if len(ids) > maxUploadRecords {
		return nil, errors.New("upload session inventory limit exceeded")
	}
	return ids, nil
}

func (d *uploadDisk) reclaimInitialization(name string) error {
	if !validUploadTemporary(name) {
		return os.ErrInvalid
	}
	dir, err := uploadOpenDir(d.root, name) // no-follow, including the directory itself
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := uploadPrivateDir(dir); err != nil {
		return err
	}
	names, err := dir.Readdirnames(8)
	if err != nil && err != io.EOF {
		return err
	}
	if len(names) >= 8 {
		return errors.New("unrecognized initialization contents")
	}
	// Validate ALL contents before unlinking anything. Initialization never
	// contains payload parts: only a small complete manifest or its private
	// atomic-write temporary (which can be truncated by a process crash).
	for _, leaf := range names {
		if leaf != "manifest.json" && !validUploadTemporary(leaf) {
			return errors.New("unrecognized initialization contents")
		}
		data, err := uploadReadFile(dir, leaf, 1<<20)
		if err != nil {
			return err
		}
		if leaf == "manifest.json" {
			var record uploadRecord
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || !validUploadRecord(record) || record.Status.State != "uploading" || len(record.Status.Parts) != 0 || record.Released {
				return errors.New("unrecognized initialization manifest")
			}
		}
	}
	for _, leaf := range names {
		if err := uploadUnlink(dir, leaf, false); err != nil {
			return err
		}
	}
	if err := dir.Sync(); err != nil {
		return err
	}
	if err := uploadUnlink(d.root, name, true); err != nil {
		return err
	}
	return d.root.Sync()
}

func uploadPartName(index int) string {
	return fmt.Sprintf("part-%06d", index)
}

func validUploadTemporary(name string) bool {
	id, ok := strings.CutPrefix(name, "tmp-")
	return ok && validUploadID(id)
}

func validUploadPartName(name string) bool {
	value, ok := strings.CutPrefix(name, "part-")
	if !ok || len(value) != 6 {
		return false
	}
	index, err := strconv.Atoi(value)
	return err == nil && index >= 0 && index < 8192 && uploadPartName(index) == name
}

func (d *uploadDisk) part(id string, index int) (*os.File, error) {
	if index < 0 {
		return nil, os.ErrInvalid
	}
	dir, err := d.session(id)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return uploadOpenFile(dir, uploadPartName(index), os.O_RDONLY)
}

// removeFiles leaves the durable manifest as an idempotency receipt. Unknown
// entries cause cleanup to stop; they are never recursively removed.
func (d *uploadDisk) removeFiles(id string, removeSession bool) error {
	dir, err := d.session(id)
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "manifest.json" || validUploadPartName(name) || validUploadTemporary(name) {
			continue
		}
		return errors.New("unrecognized upload session entry")
	}
	for _, name := range names {
		if name == "manifest.json" {
			continue
		}
		if err := uploadUnlink(dir, name, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := dir.Sync(); err != nil {
		return err
	}
	if !removeSession {
		return nil
	}
	if err := uploadUnlink(dir, "manifest.json", false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := uploadUnlink(d.root, id, true); err != nil {
		return err
	}
	return d.root.Sync()
}

func uploadReadFile(dir *os.File, name string, limit int64) ([]byte, error) {
	file, err := uploadOpenFile(dir, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("upload metadata too large")
	}
	return data, nil
}

func uploadAtomicWrite(dir *os.File, name string, data []byte) error {
	temporary := "tmp-" + uuid.NewString()
	file, err := uploadOpenFile(dir, temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	defer uploadUnlink(dir, temporary, false)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := uploadRename(dir, temporary, name); err != nil {
		return err
	}
	return dir.Sync()
}
