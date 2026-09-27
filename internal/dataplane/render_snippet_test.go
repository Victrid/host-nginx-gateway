// Verbatim snippet rendering tests (DESIGN-multinode-addresses.md §5,
// --dangerously-allow-nginx-snippets): Server.RawServerSnippet is injected
// after the server_name/ssl region and before the locations;
// Location.RawSnippet is appended at the end of the location body. Both
// are rendered BYTE-FOR-BYTE (no re-indentation, no escaping).
package dataplane

import (
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func TestRender_ServerSnippetVerbatim(t *testing.T) {
	c := cfg()
	c.Upstreams = []*contract.Upstream{{Name: "u", Endpoints: []contract.Endpoint{{IP: "10.0.0.1", Port: 80, Ready: true}}}}
	snippet := "    # admin-tuned server tuning\n    sub_filter_types text/css;"
	c.Servers = []*contract.Server{{
		Hostname:         "example.com",
		Listens:          []contract.Listen{{Port: 80}},
		RawServerSnippet: snippet,
		Locations:        []*contract.Location{{Path: "/", Upstream: "u"}},
	}}
	out := render(t, c)
	if !strings.Contains(out, snippet) {
		t.Fatalf("server snippet not verbatim in output:\n%s", out)
	}
	// Position: after server_name, before the first location block.
	iName := strings.Index(out, "server_name example.com;")
	iSnippet := strings.Index(out, snippet)
	iLoc := strings.Index(out, "location / {")
	if !(iName < iSnippet && iSnippet < iLoc) {
		t.Fatalf("server snippet position wrong (server_name@%d, snippet@%d, location@%d):\n%s",
			iName, iSnippet, iLoc, out)
	}
}

func TestRender_ServerSnippetWithTLSAfterCertDirectives(t *testing.T) {
	c := cfg()
	snippet := "    ssl_protocols TLSv1.3;"
	c.Servers = []*contract.Server{{
		Hostname:         "secure.example.com",
		Listens:          []contract.Listen{{Port: 443, SSL: true}},
		TLSCert:          "/certs/ns_tls.pem",
		RawServerSnippet: snippet,
	}}
	out := render(t, c)
	if !strings.Contains(out, snippet) {
		t.Fatalf("snippet missing:\n%s", out)
	}
	if iCert, iSnippet := strings.Index(out, "ssl_certificate_key"), strings.Index(out, snippet); iCert > iSnippet {
		t.Fatalf("snippet must follow the ssl_certificate region:\n%s", out)
	}
}

func TestRender_LocationSnippetVerbatim(t *testing.T) {
	c := cfg()
	c.Upstreams = []*contract.Upstream{{Name: "u", Endpoints: []contract.Endpoint{{IP: "10.0.0.1", Port: 80, Ready: true}}}}
	snippet := "        # per-route lua\n        content_by_lua_file /etc/nginx/conf.d/k8s-gw/files/ns_lua/app.lua;"
	c.Servers = []*contract.Server{{
		Hostname: "example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{
			{Path: "/", Upstream: "u"},
			{Path: "/lua", Upstream: "u", RawSnippet: snippet},
		},
	}}
	out := render(t, c)
	if !strings.Contains(out, snippet) {
		t.Fatalf("location snippet not verbatim:\n%s", out)
	}
	// The snippet must be INSIDE the /lua location: after its opening
	// line and before the generated directives of the NEXT location (the
	// template emits locations sorted, "/" before "/lua"; the closing
	// brace of /lua precedes any further content).
	iLoc := strings.Index(out, "location /lua {")
	iSnippet := strings.Index(out, snippet)
	if iLoc < 0 || iSnippet < iLoc {
		t.Fatalf("snippet must follow its location opening:\n%s", out)
	}
	tail := out[iSnippet+len(snippet):]
	if !strings.HasPrefix(tail, "\n    }") {
		t.Fatalf("snippet must be the last content of the location block, tail = %q:\n%s", tail, out)
	}
}

func TestRender_SnippetsOnStaticAndRedirectLocations(t *testing.T) {
	// The injection is uniform: snippet-bearing locations that answer a
	// static return / redirect still carry their snippet (nginx parses it;
	// `nginx -t` remains the safety net).
	c := cfg()
	snippet := "        # even here"
	c.Servers = []*contract.Server{{
		Hostname: "example.com",
		Listens:  []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{
			{Path: "/static", Upstream: "hng_static_500", RawSnippet: snippet},
		},
	}}
	out := render(t, c)
	if !strings.Contains(out, snippet) {
		t.Fatalf("snippet missing on static location:\n%s", out)
	}
}

func TestRender_NoSnippetFieldsNoChange(t *testing.T) {
	c := cfg()
	c.Servers = []*contract.Server{{
		Hostname:  "example.com",
		Listens:   []contract.Listen{{Port: 80}},
		Locations: []*contract.Location{{Path: "/", Upstream: "hng_static_500"}},
	}}
	out := render(t, c)
	for _, banned := range []string{"RawServerSnippet", "RawSnippet"} {
		if strings.Contains(out, banned) {
			t.Fatalf("empty snippet leaked into output:\n%s", out)
		}
	}
}
