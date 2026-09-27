// Unit tests for per-route hostname dispatch (Gateway API v1, HTTPRoute
// spec.hostnames):
//
//   - attachment requires a non-empty intersection between the route
//     hostnames and the listener hostname (exact, or wildcard/subdomain per
//     the Hostname type's matching rules);
//   - a route without hostnames inherits the listener hostname;
//   - dispatch in the rendered IR: one contract.Server per distinct
//     effective hostname, so requests reach only the locations of the route
//     whose hostname matches the request Host.
package provider

import (
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/dataplane"
)

// serversByHostname indexes Configuration.Servers by server_name value.
func serversByHostname(t *testing.T, cfg *contract.Configuration) map[string]*contract.Server {
	t.Helper()
	out := map[string]*contract.Server{}
	for _, s := range cfg.Servers {
		if _, dup := out[s.Hostname]; dup {
			t.Fatalf("duplicate server for hostname %q", s.Hostname)
		}
		out[s.Hostname] = s
	}
	return out
}

// upstreamsOf maps location path → upstream for a server block.
func upstreamsOf(s *contract.Server) map[string]string {
	out := map[string]string{}
	for _, l := range s.Locations {
		out[l.Path] = l.Upstream
	}
	return out
}

func TestHostnameDispatch_ExactIntersection(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, host("foo.com"))),
		testRoute("default", "a", 1, []gatewayv1.Hostname{"foo.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/a", "svca", 8080)),
		testRoute("default", "b", 1, []gatewayv1.Hostname{"foo.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/b", "svcb", 8080)),
		testSlice("default", "svca", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "svcb", 8080, ptr(true), "10.0.0.2"),
	)
	cfg := g.Configuration()
	servers := serversByHostname(t, cfg)
	srv, ok := servers["foo.com"]
	if !ok {
		t.Fatalf("expected a foo.com server, got %+v", cfg.Servers)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("one effective hostname → exactly one named block (no synthetic default), got %+v", cfg.Servers)
	}
	upstreams := upstreamsOf(srv)
	if upstreams["/a/"] != "default_svca_8080" || upstreams["= /a"] != "default_svca_8080" {
		t.Fatalf("route a locations missing: %+v", upstreams)
	}
	if upstreams["/b/"] != "default_svcb_8080" {
		t.Fatalf("route b locations missing: %+v", upstreams)
	}
}

func TestHostnameDispatch_RouteWithoutHostnamesInheritsListener(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, host("foo.com"))),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	if len(cfg.Servers) != 1 {
		t.Fatalf("exactly one named block expected (no synthetic default): %+v", cfg.Servers)
	}
	servers := serversByHostname(t, cfg)
	if up := upstreamsOf(servers["foo.com"]); up["/"] != "default_svc_8080" {
		t.Fatalf("hostname-less route must land in the listener-hostname server: %+v", cfg.Servers)
	}
	if _, has := servers[""]; has {
		t.Fatalf("no synthetic default block may be emitted: %+v", cfg.Servers)
	}
}

func TestHostnameDispatch_CatchAllListener(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	if len(cfg.Servers) != 1 || cfg.Servers[0].Hostname != "" {
		t.Fatalf("nil listener + nil route hostnames → single catch-all server: %+v", cfg.Servers)
	}
}

func TestHostnameDispatch_DistinctHostnames(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "a", 1, []gatewayv1.Hostname{"a.example.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/x", "svca", 8080)),
		testRoute("default", "b", 1, []gatewayv1.Hostname{"b.example.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/x", "svcb", 8080)),
		testSlice("default", "svca", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "svcb", 8080, ptr(true), "10.0.0.2"),
	)
	cfg := g.Configuration()
	if len(cfg.Servers) != 2 {
		t.Fatalf("two effective hostnames → exactly two named blocks (no synthetic default), got %+v", cfg.Servers)
	}
	servers := serversByHostname(t, cfg)
	// The SAME path under DIFFERENT hostnames must NOT be dropped: dispatch
	// is per Host, the duplicate-path first-wins rule is per hostname group.
	if up := upstreamsOf(servers["a.example.com"]); up["/x/"] != "default_svca_8080" || up["= /x"] != "default_svca_8080" {
		t.Fatalf("a.example.com locations: %+v", up)
	}
	if up := upstreamsOf(servers["b.example.com"]); up["/x/"] != "default_svcb_8080" || up["= /x"] != "default_svcb_8080" {
		t.Fatalf("b.example.com locations: %+v", up)
	}
	// Both blocks listen on the same socket so nginx dispatches on Host.
	for name, s := range servers {
		if len(s.Listens) != 1 || s.Listens[0].Port != 80 {
			t.Fatalf("%s listens: %+v", name, s.Listens)
		}
	}
}

func TestHostnameDispatch_WildcardListenerAndHostnamelessRoute(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, host("*.foo.com"))),
		testRoute("default", "a", 1, []gatewayv1.Hostname{"a.foo.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/x", "svca", 8080)),
		testRoute("default", "b", 1, []gatewayv1.Hostname{"b.foo.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/x", "svcb", 8080)),
		testRoute("default", "c", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/y", "svcc", 8080)),
		testSlice("default", "svca", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "svcb", 8080, ptr(true), "10.0.0.2"),
		testSlice("default", "svcc", 8080, ptr(true), "10.0.0.3"),
	)
	cfg := g.Configuration()
	if len(cfg.Servers) != 3 {
		t.Fatalf("three effective hostnames → exactly three named blocks (no synthetic default), got %+v", cfg.Servers)
	}
	servers := serversByHostname(t, cfg)
	// GEP-722 fall-through: the exact subdomain blocks carry their own /x
	// AND the wildcard route's /y (a request a.foo.com/x hits the more
	// specific block; a.foo.com/y falls through to the wildcard route).
	if up := upstreamsOf(servers["a.foo.com"]); up["/x/"] != "default_svca_8080" || up["/y/"] != "default_svcc_8080" {
		t.Fatalf("a.foo.com must carry own /x and merged /y: %+v", up)
	}
	if up := upstreamsOf(servers["b.foo.com"]); up["/x/"] != "default_svcb_8080" || up["/y/"] != "default_svcc_8080" {
		t.Fatalf("b.foo.com must carry own /x and merged /y: %+v", up)
	}
	// The hostname-less route inherits the LISTENER hostname (wildcard) —
	// its locations live only in the wildcard block.
	wc := servers["*.foo.com"]
	if wc == nil {
		t.Fatalf("wildcard block missing: %+v", cfg.Servers)
	}
	if up := upstreamsOf(wc); up["/y/"] != "default_svcc_8080" {
		t.Fatalf("*.foo.com must carry the inherited route: %+v", up)
	}
	if _, leak := upstreamsOf(wc)["/x/"]; leak {
		t.Fatalf("wildcard block must not carry subdomain-route locations: %+v", wc.Locations)
	}
}

func TestHostnameDispatch_MultiHostnameRouteServesBoth(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, []gatewayv1.Hostname{"a.com", "b.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/m", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	servers := serversByHostname(t, cfg)
	for _, h := range []string{"a.com", "b.com"} {
		srv, ok := servers[h]
		if !ok {
			t.Fatalf("multi-hostname route must appear under %q: %+v", h, cfg.Servers)
		}
		if up := upstreamsOf(srv); up["/m/"] != "default_svc_8080" {
			t.Fatalf("%s: %+v", h, up)
		}
	}
}

func TestHostnameDispatch_RouteWildcardUnderExactListener(t *testing.T) {
	// Gateway API v1 spec: a listener test.example.com matches routes with
	// the identical hostname OR the covering wildcard (*.example.com). The
	// route's own wildcard is the effective dispatch hostname, so requests
	// for any subdomain reach this route while the apex reaches only
	// routes/exact matches claiming it.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, host("foo.com"))),
		testRoute("default", "wild", 1, []gatewayv1.Hostname{"*.foo.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svcwild", 8080)),
		testSlice("default", "svcwild", 8080, ptr(true), "10.0.0.9"),
	)
	pi := parentOf(t, routeOf(t, g, "default", "wild"), "gw")
	if !pi.Accepted {
		t.Fatalf("route wildcard *.foo.com must attach to exact listener foo.com: %s", pi.Reason)
	}
	cfg := g.Configuration()
	// The route ATTACHES (status), but its wildcard claim shares no host
	// with the exact listener's apex claim (*.foo.com does not match
	// foo.com): the attachment serves no requests and emits no block.
	if len(cfg.Servers) != 0 {
		t.Fatalf("attachment with empty common claim must not emit blocks: %+v", cfg.Servers)
	}
}

func TestHostnameDispatch_NoIntersectionRejected(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, host("foo.com"))),
		testRoute("default", "r", 1, []gatewayv1.Hostname{"bar.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	pi := parentOf(t, routeOf(t, g, "default", "r"), "gw")
	if pi.Accepted {
		t.Fatal("route with disjoint hostname must not attach")
	}
	if pi.Reason != string(gatewayv1.RouteReasonNoMatchingListenerHostname) {
		t.Fatalf("reason = %q, want NoMatchingListenerHostname (standard reason)", pi.Reason)
	}
	if cond := condByType(routeOf(t, g, "default", "r").ParentStatuses()[0].Conditions, "Accepted"); cond.Reason != string(gatewayv1.RouteReasonNoMatchingListenerHostname) {
		t.Fatalf("status reason = %q", cond.Reason)
	}
	if len(g.Gateways[0].Listeners[0].Attachments) != 0 {
		t.Fatal("no attachment expected")
	}
}

func TestHostnameIntersection_SpecSemantics(t *testing.T) {
	// Expectations follow the Gateway API v1 HTTPRoute spec.hostnames doc:
	// a wildcard is a SUFFIX match (*.example.com matches test.example.com
	// AND foo.test.example.com, not the apex); a listener with an exact
	// hostname also matches the route wildcard that covers it
	// (*.example.com); non-matching route hostnames are ignored.
	listener := host("*.foo.com")
	cases := []struct {
		name    string
		route   string
		wantLen int
	}{
		{"identical wildcards", "*.foo.com", 1},
		{"single-label subdomain", "a.foo.com", 1},
		{"multi-label subdomain matches the wildcard suffix", "x.y.foo.com", 1},
		{"apex is NOT matched by its own wildcard", "foo.com", 0},
		{"disjoint domain", "bar.com", 0},
		{"sub-wildcard covered by the listener wildcard", "*.sub.foo.com", 1},
		{"sibling wildcard not covered", "*.bar.com", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hostnameIntersection([]gatewayv1.Hostname{gatewayv1.Hostname(tc.route)}, listener)
			if len(got) != tc.wantLen {
				t.Fatalf("intersection(%q, %q) = %v, want %d entries", tc.route, *listener, got, tc.wantLen)
			}
		})
	}
	// Exact listener: identical route hostname intersects; the route
	// wildcard covering the listener hostname intersects too (spec: a
	// listener test.example.com matches route hostnames test.example.com or
	// *.example.com — including subdomain listeners and narrower wildcard
	// listeners).
	if got := hostnameIntersection([]gatewayv1.Hostname{"foo.com"}, host("foo.com")); len(got) != 1 {
		t.Fatalf("exact intersection: %v", got)
	}
	if got := hostnameIntersection([]gatewayv1.Hostname{"*.foo.com"}, host("foo.com")); len(got) != 1 {
		t.Fatalf("route wildcard *.foo.com must intersect exact listener foo.com: %v", got)
	}
	if got := hostnameIntersection([]gatewayv1.Hostname{"*.foo.com"}, host("a.foo.com")); len(got) != 1 {
		t.Fatalf("route wildcard *.foo.com must intersect subdomain listener a.foo.com: %v", got)
	}
	if got := hostnameIntersection([]gatewayv1.Hostname{"*.foo.com"}, host("*.a.foo.com")); len(got) != 1 {
		t.Fatalf("route wildcard *.foo.com must intersect narrower wildcard listener *.a.foo.com: %v", got)
	}
	if got := hostnameIntersection([]gatewayv1.Hostname{"*.a.foo.com"}, host("*.foo.com")); len(got) != 1 {
		t.Fatalf("narrower route wildcard must intersect broader wildcard listener: %v", got)
	}
	if got := hostnameIntersection([]gatewayv1.Hostname{"*.bar.com"}, host("foo.com")); got != nil {
		t.Fatalf("unrelated route wildcard must not intersect: %v", got)
	}
	if got := hostnameIntersection([]gatewayv1.Hostname{"*.bar.com"}, host("*.foo.com")); got != nil {
		t.Fatalf("unrelated route wildcard must not intersect wildcard listener: %v", got)
	}
	if got := hostnameIntersection([]gatewayv1.Hostname{"example.com"}, host("*.foo.com")); got != nil {
		t.Fatalf("exact route hostname outside wildcard listener must not intersect: %v", got)
	}
	// Catch-all listener: every route hostname is kept as-is.
	if got := hostnameIntersection([]gatewayv1.Hostname{"a.com", "b.com"}, nil); len(got) != 2 {
		t.Fatalf("catch-all listener keeps route hostnames: %v", got)
	}
}

func TestHostnameDispatch_DeterministicAndRenderable(t *testing.T) {
	objs := []any{
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "a", 1, []gatewayv1.Hostname{"b.com", "a.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	}
	a := build(t, objs...).Configuration()
	b := build(t, objs...).Configuration()
	if a.String() != b.String() {
		t.Fatalf("configuration must be deterministic:\n%s\nvs\n%s", a, b)
	}
	// Named hostname groups sort lexically and deterministically; no
	// synthetic default block exists (the host nginx.conf owns default
	// servers).
	if len(a.Servers) != 2 || a.Servers[0].Hostname != "a.com" || a.Servers[1].Hostname != "b.com" {
		t.Fatalf("expected deterministic a.com/b.com blocks: %+v", a.Servers)
	}
	rendered, err := dataplane.Render(a)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"server_name a.com;", "server_name b.com;"} {
		if !strings.Contains(string(rendered), want) {
			t.Fatalf("rendered config missing %q:\n%s", want, rendered)
		}
	}
	// Host header preservation (Gateway API v1: the Host header "MUST be
	// forwarded unmodified to the backend") — every proxying location
	// carries proxy_set_header Host $http_host.
	if !strings.Contains(string(rendered), "proxy_set_header Host $http_host;") {
		t.Fatalf("rendered config must preserve the client Host header:\n%s", rendered)
	}
}

func TestConfiguration_NoBlocksBeyondRouteBackedGroups(t *testing.T) {
	// The controller never injects a synthetic default-server block: every
	// emitted server block must be backed by a route claim (an effective
	// hostname of some attachment, or the listener hostname a
	// hostname-less route inherits). Unmatched hosts are the host
	// administrator's business (their nginx.conf declares default
	// servers).
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1,
			plainListener("web", 80, host("foo.com")),
			plainListener("secure", 443, host("*.bar.com")),
		),
		testRoute("default", "a", 1, []gatewayv1.Hostname{"foo.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/a", "svca", 8080)),
		testRoute("default", "b", 1, []gatewayv1.Hostname{"*.bar.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/b", "svcb", 8080)),
		testSlice("default", "svca", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "svcb", 8080, ptr(true), "10.0.0.2"),
	)
	cfg := g.Configuration()
	want := map[string]bool{"foo.com": true, "*.bar.com": true}
	if len(cfg.Servers) != len(want) {
		t.Fatalf("route-backed blocks only: want hostnames %v, got %+v", want, cfg.Servers)
	}
	for _, s := range cfg.Servers {
		if !want[s.Hostname] {
			t.Fatalf("block %q is not backed by any route claim (synthetic default?): %+v", s.Hostname, s)
		}
	}
}
