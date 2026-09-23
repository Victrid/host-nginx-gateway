// Unit tests for GEP-722 hostname-precedence fall-through (Gateway API v1,
// GatewaySpec "Listeners that are distinct only by Hostname": exact matches
// MUST be processed before wildcard matches, more-specific wildcards before
// less-specific ones, and the empty hostname last):
//
//   - a request whose Host matches a more-specific route WITHOUT a matching
//     path must fall through to a broader-hostname route that does;
//   - hosts that no route claims must not leak into any route (the socket's
//     default server answers 404);
//   - a hostname-less route attached to a hostname-less listener is the
//     legitimate catch-all and matches every host.
package provider

import (
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// fallThroughGraph: one hostname-less listener; very.specific.com → v1 (/s1),
// *.specific.com → v3 (/s3).
func fallThroughGraph(t *testing.T) *contract.Configuration {
	t.Helper()
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "exact", 1, []gatewayv1.Hostname{"very.specific.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/s1", "v1", 3000)),
		testRoute("default", "wild", 1, []gatewayv1.Hostname{"*.specific.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/s3", "v3", 3000)),
		testSlice("default", "v1", 3000, ptr(true), "10.0.0.1"),
		testSlice("default", "v3", 3000, ptr(true), "10.0.0.3"),
	)
	return g.Configuration()
}

func TestGEP722_ExactHostFallsThroughToWildcardPath(t *testing.T) {
	cfg := fallThroughGraph(t)
	servers := serversByHostname(t, cfg)

	exact := servers["very.specific.com"]
	if exact == nil {
		t.Fatalf("exact-hostname block missing: %+v", cfg.Servers)
	}
	up := upstreamsOf(exact)
	if up["/s1/"] != "default_v1_3000" {
		t.Fatalf("exact route's own /s1 missing: %+v", up)
	}
	// The core fall-through: very.specific.com/s3 is served by the
	// broader-wildcard route because the exact route has no /s3.
	if up["/s3/"] != "default_v3_3000" {
		t.Fatalf("very.specific.com/s3 must fall through to the *.specific.com route: %+v", up)
	}
}

func TestGEP722_WildcardBlockUnchanged(t *testing.T) {
	cfg := fallThroughGraph(t)
	servers := serversByHostname(t, cfg)
	wc := servers["*.specific.com"]
	if wc == nil {
		t.Fatalf("wildcard block missing: %+v", cfg.Servers)
	}
	up := upstreamsOf(wc)
	if up["/s3/"] != "default_v3_3000" || up["= /s3"] != "default_v3_3000" || len(up) != 2 {
		t.Fatalf("wildcard block carries only its own locations: %+v", up)
	}
}

func TestGEP722_UnmatchedHostsDoNotLeak(t *testing.T) {
	cfg := fallThroughGraph(t)
	servers := serversByHostname(t, cfg)
	// Synthetic default block: empty (404), listed first for the socket.
	dflt, ok := servers[""]
	if !ok {
		t.Fatalf("synthetic default block missing: %+v", cfg.Servers)
	}
	if len(dflt.Locations) != 0 {
		t.Fatalf("unmatched hosts must not reach any route location: %+v", dflt.Locations)
	}
	if cfg.Servers[0].Hostname != "" {
		t.Fatalf("default server must be listed first: %+v", cfg.Servers[0])
	}
}

func TestGEP722_CatchAllRouteIsTheDefaultServer(t *testing.T) {
	// A hostname-less route on a hostname-less listener matches ALL hosts
	// per spec (hostnameIntersection: route hostnames nil → inherit the
	// listener's catch-all): its block IS the default server.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "catchall", 1, nil,
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testRoute("default", "named", 1, []gatewayv1.Hostname{"named.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/n", "svc2", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "svc2", 8080, ptr(true), "10.0.0.2"),
	)
	cfg := g.Configuration()
	if cfg.Servers[0].Hostname != "" {
		t.Fatalf("catch-all group must be the default server: %+v", cfg.Servers[0])
	}
	if up := upstreamsOf(cfg.Servers[0]); up["/"] != "default_svc_8080" {
		t.Fatalf("catch-all block: %+v", cfg.Servers[0])
	}
	// The named block carries its own path AND the catch-all's (a request
	// named.com/whatever-unmatched falls through to the catch-all route).
	servers := serversByHostname(t, cfg)
	namedUp := upstreamsOf(servers["named.com"])
	if namedUp["/"] != "default_svc_8080" {
		t.Fatalf("named.com must fall through to the catch-all route: %+v", namedUp)
	}
	if namedUp["/n/"] != "default_svc2_8080" {
		t.Fatalf("named.com own location must win: %+v", namedUp)
	}
}

func TestGEP722_PathConflictPrefersMoreSpecificHostname(t *testing.T) {
	// Both routes match Host=very.specific.com AND path /dup: the exact-
	// hostname route wins (GEP-722: exact before wildcard).
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "exact", 1, []gatewayv1.Hostname{"very.specific.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/dup", "v1", 3000)),
		testRoute("default", "wild", 1, []gatewayv1.Hostname{"*.specific.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/dup", "v3", 3000)),
		testSlice("default", "v1", 3000, ptr(true), "10.0.0.1"),
		testSlice("default", "v3", 3000, ptr(true), "10.0.0.3"),
	)
	servers := serversByHostname(t, g.Configuration())
	up := upstreamsOf(servers["very.specific.com"])
	if up["/dup/"] != "default_v1_3000" || up["= /dup"] != "default_v1_3000" {
		t.Fatalf("exact-hostname route must win the shared path: %+v", up)
	}
}

func TestGEP722_WildcardSpecificityOrder(t *testing.T) {
	// *.a.b.com is more specific than *.b.com; a shared path in the
	// *.a.b.com block must resolve to the more specific route.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "narrow", 1, []gatewayv1.Hostname{"*.a.b.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/p", "narrow", 3000)),
		testRoute("default", "broad", 1, []gatewayv1.Hostname{"*.b.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/p", "broad", 3000)),
		testSlice("default", "narrow", 3000, ptr(true), "10.0.0.1"),
		testSlice("default", "broad", 3000, ptr(true), "10.0.0.2"),
	)
	servers := serversByHostname(t, g.Configuration())
	up := upstreamsOf(servers["*.a.b.com"])
	if up["/p/"] != "default_narrow_3000" {
		t.Fatalf("narrower wildcard must win the shared path: %+v", up)
	}
	// Non-conflicting paths merge both ways within coverage.
	if up := upstreamsOf(servers["*.b.com"]); up["/p/"] != "default_broad_3000" {
		t.Fatalf("broader block keeps its own path: %+v", up)
	}
}

func TestGEP722_DeterministicRender(t *testing.T) {
	// Repeated builds produce byte-identical configs (applied-hash no-op, S6).
	a, b := fallThroughGraph(t), fallThroughGraph(t)
	if a.String() != b.String() {
		t.Fatalf("configuration must be deterministic:\n%s\nvs\n%s", a, b)
	}
}

func TestGEP722_ListenerClaimScopesRouteHostnames(t *testing.T) {
	// Gateway API v1 "General Listener behavior": requests SHOULD match at
	// most one Listener. The exact listener very.specific.com claims only
	// its apex host, so:
	//   - very.specific.com/s3 falls through to the *.specific.com route
	//     (its wildcard covers the listener's apex), and
	//   - foo.specific.com is claimed by NO listener of this gateway →
	//     nothing serves it (no *.specific.com block may exist).
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, host("very.specific.com"))),
		testRoute("default", "exact", 1, []gatewayv1.Hostname{"very.specific.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/s1", "v1", 3000)),
		testRoute("default", "wild", 1, []gatewayv1.Hostname{"*.specific.com"},
			[]gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/s3", "v3", 3000)),
		testSlice("default", "v1", 3000, ptr(true), "10.0.0.1"),
		testSlice("default", "v3", 3000, ptr(true), "10.0.0.3"),
	)
	// Attachment status: the wildcard route intersects the exact listener.
	if pi := parentOf(t, routeOf(t, g, "default", "wild"), "gw"); !pi.Accepted {
		t.Fatalf("wildcard route must attach to the covering exact listener: %+v", pi)
	}
	cfg := g.Configuration()
	servers := serversByHostname(t, cfg)
	if _, exists := servers["*.specific.com"]; exists {
		t.Fatalf("no listener claims *.specific.com hosts — no such block may exist: %+v", cfg.Servers)
	}
	up := upstreamsOf(servers["very.specific.com"])
	if up["/s1/"] != "default_v1_3000" {
		t.Fatalf("own route: %+v", up)
	}
	if up["/s3/"] != "default_v3_3000" {
		t.Fatalf("very.specific.com/s3 must fall through to the wildcard route: %+v", up)
	}
}
