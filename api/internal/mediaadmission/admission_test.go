package mediaadmission

import (
	"context"
	"errors"
	"testing"
)

func TestAdmissionSharedTryAndCancelledWait(t *testing.T) {
	a := New()
	if !a.TryAcquire() {
		t.Fatal("initial admission")
	}
	if a.TryAcquire() {
		t.Fatal("second decoder admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if New().TryAcquire() == false {
		t.Fatal("unrelated instance coupled")
	}
	a.Release()
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Release()
}
