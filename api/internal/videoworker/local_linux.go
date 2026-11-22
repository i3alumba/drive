//go:build linux

package videoworker

import (
	"context"
	"io"
	"os/exec"
	"time"
)

type hlsSource struct {
	endpoint       string
	Size           int64
	startTime      float64
	forceTranscode bool
	development    *DevelopmentSource
}

// CheckLocalTools executes only fixed -version commands, without network input.
func CheckLocalTools(ctx context.Context, options Options) error {
	for _, name := range []string{options.FFmpeg, options.FFprobe} {
		path, e := exec.LookPath(name)
		if e != nil {
			return e
		}
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(check, path, "-version")
		configureMediaProcess(cmd)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.WaitDelay = time.Second
		e = cmd.Run()
		_ = killMediaGroup(cmd)
		cancel()
		if e != nil {
			return e
		}
	}
	return nil
}

// RunLocalHLS runs tracked probe/decoder children under the API UID and mounts.
// It supplies no sandbox qualification or hostile-descendant containment claim.
// Source server revocation/join and durable admission fencing belong to the API.
func RunLocalHLS(ctx context.Context, root string, identity HLSIdentity, format string, options Options, maxOutputBytes int64, source RemoteSource, emit func(HLSEvent) error) (HLSConversion, error) {
	if !source.ValidFor(identity) {
		return HLSConversion{}, failure("invalid_request")
	}
	bounded, cancel := context.WithDeadline(ctx, source.Deadline)
	defer cancel()
	return runHLS(bounded, root, identity.JobID, format, options, maxOutputBytes, emit, &hlsSource{endpoint: source.Endpoint, Size: source.Size, startTime: source.StartTime, forceTranscode: source.ForceTranscode})
}
