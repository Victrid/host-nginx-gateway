// Unit tests for the Render function and template helpers
// (DESIGN.md §5.4 item 1 + §3.1 listen semantics).
//
// These tests are intentionally table-driven and avoid golden-file
// dependence on the host filesystem where possible: we render into a
// t.TempDir() and compare against an expected string built inside the
// test, which makes the intent clear when reading the test source.

package dataplane

import (
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// cfg is a small builder helper for tests; keeping the construction terse
// makes the expected-output blocks readable.
func cfg() *contract.Configuration {
	return &contract.Configuration{}
}

func TestRender_NilConfigReturnsError(t *testing.T) {
	if _, err := Render(nil); err == nil {
		t.Fatal("expected error for nil configuration")
	}
}

func TestRender_DeterministicHash(t *testing.T) {
	c := cfg()
	c.Upstreams = []*contract.Upstream{{
		Name: "ns_svc_80",
		Endpoints: []contract.Endpoint{
			{IP: "10.0.0.1", Port: 8080, Ready: true},
			{IP: "10.0.0.2", Port: 8080, Ready: false},
		},
	}}
	c.Servers = []*contract.Server{{
		Hostname: "example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{{
			Path: "/", Upstream: "ns_svc_80",
		}},
	}}
	a, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("Render not deterministic:\nA=%q\nB=%q", a, b)
	}
	if Hash(a) != Hash(b) {
		t.Fatalf("Hash not deterministic")
	}
}

func TestRender_HTTPServerBlockStructure(t *testing.T) {
	c := cfg()
	c.Servers = []*contract.Server{{
		Hostname: "example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{{
			Path: "/", Upstream: "default_svc",
		}},
	}}
	got, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(got)
	must := []string{
		"server {",
		"listen 80;",
		`server_name example.com;`,
		"location / {",
		"proxy_pass http://default_svc;",
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("missing %q\ngot:\n%s", m, s)
		}
	}
}

func TestRender_HTTPSServerBlockWithCert(t *testing.T) {
	c := cfg()
	c.Servers = []*contract.Server{{
		Hostname: "secure.example.com",
		Listens:  []contract.Listen{{Port: 443, SSL: true, HTTP2: true}},
		TLSCert:  "/etc/nginx/conf.d/k8s-gw/certs/ns_tls.pem",
		Locations: []*contract.Location{{
			Path: "/", Upstream: "default_svc",
			Timeouts: &contract.Timeouts{
				Connect: "5s", Send: "10s", Read: "30s",
			},
		}},
	}}
	got, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(got)
	for _, m := range []string{
		"listen 443 ssl http2;",
		"ssl_certificate /etc/nginx/conf.d/k8s-gw/certs/ns_tls.pem;",
		"ssl_certificate_key /etc/nginx/conf.d/k8s-gw/certs/ns_tls.pem;",
		"proxy_connect_timeout 5s;",
		"proxy_send_timeout 10s;",
		"proxy_read_timeout 30s;",
	} {
		if !strings.Contains(s, m) {
			t.Errorf("missing %q\ngot:\n%s", m, s)
		}
	}
}

func TestRender_ListenVariations(t *testing.T) {
	cases := []struct {
		name string
		in   contract.Listen
		want string
	}{
		{"port only", contract.Listen{Port: 8080}, "listen 8080;"},
		{"ssl only", contract.Listen{Port: 443, SSL: true}, "listen 443 ssl;"},
		{"ssl+http2", contract.Listen{Port: 443, SSL: true, HTTP2: true}, "listen 443 ssl http2;"},
		{"http2 only (no ssl)", contract.Listen{Port: 443, HTTP2: true}, "listen 443 http2;"},
		{"ipv4 address", contract.Listen{Port: 80, Address: "192.168.1.10"}, "listen 192.168.1.10:80;"},
		{"ipv6 address", contract.Listen{Port: 80, Address: "[::]"}, "listen [::]:80;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildListen(tc.in)
			if got != tc.want {
				t.Errorf("buildListen(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRender_UpstreamBlock(t *testing.T) {
	c := cfg()
	c.Upstreams = []*contract.Upstream{{
		Name: "ns_svc_80",
		Endpoints: []contract.Endpoint{
			{IP: "10.0.0.1", Port: 8080, Ready: true},
			{IP: "10.0.0.2", Port: 8080, Ready: false},
		},
	}}
	got, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(got)
	for _, m := range []string{
		"upstream ns_svc_80 {",
		"zone ns_svc_80 64k;",
		"server 10.0.0.1:8080;",
		"server 10.0.0.2:8080 down;",
	} {
		if !strings.Contains(s, m) {
			t.Errorf("missing %q\ngot:\n%s", m, s)
		}
	}
}

func TestRender_ServerNameDefaultIsCatchAll(t *testing.T) {
	if got := buildServerName(""); got != "_" {
		t.Errorf("buildServerName(\"\") = %q, want %q", got, "_")
	}
	if got := buildServerName("example.com"); got != "example.com" {
		t.Errorf("buildServerName(example.com) = %q", got)
	}
}

func TestRender_RewriteEmitted(t *testing.T) {
	c := cfg()
	c.Servers = []*contract.Server{{
		Hostname: "example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{{
			Path:     "/old",
			Upstream: "default_svc",
			Rewrite:  "^/old(.*)$ /new$1 break",
		}},
	}}
	got, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(got), "rewrite ^/old(.*)$ /new$1 break;") {
		t.Errorf("rewrite directive missing\ngot:\n%s", got)
	}
}

func TestRender_GoldenSimpleHTTPServer(t *testing.T) {
	// Golden test capturing the full expected output for a simple HTTP server.
	// Future changes to the template will force an explicit update here.
	c := cfg()
	c.Upstreams = []*contract.Upstream{{
		Name: "default_app",
		Endpoints: []contract.Endpoint{
			{IP: "10.0.0.1", Port: 80, Ready: true},
		},
	}}
	c.Servers = []*contract.Server{{
		Hostname: "app.example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{{
			Path: "/", Upstream: "default_app",
		}},
	}}
	got, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "# Managed by HostNginxGateway. Do not edit by hand -- next sync will overwrite.\n" +
		"\n" +
		"# WebSocket proxying: HTTP/1.1 with hop-by-hop Upgrade/Connection handling.\n" +
		"# One map for the whole http context; every proxying location forwards the\n" +
		"# Upgrade/Connection headers through it.\n" +
		"map $http_upgrade $connection_upgrade {\n" +
		"    default upgrade;\n" +
		"    '' close;\n" +
		"}\n" +
		"\n" +
		"upstream default_app {\n" +
		"    zone default_app 64k;\n" +
		"    server 10.0.0.1:80;\n" +
		"}\n" +
		"\n" +
		"server {\n" +
		"    listen 80;\n" +
		"    server_name app.example.com;\n" +
		"    location / {\n" +
		"        proxy_pass http://default_app;\n" +
		"        proxy_http_version 1.1;\n" +
		"        proxy_set_header Upgrade $http_upgrade;\n" +
		"        proxy_set_header Connection $connection_upgrade;\n" +
		"        proxy_set_header Host $http_host;\n" +
		"    }\n" +
		"}"
	if string(got) != want {
		t.Errorf("rendered output mismatch:\nGOT (%d bytes): <<<%s>>>\nWANT (%d bytes): <<<%s>>>", len(got), got, len(want), want)
	}
}
