//go:build linux

package storage

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWriterLockErrorDoesNotTreatBrokenNLMAsAnotherWriter(t *testing.T) {
	if writerLockError(nil) != nil {
		t.Fatal("successful lock treated as a failure")
	}
	if !errors.Is(writerLockError(unix.EWOULDBLOCK), ErrWriterLocked) {
		t.Fatal("confirmed lock contention not identified")
	}
	for _, failure := range []error{unix.ENOLCK, unix.EIO, unix.EACCES, unix.ESTALE} {
		got := writerLockError(failure)
		if errors.Is(got, ErrWriterLocked) || !errors.Is(got, ErrVolumeUnavailable) || !errors.Is(got, failure) {
			t.Fatalf("failure misclassified: %v", failure)
		}
	}
}
