//go:build linux

package videoworker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRemoteCapabilitiesRequireSourceAndEgress(t *testing.T) {
	for _, egress := range []string{"selected-broker-only-no-redirect-no-proxy", "http-whitelist", ""} {
		client := hlsUnixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"v":3,"isolation":%q,"controller_epoch":%q,"source":"bounded-http-range-timeline-v3","egress":%q}`, RemoteControllerRole, hlsClientIdentity.ControllerEpoch, egress)
		}))
		_, e := client.HLSRemoteReady(context.Background())
		if (e == nil) != (egress == "selected-broker-only-no-redirect-no-proxy") {
			t.Fatalf("egress %q: %v", egress, e)
		}
	}
}
func TestRemoteSourceIdentityAndEndpoint(t *testing.T) {
	source := RemoteSource{V: 3, Identity: hlsClientIdentity, Endpoint: "http://127.0.0.1:1234/input/" + strings.Repeat("a", 64), Size: 10, Deadline: time.Now().Add(time.Minute), MaxReadBytes: 80, ChunkBytes: 10, Concurrency: 1}
	if !source.ValidFor(hlsClientIdentity) {
		t.Fatal("valid rejected")
	}
	other := hlsClientIdentity
	other.ExecutionID = strings.Repeat("f", 32)
	if source.ValidFor(other) {
		t.Fatal("execution mismatch accepted")
	}
	for _, endpoint := range []string{"https://127.0.0.1:1234/input/" + strings.Repeat("a", 64), "http://example.org:1234/input/" + strings.Repeat("a", 64), source.Endpoint + "?key=other", strings.Replace(source.Endpoint, "aaaa", "zzzz", 1)} {
		changed := source
		changed.Endpoint = endpoint
		if changed.ValidFor(hlsClientIdentity) {
			t.Fatalf("accepted %q", endpoint)
		}
	}
}
