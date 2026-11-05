package server

import (
	"context"
	"errors"
	"os"
	"time"

	"remote-drive/api/internal/videomedia"
	"remote-drive/api/internal/videoworker"
)

func (s *Server) prepareVideo(ctx context.Context, m *videoPreviewManager, job *videoPreviewJob, format string) {
	defer m.workers.Done()
	defer job.cancel()
	dir, err := uploadOpenDir(m.incoming, job.ID)
	if err == nil {
		defer dir.Close()
		err = s.stageVideoSource(ctx, m, job, dir)
	}
	if err != nil {
		m.finishVideo(job, false, errors.Is(ctx.Err(), context.Canceled), videoFailureCode(ctx), true)
		return
	}
	m.mu.Lock()
	if ctx.Err() != nil || job.expired {
		m.mu.Unlock()
		m.finishVideo(job, false, true, videoFailureCode(ctx), true)
		return
	}
	job.Status, job.attempted = "preparing", true
	m.mu.Unlock()

	// This ledger records only artifacts whose exact private bytes passed
	// validation; a transport ledger alone is not publication authorization.
	ledger := videoworker.HLSSequence{Identity: job.identity, MaxOutputBytes: m.options.MaxOutputBytes}
	_, err = m.processor.StreamHLS(ctx, job.identity, format, m.options.MaxOutputBytes, func(event videoworker.HLSEvent) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		candidate := ledger
		if err := candidate.Accept(event); err != nil {
			return err
		}
		if err := m.publishVideoEvent(ctx, job, dir, event); err != nil {
			return err
		}
		ledger = candidate
		return nil
	})
	complete := err == nil && ledger.Terminal && ledger.Outcome == "complete" && ctx.Err() == nil
	if !complete {
		m.mu.Lock()
		if job.Status != "cancelling" {
			job.Status, job.Error = "stopping", videoFailureCode(ctx)
		}
		m.mu.Unlock()
	}
	cancelled := errors.Is(ctx.Err(), context.Canceled) || ledger.Outcome == "cancelled"
	quiescent := ledger.Terminal
	if !quiescent {
		// EOF, failed conversion and cancellation are not teardown receipts.
		quiescent = m.confirmVideoStopped(job.identity)
	}
	m.finishVideo(job, complete, cancelled, videoFailureCode(ctx), quiescent)
}

func videoFailureCode(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "video_preparation_timeout"
	}
	return "video_preparation_failed"
}

func (m *videoPreviewManager) publishVideoEvent(ctx context.Context, job *videoPreviewJob, sourceDir *os.File, event videoworker.HLSEvent) error {
	switch event.Type {
	case "heartbeat":
		return nil
	case "conversion_end", "terminal":
		if event.Outcome != "complete" {
			m.mu.Lock()
			if job.Status != "cancelling" {
				job.Status, job.Error = "stopping", "video_preparation_failed"
			}
			m.mu.Unlock()
		}
		return nil
	case "started":
		m.mu.Lock()
		defer m.mu.Unlock()
		if ctx.Err() != nil || job.expired || job.Status == "cancelling" {
			return context.Canceled
		}
		job.Mode, job.Duration = event.Mode, float64(event.ExpectedDurationUS)/1000000
		return nil
	}
	name := videoworker.HLSInitName
	if event.Type == "segment" {
		var err error
		name, err = videoworker.HLSSegmentName(event.Index)
		if err != nil {
			return err
		}
	} else if event.Type != "init" {
		return errors.New("invalid video event")
	}
	m.mu.Lock()
	config, timeline := job.config, job.timeline
	if job.expired || ctx.Err() != nil || job.Status == "cancelling" || event.Bytes > m.options.MaxOutputBytes-job.bytes || job.assets[name] != 0 || (event.Type == "segment" && event.Index != len(job.durations)) {
		m.mu.Unlock()
		return errors.New("invalid video publication state")
	}
	m.mu.Unlock()
	outputDir, err := uploadOpenDir(sourceDir, videoworker.HLSDirectoryName)
	if err != nil {
		return err
	}
	defer outputDir.Close()
	if uploadPrivateDir(outputDir) != nil {
		return os.ErrPermission
	}
	file, err := uploadOpenFile(outputDir, name, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.Size() != event.Bytes {
		return errors.New("video artifact size changed")
	}
	var next videomedia.Segment
	validate := func(private []byte) error {
		if event.Type == "init" {
			var err error
			config, err = videomedia.ValidateInit(private)
			return err
		}
		if config == nil {
			return errors.New("video initialization missing")
		}
		var err error
		next, err = videomedia.ValidateSegment(private, config, timeline)
		if err == nil && (next.DurationUS-event.DurationUS > 1 || event.DurationUS-next.DurationUS > 1) {
			return errors.New("video duration disagrees with private fragment")
		}
		return err
	}
	if err := m.copyVideoAsset(ctx, job.ID, name, file, event.Bytes, event.SHA256, validate); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || job.expired || job.Status == "cancelling" {
		return context.Canceled
	}
	job.assets[name] = event.Bytes
	job.bytes += event.Bytes
	if event.Type == "init" {
		job.config = config
	} else {
		job.timeline = next.Timeline
		job.durations = append(job.durations, next.DurationUS)
		job.Segments = len(job.durations)
		job.PreparedDuration += float64(next.DurationUS) / 1000000
		job.Status = "streaming"
	}
	return nil
}

func (m *videoPreviewManager) confirmVideoStopped(identity videoworker.HLSIdentity) bool {
	control, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if status, err := m.processor.CancelHLS(control, identity); err == nil && status.ConfirmedFor(identity) {
		return true
	}
	for attempt := 0; attempt < 3 && control.Err() == nil; attempt++ {
		status, err := m.processor.LookupHLS(control, identity)
		if err == nil && status.ConfirmedFor(identity) {
			return true
		}
		if attempt < 2 {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-control.Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
		}
	}
	return false
}

func (m *videoPreviewManager) reconcileVideo(job *videoPreviewJob) {
	defer m.workers.Done()
	quiescent := m.confirmVideoStopped(job.identity)
	m.finishVideo(job, false, true, "video_preparation_failed", quiescent)
}

func (m *videoPreviewManager) finishVideo(job *videoPreviewJob, complete, cancelled bool, problem string, quiescent bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job.running = false
	if !quiescent {
		job.Status, job.Error, job.uncertain = "stopping", "video_execution_unconfirmed", true
		m.busy, m.fenced = true, true
		return
	}
	complete = complete && !job.expired && job.Status != "cancelling" && !m.closed && (job.execution == nil || job.execution.Err() == nil)
	cancelled = cancelled || job.Status == "cancelling" || job.expired || m.closed
	if err := m.cleanVideoDirectory(m.incoming, job.ID); err != nil {
		job.Status, job.Error, job.uncertain = "stopping", "video_cleanup_unconfirmed", true
		m.busy, m.fenced = true, true
		return
	}
	if !complete {
		if err := m.cleanVideoDirectory(m.ready, job.ID); err != nil {
			job.Status, job.Error, job.uncertain = "stopping", "video_cleanup_unconfirmed", true
			m.busy, m.fenced = true, true
			return
		}
		job.assets = make(map[string]int64)
		job.durations, job.config = nil, nil
	}
	if err := m.removeFence(job.identity); err != nil {
		job.Status, job.Error, job.uncertain = "stopping", "video_execution_fence_unconfirmed", true
		m.busy, m.fenced = true, true
		return
	}
	job.uncertain, m.busy, m.fenced = false, false, false
	switch {
	case complete:
		job.Status, job.Error = "complete", ""
	case cancelled:
		job.Status, job.Error = "cancelled", ""
	default:
		job.Status, job.Error = "failed", problem
	}
}
