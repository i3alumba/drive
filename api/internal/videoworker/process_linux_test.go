//go:build linux

package videoworker

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestTrackedMediaProcessGroupCancelWaitsAndReaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sleep", "60")
	configureMediaProcess(cmd)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer killMediaGroup(cmd)
	group, e := syscall.Getpgid(cmd.Process.Pid)
	if e != nil || group != cmd.Process.Pid {
		t.Fatal("child does not own process group", group, e)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case e := <-done:
		if e == nil || cmd.ProcessState == nil {
			t.Fatal("canceled child not waited")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tracked process did not stop")
	}
	if e := syscall.Kill(-group, 0); e != syscall.ESRCH {
		t.Fatalf("tracked group remains: %v", e)
	}
}
