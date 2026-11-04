package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-drive/api/internal/config"
	"remote-drive/api/internal/server"
)

func apiIdentity(t *testing.T) config.Config {
	t.Helper()
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer fixture.Close()
	identity := fixture.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(identity.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := config.Config{TLSCertFile: filepath.Join(root, "server.crt"), TLSKeyFile: filepath.Join(root, "server.key")}
	for path, block := range map[string]*pem.Block{cfg.TLSCertFile: {Type: "CERTIFICATE", Bytes: identity.Certificate[0]}, cfg.TLSKeyFile: {Type: "PRIVATE KEY", Bytes: key}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestAPIHTTPSIdentityRequiredAndMatching(t *testing.T) {
	cfg := apiIdentity(t)
	for _, missing := range []config.Config{{}, {TLSCertFile: cfg.TLSCertFile}, {TLSKeyFile: cfg.TLSKeyFile}, {TLSCertFile: cfg.TLSCertFile, TLSKeyFile: cfg.TLSKeyFile + ".missing"}} {
		if _, err := apiTLS(missing); err == nil {
			t.Fatal("missing HTTPS identity accepted")
		}
	}
	loaded, err := apiTLS(cfg)
	if err != nil || loaded.MinVersion < tls.VersionTLS12 || len(loaded.Certificates) != 1 {
		t.Fatal("valid API TLS identity rejected")
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.TLSKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := apiTLS(cfg); err == nil {
		t.Fatal("mismatched certificate/key accepted")
	}
}

func TestAPIHTTPSWireRetainsHealthAndFailClosedAuth(t *testing.T) {
	transportTLS, err := apiTLS(apiIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	var validator *server.AuthValidator
	fixture := httptest.NewUnstartedServer(validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })))
	fixture.TLS = transportTLS
	fixture.StartTLS()
	defer fixture.Close()
	for path, status := range map[string]int{"/healthz": 200, "/api/files": 503} {
		response, err := fixture.Client().Get(fixture.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != status {
			t.Fatal("HTTPS changed health/Auth guard")
		}
	}
	untrusted := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	defer untrusted.CloseIdleConnections()
	if response, err := untrusted.Get(fixture.URL + "/healthz"); err == nil {
		response.Body.Close()
		t.Fatal("untrusted API certificate accepted")
	}
	response, err := untrusted.Get(strings.Replace(fixture.URL, "https:", "http:", 1) + "/healthz")
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("plaintext request was not refused")
		}
	}
}

func TestRequireHTTPSCapabilityFailsBeforeQueueState(t *testing.T) {
	if os.Getenv("DRIVE_TLS_CAPABILITY_CHILD") == "1" {
		os.Args = []string{"drive-api", "--require-https"}
		main()
		return
	}
	root := filepath.Join(t.TempDir(), "must-not-create-state")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRequireHTTPSCapabilityFailsBeforeQueueState$")
	command.Env = []string{"DRIVE_TLS_CAPABILITY_CHILD=1", "STORAGE_BACKEND=filesystem", "FILESYSTEM_ROOT=" + root, "TORRENT_WORK_DIR=" + root}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "API HTTPS certificate and key required") {
		t.Fatalf("capability flag bypassed TLS: %v\\n%s", err, output)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing identity initialized durable queue or filesystem state")
	}
}

func TestAPIStartupMissingTLSFailsBeforeStorage(t *testing.T) {
	if os.Getenv("DRIVE_TLS_MAIN_CHILD") == "1" {
		os.Args = []string{"drive-api"}
		main()
		return
	}
	root := filepath.Join(t.TempDir(), "must-not-initialize")
	work := filepath.Join(t.TempDir(), "must-not-start-uploads")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAPIStartupMissingTLSFailsBeforeStorage$")
	command.Env = []string{"DRIVE_TLS_MAIN_CHILD=1", "STORAGE_BACKEND=filesystem", "FILESYSTEM_ROOT=" + root, "FILESYSTEM_VOLUME_ID=00000000-0000-4000-8000-000000000001", "TORRENT_WORK_DIR=" + work}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "API HTTPS certificate and key required") || strings.Contains(string(output), "connect storage") {
		t.Fatalf("startup did not reject TLS before storage: %v\n%s", err, output)
	}
	for _, path := range []string{root, work} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("TLS failure initialized storage or upload staging")
		}
	}
}
