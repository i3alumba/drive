//go:build linux

// video-worker is a private Linux Unix-socket conversion sidecar, not an API server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"remote-drive/api/internal/videoworker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "video-worker: startup or shutdown failed")
		os.Exit(1)
	}
}
func run() error {
	root := flag.String("root", "", "incoming directory (or fresh scratch base with --init-root)")
	socket := flag.String("socket", "", "Unix socket (default root/worker.sock)")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "FFmpeg binary")
	ffprobe := flag.String("ffprobe", "ffprobe", "FFprobe binary")
	timeout := flag.Duration("timeout", time.Hour, "conversion timeout")
	maxInput := flag.Int64("max-input-bytes", 10<<30, "maximum input bytes")
	maxOutput := flag.Int64("max-output-bytes", 10<<30, "maximum output bytes")
	initRoot := flag.Bool("init-root", false, "initialize an existing empty scratch base and exit")
	flag.Parse()
	if *root == "" || flag.NArg() != 0 {
		return errors.New("root required")
	}
	// CLI-only: no package code mutates process-global umask.
	syscall.Umask(0077)
	if os.Geteuid() != 10001 {
		return errors.New("worker requires uid 10001")
	}
	if *initRoot {
		return videoworker.Initialize(*root)
	}
	if *socket == "" {
		*socket = filepath.Join(*root, videoworker.SocketName)
	}
	h, err := videoworker.NewHandler(*root, videoworker.Options{FFmpeg: *ffmpeg, FFprobe: *ffprobe, Timeout: *timeout, MaxInputBytes: *maxInput, MaxOutputBytes: *maxOutput})
	if err != nil {
		return err
	}
	// Never remove or adopt a preexisting socket/file, including after crashes.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: *socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(*socket, 0600); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = server.Shutdown(shutdown)
	if err != nil {
		_ = server.Close()
	}
	return err
}
