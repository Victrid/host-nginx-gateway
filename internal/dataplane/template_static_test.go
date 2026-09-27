// Tests for static-marker upstream rendering (GEP-1364 / DESIGN.md §3.4):
// a Location whose Upstream carries the hng_static_ prefix must render a
// `return <code>;` line instead of proxy_pass, and must never cause an
// upstream block to be emitted (the provider emits no such entry —
// translate.go Configuration doc).
package dataplane

import (
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"

	// Test-only import: locks staticUpstreamPrefix against the provider's
	// constant so the two lanes cannot drift. The dataplane package itself
	// stays free of Kubernetes dependencies.
	"github.com/Victrid/HostNginxGateway/internal/provider"
)

// TestStaticUpstreamPrefixMatchesProvider guarantees the renderer's marker
// prefix stays identical to the one the provider emits
// (internal/provider.StaticUpstreamPrefix).
func TestStaticUpstreamPrefixMatchesProvider(t *testing.T) {
	if staticUpstreamPrefix != provider.StaticUpstreamPrefix {
		t.Fatalf("prefix drift: dataplane %q != provider %q",
			staticUpstreamPrefix, provider.StaticUpstreamPrefix)
	}
	if !IsStaticUpstream(provider.StaticUpstreamName(500)) {
		t.Fatalf("provider marker %q not recognized by renderer", provider.StaticUpstreamName(500))
	}
}

func TestIsStaticUpstream(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"static 500", "hng_static_500", true},
		{"static bare prefix", "hng_static_", true},
		{"static other code", "hng_static_503", true},
		{"regular upstream", "ns_svc_80", false},
		{"empty", "", false},
		{"prefix embedded mid-name", "upstream_hng_static_500", false},
	}
	for _, tc := range cases {
		if got := IsStaticUpstream(tc.in); got != tc.want {
			t.Errorf("IsStaticUpstream(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestStaticReturnCode(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantCode int
		wantOK   bool
	}{
		{"static 500", "hng_static_500", 500, true},
		{"static 503", "hng_static_503", 503, true},
		{"bare prefix falls back", "hng_static_", 500, true},
		{"non-numeric falls back", "hng_static_abc", 500, true},
		{"zero falls back", "hng_static_0", 500, true},
		{"negative falls back", "hng_static_-1", 500, true},
		{"empty upstream is static placeholder", "", 500, true},
		{"regular upstream not static", "ns_svc_80", 0, false},
	}
	for _, tc := range cases {
		code, ok := staticReturnCode(tc.in)
		if code != tc.wantCode || ok != tc.wantOK {
			t.Errorf("staticReturnCode(%q) = (%d, %v), want (%d, %v)",
				tc.in, code, ok, tc.wantCode, tc.wantOK)
		}
	}
}

func TestBuildLocation_StaticRendersReturn(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		want     string
	}{
		{"code 500", "hng_static_500", "        return 500;"},
		{"code 503", "hng_static_503", "        return 503;"},
		{"bare prefix default", "hng_static_", "        return 500;"},
		{"garbage suffix default", "hng_static_x", "        return 500;"},
		{"empty upstream placeholder", "", "        return 500;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildLocation(contract.Location{Path: "/", Upstream: tc.upstream})
			if got != tc.want {
				t.Errorf("buildLocation = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "proxy_pass") {
				t.Errorf("static location must not proxy_pass, got %q", got)
			}
		})
	}
}

func TestBuildLocation_NonStaticStillProxyPass(t *testing.T) {
	got := buildLocation(contract.Location{Path: "/", Upstream: "ns_svc_80"})
	if !strings.Contains(got, "proxy_pass http://ns_svc_80;") {
		t.Errorf("proxy_pass missing, got %q", got)
	}
	if strings.Contains(got, "return ") {
		t.Errorf("non-static location must not return, got %q", got)
	}
}

func TestBuildLocation_StaticSkipsProxyTimeouts(t *testing.T) {
	got := buildLocation(contract.Location{
		Path:     "/",
		Upstream: "hng_static_500",
		Timeouts: &contract.Timeouts{Connect: "5s", Send: "10s", Read: "30s"},
	})
	for _, banned := range []string{"proxy_connect_timeout", "proxy_send_timeout", "proxy_read_timeout"} {
		if strings.Contains(got, banned) {
			t.Errorf("static location emitted %s (meaningless without proxy_pass): %q", banned, got)
		}
	}
	if got != "        return 500;" {
		t.Errorf("static location with timeouts = %q, want %q", got, "        return 500;")
	}
}

func TestBuildLocation_StaticKeepsRewrite(t *testing.T) {
	got := buildLocation(contract.Location{
		Path:     "/old",
		Upstream: "hng_static_500",
		Rewrite:  "^/old(.*)$ /$1 break",
	})
	want := "        rewrite ^/old(.*)$ /$1 break;\n        return 500;"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRender_StaticLocationHasNoUpstreamBlock(t *testing.T) {
	// The provider emits NO Configuration.Upstreams entry for a static
	// marker; the rendered output must contain the return line and no
	// upstream block for the marker name.
	c := cfg()
	c.Servers = []*contract.Server{{
		Hostname: "example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{{
			Path:     "/",
			Upstream: "hng_static_500",
		}},
	}}
	got, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "return 500;") {
		t.Errorf("missing return 500;\ngot:\n%s", s)
	}
	if strings.Contains(s, "upstream hng_static_500") {
		t.Errorf("upstream block rendered for static marker\ngot:\n%s", s)
	}
	if strings.Contains(s, "proxy_pass") {
		t.Errorf("proxy_pass rendered for static marker\ngot:\n%s", s)
	}
}

func TestRender_MixedStaticAndProxiedLocations(t *testing.T) {
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
			{Path: "/missing", Upstream: "hng_static_500"},
		},
	}}
	got, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "location /missing {") || !strings.Contains(s, "return 500;") {
		t.Errorf("static location missing\ngot:\n%s", s)
	}
	if !strings.Contains(s, "proxy_pass http://ns_svc_80;") {
		t.Errorf("proxied location missing\ngot:\n%s", s)
	}
	if strings.Contains(s, "upstream hng_static_500") {
		t.Errorf("static marker must not appear as an upstream block\ngot:\n%s", s)
	}
}
