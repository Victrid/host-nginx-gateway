// WebSocket default-fix tests (DESIGN-multinode-addresses.md §0 v0.2.0):
// the template emits ONE http-context
// `map $http_upgrade $connection_upgrade` and every PROXYING location
// carries proxy_http_version 1.1 + the Upgrade/Connection headers. Static
// return / redirect locations must NOT carry them (they never proxy).
package dataplane

import (
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

const wsMapBlock = "map $http_upgrade $connection_upgrade {\n" +
	"    default upgrade;\n" +
	"    '' close;\n" +
	"}"

func TestRender_WebSocketMapEmittedOnce(t *testing.T) {
	out := render(t, cfg())
	if !strings.Contains(out, wsMapBlock) {
		t.Fatalf("websocket upgrade map missing:\n%s", out)
	}
	if n := strings.Count(out, "map $http_upgrade $connection_upgrade"); n != 1 {
		t.Fatalf("websocket map emitted %d times, want exactly 1:\n%s", n, out)
	}
	// It is emitted even for a configuration with no servers at all.
	empty := render(t, cfg())
	if !strings.Contains(empty, wsMapBlock) {
		t.Fatalf("websocket map must be unconditional:\n%s", empty)
	}
	// With servers present, the map must live at http context: before
	// any server block.
	c := cfg()
	c.Servers = []*contract.Server{{
		Hostname:  "example.com",
		Listens:   []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{{Path: "/", Upstream: "hng_static_500"}},
	}}
	withServers := render(t, c)
	if i, j := strings.Index(withServers, wsMapBlock), strings.Index(withServers, "server {"); !(i < j) {
		t.Fatalf("websocket map must precede server blocks (map@%d, server@%d):\n%s", i, j, withServers)
	}
	if n := strings.Count(withServers, "map $http_upgrade $connection_upgrade"); n != 1 {
		t.Fatalf("websocket map emitted %d times with servers, want 1:\n%s", n, withServers)
	}
}

func TestRender_ProxyLocationsEmitWebSocketHeaders(t *testing.T) {
	c := cfg()
	c.Upstreams = []*contract.Upstream{{
		Name:      "ns_svc_80",
		Endpoints: []contract.Endpoint{{IP: "10.0.0.1", Port: 80, Ready: true}},
	}}
	c.Servers = []*contract.Server{{
		Hostname: "example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{
			{Path: "/", Upstream: "ns_svc_80"},
			{Path: "/dispatch", Upstream: "$hng_s1"},
			{Path: "/gone", Upstream: "hng_static_500"},
			{Path: "/redir", Redirect: &contract.Redirect{Code: 302, URL: "https://example.org/$request_uri"}},
		},
	}}
	out := render(t, c)
	for _, want := range []string{
		"        proxy_http_version 1.1;",
		"        proxy_set_header Upgrade $http_upgrade;",
		"        proxy_set_header Connection $connection_upgrade;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("proxy location missing %q:\n%s", want, out)
		}
	}
	// Both plain and map-dispatch proxy locations carry the headers:
	// two proxy_pass sites → two occurrences of each header.
	for _, want := range []string{"proxy_set_header Upgrade $http_upgrade;"} {
		if n := strings.Count(out, want); n != 2 {
			t.Fatalf("%q appears %d times, want 2 (one per proxying location):\n%s", want, n, out)
		}
	}
	// Non-proxying locations (static return, redirect) must not.
	for _, loc := range []string{"location /gone {", "location /redir {"} {
		i := strings.Index(out, loc)
		if i < 0 {
			t.Fatalf("location %q missing:\n%s", loc, out)
		}
		end := len(out)
		if i+300 < end {
			end = i + 300
		}
		body := out[i:end]
		if strings.Contains(body, "proxy_http_version") {
			t.Fatalf("non-proxying location %s carries proxy_http_version:\n%s", loc, out)
		}
	}
}

func TestBuildLocation_WebSocketDirectiveOrder(t *testing.T) {
	// The WS directives sit directly under proxy_pass, before the Host
	// header — a stable, readable block layout.
	got := buildLocation(contract.Location{Path: "/", Upstream: "ns_svc_80"})
	want := "        proxy_pass http://ns_svc_80;\n" +
		"        proxy_http_version 1.1;\n" +
		"        proxy_set_header Upgrade $http_upgrade;\n" +
		"        proxy_set_header Connection $connection_upgrade;\n" +
		"        proxy_set_header Host $http_host;"
	if !strings.Contains(got, want) {
		t.Fatalf("buildLocation directive order mismatch:\ngot:\n%s\nwant substring:\n%s", got, want)
	}
}
