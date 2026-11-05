//go:build linux

package videoworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testID = "0123456789abcdef0123456789abcdef"

func newJob(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, testID)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return root, dir
}
func testHandler(t *testing.T, root string, o Options) http.Handler {
	t.Helper()
	h, err := NewHandler(root, o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func invoke(h http.Handler, id, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/jobs/"+id, strings.NewReader(body)))
	return w
}
func code(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	var e Error
	if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Code != want {
		t.Fatalf("status %d body %s want %s", w.Code, w.Body, want)
	}
}

const testBody = `{"format":"mkv","max_output_bytes":1048576}`

func TestRequestValidation(t *testing.T) {
	root, _ := newJob(t)
	h := testHandler(t, root, Options{})
	for _, body := range []string{`{}`, `{"format":"mp4","max_output_bytes":1}`, `{"format":"avi","max_output_bytes":-1}`, `{"format":"avi","max_output_bytes":1,"path":"/secret"}`, testBody + `{}`, strings.Repeat("x", 1025)} {
		code(t, invoke(h, testID, body), "invalid_request")
	}
	code(t, invoke(h, strings.Repeat("A", 32), testBody), "invalid_request")
}
func TestPathValidation(t *testing.T) {
	for _, kind := range []string{"missing", "symlink", "hardlink", "empty", "oversized", "directory", "jobsymlink"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := newJob(t)
			input := filepath.Join(dir, InputName)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				_ = os.Symlink(target, input)
			case "hardlink":
				_ = os.Link(target, input)
			case "empty":
				_ = os.WriteFile(input, nil, 0600)
			case "oversized":
				_ = os.WriteFile(input, []byte("12345"), 0600)
			case "directory":
				_ = os.Mkdir(input, 0700)
			case "jobsymlink":
				_ = os.Remove(dir)
				_ = os.Symlink(filepath.Dir(target), dir)
			}
			w := invoke(testHandler(t, root, Options{MaxInputBytes: 4}), testID, testBody)
			want := "invalid_media"
			if kind == "jobsymlink" {
				want = "invalid_request"
			}
			code(t, w, want)
			b, _ := os.ReadFile(target)
			if string(b) != "data" {
				t.Fatal("target changed")
			}
		})
	}
}
func TestPrivatePermissions(t *testing.T) {
	root, dir := newJob(t)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHandler(root, Options{}); err == nil {
		t.Fatal("public root accepted")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	h := testHandler(t, root, Options{})
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	code(t, invoke(h, testID, testBody), "invalid_request")
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, InputName), []byte("private input"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, InputName), 0644); err != nil {
		t.Fatal(err)
	}
	code(t, invoke(h, testID, testBody), "invalid_media")
}

func TestCorruptAndNoOverwrite(t *testing.T) {
	root, dir := newJob(t)
	_ = os.WriteFile(filepath.Join(dir, InputName), []byte("not video"), 0600)
	h := testHandler(t, root, Options{FFprobe: "/usr/bin/ffprobe"})
	code(t, invoke(h, testID, testBody), "invalid_media")
	for _, name := range []string{OutputName, "output.part.mp4"} {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte("owned elsewhere"), 0600)
		code(t, invoke(h, testID, testBody), "invalid_request")
		b, _ := os.ReadFile(p)
		if string(b) != "owned elsewhere" {
			t.Fatal("overwritten")
		}
		_ = os.Remove(p)
	}
}
func TestConcurrency(t *testing.T) {
	root, _ := newJob(t)
	active <- struct{}{}
	defer func() { <-active }()
	code(t, invoke(testHandler(t, root, Options{}), testID, testBody), "busy")
}
func TestInitialize(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"incoming", "ready", markerName} {
		st, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0700)
		if name == markerName {
			want = 0600
		}
		if st.Mode().Perm() != want {
			t.Fatal(st.Mode())
		}
	}
	if Initialize(root) == nil {
		t.Fatal("adopted nonempty root")
	}
	link := filepath.Join(t.TempDir(), "link")
	_ = os.Symlink(root, link)
	if Initialize(link) == nil {
		t.Fatal("followed symlink")
	}
}
func TestUnixClient(t *testing.T) {
	socket := filepath.Join(t.TempDir(), SocketName)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-ignored") {
		default:
			_ = json.NewEncoder(w).Encode(Result{"remux", 42, 1})
		}
	})}
	go server.Serve(l)
	defer server.Close()
	got, err := NewClient(socket).Process(context.Background(), testID, "avi", 100)
	if err != nil || got.Size != 42 {
		t.Fatalf("%+v %v", got, err)
	}
	_, err = NewClient(socket+"missing").Process(context.Background(), testID, "avi", 100)
	var e *Error
	if !errors.As(err, &e) || e.Code != "worker_unavailable" {
		t.Fatal(err)
	}
}
func TestClientBoundsAndDeadline(t *testing.T) {
	for _, kind := range []string{"large", "redirect", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), SocketName)
			l, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "large":
					fmt.Fprint(w, strings.Repeat("x", 4097))
				case "redirect":
					http.Redirect(w, r, "http://127.0.0.1:1", 302)
				case "deadline":
					<-r.Context().Done()
				}
			})}
			go server.Serve(l)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err = NewClient(socket).Process(ctx, testID, "mkv", 100)
			if err == nil {
				t.Fatal("accepted bad response")
			}
		})
	}
}
func TestDurationValidation(t *testing.T) {
	for _, pair := range [][2]float64{{100, 97}, {100, 99.1}, {1, 0.5}, {1, 0}, {10, 13}} {
		want := pair[0] == 100 && pair[1] == 99.1
		if durationMatches(pair[0], pair[1]) != want {
			t.Fatal(pair)
		}
	}
}

func generate(t *testing.T, dir, format string) {
	t.Helper()
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "2", "-threads", "1"}
	if format == "mkv" {
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-f", "matroska")
	} else {
		args = append(args, "-c:v", "mpeg4", "-c:a", "mp3", "-f", "avi")
	}
	args = append(args, "-metadata", "title=source-private-title", "-metadata:s:v:0", "title=source-private-stream", filepath.Join(dir, InputName))
	if b, err := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v %s", err, b)
	}
	if err := os.Chmod(filepath.Join(dir, InputName), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestFFmpegIntegration(t *testing.T) {
	if os.Getenv("VIDEO_WORKER_INTEGRATION") != "1" {
		t.Skip("set VIDEO_WORKER_INTEGRATION=1 for synthetic media tests")
	}
	for _, format := range []string{"mkv", "avi"} {
		t.Run(format, func(t *testing.T) {
			root, dir := newJob(t)
			generate(t, dir, format)
			original, _ := os.ReadFile(filepath.Join(dir, InputName))
			w := invoke(testHandler(t, root, Options{FFmpeg: "/usr/bin/ffmpeg", FFprobe: "/usr/bin/ffprobe"}), testID, fmt.Sprintf(`{"format":%q,"max_output_bytes":1048576}`, format))
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			metadata, err := exec.Command("/usr/bin/ffprobe", "-v", "error", "-show_entries", "format_tags:stream_tags", "-of", "json", filepath.Join(dir, OutputName)).CombinedOutput()
			if err != nil || strings.Contains(string(metadata), "source-private") {
				t.Fatalf("source metadata survived: %v %s", err, metadata)
			}
			var result Result
			_ = json.Unmarshal(w.Body.Bytes(), &result)
			want := "remux"
			if format == "avi" {
				want = "transcode"
			}
			if result.Mode != want {
				t.Fatal(result)
			}
			st, err := os.Stat(filepath.Join(dir, OutputName))
			if err != nil || st.Size() != result.Size {
				t.Fatalf("%v %v", st, err)
			}
			if st.Mode().Perm() != 0600 {
				t.Fatal(st.Mode())
			}
			after, _ := os.ReadFile(filepath.Join(dir, InputName))
			if string(original) != string(after) {
				t.Fatal("input changed")
			}
			if _, err = os.Stat(filepath.Join(dir, "output.part.mp4")); !os.IsNotExist(err) {
				t.Fatal("partial remains")
			}
		})
	}
	for _, kind := range []string{"cap", "failure", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := newJob(t)
			generate(t, dir, "mkv")
			o := Options{FFmpeg: "/usr/bin/ffmpeg", FFprobe: "/usr/bin/ffprobe"}
			body := testBody
			if kind == "cap" {
				body = `{"format":"mkv","max_output_bytes":15000}`
			}
			if kind == "failure" {
				o.FFmpeg = "/usr/bin/false"
			}
			if kind == "deadline" {
				o.Timeout = time.Nanosecond
			}
			w := invoke(testHandler(t, root, o), testID, body)
			if w.Code == 200 {
				t.Fatal("unexpected success")
			}
			for _, name := range []string{OutputName, "output.part.mp4"} {
				if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("left %s", name)
				}
			}
		})
	}
}

func TestOutputLinksAndUnknownFilesPreserved(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := newJob(t)
			_ = os.WriteFile(filepath.Join(dir, InputName), []byte("not video"), 0600)
			target := filepath.Join(t.TempDir(), "original")
			_ = os.WriteFile(target, []byte("preserve"), 0600)
			output := filepath.Join(dir, OutputName)
			if kind == "symlink" {
				_ = os.Symlink(target, output)
			} else {
				_ = os.Link(target, output)
			}
			unknown := filepath.Join(dir, "unknown")
			_ = os.WriteFile(unknown, []byte("unknown"), 0600)
			code(t, invoke(testHandler(t, root, Options{}), testID, testBody), "invalid_request")
			b, _ := os.ReadFile(target)
			if string(b) != "preserve" {
				t.Fatal("changed linked target")
			}
			b, _ = os.ReadFile(unknown)
			if string(b) != "unknown" {
				t.Fatal("changed unknown file")
			}
		})
	}
}
func TestRootSymlinkAndInvalidOptions(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	_ = os.Symlink(root, link)
	if _, err := NewHandler(link, Options{}); err == nil {
		t.Fatal("followed root symlink")
	}
	for _, o := range []Options{{MaxInputBytes: -1}, {MaxOutputBytes: -1}, {Timeout: -1}} {
		if _, err := NewHandler(root, o); err == nil {
			t.Fatal("accepted negative bounds")
		}
	}
}
func TestPinnedInputAndDirectory(t *testing.T) {
	root, dir := newJob(t)
	_ = os.WriteFile(filepath.Join(dir, InputName), []byte("original"), 0600)
	fd, err := openJob(root, testID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.NewFile(uintptr(fd), "job").Close() }()
	f, err := openInput(fd, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, InputName), []byte("replacement"), 0600)
	b := make([]byte, 8)
	if _, err = f.Read(b); err != nil || string(b) != "original" {
		t.Fatalf("lost pinned input %q %v", b, err)
	}
}
