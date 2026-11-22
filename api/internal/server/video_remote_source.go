package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/videoreader"
	"remote-drive/api/internal/videoworker"
)

type remoteVideoProcessor interface {
	HLSRemoteReady(context.Context) (string, error)
	StreamRemoteHLS(context.Context, videoworker.HLSIdentity, string, int64, videoworker.RemoteSource, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error)
}
type videoSourceStore struct {
	s   *Server
	job *videoPreviewJob
}

func (s videoSourceStore) authorized(ctx context.Context) error {
	r := s.job.sourceRequest.Clone(ctx)
	user, ok := s.s.auth.Validate(ctx, bearerToken(r))
	if !ok || !user.DriveAllowed || user.ID != s.job.owner {
		return errors.New("owner access revoked")
	}
	r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
	resolved, e := s.s.resolvePath(videoRequestSpace(r, s.job.space), s.job.sourcePath, false)
	if e != nil || resolved.Key != s.job.sourceKey {
		return errors.New("source authorization revoked")
	}
	info, e := s.s.store.StatObject(ctx, s.job.sourceKey)
	if e != nil || info.IsDir || info.Size != s.job.source.Size || !info.LastModified.Equal(s.job.source.LastModified) {
		return errors.New("source changed")
	}
	return nil
}
func (s videoSourceStore) GetObjectRange(ctx context.Context, key string, r *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	if key != s.job.sourceKey {
		return nil, storage.ObjectInfo{}, errors.New("source selection")
	}
	if e := s.authorized(ctx); e != nil {
		return nil, storage.ObjectInfo{}, e
	}
	body, info, e := s.s.store.GetObjectRange(ctx, key, r)
	if e != nil {
		return nil, storage.ObjectInfo{}, e
	}
	if info.Size != s.job.source.Size || !info.Modified.Equal(s.job.source.LastModified) {
		body.Close()
		return nil, storage.ObjectInfo{}, errors.New("acquired source changed")
	}
	return body, info, nil
}

// openVideoSource is opt-in and loopback-only. Capability advertisements are
// operator trust assertions, not evidence that a sandbox is actually qualified.
func (s *Server) openVideoSource(ctx context.Context, job *videoPreviewJob) (videoworker.RemoteSource, func(), error) {
	var zero videoworker.RemoteSource
	deadline, ok := ctx.Deadline()
	if !ok {
		return zero, nil, errors.New("source deadline required")
	}
	store := videoSourceStore{s, job}
	limits := videoreader.Limits{Deadline: deadline, MaxRequestBytes: 64 << 10, MaxReadBytes: job.source.Size * 8, Concurrency: 2}
	reader, e := videoreader.New(ctx, store, job.sourceKey, job.source.Size, limits)
	if e != nil {
		return zero, nil, e
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		reader.Revoke()
		return zero, nil, e
	}
	var mu sync.Mutex
	var handlers sync.WaitGroup
	closed := false
	requests := make(chan struct{}, limits.Concurrency)
	server := &http.Server{ReadHeaderTimeout: time.Second, IdleTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if closed {
			mu.Unlock()
			http.Error(w, "revoked", 410)
			return
		}
		handlers.Add(1)
		mu.Unlock()
		defer handlers.Done()
		if r.URL.Path != reader.Path() || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		select {
		case requests <- struct{}{}:
			defer func() { <-requests }()
		default:
			http.Error(w, "busy", 429)
			return
		}
		readctx, cancel := context.WithCancel(r.Context())
		stopCancel := context.AfterFunc(ctx, cancel)
		defer func() { stopCancel(); cancel() }()
		r = r.Clone(readctx)
		if e := store.authorized(r.Context()); e != nil {
			http.Error(w, "source unavailable", 403)
			return
		}
		reader.ServeHTTP(w, r)
	})}
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(listener) }()
	stop := func() {
		reader.Revoke()
		mu.Lock()
		closed = true
		mu.Unlock()
		_ = server.Close()
		<-served
		handlers.Wait()
	}
	descriptor := videoworker.RemoteSource{StartTime: job.startTime, ForceTranscode: job.forceTranscode, V: videoworker.RemoteSourceVersion, Identity: job.identity, Endpoint: "http://" + listener.Addr().String() + reader.Path(), Size: job.source.Size, Deadline: deadline, MaxReadBytes: limits.MaxReadBytes, ChunkBytes: limits.MaxRequestBytes, Concurrency: limits.Concurrency}
	return descriptor, stop, nil
}
