package server

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func trustAuthFixture(t *testing.T, validator *AuthValidator, fixture *httptest.Server) {
	t.Helper()
	if validator == nil {
		t.Fatal("valid HTTPS Auth fixture rejected")
	}
	transport := validator.client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = fixture.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs.Clone()
	t.Cleanup(transport.CloseIdleConnections)
}

func TestAuthRequiresFixedVerifiedHTTPSWithoutProxyOrFallback(t *testing.T) {
	for _, endpoint := range []string{"", "http://auth:8000/api/user/", "https://auth/api/user", "https://auth/other/", "https://user:pass@auth/api/user/", "https://auth/api/user/?x=1", "https://auth/api/user/?", "https://auth/api/user/#", "https://auth:0/api/user/", "https://auth:65536/api/user/", "https://auth:/api/user/", " https://auth/api/user/", "https://auth/api/%75ser/"} {
		t.Run(endpoint, func(t *testing.T) {
			validator := NewAuthValidator(endpoint)
			if validator != nil {
				t.Fatal("unsafe Auth endpoint accepted")
			}
			if validator.Valid(context.Background(), "synthetic") {
				t.Fatal("invalid endpoint authorized")
			}
		})
	}
	validator := NewAuthValidator("https://auth.public-page.svc.cluster.local:8443/api/user/")
	if validator == nil {
		t.Fatal("private HTTPS endpoint rejected")
	}
	transport := validator.client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 || transport.TLSClientConfig.RootCAs != nil || validator.client.CheckRedirect == nil {
		t.Fatal("default proxy/trust/redirect policy changed")
	}
}

func TestRealAuthTLSRequiresTrustAndHostnameAndStillReadsLiveGrant(t *testing.T) {
	var calls atomic.Int32
	var allowed atomic.Bool
	allowed.Store(true)
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/user/" || r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("fixed Auth path/header changed")
		}
		body := `{"id":1,"username":"owned","services":{"drive":false}}`
		if allowed.Load() {
			body = `{"id":1,"username":"owned","services":{"drive":true}}`
		}
		_, _ = io.WriteString(w, body)
	}))
	defer fixture.Close()
	untrusted := NewAuthValidator(fixture.URL + "/api/user/")
	if untrusted.Valid(context.Background(), "synthetic") || calls.Load() != 0 {
		t.Fatal("untrusted certificate accepted")
	}
	untrusted.client.CloseIdleConnections()
	trusted := NewAuthValidator(fixture.URL + "/api/user/")
	trustAuthFixture(t, trusted, fixture)
	user, ok := trusted.Validate(context.Background(), "synthetic")
	if !ok || !user.DriveAllowed || user.ID != "1" {
		t.Fatal("trusted Auth identity rejected or changed")
	}
	allowed.Store(false)
	user, ok = trusted.Validate(context.Background(), "synthetic")
	if !ok || user.DriveAllowed || calls.Load() != 2 {
		t.Fatal("live grant was cached or ignored")
	}
	wrongName := NewAuthValidator(fixture.URL + "/api/user/")
	trustAuthFixture(t, wrongName, fixture)
	wrongName.client.Transport.(*http.Transport).TLSClientConfig.ServerName = "wrong.test"
	if wrongName.Valid(context.Background(), "synthetic") || calls.Load() != 2 {
		t.Fatal("wrong certificate SAN accepted")
	}
}

func TestAuthUsesAdditiveLinuxSystemCertDirectories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux certificate-directory contract")
	}
	if os.Getenv("DRIVE_AUTH_TLS_CHILD") == "1" {
		validator := NewAuthValidator(os.Getenv("DRIVE_AUTH_TLS_URL"))
		if !validator.Valid(context.Background(), "synthetic") {
			t.Fatal("additive private CA directory not used")
		}
		return
	}
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":1,"username":"owned","services":{"drive":true}}`)
	}))
	defer fixture.Close()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAuthUsesAdditiveLinuxSystemCertDirectories$")
	// New process: no global root-cache mutation, environment proxy or live credentials.
	command.Env = []string{"DRIVE_AUTH_TLS_CHILD=1", "DRIVE_AUTH_TLS_URL=" + fixture.URL + "/api/user/", "SSL_CERT_DIR=/etc/ssl/certs:/etc/pki/tls/certs:" + root}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("owned CA-directory oracle failed: %v\n%s", err, output)
	}
}
