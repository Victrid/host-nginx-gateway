package provider

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// ---------------------------------------------------------------------------
// Ownership / class filtering
// ---------------------------------------------------------------------------

func TestBuildGraph_ClassFiltering(t *testing.T) {
	g := build(t,
		testClass("ours", ControllerName, 1),
		testClass("theirs", "other.example.com/controller", 1),
		testGateway("default", "gw", "ours", 1, plainListener("web", 80, nil)),
		testGateway("default", "foreign", "theirs", 1, plainListener("web", 80, nil)),
		testGateway("default", "orphan", "missing", 1, plainListener("web", 80, nil)),
	)
	if len(g.Classes) != 1 || g.Classes[0].Resource.Name != "ours" {
		t.Fatalf("expected only class ours, got %+v", g.Classes)
	}
	if len(g.Gateways) != 1 || g.Gateways[0].Resource.Name != "gw" {
		t.Fatalf("expected only gateway gw, got %+v", g.Gateways)
	}
	if got := g.Classes[0].AcceptedCondition(); got.Status != metav1.ConditionTrue ||
		got.Reason != string(gatewayv1.GatewayClassReasonAccepted) || got.ObservedGeneration != 1 {
		t.Fatalf("class Accepted condition = %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Listener validity matrix (§3.1 / S1)
// ---------------------------------------------------------------------------

func TestListenerValidity(t *testing.T) {
	tests := []struct {
		name          string
		listener      gatewayv1.Listener
		wantValid     bool
		wantReason    string
		wantRefsOK    bool
		wantRefsReasn string
	}{
		{
			name:       "port missing",
			listener:   gatewayv1.Listener{Name: "l", Protocol: gatewayv1.HTTPProtocolType},
			wantValid:  false,
			wantReason: string(gatewayv1.ListenerReasonInvalid),
		},
		{
			name:      "unsupported protocol",
			listener:  gatewayv1.Listener{Name: "l", Port: 80, Protocol: gatewayv1.UDPProtocolType},
			wantValid: false, wantReason: string(gatewayv1.ListenerReasonUnsupportedProtocol),
		},
		{
			name: "HTTP with tls set",
			listener: gatewayv1.Listener{Name: "l", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				TLS: &gatewayv1.ListenerTLSConfig{Mode: ptr(gatewayv1.TLSModeTerminate)}},
			wantValid: false, wantReason: string(gatewayv1.ListenerReasonInvalid),
		},
		{
			name:      "HTTPS without tls",
			listener:  gatewayv1.Listener{Name: "l", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			wantValid: false, wantReason: string(gatewayv1.ListenerReasonInvalid),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := build(t, testClass("c", ControllerName, 1),
				testTLSSecret("default", "cert"),
				testGateway("default", "gw", "c", 1, tc.listener))
			li := g.Gateways[0].Listeners[0]
			if li.Valid != tc.wantValid {
				t.Fatalf("Valid = %v, want %v (msg %q)", li.Valid, tc.wantValid, li.InvalidMsg)
			}
			if li.InvalidReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", li.InvalidReason, tc.wantReason)
			}
		})
	}
	// Passthrough mode is unsupported (checked separately: the tlsListener
	// helper pins mode=Terminate).
	g := build(t, testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, gatewayv1.Listener{
			Name: "l", Port: 443, Protocol: gatewayv1.TLSProtocolType,
			TLS: &gatewayv1.ListenerTLSConfig{Mode: ptr(gatewayv1.TLSModePassthrough)},
		}))
	if li := g.Gateways[0].Listeners[0]; li.Valid || li.InvalidReason != string(gatewayv1.ListenerReasonUnsupportedProtocol) {
		t.Fatalf("passthrough listener should be invalid/UnsupportedProtocol, got valid=%v reason=%q", li.Valid, li.InvalidReason)
	}
}

func TestTLSResolution(t *testing.T) {
	missing := tlsListener("web", 443, nil, gatewayv1.HTTPSProtocolType, nil, "missing")
	wrongType := tlsListener("api", 8443, nil, gatewayv1.HTTPSProtocolType, nil, "opaque")
	crossNS := tlsListener("ext", 9443, nil, gatewayv1.HTTPSProtocolType, ptr(gatewayv1.Namespace("other")), "cert")
	valid := tlsListener("ok", 4433, nil, gatewayv1.HTTPSProtocolType, nil, "cert")

	wrongSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "opaque"}, Type: corev1.SecretTypeOpaque}

	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, missing, wrongType, crossNS, valid),
		testTLSSecret("default", "cert"),
		wrongSecret,
	)
	gw := g.Gateways[0]

	li := listenerOf(t, gw, "web") // secret absent: listener stays valid, refs fail, no TLS
	if !li.Valid || li.ResolvedRefs || li.RefsReason != string(gatewayv1.ListenerReasonInvalidCertificateRef) {
		t.Fatalf("missing cert: valid=%v refsOK=%v reason=%q", li.Valid, li.ResolvedRefs, li.RefsReason)
	}
	if li.TLSCert != "" {
		t.Fatalf("missing cert should have no TLSCert, got %q", li.TLSCert)
	}
	li = listenerOf(t, gw, "api") // wrong secret type
	if li.ResolvedRefs || li.RefsReason != string(gatewayv1.ListenerReasonInvalidCertificateRef) {
		t.Fatalf("opaque secret: refsOK=%v reason=%q", li.ResolvedRefs, li.RefsReason)
	}
	li = listenerOf(t, gw, "ext") // cross-namespace certificateRef (§3.5)
	if li.ResolvedRefs || li.RefsReason != string(gatewayv1.ListenerReasonRefNotPermitted) {
		t.Fatalf("cross-ns cert: refsOK=%v reason=%q", li.ResolvedRefs, li.RefsReason)
	}
	li = listenerOf(t, gw, "ok")
	if li.TLSCert != "default_cert.pem" {
		t.Fatalf("TLSCert = %q, want default_cert.pem", li.TLSCert)
	}
	if !strings.Contains(string(li.CertData), "BEGIN CERTIFICATE") || !strings.Contains(string(li.CertData), "BEGIN PRIVATE KEY") {
		t.Fatalf("cert PEM should concatenate crt+key, got %q", li.CertData)
	}
}

// ---------------------------------------------------------------------------
// Conflict matrix (§3.2)
// ---------------------------------------------------------------------------

func TestConflicts_SameGateway(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1,
			plainListener("a", 80, host("example.com")),
			plainListener("b", 80, host("example.com")),  // same port+hostname → both conflicted
			plainListener("d", 80, host("other.com")),    // same port, disjoint hostname → fine
			plainListener("e", 443, host("example.com")), // same hostname, other port → fine
		),
	)
	gw := g.Gateways[0]
	for name, want := range map[string]bool{"a": true, "b": true, "d": false, "e": false} {
		li := listenerOf(t, gw, name)
		if li.Conflicted != want {
			t.Fatalf("listener %q conflicted=%v, want %v (msg %q)", name, li.Conflicted, want, li.ConflictMsg)
		}
		if want && li.ConflictReason != string(gatewayv1.ListenerReasonHostnameConflict) {
			t.Fatalf("listener %q reason=%q, want HostnameConflict", name, li.ConflictReason)
		}
	}
	// Conflicted listeners must not produce server blocks; the two valid
	// listeners each emit their named block (no synthetic default).
	cfg := g.Configuration()
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers (d, e), got %d", len(cfg.Servers))
	}
}

func TestConflicts_SameGatewayWildcardOverlap(t *testing.T) {
	// §3.2 refinement: a wildcard listener and a hostname'd listener on the
	// same port coexist (nginx dispatches to the specific server_name);
	// identical (wildcard×wildcard) listeners still conflict on both sides.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1,
			plainListener("wild1", 80, nil),
			plainListener("specific", 80, host("example.com")),
			plainListener("wild2", 8080, nil),
			plainListener("wild3", 8080, nil),
		),
	)
	gw := g.Gateways[0]
	if listenerOf(t, gw, "wild1").Conflicted || listenerOf(t, gw, "specific").Conflicted {
		t.Fatal("wildcard and hostname'd listeners on the same port must coexist")
	}
	if !listenerOf(t, gw, "wild2").Conflicted || !listenerOf(t, gw, "wild3").Conflicted {
		t.Fatal("two wildcard listeners on the same port conflict (both sides)")
	}
	if servers := g.Configuration().Servers; len(servers) != 2 {
		t.Fatalf("expected 2 server blocks (wildcard + specific), got %d", len(servers))
	}
}

func TestConflicts_CrossGateway(t *testing.T) {
	// Both gateways bind all addresses on port 80 with overlapping hostnames:
	// indistinct listeners across the merged data plane (Gateway API v1
	// "Distinct Listeners" applies across Gateways). The controller assigns
	// each Gateway its own loopback bind so BOTH program cleanly, and
	// reports the assigned address in status.addresses.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw1", "c", 1, plainListener("web", 80, host("example.com"))),
		testGateway("default", "gw2", "c", 1, plainListener("web", 80, host("example.com"))),
	)
	addrs := map[string]string{}
	for _, gwName := range []string{"gw1", "gw2"} {
		gw := gatewayOf(t, g, "default", gwName)
		if li := listenerOf(t, gw, "web"); li.Conflicted {
			t.Fatalf("%s listener must be programmed via address assignment, got conflicted: %+v", gwName, li)
		}
		if gw.AutoAddress == "" {
			t.Fatalf("%s must receive an auto-assigned address", gwName)
		}
		addrs[gwName] = gw.AutoAddress
		if li := listenerOf(t, gw, "web"); len(li.Addresses) != 1 || li.Addresses[0] != gw.AutoAddress {
			t.Fatalf("%s listener binds %v, want [%s]", gwName, li.Addresses, gw.AutoAddress)
		}
	}
	if addrs["gw1"] == addrs["gw2"] {
		t.Fatalf("distinct addresses required, both got %s", addrs["gw1"])
	}
	if addrs["gw1"] != "127.0.0."+fmt.Sprint(autoAssignBase+int(fnv32a("default/gw1"))%autoAssignSpan) {
		t.Fatalf("assignment must be the stable hash candidate, got %s", addrs["gw1"])
	}
	// Both gateways program: per gateway its named block.
	if servers := g.Configuration().Servers; len(servers) != 2 {
		t.Fatalf("cross-gateway indistinct listeners must both program, got %d servers", len(servers))
	}
}

func TestConflicts_CrossGatewayAnnotatedCollision(t *testing.T) {
	// Explicit (annotation) binds that collide cannot be separated by the
	// controller — §3.2 marks both sides Conflicted (no winner).
	gw1 := testGateway("default", "gw1", "c", 1, plainListener("web", 80, host("example.com")))
	gw2 := testGateway("default", "gw2", "c", 1, plainListener("web", 80, host("example.com")))
	gw1.Annotations = map[string]string{ListenAddressesAnnotation: "192.168.1.10"}
	gw2.Annotations = map[string]string{ListenAddressesAnnotation: "192.168.1.10"}
	g := build(t, testClass("c", ControllerName, 1), gw1, gw2)
	for _, gwName := range []string{"gw1", "gw2"} {
		li := listenerOf(t, gatewayOf(t, g, "default", gwName), "web")
		if !li.Conflicted || li.ConflictReason != string(gatewayv1.ListenerReasonHostnameConflict) {
			t.Fatalf("%s listener should be conflicted (HostnameConflict), got %+v", gwName, li)
		}
	}
	if servers := g.Configuration().Servers; len(servers) != 0 {
		t.Fatalf("unresolvable cross-gateway conflict must suppress both server blocks, got %d", len(servers))
	}
}

func TestConflicts_CrossGatewayDisjointAddresses(t *testing.T) {
	gw1 := testGateway("default", "gw1", "c", 1, plainListener("web", 80, host("example.com")))
	gw2 := testGateway("default", "gw2", "c", 1, plainListener("web", 80, host("example.com")))
	gw1.Annotations = map[string]string{ListenAddressesAnnotation: "192.168.1.10"}
	gw2.Annotations = map[string]string{ListenAddressesAnnotation: "10.0.0.1"}
	g := build(t, testClass("c", ControllerName, 1), gw1, gw2)
	for _, gwName := range []string{"gw1", "gw2"} {
		if li := listenerOf(t, gatewayOf(t, g, "default", gwName), "web"); li.Conflicted {
			t.Fatalf("%s listener must not conflict on disjoint bind addresses", gwName)
		}
	}
	// Two sockets: each annotated gateway emits its named block.
	if len(g.Configuration().Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(g.Configuration().Servers))
	}
}

func TestConflicts_CrossGatewayProtocolConflict(t *testing.T) {
	// HTTP vs HTTPS on the same port/hostname across Gateways: indistinct
	// listeners are separated by the auto-assigned addresses; both program.
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert"),
		testGateway("default", "gw1", "c", 1, plainListener("web", 80, host("example.com"))),
		testGateway("default", "gw2", "c", 1, tlsListener("secure", 80, host("example.com"), gatewayv1.HTTPSProtocolType, nil, "cert")),
	)
	for _, gwName := range []string{"gw1", "gw2"} {
		gw := gatewayOf(t, g, "default", gwName)
		li := gw.Listeners[0]
		if li.Conflicted {
			t.Fatalf("%s: address separation resolves the cross-gateway pair, got conflicted: %+v", gwName, li)
		}
		if gw.AutoAddress == "" {
			t.Fatalf("%s: expected an auto-assigned address", gwName)
		}
	}
	// Same Gateway, however: protocol conflict is a spec MUST.
	gw := testGateway("default", "gw", "c", 1,
		plainListener("web", 80, host("example.com")),
		tlsListener("secure", 80, host("example.com"), gatewayv1.HTTPSProtocolType, nil, "cert"),
	)
	g2 := build(t, testClass("c", ControllerName, 1), testTLSSecret("default", "cert"), gw)
	for _, name := range []string{"web", "secure"} {
		if li := listenerOf(t, gatewayOf(t, g2, "default", "gw"), name); !li.Conflicted || li.ConflictReason != string(gatewayv1.ListenerReasonProtocolConflict) {
			t.Fatalf("same-gateway %s: expected ProtocolConflict, got %+v", name, li)
		}
	}
}

func TestConflicts_InvalidListenersExcluded(t *testing.T) {
	// An invalid listener (port 0) must not trigger conflicts on others.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1,
			gatewayv1.Listener{Name: "bad", Protocol: gatewayv1.HTTPProtocolType}, // port missing
			plainListener("good", 80, nil),
		),
	)
	if li := listenerOf(t, g.Gateways[0], "good"); li.Conflicted {
		t.Fatal("invalid listener must not conflict valid listeners")
	}
}

// ---------------------------------------------------------------------------
// Hostname intersection (§3.3 / S4)
// ---------------------------------------------------------------------------

func TestHostnameMatches(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "EXAMPLE.com", true},
		{"*.example.com", "foo.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "a.b.example.com", true}, // wildcard = suffix match (Gateway API v1, Hostname doc)
		{"*.example.com", "*.example.com", true},
		{"example.com", "other.com", false},
		{"example.com.", "example.com", true}, // trailing dot tolerated
	}
	for _, tc := range cases {
		if got := hostnameMatches(tc.pattern, tc.name); got != tc.want {
			t.Errorf("hostnameMatches(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestRouteAttachment_HostnameIntersection(t *testing.T) {
	base := func(routeHostnames []gatewayv1.Hostname, listenerHostname *gatewayv1.Hostname) *RouteParentInfo {
		g := build(t,
			testClass("c", ControllerName, 1),
			testGateway("default", "gw", "c", 1, plainListener("web", 80, listenerHostname)),
			testRoute("default", "r", 1, routeHostnames, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
			testSlice("default", "svc", 8080, ptr(true), "10.1.1.1"),
		)
		return parentOf(t, routeOf(t, g, "default", "r"), "gw")
	}

	if pi := base([]gatewayv1.Hostname{"app.example.com"}, host("*.example.com")); !pi.Accepted || len(pi.Attached) != 1 {
		t.Fatalf("wildcard listener should accept matching route: %+v", pi)
	}
	if pi := base([]gatewayv1.Hostname{"app.example.com"}, host("other.com")); pi.Accepted ||
		pi.Reason != string(gatewayv1.RouteReasonNoMatchingListenerHostname) {
		t.Fatalf("disjoint hostnames: accepted=%v reason=%q", pi.Accepted, pi.Reason)
	}
	if pi := base([]gatewayv1.Hostname{"a.com", "b.com"}, nil); !pi.Accepted {
		t.Fatalf("nil listener hostname accepts all route hostnames")
	}
	if pi := base(nil, host("listener.com")); !pi.Accepted {
		t.Fatalf("route without hostnames attaches to any listener")
	}
	if pi := base([]gatewayv1.Hostname{"app.other.org"}, host("*.example.com")); pi.Accepted {
		t.Fatalf("no intersection must not attach (S4)")
	}
}

func TestRouteAttachment_SectionAndPort(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1,
			plainListener("web", 80, nil),
			plainListener("api", 8080, nil),
		),
		testRoute("default", "r1", 1, nil, []gatewayv1.ParentReference{gwParentSection("gw", "api")}, backendRule("svc", 8080)),
		testRoute("default", "r2", 1, nil, []gatewayv1.ParentReference{
			{Name: gatewayv1.ObjectName("gw"), Port: ptr(gatewayv1.PortNumber(9999))},
		}, backendRule("svc", 8080)),
		testRoute("default", "r3", 1, nil, []gatewayv1.ParentReference{gwParentSection("gw", "nope")}, backendRule("svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.1.1.1"),
	)
	p1 := parentOf(t, routeOf(t, g, "default", "r1"), "gw")
	if !p1.Accepted || len(p1.Attached) != 1 || string(p1.Attached[0].Listener.Spec.Name) != "api" {
		t.Fatalf("sectionName=api should attach only to api: %+v", p1)
	}
	p2 := parentOf(t, routeOf(t, g, "default", "r2"), "gw")
	if p2.Accepted || p2.Reason != string(gatewayv1.RouteReasonNoMatchingParent) {
		t.Fatalf("port 9999: %+v", p2)
	}
	p3 := parentOf(t, routeOf(t, g, "default", "r3"), "gw")
	if p3.Accepted || p3.Reason != string(gatewayv1.RouteReasonNoMatchingParent) {
		t.Fatalf("unknown section: %+v", p3)
	}
}

// ---------------------------------------------------------------------------
// allowedRoutes
// ---------------------------------------------------------------------------

func TestRouteAttachment_AllowedRoutes(t *testing.T) {
	from := func(f gatewayv1.FromNamespaces) *gatewayv1.AllowedRoutes {
		return &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: ptr(f)}}
	}
	mk := func(ar *gatewayv1.AllowedRoutes, routeNS string) *RouteParentInfo {
		l := plainListener("web", 80, nil)
		l.AllowedRoutes = ar
		g := build(t,
			testClass("c", ControllerName, 1),
			testGateway("default", "gw", "c", 1, l),
			testRoute(routeNS, "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
		)
		return parentOf(t, routeOf(t, g, routeNS, "r"), "gw")
	}
	// mkCrossNS builds a route in namespace "apps" whose parentRef
	// explicitly targets the Gateway in "default".
	mkCrossNS := func(ar *gatewayv1.AllowedRoutes) *RouteParentInfo {
		l := plainListener("web", 80, nil)
		l.AllowedRoutes = ar
		g := build(t,
			testClass("c", ControllerName, 1),
			testGateway("default", "gw", "c", 1, l),
			testRoute("apps", "r", 1, nil,
				[]gatewayv1.ParentReference{gwParentInNS("gw", "default")}, backendRule("svc", 8080)),
		)
		return parentOf(t, routeOf(t, g, "apps", "r"), "gw")
	}

	if pi := mk(nil, "default"); !pi.Accepted {
		t.Fatalf("nil allowedRoutes (CRD default from=Same) must accept a same-namespace route: %+v", pi)
	}
	if pi := mk(from(gatewayv1.NamespacesFromSame), "default"); !pi.Accepted {
		t.Fatalf("Same with same-namespace route must accept: %+v", pi)
	}
	// Cross-ns attachment is governed by the TARGET listener's allowedRoutes:
	// from=All permits the route from the other namespace (Gateway API v1 —
	// allowedRoutes IS the authorization; no ReferenceGrant for parentRefs).
	// (BackendRefs are independent of the attachment: they default to the
	// ROUTE's namespace per spec — this snapshot has no slices, so the
	// GEP-1364 ResolvedRefs failure must not affect attachment.)
	pi := mkCrossNS(from(gatewayv1.NamespacesFromAll))
	if !pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonAccepted) {
		t.Fatalf("cross-ns route with from=All must attach: %+v", pi)
	}
	pi = mk(from(gatewayv1.NamespacesFromSelector), "default")
	if pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("Selector unsupported: %+v", pi)
	}
	pi = mk(from(gatewayv1.NamespacesFromNone), "default")
	if pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("None rejects: %+v", pi)
	}

	// kinds excluding HTTPRoute.
	l := plainListener("web", 80, nil)
	l.AllowedRoutes = &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{{Kind: "GRPCRoute"}}}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, l),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
	)
	if pi := parentOf(t, routeOf(t, g, "default", "r"), "gw"); pi.Accepted ||
		pi.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("kinds without HTTPRoute: %+v", pi)
	}
}

// ---------------------------------------------------------------------------
// Backend resolution / GEP-1364
// ---------------------------------------------------------------------------

func TestGEP1364_BackendNotFound(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 2, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "ghost", 8080)),
		// no EndpointSlice for "ghost"
	)
	r := routeOf(t, g, "default", "r")
	pi := parentOf(t, r, "gw")
	if !pi.Accepted {
		t.Fatalf("GEP-1364: Accepted must stay True on missing backend: %+v", pi)
	}
	if pi.ResolvedRefs || pi.RefsReason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Fatalf("ResolvedRefs: %+v", pi)
	}
	// The rule still emits a location pointing at the static-500 marker.
	rule := r.Rules[0]
	if !rule.Valid || rule.Upstream != StaticUpstreamName(500) {
		t.Fatalf("rule should be valid with static upstream, got %+v", rule)
	}
	cfg := g.Configuration()
	if len(cfg.Servers) != 1 || len(cfg.Servers[0].Locations) != 2 { // "= /api" + "/api/"
		t.Fatalf("expected location for /api, got %+v", cfg.Servers)
	}
	if loc := cfg.Servers[0].Locations[0]; loc.Upstream != StaticUpstreamName(500) {
		t.Fatalf("location upstream = %q, want static marker", loc.Upstream)
	}
	for _, u := range cfg.Upstreams {
		if IsStaticUpstream(u.Name) {
			t.Fatalf("static marker must not become a real upstream block: %+v", u)
		}
	}
}

func TestBackendResolution_Endpoints(t *testing.T) {
	ready, notReady := ptr(true), ptr(false)
	slices := []*discoveryv1.EndpointSlice{
		testSlice("default", "svc", 8080, ready, "10.0.0.1", "10.0.0.2"),
		testSlice("default", "svc", 8080, notReady, "10.0.0.3"),
		{ // IPv6
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc-v6",
				Labels: map[string]string{discoveryv1.LabelServiceName: "svc"}},
			AddressType: discoveryv1.AddressTypeIPv6,
			Ports:       []discoveryv1.EndpointPort{{Port: ptr(int32(8080))}},
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"fd00::1"}, Conditions: discoveryv1.EndpointConditions{Ready: nil}}},
		},
		{ // FQDN slices are skipped
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc-fqdn",
				Labels: map[string]string{discoveryv1.LabelServiceName: "svc"}},
			AddressType: discoveryv1.AddressTypeFQDN,
			Ports:       []discoveryv1.EndpointPort{{Port: ptr(int32(8080))}},
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"db.example.com"}}},
		},
	}
	res := &Resources{
		GatewayClasses: []*gatewayv1.GatewayClass{testClass("c", ControllerName, 1)},
		Gateways:       []*gatewayv1.Gateway{testGateway("default", "gw", "c", 1, plainListener("web", 80, nil))},
		HTTPRoutes: []*gatewayv1.HTTPRoute{testRoute("default", "r", 1, nil,
			[]gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080))},
		EndpointSlices: slices,
	}
	g := BuildGraph(res)
	pi := parentOf(t, routeOf(t, g, "default", "r"), "gw")
	if !pi.Accepted || !pi.ResolvedRefs {
		t.Fatalf("route should resolve: %+v", pi)
	}
	if got := g.Routes[0].Rules[0].Upstream; got != "default_svc_8080" {
		t.Fatalf("upstream name = %q, want default_svc_8080", got)
	}
	cfg := g.Configuration()
	if len(cfg.Upstreams) != 1 || cfg.Upstreams[0].Name != "default_svc_8080" {
		t.Fatalf("upstreams: %+v", cfg.Upstreams)
	}
	eps := cfg.Upstreams[0].Endpoints
	if len(eps) != 4 {
		t.Fatalf("expected 4 endpoints (2 ready, 1 not-ready, 1 IPv6 nil-ready), got %+v", eps)
	}
	for _, e := range eps {
		if e.IP == "10.0.0.3" && e.Ready {
			t.Fatal("10.0.0.3 must be Ready=false (rendered as down)")
		}
		if e.IP == "fd00::1" && !e.Ready {
			t.Fatal("nil Ready must be interpreted as ready")
		}
		if e.Port != 8080 {
			t.Fatalf("endpoint port = %d, want 8080", e.Port)
		}
	}
}

func TestBackendResolution_MultiPortService(t *testing.T) {
	s1 := testSlice("default", "svc", 8080, ptr(true), "10.0.0.1")
	s2 := testSlice("default", "svc", 9090, ptr(true), "10.0.0.1")
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 80)),
		s1, s2,
	)
	pi := parentOf(t, routeOf(t, g, "default", "r"), "gw")
	if !pi.Accepted {
		t.Fatalf("still accepted (GEP-1364): %+v", pi)
	}
	if pi.ResolvedRefs || pi.RefsReason != string(gatewayv1.RouteReasonUnsupportedValue) {
		t.Fatalf("multi-port: %+v", pi)
	}
	if u := g.Routes[0].Rules[0].Upstream; !IsStaticUpstream(u) {
		t.Fatalf("multi-port backend must fall back to static 500, got %q", u)
	}
}

func TestBackendResolution_MissingPort(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{{
		BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc"}},
	}}}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, rule),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	if pi := parentOf(t, routeOf(t, g, "default", "r"), "gw"); pi.ResolvedRefs ||
		pi.RefsReason != string(gatewayv1.RouteReasonUnsupportedValue) {
		t.Fatalf("missing backendRef.port: %+v", pi)
	}
}

func TestBackendResolution_NonServiceKind(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{{
		BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
			Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")),
			Kind:  ptr(gatewayv1.Kind("HTTPRoute")),
			Name:  "x", Port: ptr(gatewayv1.PortNumber(80)),
		}},
	}}}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, rule),
	)
	if pi := parentOf(t, routeOf(t, g, "default", "r"), "gw"); pi.ResolvedRefs ||
		pi.RefsReason != string(gatewayv1.RouteReasonInvalidKind) {
		t.Fatalf("non-Service backendRef: %+v", pi)
	}
}

// ---------------------------------------------------------------------------
// Cross-namespace attachment (allowedRoutes-governed) + §3.5 grants
// ---------------------------------------------------------------------------

func TestSameNamespaceRestriction(t *testing.T) {
	crossBackend := gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{{
		BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
			Name:      "svc",
			Namespace: ptr(gatewayv1.Namespace("other")),
			Port:      ptr(gatewayv1.PortNumber(80)),
		}},
	}}}
	// Cross-ns parentRef: the TARGET listener's allowedRoutes decides.
	// from=All attaches the route (allowedRoutes IS the authorization —
	// no ReferenceGrant exists for Gateway-route attachment); from=Same
	// rejects it with the more precise NotAllowedByListeners. ReferenceGrant
	// still applies to the cross-ns backendRef of the same-namespace route.
	allListener := plainListener("web", 80, nil)
	allListener.AllowedRoutes = &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: ptr(gatewayv1.NamespacesFromAll)},
	}
	sameListener := plainListener("web", 80, nil)
	sameListener.AllowedRoutes = &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: ptr(gatewayv1.FromNamespaces("Same"))},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testGateway("other", "gw-all", "c", 1, allListener),
		testGateway("other", "gw-same", "c", 1, sameListener),
		testRoute("default", "backend", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, crossBackend),
		testRoute("default", "parent-all", 1, nil, []gatewayv1.ParentReference{gwParentInNS("gw-all", "other")}, backendRule("svc", 80)),
		testRoute("default", "parent-same", 1, nil, []gatewayv1.ParentReference{gwParentInNS("gw-same", "other")}, backendRule("svc", 80)),
	)
	pb := parentOf(t, routeOf(t, g, "default", "backend"), "gw")
	if !pb.Accepted || pb.ResolvedRefs || pb.RefsReason != string(gatewayv1.RouteReasonRefNotPermitted) {
		t.Fatalf("cross-ns backendRef: accepted stays true, ResolvedRefs=False RefNotPermitted: %+v", pb)
	}
	pa := parentOf(t, routeOf(t, g, "default", "parent-all"), "gw-all")
	if !pa.Accepted || pa.Reason != string(gatewayv1.RouteReasonAccepted) {
		t.Fatalf("cross-ns parentRef with from=All must attach: %+v", pa)
	}
	if len(pa.Attached) != 1 || pa.Attached[0].Listener.Gateway != gatewayOf(t, g, "other", "gw-all") {
		t.Fatalf("attachment must land on the target Gateway's listener: %+v", pa.Attached)
	}
	ps := parentOf(t, routeOf(t, g, "default", "parent-same"), "gw-same")
	if ps.Accepted || ps.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("cross-ns parentRef (from=Same): %+v", ps)
	}
}

// ---------------------------------------------------------------------------
// Rule support subset
// ---------------------------------------------------------------------------

func TestRuleSupport(t *testing.T) {
	regexRule := pathBackendRule(gatewayv1.PathMatchRegularExpression, "/x", "svc", 8080)
	headerRule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path:    &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/h")},
			Headers: []gatewayv1.HTTPHeaderMatch{{Name: "x", Value: "y"}},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendOf("svc", 8080)},
	}
	filterRule := gatewayv1.HTTPRouteRule{
		Filters: []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier}},
	}
	multiBackend := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{
			backendOf("a", 8080), backendOf("b", 8080),
		},
	}
	okRule := pathBackendRule(gatewayv1.PathMatchExact, "/exact", "svc", 8080)

	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "partial", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, regexRule, headerRule, filterRule, multiBackend, okRule),
		testRoute("default", "allbad", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, regexRule),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	rp := routeOf(t, g, "default", "partial")
	pi := parentOf(t, rp, "gw")
	if !pi.Accepted {
		t.Fatalf("route with some valid rules stays Accepted: %+v", pi)
	}
	ps := rp.ParentStatuses()
	var partial metav1.Condition
	for _, c := range ps[0].Conditions {
		if c.Type == string(gatewayv1.RouteConditionPartiallyInvalid) {
			partial = c
		}
	}
	if partial.Status != metav1.ConditionTrue || partial.Reason != string(gatewayv1.RouteReasonUnsupportedValue) {
		t.Fatalf("PartiallyInvalid condition: %+v", partial)
	}
	// The okRule survives statically; headerRule and multiBackend are now
	// supported (§3.3 round 5); the filter-only rule has no backends →
	// GEP-1364 static 500; multiBackend's Services don't resolve → also a
	// static 500 (GEP-1364).
	locs := g.Configuration().Servers[0].Locations
	if len(locs) != 4 {
		t.Fatalf("locations: %+v", locs)
	}
	byPath := map[string]*contract.Location{}
	for _, l := range locs {
		byPath[l.Path] = l
	}
	if l := byPath["= /exact"]; l == nil || l.Upstream != "default_svc_8080" {
		t.Fatalf("= /exact: %+v", byPath["= /exact"])
	}
	// Header-matched locations dispatch through a map variable.
	if l := byPath["/h/"]; l == nil || !strings.HasPrefix(l.Upstream, "$") {
		t.Fatalf("/h/ must dispatch through a map variable: %+v", byPath["/h/"])
	}
	if l := byPath["= /h"]; l == nil || !strings.HasPrefix(l.Upstream, "$") {
		t.Fatalf("= /h must dispatch through a map variable: %+v", byPath["= /h"])
	}
	// filterRule (no backends) + multiBackend (unresolvable backends) at
	// "/" → static GEP-1364 marker.
	if l := byPath["/"]; l == nil || l.Upstream != StaticUpstreamName(500) {
		t.Fatalf("/ must answer the static 500 marker: %+v", byPath["/"])
	}

	ra := routeOf(t, g, "default", "allbad")
	pa := parentOf(t, ra, "gw")
	if pa.Accepted || pa.Reason != string(gatewayv1.RouteReasonUnsupportedValue) {
		t.Fatalf("all-invalid route: %+v", pa)
	}
	var hasPartial bool
	for _, c := range ra.ParentStatuses()[0].Conditions {
		if c.Type == string(gatewayv1.RouteConditionPartiallyInvalid) {
			hasPartial = true
		}
	}
	if hasPartial {
		t.Fatal("PartiallyInvalid must not be set for fully invalid routes")
	}
}

func backendOf(svc string, port int32) gatewayv1.HTTPBackendRef {
	return gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{
			Name: gatewayv1.ObjectName(svc), Port: ptr(gatewayv1.PortNumber(port)),
		},
	}}
}

// ---------------------------------------------------------------------------
// Conformance round-2 additions
// ---------------------------------------------------------------------------

// TestAllowedRouteKinds_Status: allowedRoutes.kinds shapes both the
// supportedKinds status and the InvalidRouteKinds listener condition.
func TestAllowedRouteKinds_Status(t *testing.T) {
	badOnly := plainListener("only-invalid", 80, nil)
	badOnly.AllowedRoutes = &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{
		{Kind: "InvalidRoute"},
	}}
	mixed := plainListener("mixed", 80, nil)
	mixed.AllowedRoutes = &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{
		{Kind: "InvalidRoute"}, {Kind: "HTTPRoute"},
	}}
	unconstrained := plainListener("plain", 80, nil)
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, badOnly, mixed, unconstrained),
	)
	gw := g.Gateways[0]
	if li := listenerOf(t, gw, "only-invalid"); li.SupportedKinds != nil || li.ResolvedRefs ||
		li.RefsReason != string(gatewayv1.ListenerReasonInvalidRouteKinds) {
		t.Fatalf("only-invalid kinds: supportedKinds empty + InvalidRouteKinds, got %+v", li)
	}
	li := listenerOf(t, gw, "mixed")
	if len(li.SupportedKinds) != 1 || string(li.SupportedKinds[0].Kind) != "HTTPRoute" || li.ResolvedRefs ||
		li.RefsReason != string(gatewayv1.ListenerReasonInvalidRouteKinds) {
		t.Fatalf("mixed kinds: [HTTPRoute] + InvalidRouteKinds, got %+v", li)
	}
	if li := listenerOf(t, gw, "plain"); len(li.SupportedKinds) != 1 || string(li.SupportedKinds[0].Kind) != "HTTPRoute" || !li.ResolvedRefs {
		t.Fatalf("unconstrained kinds: defaults to [HTTPRoute], got %+v", li)
	}
}

// TestMultiPortServiceResolution: backendRef.port selects the Service port;
// the upstream is keyed by the resolved endpoint (target) port.
func TestMultiPortServiceResolution(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "first-port", Port: 8080, TargetPort: intstr.FromInt32(3000)},
				{Name: "second-port", Port: 8081, TargetPort: intstr.FromInt32(3001)},
			},
		},
	}
	p3000, p3001 := int32(3000), int32(3001)
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "svc-slice",
			Labels: map[string]string{discoveryv1.LabelServiceName: "svc"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{
			{Name: ptr("first-port"), Port: &p3000},
			{Name: ptr("second-port"), Port: &p3001},
		},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r1", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
		testRoute("default", "r2", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8081)),
		svc, slice,
	)
	for _, r := range g.Routes {
		pi := parentOf(t, r, "gw")
		if !pi.ResolvedRefs {
			t.Fatalf("route %s: ResolvedRefs should be true: %+v", r.Resource.Name, pi)
		}
	}
	var names []string
	for _, u := range g.Configuration().Upstreams {
		names = append(names, u.Name)
	}
	sort.Strings(names)
	want := []string{"default_svc_3000", "default_svc_3001"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("upstreams: got %v want %v", names, want)
	}
}

// TestZeroBackendRuleAnswers500: rules with omitted/empty backendRefs are
// valid rules serving a static 500 (GEP-1364), not PartiallyInvalid.
func TestZeroBackendRuleAnswers500(t *testing.T) {
	empty := gatewayv1.HTTPRouteRule{
		Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/empty")}}},
		BackendRefs: []gatewayv1.HTTPBackendRef{},
	}
	omitted := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/omitted")}}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, empty, omitted),
	)
	r := routeOf(t, g, "default", "r")
	pi := parentOf(t, r, "gw")
	if !pi.Accepted || !pi.ResolvedRefs {
		t.Fatalf("zero-backend rule route must stay Accepted+ResolvedRefs: %+v", pi)
	}
	for _, rule := range r.Rules {
		if !rule.Valid || rule.Upstream != StaticUpstreamName(500) {
			t.Fatalf("rule %d must be a valid static-500 rule: %+v", rule.Index, rule)
		}
	}
}

// TestParametersRefRejection: a GatewayClass with parametersRef is rejected
// (Accepted=False/InvalidParameters) and its Gateways follow.
func TestParametersRefRejection(t *testing.T) {
	bad := testClass("bad", ControllerName, 1)
	bad.Spec.ParametersRef = &gatewayv1.ParametersReference{
		Group: "example.com", Kind: "ConfigMap", Name: "nope",
	}
	g := build(t,
		testClass("good", ControllerName, 1), bad,
		testGateway("default", "gw-bad", "bad", 1, plainListener("web", 9090, nil)),
		testGateway("default", "gw-good", "good", 1, plainListener("web", 80, nil)),
	)
	if c := classOf(t, g, "bad"); c.Accepted {
		t.Fatal("parametersRef class must be rejected")
	}
	condBad := classOf(t, g, "bad").AcceptedCondition()
	if condBad.Status != metav1.ConditionFalse || condBad.Reason != string(gatewayv1.GatewayClassReasonInvalidParameters) {
		t.Fatalf("class condition: %+v", condBad)
	}
	badGW := gatewayOf(t, g, "default", "gw-bad")
	apply := ApplyResult{Applied: true, Reason: string(gatewayv1.GatewayReasonProgrammed), Message: "ok"}
	conds := badGW.Conditions(apply)
	if conds[0].Reason != string(gatewayv1.GatewayClassReasonInvalidParameters) || conds[0].Status != metav1.ConditionFalse {
		t.Fatalf("gateway under rejected class: %+v", conds[0])
	}
	if servers := g.Configuration().Servers; len(servers) != 1 {
		t.Fatalf("rejected class must suppress its gateway's server block, got %d", len(servers))
	}
}

// TestAllowedRoutesSelector: from=Selector gates attachment by namespace labels.
func TestAllowedRoutesSelector(t *testing.T) {
	l := plainListener("web", 80, nil)
	l.AllowedRoutes = &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{
			From:     ptr(gatewayv1.FromNamespaces("Selector")),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
		},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, l),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 80)),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"team": "a"}}},
		testSlice("default", "svc", 80, ptr(true), "10.0.0.1"),
	)
	if pi := parentOf(t, routeOf(t, g, "default", "r"), "gw"); !pi.Accepted {
		t.Fatalf("selector-matching namespace must attach: %+v", pi)
	}

	g2 := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, l),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 80)),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"team": "b"}}},
		testSlice("default", "svc", 80, ptr(true), "10.0.0.1"),
	)
	if pi := parentOf(t, routeOf(t, g2, "default", "r"), "gw"); pi.Accepted || pi.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("non-matching namespace must be refused with NotAllowedByListeners: %+v", pi)
	}
}

// TestCertificateRefGroupValidation: non-core groups are InvalidCertificateRef.
func TestCertificateRefGroupValidation(t *testing.T) {
	l := plainListener("https", 443, nil)
	l.Protocol = gatewayv1.HTTPSProtocolType
	l.TLS = &gatewayv1.ListenerTLSConfig{
		Mode: ptr(gatewayv1.TLSModeTerminate),
		CertificateRefs: []gatewayv1.SecretObjectReference{{
			Group: ptr(gatewayv1.Group("wrong.group.company.io")),
			Kind:  ptr(gatewayv1.Kind("Secret")),
			Name:  "cert",
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, l),
	)
	li := listenerOf(t, gatewayOf(t, g, "default", "gw"), "https")
	if li.ResolvedRefs || li.RefsReason != string(gatewayv1.ListenerReasonInvalidCertificateRef) {
		t.Fatalf("unsupported group: %+v", li)
	}
}

func classOf(t *testing.T, g *Graph, name string) *ClassInfo {
	t.Helper()
	for _, c := range g.Classes {
		if c.Resource.Name == name {
			return c
		}
	}
	t.Fatalf("class %s not in graph", name)
	return nil
}
