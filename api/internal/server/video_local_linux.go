//go:build linux

package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"remote-drive/api/internal/videoworker"
)

// localVideoProcessor reports only process-local tracked-child completion.
// It neither advertises a qualified controller role nor survives API restart.
type localVideoProcessor struct {
	mu      sync.Mutex
	epoch   string
	root    string
	options videoworker.Options
	active  bool
	records map[videoworker.HLSIdentity]videoworker.HLSStatus
}

func newLocalVideoProcessor(options VideoPreviewOptions) (videoProcessor, error) {
	tools := videoworker.Options{FFmpeg: "ffmpeg", FFprobe: "ffprobe", MaxInputBytes: options.MaxInputBytes, MaxOutputBytes: options.MaxOutputBytes, Timeout: options.Timeout}
	if err := videoworker.CheckLocalTools(context.Background(), tools); err != nil {
		return nil, err
	}
	return &localVideoProcessor{epoch: randomID()[:32], root: filepath.Join(options.Root, "incoming"), options: tools, records: make(map[videoworker.HLSIdentity]videoworker.HLSStatus)}, nil
}
func (p *localVideoProcessor) LocalReady(context.Context) (string, error) { return p.epoch, nil }
func (p *localVideoProcessor) HLSReady(context.Context) (string, error) {
	return "", errors.New("local execution is not a qualified remote controller")
}
func (p *localVideoProcessor) HLSRemoteReady(ctx context.Context) (string, error) {
	return p.HLSReady(ctx)
}
func (p *localVideoProcessor) StreamHLS(context.Context, videoworker.HLSIdentity, string, int64, func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
	return videoworker.HLSSequence{}, errors.New("local source descriptor required")
}
func (p *localVideoProcessor) StreamRemoteHLS(ctx context.Context, id videoworker.HLSIdentity, format string, limit int64, source videoworker.RemoteSource, emit func(videoworker.HLSEvent) error) (videoworker.HLSSequence, error) {
	p.mu.Lock()
	if p.active || id.ControllerEpoch != p.epoch || !source.ValidFor(id) {
		p.mu.Unlock()
		return videoworker.HLSSequence{}, errors.New("invalid local execution")
	}
	// The API retains authoritative history; keep only the latest local receipt.
	p.records = map[videoworker.HLSIdentity]videoworker.HLSStatus{id: {Identity: id, State: "running"}}
	p.active = true
	p.mu.Unlock()
	result, err := videoworker.RunLocalHLS(ctx, p.root, id, format, p.options, limit, source, emit)
	// RunLocalHLS has waited for tracked direct children. This is not evidence
	// about hostile processes that escaped the group. The source is joined by API.
	terminal := videoworker.HLSEvent{V: videoworker.HLSVersion, Type: "terminal", Outcome: result.Outcome, Code: result.Code, SegmentCount: result.SegmentCount, DurationUS: result.DurationUS, JobID: id.JobID, ControllerEpoch: id.ControllerEpoch, ExecutionID: id.ExecutionID, Quiescence: "confirmed"}
	p.mu.Lock()
	p.active = false
	p.records[id] = videoworker.HLSStatus{Identity: id, State: "quiescent", Terminal: &terminal}
	p.mu.Unlock()
	if err == nil {
		err = emit(terminal)
	}
	return videoworker.HLSSequence{}, err
}
func (p *localVideoProcessor) LookupHLS(_ context.Context, id videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.records[id]; ok {
		return r, nil
	}
	return videoworker.HLSStatus{Identity: id, State: "unknown"}, nil
}
func (p *localVideoProcessor) CancelHLS(ctx context.Context, id videoworker.HLSIdentity) (videoworker.HLSStatus, error) {
	// The owning API job cancels its execution context; this method only observes
	// join state and never fabricates completion for an untracked execution.
	return p.LookupHLS(ctx, id)
}
