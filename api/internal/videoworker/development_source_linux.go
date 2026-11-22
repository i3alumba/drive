//go:build linux

package videoworker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"remote-drive/api/internal/videoreader"
)

const DevelopmentSourceVersion = 1

// DevelopmentSource is a trusted controller-side source selection, not a wire
// request. No caller-controlled URL is accepted. This development path is never
// a qualified sandbox, authorization check, teardown receipt, or activation gate.
type DevelopmentSource struct {
	endpoint       string
	startTime      float64
	forceTranscode bool
	Version        int
	Store          videoreader.Store
	Key            string
	Size           int64
	Limits         videoreader.Limits
}

func startDevelopmentReader(ctx context.Context, s DevelopmentSource) (string, func(), error) {
	if s.endpoint != "" {
		return s.endpoint, func() {}, nil
	}
	if s.Version != DevelopmentSourceVersion {
		return "", nil, errors.New("source version")
	}
	reader, err := videoreader.New(ctx, s.Store, s.Key, s.Size, s.Limits)
	if err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		reader.Revoke()
		return "", nil, err
	}
	server := &http.Server{Handler: reader, ReadHeaderTimeout: time.Second, IdleTimeout: time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	stop := func() { reader.Revoke(); _ = server.Close(); <-done }
	return "http://" + listener.Addr().String() + reader.Path(), stop, nil
}

// RunDevelopmentRemoteHLS uses a fresh loopback broker for real decoder tests.
// The default API lane still stages input; this helper does not change public routes or
// the permanently unqualified development controller. Production egress remains
// unqualified: protocol whitelists do not constrain network destinations.
func RunDevelopmentRemoteHLS(ctx context.Context, root, id, format string, options Options, maxOutputBytes int64, source DevelopmentSource, emit func(HLSEvent) error) (HLSConversion, error) {
	return runHLS(ctx, root, id, format, options, maxOutputBytes, emit, &hlsSource{Size: source.Size, startTime: source.startTime, forceTranscode: source.forceTranscode, development: &source})
}

// RunDevelopmentBrokerHLS is a test/development decoder entry point, not an
// isolated controller. The public development handler never exposes this path.
func RunDevelopmentBrokerHLS(ctx context.Context, root string, identity HLSIdentity, format string, options Options, maxOutputBytes int64, source RemoteSource, emit func(HLSEvent) error) (HLSConversion, error) {
	if !source.ValidFor(identity) {
		return HLSConversion{}, failure("invalid_request")
	}
	deadlineCtx, cancel := context.WithDeadline(ctx, source.Deadline)
	defer cancel()
	return runHLS(deadlineCtx, root, identity.JobID, format, options, maxOutputBytes, emit, &hlsSource{endpoint: source.Endpoint, Size: source.Size, startTime: source.StartTime, forceTranscode: source.ForceTranscode})
}
