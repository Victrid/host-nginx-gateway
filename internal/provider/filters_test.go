// Unit tests for the B-grade core features (DESIGN.md §3.3, Gateway API v1):
// backendRef weights, the RequestRedirect / URLRewrite filters and the
// request/response header modifiers.
package provider

import (
	"strconv"
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func filterRoute(name string, rules ...gatewayv1.HTTPRouteRule) *gatewayv1.HTTPRoute {
	return testRoute("default", name, 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, rules...)
}

func TestWeights_MultipleBackendsProduceWeightedUpstream(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/w")},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "a", Port: ptr(gatewayv1.PortNumber(8080))}, Weight: ptr(int32(3))}},
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "b", Port: ptr(gatewayv1.PortNumber(8080))}, Weight: ptr(int32(7))}},
		},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("w", rule),
		testSlice("default", "a", 8080, ptr(true), "10.1.0.1"),
		testSlice("default", "b", 8080, ptr(true), "10.2.0.1"),
	)
	cfg := g.Configuration()
	if len(cfg.Upstreams) != 1 {
		t.Fatalf("weighted rule must produce ONE combined upstream, got %+v", cfg.Upstreams)
	}
	up := cfg.Upstreams[0]
	if !strings.HasPrefix(up.Name, "hng_wr_") {
		t.Fatalf("rule-private upstream name: %+v", up)
	}
	if len(up.Endpoints) != 2 {
		t.Fatalf("both backends' endpoints expected: %+v", up.Endpoints)
	}
	weights := map[string]int{}
	for _, ep := range up.Endpoints {
		weights[ep.IP] = ep.Weight
	}
	if weights["10.1.0.1"] != 3 || weights["10.2.0.1"] != 7 {
		t.Fatalf("per-endpoint weights must mirror backendRef weights: %+v", up.Endpoints)
	}
	loc := cfg.Servers[0].Locations[0]
	if loc.Upstream != up.Name {
		t.Fatalf("location must point at the weighted upstream: %+v", loc)
	}
}

func TestWeights_SingleBackendKeepsServiceUpstream(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("w", pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	if len(cfg.Upstreams) != 1 || cfg.Upstreams[0].Name != "default_svc_8080" {
		t.Fatalf("single backend keeps the deterministic service upstream: %+v", cfg.Upstreams)
	}
}

func TestWeights_ZeroWeightBackendReceivesNoServers(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "a", Port: ptr(gatewayv1.PortNumber(8080))}}},
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "b", Port: ptr(gatewayv1.PortNumber(8080))}, Weight: ptr(int32(0))}},
		},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("w", rule),
		testSlice("default", "a", 8080, ptr(true), "10.1.0.1"),
		testSlice("default", "b", 8080, ptr(true), "10.2.0.1"),
	)
	cfg := g.Configuration()
	if len(cfg.Upstreams) != 1 || len(cfg.Upstreams[0].Endpoints) != 1 {
		t.Fatalf("zero-weight backend must be excluded: %+v", cfg.Upstreams)
	}
}

func TestRedirect_SchemeHostPortAndStatusCode(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/redir")},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Scheme:     ptr("https"),
				Hostname:   ptr(gatewayv1.PreciseHostname("redirect.example.com")),
				Port:       ptr(gatewayv1.PortNumber(8443)),
				StatusCode: ptr(308),
			},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("r", rule),
	)
	loc := g.Configuration().Servers[0].Locations[0]
	if loc.Redirect == nil {
		t.Fatalf("redirect location expected: %+v", loc)
	}
	if loc.Redirect.Code != 308 {
		t.Fatalf("status code: %+v", loc.Redirect)
	}
	// 8443 is not https' default → explicit port.
	want := "https://redirect.example.com:8443$request_uri"
	if loc.Redirect.URL != want {
		t.Fatalf("redirect URL = %q, want %q", loc.Redirect.URL, want)
	}
	if loc.Upstream != "" {
		t.Fatalf("redirect location must not proxy: %+v", loc)
	}
}

func TestRedirect_DefaultPortOmittedAndDefaultCode301(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{}, // no backends: redirect-only rule
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Scheme: ptr("https"),
				Port:   ptr(gatewayv1.PortNumber(443)),
			},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("r", rule),
	)
	loc := g.Configuration().Servers[0].Locations[0]
	if loc.Redirect == nil || loc.Redirect.Code != 301 {
		t.Fatalf("default redirect: %+v", loc)
	}
	if strings.Contains(loc.Redirect.URL, ":443") {
		t.Fatalf("default port for scheme must be omitted: %q", loc.Redirect.URL)
	}
}

func TestRedirect_ReplaceFullPath(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Hostname: ptr(gatewayv1.PreciseHostname("rd.example.com")),
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: ptr("/newpath"),
				},
			},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("r", rule),
	)
	loc := g.Configuration().Servers[0].Locations[0]
	// Scheme unset → preserved via $scheme.
	if loc.Redirect == nil || loc.Redirect.URL != "$scheme://rd.example.com/newpath$is_args$args" {
		t.Fatalf("replaceFullPath redirect: %+v", loc)
	}
}

func TestRedirect_ReplacePrefixMatchPreservesRemainder(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/cardamom")},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Hostname: ptr(gatewayv1.PreciseHostname("rd.example.com")),
				Path: &gatewayv1.HTTPPathModifier{
					Type:               gatewayv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: ptr("/fennel"),
				},
			},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("r", rule),
	)
	var exact, prefix *contract.Location
	for _, l := range g.Configuration().Servers[0].Locations {
		if l.Path == "= /cardamom" {
			exact = l
		}
		if l.Path == "/cardamom/" {
			prefix = l
		}
	}
	if exact == nil || exact.Redirect == nil || exact.Redirect.URL != "$scheme://rd.example.com/fennel$is_args$args" {
		t.Fatalf("exact twin redirect: %+v", exact)
	}
	if prefix == nil || prefix.RedirectIf == nil {
		t.Fatalf("prefix location must carry the guarded redirect: %+v", prefix)
	}
	if prefix.RedirectIf.Match != `^/cardamom/(?<hng_r>.*)$` {
		t.Fatalf("guard regex: %+v", prefix.RedirectIf)
	}
	if prefix.RedirectIf.URL != "$scheme://rd.example.com/fennel/$hng_r$is_args$args" {
		t.Fatalf("guarded redirect URL: %+v", prefix.RedirectIf)
	}
}

func TestURLRewrite_PathAndHostname(t *testing.T) {
	rewrite := func(mod *gatewayv1.HTTPPathModifier, host *gatewayv1.PreciseHostname) gatewayv1.HTTPRouteRule {
		return gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/svc")},
			}},
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type:       gatewayv1.HTTPRouteFilterURLRewrite,
				URLRewrite: &gatewayv1.HTTPURLRewriteFilter{Hostname: host, Path: mod},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr(gatewayv1.PortNumber(8080))}},
			}},
		}
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("full", rewrite(&gatewayv1.HTTPPathModifier{
			Type:            gatewayv1.FullPathHTTPPathModifier,
			ReplaceFullPath: ptr("/replaced"),
		}, ptr(gatewayv1.PreciseHostname("rewritten.example.com")))),
		func() *gatewayv1.HTTPRoute {
			r := rewrite(&gatewayv1.HTTPPathModifier{
				Type:               gatewayv1.PrefixMatchHTTPPathModifier,
				ReplacePrefixMatch: ptr("/new"),
			}, nil)
			r.Matches[0].Path.Value = ptr("/other")
			return filterRoute("prefix", r)
		}(),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	servers := map[string]*contract.Server{}
	for _, s := range g.Configuration().Servers {
		if s.Hostname == "" {
			servers[s.Locations[0].Rewrite] = s
		}
	}
	var full, prefix *contract.Location
	for _, s := range g.Configuration().Servers {
		for _, l := range s.Locations {
			switch {
			case strings.HasPrefix(l.Rewrite, "^ /replaced"):
				full = l
			case strings.HasPrefix(l.Rewrite, "^/other/(?<hng_r>.*)$"):
				prefix = l
			}
		}
	}
	if full == nil || full.ProxyHost != "rewritten.example.com" {
		t.Fatalf("replaceFullPath rewrite + hostname: %+v", full)
	}
	if prefix == nil || prefix.Rewrite != "^/other/(?<hng_r>.*)$ /new/$hng_r break" {
		t.Fatalf("replacePrefixMatch rewrite: %+v", prefix)
	}
	// Rewrites must use break so the proxy forwards the rewritten URI.
	if !strings.Contains(full.Rewrite, "break") {
		t.Fatalf("rewrite must use break so the proxy uses the rewritten URI")
	}
}

func TestRequestHeaderModifier_SetAddRemove(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/hdr")},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set:    []gatewayv1.HTTPHeader{{Name: "X-Set", Value: "one"}},
				Add:    []gatewayv1.HTTPHeader{{Name: "X-Add", Value: "two"}},
				Remove: []string{"X-Drop"},
			},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr(gatewayv1.PortNumber(8080))}},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("h", rule),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	loc := cfg.Servers[0].Locations[0]
	want := []contract.Header{
		{Name: "X-Set", Value: "one"},
		{Name: "X-Add", Value: "$hng_app_x_add_" + mapValHash("two")},
		{Name: "X-Drop", Value: ""}, // remove → empty value (nginx suppression)
	}
	if len(loc.RequestHeaders) != len(want) {
		t.Fatalf("request headers: %+v", loc.RequestHeaders)
	}
	for i, h := range want {
		if h.Name != loc.RequestHeaders[i].Name {
			t.Fatalf("request header %d = %+v, want %+v", i, loc.RequestHeaders[i], h)
		}
		if h.Value == "" || !strings.HasPrefix(h.Value, "$hng_app") {
			if loc.RequestHeaders[i].Value != h.Value {
				t.Fatalf("request header %d = %+v, want %+v", i, loc.RequestHeaders[i], h)
			}
		}
	}
	// The add-entry's map appends to the inbound value.
	var found bool
	for _, m := range cfg.Maps {
		if m.Source == "$http_x_add" {
			found = true
			if m.Entries[0].Key != "" || m.Entries[0].Value != "two" {
				t.Fatalf("absent branch: %+v", m.Entries)
			}
			if m.Entries[1].Key != "default" || m.Entries[1].Value != "$http_x_add,two" {
				t.Fatalf("append branch: %+v", m.Entries)
			}
		}
	}
	if !found {
		t.Fatalf("append map missing: %+v", cfg.Maps)
	}
}

// mapValHash mirrors the dispatchBuilder's deterministic append-map suffix.
func mapValHash(v string) string {
	return strconv.FormatUint(uint64(fnv32a(v)), 16)
}

func TestResponseHeaderModifier_AddSetRemove(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/resp")},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
			ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set:    []gatewayv1.HTTPHeader{{Name: "X-Resp", Value: "set"}},
				Add:    []gatewayv1.HTTPHeader{{Name: "X-Extra", Value: "added"}},
				Remove: []string{"Server"},
			},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr(gatewayv1.PortNumber(8080))}},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("h", rule),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	loc := g.Configuration().Servers[0].Locations[0]
	// set = suppress the backend's value first, then add ours.
	var found bool
	for _, h := range loc.HideHeaders {
		if h == "X-Resp" || h == "Server" {
			found = true
		}
	}
	if !found {
		t.Fatalf("set/remove must hide original response headers: %+v", loc)
	}
	if len(loc.ResponseHeaders) != 2 {
		t.Fatalf("response headers: %+v", loc.ResponseHeaders)
	}
}

func TestFilter_UnsupportedCombinationsDropRules(t *testing.T) {
	dupRedirect := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{},
		Filters: []gatewayv1.HTTPRouteFilter{
			{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{}},
			{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{}},
		},
	}
	mirror := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr(gatewayv1.PortNumber(8080))}},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestMirror}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("r", dupRedirect, mirror),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	rp := routeOf(t, g, "default", "r")
	pi := parentOf(t, rp, "gw")
	if pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonUnsupportedValue) {
		t.Fatalf("a route whose every rule is unimplementable is rejected: %+v", pi)
	}
	if rp.partiallyInvalid() {
		t.Fatalf("PartiallyInvalid must not be set for fully invalid routes (GEP-1748)")
	}
	if len(g.Configuration().Servers[0].Locations) != 0 {
		t.Fatalf("both rules were dropped: %+v", g.Configuration().Servers[0].Locations)
	}
}
