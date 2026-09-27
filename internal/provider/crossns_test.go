// Cross-namespace HTTPRoute attachment (Gateway API v1): a route MAY attach
// to a Gateway in another namespace when the TARGET Gateway's (per-listener)
// allowedRoutes.namespaces policy admits the route's namespace. allowedRoutes
// IS the authorization — ReferenceGrant plays NO part in Gateway-route
// attachment (§3.5 keeps grants for backendRefs / certificateRefs only).
// BackendRefs keep defaulting to the ROUTE's namespace regardless of where
// the Gateway lives.
package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func allowedRoutesFrom(f gatewayv1.FromNamespaces) *gatewayv1.AllowedRoutes {
	return &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: ptr(f)}}
}

func allowedRoutesSelector(sel metav1.LabelSelector) *gatewayv1.AllowedRoutes {
	return &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
		From:     ptr(gatewayv1.NamespacesFromSelector),
		Selector: &sel,
	}}
}

// crossNSFixture: GatewayClass "c", Gateway gw in ns "infra", route in ns
// "apps" pointing at it via an explicit parentRef namespace.
func crossNSFixture(t *testing.T, l gatewayv1.Listener, extra ...any) (*Graph, *RouteParentInfo, *RouteInfo) {
	t.Helper()
	objs := []any{
		testClass("c", ControllerName, 1),
		testGateway("infra", "gw", "c", 1, l),
		testRoute("apps", "r", 1, nil,
			[]gatewayv1.ParentReference{gwParentInNS("gw", "infra")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "svc", 8080)),
	}
	objs = append(objs, extra...)
	g := build(t, objs...)
	ri := routeOf(t, g, "apps", "r")
	return g, parentOf(t, ri, "gw"), ri
}

// TestCrossNamespaceAttach_FromAll: from=All admits the cross-namespace
// route; the attachment lands on the target listener; status carries the
// controllerName and the Gateway's namespace in the parentRef.
func TestCrossNamespaceAttach_FromAll(t *testing.T) {
	l := plainListener("web", 80, nil)
	l.AllowedRoutes = allowedRoutesFrom(gatewayv1.NamespacesFromAll)
	g, pi, ri := crossNSFixture(t, l,
		testSlice("apps", "svc", 8080, ptr(true), "10.1.0.1"),
	)
	if !pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonAccepted) {
		t.Fatalf("from=All must admit the cross-namespace route: %+v", pi)
	}
	if !pi.ResolvedRefs {
		t.Fatalf("backend must resolve in the route's namespace: %+v", pi)
	}
	if u := ri.Rules[0].Upstream; u != "apps_svc_8080" {
		t.Fatalf("upstream = %q, want apps_svc_8080 (route-namespace default)", u)
	}
	if len(pi.Attached) != 1 || pi.Attached[0].Listener.Gateway.Resource.Name != "gw" ||
		pi.Attached[0].Listener.Gateway.Resource.Namespace != "infra" {
		t.Fatalf("attachment must target the infra/gw listener: %+v", pi.Attached)
	}
	if n := len(gatewayOf(t, g, "infra", "gw").Listeners[0].Attachments); n != 1 {
		t.Fatalf("listener must hold 1 attachment, got %d", n)
	}
	// Status shape: parentRef mirrors the spec ref (carrying the Gateway's
	// namespace) and controllerName identifies this controller.
	sts := ri.ParentStatuses()
	if len(sts) != 1 {
		t.Fatalf("expected exactly one status parent, got %+v", sts)
	}
	ps := sts[0]
	if ps.ControllerName != ControllerName {
		t.Fatalf("controllerName = %q, want %q", ps.ControllerName, ControllerName)
	}
	if ps.ParentRef.Namespace == nil || string(*ps.ParentRef.Namespace) != "infra" {
		t.Fatalf("status parentRef must carry the Gateway namespace: %+v", ps.ParentRef)
	}
	acc := condByType(ps.Conditions, string(gatewayv1.RouteConditionAccepted))
	if acc.Status != metav1.ConditionTrue || acc.Reason != string(gatewayv1.RouteReasonAccepted) {
		t.Fatalf("Accepted condition: %+v", acc)
	}
	// The dataplane translation emits the attached route's location.
	cfg := g.Configuration()
	if len(cfg.Servers) == 0 || len(cfg.Servers[0].Locations) == 0 {
		t.Fatalf("cross-ns attachment must generate server locations: %+v", cfg.Servers)
	}
}

// TestCrossNamespaceAttach_FromSelector: the route namespace's labels decide.
func TestCrossNamespaceAttach_FromSelector(t *testing.T) {
	l := plainListener("web", 80, nil)
	l.AllowedRoutes = allowedRoutesSelector(metav1.LabelSelector{
		MatchLabels: map[string]string{"team": "apps"},
	})
	matching := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "apps", Labels: map[string]string{"team": "apps"}},
	}
	other := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "apps", Labels: map[string]string{"team": "other"}},
	}

	g, pi, _ := crossNSFixture(t, l, matching)
	if !pi.Accepted {
		t.Fatalf("from=Selector must admit a namespace whose labels match: %+v", pi)
	}
	if n := len(gatewayOf(t, g, "infra", "gw").Listeners[0].Attachments); n != 1 {
		t.Fatalf("matching selector must attach, got %d", n)
	}

	_, pi, _ = crossNSFixture(t, l, other)
	if pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("non-matching selector must refuse with NotAllowedByListeners: %+v", pi)
	}

	// No such namespace in the snapshot at all → refuse as well.
	_, pi, _ = crossNSFixture(t, l)
	if pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("unknown route namespace must refuse with NotAllowedByListeners: %+v", pi)
	}
}

// TestCrossNamespaceReject_FromSame: from=Same refuses the cross-namespace
// route with the standard reason; the same route in the Gateway's own
// namespace still attaches.
func TestCrossNamespaceReject_FromSame(t *testing.T) {
	l := plainListener("web", 80, nil)
	l.AllowedRoutes = allowedRoutesFrom(gatewayv1.NamespacesFromSame)
	_, pi, _ := crossNSFixture(t, l)
	if pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("from=Same must refuse cross-namespace with NotAllowedByListeners: %+v", pi)
	}
	if len(pi.Attached) != 0 {
		t.Fatalf("refused route must leave no attachments: %+v", pi.Attached)
	}

	// Control: identical route name/ns on the Gateway side attaches.
	g2 := build(t,
		testClass("c", ControllerName, 1),
		testGateway("infra", "gw", "c", 1, l),
		testRoute("infra", "r", 1, nil,
			[]gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
	)
	if pi := parentOf(t, routeOf(t, g2, "infra", "r"), "gw"); !pi.Accepted {
		t.Fatalf("from=Same must keep admitting same-namespace routes: %+v", pi)
	}
}

// TestCrossNamespaceAttach_ListenerPolicyPrecedence: allowedRoutes is a
// PER-LISTENER policy — with one from=Same and one from=All listener on the
// same Gateway, a cross-ns route attaches only to the admitting listener.
func TestCrossNamespaceAttach_ListenerPolicyPrecedence(t *testing.T) {
	same := plainListener("same", 80, nil)
	same.AllowedRoutes = allowedRoutesFrom(gatewayv1.NamespacesFromSame)
	all := plainListener("all", 8080, nil)
	all.AllowedRoutes = allowedRoutesFrom(gatewayv1.NamespacesFromAll)

	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("infra", "gw", "c", 1, same, all),
		testRoute("apps", "r", 1, nil,
			[]gatewayv1.ParentReference{gwParentInNS("gw", "infra")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "svc", 8080)),
		testSlice("apps", "svc", 8080, ptr(true), "10.1.0.1"),
	)
	gw := gatewayOf(t, g, "infra", "gw")
	pi := parentOf(t, routeOf(t, g, "apps", "r"), "gw")
	if !pi.Accepted {
		t.Fatalf("at least one admitting listener must accept the route: %+v", pi)
	}
	if n := len(sameOf(t, gw).Attachments); n != 0 {
		t.Fatalf("from=Same listener must hold no cross-ns attachment, got %d", n)
	}
	if n := len(allOf(t, gw).Attachments); n != 1 {
		t.Fatalf("from=All listener must hold the attachment, got %d", n)
	}
}

// TestCrossNamespaceAttach_HostnameIntersection: the §3.3 hostname
// intersection applies to cross-namespace attachments unchanged — matching
// hostnames attach, disjoint ones report NoMatchingListenerHostname.
func TestCrossNamespaceAttach_HostnameIntersection(t *testing.T) {
	l := plainListener("web", 80, host("api.example.com"))
	l.AllowedRoutes = allowedRoutesFrom(gatewayv1.NamespacesFromAll)

	routeWithHostname := func(hostnames []gatewayv1.Hostname) (*RouteParentInfo, *RouteInfo) {
		g := build(t,
			testClass("c", ControllerName, 1),
			testGateway("infra", "gw", "c", 1, l),
			testRoute("apps", "r", 1, hostnames,
				[]gatewayv1.ParentReference{gwParentInNS("gw", "infra")},
				pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "svc", 8080)),
			testSlice("apps", "svc", 8080, ptr(true), "10.1.0.1"),
		)
		ri := routeOf(t, g, "apps", "r")
		return parentOf(t, ri, "gw"), ri
	}

	// Listener hostname covers the route hostname.
	pi, ri := routeWithHostname([]gatewayv1.Hostname{host2("api.example.com")})
	if !pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonAccepted) {
		t.Fatalf("intersecting hostname must attach: %+v", pi)
	}
	if u := ri.Rules[0].Upstream; u != "apps_svc_8080" {
		t.Fatalf("backend still resolves in the route namespace: %q", u)
	}

	// Wildcard route under an exact listener intersects (§3.3 suffix rules).
	pi, _ = routeWithHostname([]gatewayv1.Hostname{host2("*.example.com")})
	if !pi.Accepted {
		t.Fatalf("route wildcard *.example.com intersects exact listener api.example.com: %+v", pi)
	}

	// No common host → NoMatchingListenerHostname.
	pi, _ = routeWithHostname([]gatewayv1.Hostname{host2("other.example.com")})
	if pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonNoMatchingListenerHostname) {
		t.Fatalf("disjoint hostname must refuse with NoMatchingListenerHostname: %+v", pi)
	}
}

// TestCrossNamespaceBackend_DefaultAndGrant: after a cross-namespace
// attachment, backendRefs WITHOUT a namespace resolve in the ROUTE's
// namespace (spec), while an explicit cross-namespace backendRef still
// requires a ReferenceGrant in the target namespace — the grant scope is
// unchanged by attachment.
func TestCrossNamespaceBackend_DefaultAndGrant(t *testing.T) {
	l := plainListener("web", 80, nil)
	l.AllowedRoutes = allowedRoutesFrom(gatewayv1.NamespacesFromAll)

	crossBackendRule := gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{{
		BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
			Name:      "svc",
			Namespace: ptr(gatewayv1.Namespace("backend")),
			Port:      ptr(gatewayv1.PortNumber(80)),
		}},
	}}}

	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("infra", "gw", "c", 1, l),
		testRoute("apps", "r-default", 1, nil,
			[]gatewayv1.ParentReference{gwParentInNS("gw", "infra")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "svc", 8080)),
		testRoute("apps", "r-cross", 1, nil,
			[]gatewayv1.ParentReference{gwParentInNS("gw", "infra")}, crossBackendRule),
		testSlice("apps", "svc", 8080, ptr(true), "10.1.0.1"),
		testSlice("backend", "svc", 80, ptr(true), "10.2.0.1"),
	)

	// Default: ROUTE namespace ("apps"), not the Gateway's ("infra").
	ri := routeOf(t, g, "apps", "r-default")
	if pi := parentOf(t, ri, "gw"); !pi.ResolvedRefs {
		t.Fatalf("route-namespace backend must resolve: %+v", pi)
	}
	if u := ri.Rules[0].Upstream; u != "apps_svc_8080" {
		t.Fatalf("upstream = %q, want apps_svc_8080 (route namespace default)", u)
	}

	// Explicit cross-namespace backendRef without a grant: GEP-1364 static
	// 500 + RefNotPermitted (grant scope unchanged).
	ri = routeOf(t, g, "apps", "r-cross")
	if pi := parentOf(t, ri, "gw"); pi.ResolvedRefs ||
		pi.RefsReason != string(gatewayv1.RouteReasonRefNotPermitted) {
		t.Fatalf("ungranted cross-ns backendRef: %+v", pi)
	}
	if u := ri.Rules[0].Upstream; u != StaticUpstreamName(500) {
		t.Fatalf("ungranted cross-ns backendRef must serve 500, got %q", u)
	}

	// With a ReferenceGrant in the TARGET namespace the same backendRef
	// resolves; the parentRef itself needed no grant all along.
	g2 := build(t,
		testClass("c", ControllerName, 1),
		testGateway("infra", "gw", "c", 1, l),
		testRoute("apps", "r-cross", 1, nil,
			[]gatewayv1.ParentReference{gwParentInNS("gw", "infra")}, crossBackendRule),
		testSlice("backend", "svc", 80, ptr(true), "10.2.0.1"),
		&gatewayv1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Namespace: "backend", Name: "allow-apps"},
			Spec: gatewayv1.ReferenceGrantSpec{
				From: []gatewayv1.ReferenceGrantFrom{{
					Group:     "gateway.networking.k8s.io",
					Kind:      "HTTPRoute",
					Namespace: "apps",
				}},
				To: []gatewayv1.ReferenceGrantTo{{
					Group: "", Kind: "Service",
				}},
			},
		},
	)
	ri = routeOf(t, g2, "apps", "r-cross")
	if pi := parentOf(t, ri, "gw"); !pi.Accepted || !pi.ResolvedRefs {
		t.Fatalf("granted cross-ns backendRef must resolve: %+v", pi)
	}
	if u := ri.Rules[0].Upstream; u != "backend_svc_80" {
		t.Fatalf("granted backend upstream = %q, want backend_svc_80", u)
	}
}

// sameOf / allOf return the listeners named "same"/"all" of gw.
func sameOf(t *testing.T, gw *GatewayInfo) *ListenerInfo {
	t.Helper()
	return listenerOf(t, gw, "same")
}

func allOf(t *testing.T, gw *GatewayInfo) *ListenerInfo {
	t.Helper()
	return listenerOf(t, gw, "all")
}

// host2 is a non-pointer hostname helper (host() already returns a pointer).
func host2(h string) gatewayv1.Hostname { return gatewayv1.Hostname(h) }
