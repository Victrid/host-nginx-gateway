// Rendered-output tests for the round-5 contract extensions: map blocks,
// weighted upstream servers, redirects, guarded redirects and header
// modification directives. The rendered nginx config must be syntactically
// valid for `nginx -t` (locked via the fake-nginx validator in the e2e
// lane; here we assert the exact directive shapes).
package dataplane

import (
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func render(t *testing.T, cfg *contract.Configuration) string {
	t.Helper()
	b, err := Render(cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(b)
}

func TestRender_MapBlocksAndDispatchLocation(t *testing.T) {
	cfg := &contract.Configuration{
		Maps: []*contract.MapBlock{{
			Name:   "hng_sel_m_001",
			Source: "$request_method",
			Entries: []contract.MapEntry{
				{Key: "GET", Value: "ns_svc_80"},
				{Key: "~^POST$", Value: "$hng_sel_m_002"},
				{Key: "", Value: ""},
			},
		}},
		Servers: []*contract.Server{{
			Hostname: "example.com",
			Listens:  []contract.Listen{{Port: 80}},
			Locations: []*contract.Location{{
				Path:     "/m/",
				Upstream: "$hng_sel_m_001",
			}},
		}},
	}
	out := render(t, cfg)
	for _, want := range []string{
		`map $request_method $hng_sel_m_001 {`,
		`"GET" "ns_svc_80";`,
		`~"^POST$" "$hng_sel_m_002";`,
		`"" "";`,
		`if ($hng_sel_m_001 = "") { return 404; }`,
		`proxy_pass http://$hng_sel_m_001;`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, out)
		}
	}
	// Maps are emitted at http level, before servers.
	if strings.Index(out, "map $request_method") > strings.Index(out, "server {") {
		t.Fatalf("map blocks must precede server blocks:\n%s", out)
	}
}

func TestRender_WeightedUpstreamServers(t *testing.T) {
	cfg := &contract.Configuration{
		Upstreams: []*contract.Upstream{{
			Name: "hng_wr_abc",
			Endpoints: []contract.Endpoint{
				{IP: "10.0.0.1", Port: 8080, Ready: true, Weight: 90},
				{IP: "10.0.0.2", Port: 8080, Ready: true, Weight: 10},
				{IP: "10.0.0.3", Port: 8080, Ready: false},
			},
		}},
	}
	out := render(t, cfg)
	if !strings.Contains(out, "server 10.0.0.1:8080 weight=90;") {
		t.Fatalf("weighted server line missing:\n%s", out)
	}
	if !strings.Contains(out, "server 10.0.0.2:8080 weight=10;") {
		t.Fatalf("second weighted server missing:\n%s", out)
	}
	if !strings.Contains(out, "server 10.0.0.3:8080 down;") {
		t.Fatalf("unweighted down server must carry no weight:\n%s", out)
	}
}

func TestRender_RedirectLocation(t *testing.T) {
	cfg := &contract.Configuration{
		Servers: []*contract.Server{{
			Hostname:  "example.com",
			Listens:   []contract.Listen{{Port: 80}},
			Locations: []*contract.Location{{Path: "= /old", Redirect: &contract.Redirect{Code: 308, URL: "https://new.example.com:8443$request_uri"}}},
		}},
	}
	out := render(t, cfg)
	if !strings.Contains(out, "return 308 https://new.example.com:8443$request_uri;") {
		t.Fatalf("redirect directive missing:\n%s", out)
	}
	if strings.Contains(out, "proxy_pass") {
		t.Fatalf("redirect location must not proxy:\n%s", out)
	}
}

func TestRender_GuardedRedirect(t *testing.T) {
	cfg := &contract.Configuration{
		Servers: []*contract.Server{{
			Hostname: "example.com",
			Listens:  []contract.Listen{{Port: 80}},
			Locations: []*contract.Location{{
				Path: "/cardamom/",
				RedirectIf: &contract.RedirectIf{
					Match: `^/cardamom/(?<hng_r>.*)$`,
					Code:  302,
					URL:   "https://rd.example.com/fennel/$hng_r$is_args$args",
				},
			}},
		}},
	}
	out := render(t, cfg)
	for _, want := range []string{
		`if ($uri ~ "^/cardamom/(?<hng_r>.*)$") {`,
		`return 302 https://rd.example.com/fennel/$hng_r$is_args$args;`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("guarded redirect missing %q:\n%s", want, out)
		}
	}
}

func TestRender_HeaderModificationDirectives(t *testing.T) {
	cfg := &contract.Configuration{
		Upstreams: []*contract.Upstream{{Name: "ns_svc_80", Endpoints: []contract.Endpoint{{IP: "10.0.0.1", Port: 80, Ready: true}}}},
		Servers: []*contract.Server{{
			Hostname: "example.com",
			Listens:  []contract.Listen{{Port: 80}},
			Locations: []*contract.Location{{
				Path:            "/",
				Upstream:        "ns_svc_80",
				ProxyHost:       "rewritten.example.com",
				RequestHeaders:  []contract.Header{{Name: "X-Set", Value: "one"}, {Name: "X-Drop", Value: ""}},
				HideHeaders:     []string{"Server"},
				ResponseHeaders: []contract.Header{{Name: "X-Extra", Value: "added"}},
			}},
		}},
	}
	out := render(t, cfg)
	for _, want := range []string{
		`proxy_set_header Host "rewritten.example.com";`,
		`proxy_set_header "X-Set" "one";`,
		`proxy_set_header "X-Drop" "";`,
		`proxy_hide_header "Server";`,
		`add_header "X-Extra" "added" always;`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, out)
		}
	}
}

func TestRender_RewriteDirective(t *testing.T) {
	cfg := &contract.Configuration{
		Upstreams: []*contract.Upstream{{Name: "ns_svc_80"}},
		Servers: []*contract.Server{{
			Hostname: "example.com",
			Listens:  []contract.Listen{{Port: 80}},
			Locations: []*contract.Location{{
				Path:     "/svc/",
				Upstream: "ns_svc_80",
				Rewrite:  `^/svc/(?<hng_r>.*)$ /new/$hng_r break`,
			}},
		}},
	}
	out := render(t, cfg)
	if !strings.Contains(out, `rewrite ^/svc/(?<hng_r>.*)$ /new/$hng_r break;`) {
		t.Fatalf("rewrite directive missing:\n%s", out)
	}
}

func TestQuoteNginx(t *testing.T) {
	cases := map[string]string{
		`plain`:        `"plain"`,
		`with "quote"`: `"with \"quote\""`,
		`back\slash`:   `"back\\slash"`,
	}
	for in, want := range cases {
		if got := quoteNginx(in); got != want {
			t.Fatalf("quoteNginx(%q) = %q, want %q", in, got, want)
		}
	}
}
