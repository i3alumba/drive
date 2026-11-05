package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"remote-drive/api/internal/server"
	"strings"
	"testing"

	"remote-drive/api/internal/config"
)

func TestExplicitLegacyHTTPOptions(t *testing.T) {
	for _, tc := range []struct {
		name              string
		args              []string
		legacy, wantError bool
	}{
		{"default remains HTTPS", nil, false, false},
		{"explicit legacy HTTP", []string{"--legacy-http"}, true, false},
		{"require HTTPS", []string{"--require-https"}, false, false},
		{"conflicting modes", []string{"--legacy-http", "--require-https"}, false, true},
		{"explicit false still conflicts", []string{"--legacy-http", "--require-https=false"}, false, true},
		{"legacy initialization forbidden", []string{"--legacy-http", "--init-filesystem"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, err := parseRuntimeOptions(tc.args, io.Discard)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%t", err, tc.wantError)
			}
			if err == nil && options.LegacyHTTP != tc.legacy {
				t.Fatal("wrong transport selection", options)
			}
		})
	}
}

func TestExplicitLegacyHTTPWireRetainsHealthAndFailClosedAuth(t *testing.T) {
	identity, err := apiTransport(config.Config{Addr: ":8080"}, true)
	if err != nil || identity != nil {
		t.Fatal(err)
	}
	var validator *server.AuthValidator
	fixture := httptest.NewUnstartedServer(validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })))
	fixture.TLS = identity
	fixture.Start()
	defer fixture.Close()
	for path, want := range map[string]int{"/healthz": 200, "/api/files": 503} {
		response, err := fixture.Client().Get(fixture.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatal("explicit HTTP changed auth/health", response.StatusCode)
		}
	}
}

func TestLegacyProfileFailuresPrecedeStorage(t *testing.T) {
	if os.Getenv("DRIVE_HTTP_PROFILE_CHILD") == "1" {
		os.Args = []string{"drive-api", "--legacy-http"}
		if os.Getenv("DRIVE_HTTP_CONFLICT") == "1" {
			os.Args = append(os.Args, "--require-https=false")
		}
		main()
		return
	}
	for _, tc := range []struct {
		name    string
		env     []string
		message string
	}{
		{"conflicting flag", []string{"DRIVE_HTTP_CONFLICT=1"}, "mutually exclusive"},
		{"TLS identity", []string{"API_TLS_CERT_FILE=/absent/cert"}, "cannot be combined"},
		{"wrong port", []string{"API_ADDR=:8443"}, "baseline API_ADDR"},
		{"foreign Auth", []string{"AUTH_VALIDATE_URL=http://other:8000/api/user/"}, "invalid Auth endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "must-not-create-state")
			env := map[string]string{"DRIVE_HTTP_PROFILE_CHILD": "1", "API_ADDR": ":8080", "AUTH_VALIDATE_URL": "http://auth:8000/api/user/", "STORAGE_BACKEND": "filesystem", "FILESYSTEM_ROOT": root, "TORRENT_WORK_DIR": root}
			for _, value := range tc.env {
				key, val, _ := strings.Cut(value, "=")
				env[key] = val
			}
			command := exec.Command(os.Args[0], "-test.run=^TestLegacyProfileFailuresPrecedeStorage$")
			for key, value := range env {
				command.Env = append(command.Env, key+"="+value)
			}
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.message) || strings.Contains(string(output), "connect storage") {
				t.Fatalf("profile did not fail before storage: %v %s", err, output)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("invalid transport touched durable state", err)
			}
		})
	}
}

func TestLegacyHTTPNeverFallsBackFromInvalidTLS(t *testing.T) {
	if _, err := apiTransport(config.Config{}, false); err == nil {
		t.Fatal("default without TLS identity accepted")
	}
	if _, err := apiTransport(config.Config{Addr: ":8443"}, true); err == nil {
		t.Fatal("legacy HTTP accepted HTTPS default port")
	}
	if identity, err := apiTransport(config.Config{Addr: ":8080"}, true); err != nil || identity != nil {
		t.Fatal("explicit HTTP mode rejected", err)
	}
	for _, cfg := range []config.Config{
		{Addr: ":8080", TLSCertFile: "/absent/cert"}, {Addr: ":8080", TLSKeyFile: "/absent/key"},
		{Addr: ":8080", TLSCertFile: "/absent/cert", TLSKeyFile: "/absent/key"},
	} {
		if _, err := apiTransport(cfg, true); err == nil {
			t.Fatal("ambiguous legacy HTTP/TLS identity accepted")
		}
		if _, err := apiTransport(cfg, false); err == nil {
			t.Fatal("TLS identity failure became plaintext")
		}
	}
}
