//go:build linux

package videoworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-drive/api/internal/storage"
	"remote-drive/api/internal/videomedia"
	"remote-drive/api/internal/videoreader"
)

type remoteFixtureStore struct {
	mu     sync.Mutex
	data   []byte
	seen   []bool
	unique int
	calls  []storage.ByteRange
	delay  time.Duration
}

func (s *remoteFixtureStore) GetObjectRange(ctx context.Context, key string, r *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	if key != "selected" || r == nil || r.End-r.Start+1 > 16384 {
		return nil, storage.ObjectInfo{}, fmt.Errorf("unbounded/wrong selection")
	}
	select {
	case <-ctx.Done():
		return nil, storage.ObjectInfo{}, ctx.Err()
	case <-time.After(s.delay):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, *r)
	for i := r.Start; i <= r.End; i++ {
		if !s.seen[i] {
			s.seen[i] = true
			s.unique++
		}
	}
	return io.NopCloser(bytes.NewReader(s.data[r.Start : r.End+1])), storage.ObjectInfo{Size: int64(len(s.data))}, nil
}
func TestRemoteHLSFormatMatrix(t *testing.T) {
	hlsRequireIntegration(t)
	for _, format := range []string{"mkv", "avi", "mp4", "mov"} {
		for _, mode := range []string{"remux", "copy_video", "transcode"} {
			t.Run(format+"/"+mode, func(t *testing.T) { runRemoteFixture(t, format, mode, 9, false, 0) })
		}
	}
	t.Run("seek-bframes-audio", func(t *testing.T) { runRemoteFixture(t, "mp4", "transcode", 12, false, 4.3) })
	t.Run("indexed-progressive", func(t *testing.T) { runRemoteFixture(t, "mp4", "remux", 80, true, 0) })
}
func runRemoteFixture(t *testing.T, format, mode string, seconds int, progressive bool, start float64) {
	root, dir := newJob(t)
	source := filepath.Join(t.TempDir(), "source."+format)
	mux := format
	if format == "mkv" {
		mux = "matroska"
	}
	video, audio := "libx264", "aac"
	if mode == "transcode" && start == 0 {
		video = "mpeg4"
	}
	if mode == "copy_video" {
		audio = "ac3"
	}
	args := []string{"-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x180:rate=10:duration=%d", seconds), "-f", "lavfi", "-i", fmt.Sprintf("sine=sample_rate=48000:duration=%d", seconds), "-c:v", video, "-pix_fmt", "yuv420p", "-g", "40", "-bf", "0", "-flags", "+global_header", "-sc_threshold", "0", "-threads", "1", "-c:a", audio, "-ac", "2", "-f", mux, source}
	if start > 0 {
		args = append(args[:len(args)-1], "-output_ts_offset", "2", source)
		for i := range args {
			if args[i] == "-bf" {
				args[i+1] = "2"
			}
		}
	}
	if b, e := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput(); e != nil {
		t.Fatalf("fixture %v %s", e, b)
	}
	data, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	store := &remoteFixtureStore{data: data, seen: make([]bool, len(data))}
	if progressive {
		store.delay = 10 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	selection := DevelopmentSource{startTime: start, Version: DevelopmentSourceVersion, Store: store, Key: "selected", Size: int64(len(data)), Limits: videoreader.Limits{Deadline: time.Now().Add(30 * time.Second), MaxRequestBytes: 16384, MaxReadBytes: int64(len(data)) * 8, Concurrency: 2}}
	var config *videomedia.Config
	var timeline videomedia.Timeline
	segments := 0
	early := false
	result, e := RunDevelopmentRemoteHLS(ctx, root, testID, format, Options{}, 16<<20, selection, func(event HLSEvent) error {
		if event.Type == "started" && (event.OriginUS != int64(start*1000000) || event.SourceDurationUS < int64(seconds)*1000000 || event.ExpectedDurationUS != event.SourceDurationUS-event.OriginUS) {
			return fmt.Errorf("source timeline %+v", event)
		}
		if event.Type == "started" && event.Mode != mode {
			return fmt.Errorf("mode %s want %s", event.Mode, mode)
		}
		if event.Type != "segment" {
			return nil
		}
		if config == nil {
			b, e := os.ReadFile(filepath.Join(dir, HLSDirectoryName, HLSInitName))
			if e != nil {
				return e
			}
			config, e = videomedia.ValidateInit(b)
			if e != nil {
				return e
			}
		}
		name, _ := HLSSegmentName(segments)
		b, e := os.ReadFile(filepath.Join(dir, HLSDirectoryName, name))
		if e != nil {
			return e
		}
		segment, e := videomedia.ValidateSegment(b, config, timeline)
		if e != nil {
			return e
		}
		timeline = segment.Timeline
		segments++
		store.mu.Lock()
		if store.unique < len(data) {
			early = true
		}
		store.mu.Unlock()
		return nil
	})
	if e != nil {
		t.Fatalf("remote %v %+v", e, result)
	}
	if result.Outcome != "complete" || segments < 2 {
		t.Fatalf("%+v segments=%d", result, segments)
	}
	if start > 0 {
		output := filepath.Join(dir, HLSDirectoryName, HLSPlaylistName)
		frame := func(input string, seek float64) []byte {
			a := []string{"-v", "error"}
			if seek > 0 {
				a = append(a, "-ss", fmt.Sprint(seek))
			}
			a = append(a, "-i", input, "-frames:v", "1", "-threads", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
			b, e := exec.Command("/usr/bin/ffmpeg", a...).Output()
			if e != nil {
				t.Fatal(e)
			}
			return b
		}
		expected, actual := frame(source, start), frame(output, 0)
		if len(expected) != len(actual) {
			t.Fatal("seek dimensions mismatch")
		}
		var difference int64
		for i := range actual {
			d := int(actual[i]) - int(expected[i])
			if d < 0 {
				d = -d
			}
			difference += int64(d)
		}
		if float64(difference)/float64(len(actual)) > 15 {
			t.Fatal("seek first frame is not at source origin")
		}
		b, e := exec.Command("/usr/bin/ffprobe", "-v", "error", "-show_entries", "stream=codec_type,start_time", "-of", "json", output).Output()
		if e != nil {
			t.Fatal(e)
		}
		var timing struct {
			Streams []struct {
				Kind  string `json:"codec_type"`
				Start string `json:"start_time"`
			} `json:"streams"`
		}
		if json.Unmarshal(b, &timing) != nil || len(timing.Streams) != 2 {
			t.Fatalf("AV timing %s", b)
		}
		for _, track := range timing.Streams {
			ts, e := strconv.ParseFloat(track.Start, 64)
			if e != nil || ts < 0 || ts > 0.12 {
				t.Fatalf("unnormalized AV origin %s", b)
			}
		}
		packets, e := exec.Command("/usr/bin/ffprobe", "-v", "error", "-show_entries", "packet=stream_index,pts_time,duration_time", "-of", "json", output).Output()
		if e != nil {
			t.Fatal(e)
		}
		var clock struct {
			Packets []struct {
				Index    int    `json:"stream_index"`
				PTS      string `json:"pts_time"`
				Duration string `json:"duration_time"`
			} `json:"packets"`
		}
		if json.Unmarshal(packets, &clock) != nil {
			t.Fatal("packet timing")
		}
		var ends [2]float64
		for _, packet := range clock.Packets {
			if packet.Index > 1 {
				continue
			}
			pts, _ := strconv.ParseFloat(packet.PTS, 64)
			d, _ := strconv.ParseFloat(packet.Duration, 64)
			ends[packet.Index] = max(ends[packet.Index], pts+d)
		}
		drift := ends[0] - ends[1]
		if drift < 0 {
			drift = -drift
		}
		if drift > 0.15 || ends[0] < float64(seconds)-start-0.15 || ends[0] > float64(seconds)-start+0.15 {
			t.Fatalf("AV drift/end %v origin %v", ends, start)
		}

	}
	if progressive && !early {
		t.Fatal("no validated prefix before all indexed input read")
	}
	if _, e := os.Stat(filepath.Join(dir, InputName)); !os.IsNotExist(e) {
		t.Fatal("input staged")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var total int64
	for _, call := range store.calls {
		total += call.End - call.Start + 1
	}
	if total > selection.Limits.MaxReadBytes {
		t.Fatal("read amplification budget exceeded")
	}
	seeks := false
	for i := 1; i < len(store.calls); i++ {
		if store.calls[i].Start != store.calls[i-1].End+1 {
			seeks = true
		}
	}
	if !seeks {
		t.Fatal("no decoder range reconnect/seeks")
	}
}

func TestRemoteHLSCancelAndBudgetCleanup(t *testing.T) {
	hlsRequireIntegration(t)
	for _, kind := range []string{"cancel", "budget"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := newJob(t)
			hlsFixture(t, dir, "remux", 9)
			path := filepath.Join(dir, InputName)
			data, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Remove(path); e != nil {
				t.Fatal(e)
			}
			store := &remoteFixtureStore{data: data, seen: make([]bool, len(data))}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			budget := int64(len(data)) * 8
			if kind == "budget" {
				budget = 16384
			}
			source := DevelopmentSource{Version: DevelopmentSourceVersion, Store: store, Key: "selected", Size: int64(len(data)), Limits: videoreader.Limits{Deadline: time.Now().Add(10 * time.Second), MaxRequestBytes: 16384, MaxReadBytes: budget, Concurrency: 2}}
			_, e = RunDevelopmentRemoteHLS(ctx, root, testID, "mkv", Options{}, 16<<20, source, func(event HLSEvent) error {
				if kind == "cancel" && event.Type == "started" {
					cancel()
				}
				return nil
			})
			if e == nil {
				t.Fatal("cancel/budget exhaustion succeeded")
			}
			store.mu.Lock()
			count := len(store.calls)
			var bytesRead int64
			for _, call := range store.calls {
				bytesRead += call.End - call.Start + 1
			}
			store.mu.Unlock()
			if bytesRead > budget {
				t.Fatal("budget exceeded")
			}
			time.Sleep(50 * time.Millisecond)
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.calls) != count {
				t.Fatal("source activity after runner returned")
			}
		})
	}
}

type activeBlockedStore struct {
	*remoteFixtureStore
	armed   atomic.Bool
	once    sync.Once
	entered chan struct{}
}

func (s *activeBlockedStore) GetObjectRange(ctx context.Context, key string, r *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	if s.armed.Load() {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
		return nil, storage.ObjectInfo{}, ctx.Err()
	}
	return s.remoteFixtureStore.GetObjectRange(ctx, key, r)
}
func TestRemoteHLSCancelDuringActiveSourceRead(t *testing.T) {
	hlsRequireIntegration(t)
	root, dir := newJob(t)
	hlsFixture(t, dir, "remux", 9)
	path := filepath.Join(dir, InputName)
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	store := &activeBlockedStore{remoteFixtureStore: &remoteFixtureStore{data: data, seen: make([]bool, len(data))}, entered: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	source := DevelopmentSource{Version: DevelopmentSourceVersion, Store: store, Key: "selected", Size: int64(len(data)), Limits: videoreader.Limits{Deadline: time.Now().Add(15 * time.Second), MaxRequestBytes: 16384, MaxReadBytes: int64(len(data)) * 8, Concurrency: 2}}
	go func() {
		_, e := RunDevelopmentRemoteHLS(ctx, root, testID, "mkv", Options{}, 16<<20, source, func(event HLSEvent) error {
			if event.Type == "started" {
				store.armed.Store(true)
			}
			return nil
		})
		done <- e
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("decoder never entered active source read")
	}
	cancel()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active source read/decoder not joined")
	}
}
