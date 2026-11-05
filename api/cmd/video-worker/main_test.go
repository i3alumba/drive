//go:build linux

package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCLIHelper(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_CLI_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"video-worker"}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("video-worker", flag.ExitOnError)
	main()
	os.Exit(0)
}

func runCLI(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIHelper$", "--"}, args...)...)
	command.Env = append(os.Environ(), "VIDEO_WORKER_CLI_HELPER=1")
	return command.CombinedOutput()
}

func TestCLIHelpAndInvalidFlags(t *testing.T) {
	output, err := runCLI(t, "--help")
	if err != nil || !strings.Contains(string(output), "-init-root") || !strings.Contains(string(output), "-max-output-bytes") || !strings.Contains(string(output), "-development-hls") || !strings.Contains(string(output), "not a sandbox controller") {
		t.Fatalf("missing help/flags: %v %s", err, output)
	}
	for _, args := range [][]string{{}, {"--unknown-option"}, {"--root", "/unused", "unexpected"}} {
		if _, err := runCLI(t, args...); err == nil {
			t.Fatal("invalid CLI invocation accepted", args)
		}
	}
}

func TestCLIDevelopmentHLSRequiresWorkerUID(t *testing.T) {
	if os.Geteuid() == 10001 {
		t.Skip("UID-negative test requires different test UID")
	}
	root := t.TempDir()
	if _, err := runCLI(t, "--root", root, "--development-hls"); err == nil {
		t.Fatal("development mode bypassed UID requirement")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected development mode wrote artifacts", err, entries)
	}
}

func TestCLIRequiresWorkerUIDBeforeInitialization(t *testing.T) {
	if os.Geteuid() == 10001 {
		t.Skip("UID-negative test requires a different test runner UID")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	output, err := runCLI(t, "--root", root, "--init-root")
	if err == nil || !strings.Contains(string(output), "startup or shutdown failed") {
		t.Fatalf("unexpected UID accepted: %v %s", err, output)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected initialization wrote artifacts", err, entries)
	}
}
