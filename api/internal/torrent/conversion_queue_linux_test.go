//go:build linux

package torrent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"remote-drive/api/internal/mediaadmission"
	"sync"
	"testing"
	"time"
)

type conversionTestStore struct {
	mu      sync.Mutex
	created []string
}

func (s *conversionTestStore) PutObject(context.Context, string, io.Reader, int64, string) error {
	return nil
}
func (s *conversionTestStore) PutObjectIfAbsent(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	_, err := io.Copy(io.Discard, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, key)
	return err
}

type converterFunc func(context.Context, string, string) error

func (f converterFunc) Convert(ctx context.Context, in, out string) error { return f(ctx, in, out) }
func conversionManagerFixture(t *testing.T, c Converter) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	store := &conversionTestStore{}
	m := NewManager(store, root, time.Second)
	m.ConfigureConverter(c)
	m.conversionFreeSpace = func(string) bool { return true }
	m.jobs["job"] = &Job{ID: "job", Status: StatusUploading, ConvertVideos: true}
	return m, root
}
func TestConversionQueueMixedResultsKeepsOriginals(t *testing.T) {
	m, root := conversionManagerFixture(t, converterFunc(func(_ context.Context, in, out string) error {
		if filepath.Base(in) == "bad.mkv" {
			return errors.New("unsupported_hdr")
		}
		return os.WriteFile(out, []byte("validated fixture"), 0600)
	}))
	for _, name := range []string{"good.mkv", "bad.mkv", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.convertCompleted("job", root, "users/test/")
	job, _ := m.Get("job")
	if job.ConversionStatus != "partial" || len(job.ConversionResults) != 3 {
		t.Fatalf("%+v", job)
	}
	for _, name := range []string{"good.mkv", "bad.mkv", "notes.txt"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != "original" {
			t.Fatal("original changed", name, err)
		}
	}
	if m.retainedBytes != 0 || m.retainedJobs != 0 {
		t.Fatal("retention leaked")
	}
}
func TestConversionQueueCancellationWhileWaitingJoins(t *testing.T) {
	called := false
	m, root := conversionManagerFixture(t, converterFunc(func(context.Context, string, string) error { called = true; return nil }))
	if err := os.WriteFile(filepath.Join(root, "video.mkv"), []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	m.conversionSlots <- struct{}{}
	done := make(chan struct{})
	go func() { m.convertCompleted("job", root, ""); close(done) }()
	deadline := time.Now().Add(time.Second)
	for {
		job, _ := m.Get("job")
		if job.ConversionStatus == "queued" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := m.Cancel("job"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not join")
	}
	if called || m.retainedJobs != 0 || m.retainedBytes != 0 {
		t.Fatal("cancel leaked resources")
	}
	<-m.conversionSlots
}
func TestConversionQueueNoSpaceFailsWithoutStarting(t *testing.T) {
	m, root := conversionManagerFixture(t, converterFunc(func(context.Context, string, string) error {
		t.Error("converter started without free space")
		return nil
	}))
	m.conversionFreeSpace = func(string) bool { return false }
	m.convertCompleted("job", root, "")
	job, _ := m.Get("job")
	if job.ConversionStatus != "failed" || job.ConversionResults[0].Error != "conversion_no_space" {
		t.Fatalf("%+v", job)
	}
}

func TestConversionQueueRejectsFullAndBudget(t *testing.T) {
	for _, which := range []string{"jobs", "bytes"} {
		t.Run(which, func(t *testing.T) {
			m, root := conversionManagerFixture(t, converterFunc(func(context.Context, string, string) error { t.Error("overbudget converter started"); return nil }))
			want := "conversion_queue_full"
			if which == "jobs" {
				m.retainedJobs = 4
			} else {
				m.retainedBytes = 64 << 30
				want = "conversion_budget_exceeded"
			}
			m.convertCompleted("job", root, "")
			job, _ := m.Get("job")
			if job.ConversionStatus != "failed" || job.ConversionResults[0].Error != want {
				t.Fatalf("%+v", job)
			}
		})
	}
}
func TestCompletedInputsRejectsSymlinkAndOversizedSparseFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/nonexistent", filepath.Join(root, "escape.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := completedInputs(root); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Remove(filepath.Join(root, "escape.mkv")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(root, "large.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxConversionInput + 1); err != nil {
		f.Close()
		t.Skip(err)
	}
	f.Close()
	if _, _, err := completedInputs(root); err == nil {
		t.Fatal("oversized input accepted")
	}
}

// This publisher observes cancellation but deliberately cannot join until released.
// Deadline cancellation must not be mistaken for storage quiescence.
type deadlinePublisher struct {
	conversionTestStore
	expired chan struct{}
	release chan struct{}
}

func (s *deadlinePublisher) PutObjectIfAbsent(ctx context.Context, _ string, _ io.Reader, _ int64, _ string) error {
	<-ctx.Done()
	close(s.expired)
	<-s.release
	return ctx.Err()
}

type admittedConverter struct{ admission *mediaadmission.Admission }

func (c admittedConverter) Convert(ctx context.Context, in, out string) error {
	return os.WriteFile(out, []byte("validated fixture"), 0600)
}
func (c admittedConverter) ConvertAndPublish(ctx context.Context, in, out string, publish func() error) error {
	if err := c.admission.Acquire(ctx); err != nil {
		return err
	}
	defer c.admission.Release()
	if err := c.Convert(ctx, in, out); err != nil {
		return err
	}
	return publish()
}
func TestConversionPublicationDeadlineRetainsResourcesUntilStorageJoins(t *testing.T) {
	admission := mediaadmission.New()
	m, root := conversionManagerFixture(t, admittedConverter{admission})
	m.conversionFileTimeout = 40 * time.Millisecond
	publisher := &deadlinePublisher{expired: make(chan struct{}), release: make(chan struct{})}
	m.store = publisher
	if err := os.WriteFile(filepath.Join(root, "video.mkv"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.convertCompleted("job", root, ""); close(done) }()
	select {
	case <-publisher.expired:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not reach publisher")
	}
	// The publisher has not joined: no token, queue slot, or retained file is free.
	if admission.TryAcquire() {
		admission.Release()
		t.Error("media admission released before storage joined")
	}
	m.mu.RLock()
	jobs, retained := m.retainedJobs, m.retainedBytes
	m.mu.RUnlock()
	if jobs != 1 || retained != 8 || len(m.conversionSlots) != 1 {
		t.Errorf("resources released early: jobs=%d bytes=%d", jobs, retained)
	}
	job, _ := m.Get("job")
	if job.ConversionStatus != "saving" {
		t.Errorf("dishonest publication state: %s", job.ConversionStatus)
	}
	outputs, err := filepath.Glob(filepath.Join(root, ".browser-conversion-*", "output.mp4"))
	if err != nil || len(outputs) != 1 {
		t.Errorf("owned output cleaned before join: %v %v", outputs, err)
	}
	select {
	case <-done:
		t.Error("job returned before storage joined")
	default:
	}
	close(publisher.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not join publisher")
	}
	if !admission.TryAcquire() {
		t.Fatal("media admission leaked")
	}
	admission.Release()
	if m.retainedJobs != 0 || m.retainedBytes != 0 || len(m.conversionSlots) != 0 {
		t.Fatal("retained resources leaked")
	}
	outputs, err = filepath.Glob(filepath.Join(root, ".browser-conversion-*"))
	if err != nil || len(outputs) != 0 {
		t.Fatal("owned work not cleaned after join")
	}
	job, _ = m.Get("job")
	if job.ConversionResults[0].Error != "conversion_timeout" {
		t.Fatalf("%+v", job)
	}
	if data, err := os.ReadFile(filepath.Join(root, "video.mkv")); err != nil || string(data) != "original" {
		t.Fatal("original changed")
	}
}
