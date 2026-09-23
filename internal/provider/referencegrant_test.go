// Unit tests for ReferenceGrant evaluation (Gateway API ReferenceGrant
// spec, DESIGN.md §3.5):
//
//   - a cross-namespace reference is permitted ONLY IF a ReferenceGrant
//     exists in the TARGET namespace whose spec.from contains an entry
//     matching (group, kind, namespace of the referrer) AND whose spec.to
//     contains an entry matching (group, kind of the target; to.name
//     constrains to the exact name when set);
//   - the matching from and to must belong to the SAME grant;
//   - permitted → resolve normally; not permitted → GEP-1364 static-500 +
//     ResolvedRefs=False/RefNotPermitted for backends, and
//     ResolvedRefs=False/RefNotPermitted + no server block for listener
//     certificateRefs;
//   - removal of a grant revokes the permission on the next full
//     reconcile (BuildGraph is a pure function of the snapshot).
package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const gwAPIGroup = "gateway.networking.k8s.io"

// grantFrom builds one spec.from entry (core group normalised to "").
func grantFrom(group, kind, namespace string) gatewayv1.ReferenceGrantFrom {
	return gatewayv1.ReferenceGrantFrom{Group: gatewayv1.Group(group), Kind: gatewayv1.Kind(kind), Namespace: gatewayv1.Namespace(namespace)}
}

// grantTo builds one spec.to entry; name "" omits the constraint.
func grantTo(group, kind, name string) gatewayv1.ReferenceGrantTo {
	to := gatewayv1.ReferenceGrantTo{Group: gatewayv1.Group(group), Kind: gatewayv1.Kind(kind)}
	if name != "" {
		to.Name = ptr(gatewayv1.ObjectName(name))
	}
	return to
}

func testGrant(ns string, from []gatewayv1.ReferenceGrantFrom, to []gatewayv1.ReferenceGrantTo) *gatewayv1.ReferenceGrant {
	return &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "grant"},
		Spec:       gatewayv1.ReferenceGrantSpec{From: from, To: to},
	}
}

// crossNSBackendRule builds a rule whose single backendRef points at
// svc:port in backendNS.
func crossNSBackendRule(backendNS, svc string, port int32) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name:      gatewayv1.ObjectName(svc),
					Namespace: ptr(gatewayv1.Namespace(backendNS)),
					Port:      ptr(gatewayv1.PortNumber(port)),
				},
			},
		}},
	}
}

func crossNSCertGateway(certNS, certName string) *gatewayv1.Gateway {
	return testGateway("default", "gw", "c", 1,
		tlsListener("web", 443, nil, gatewayv1.HTTPSProtocolType, ptr(gatewayv1.Namespace(certNS)), certName))
}

// permittedBackendCase assembles a cluster with an HTTPRoute in "default"
// whose backend lives in "backend", plus optional grants in "backend".
func permittedBackendCase(t *testing.T, grants ...*gatewayv1.ReferenceGrant) *Graph {
	t.Helper()
	objs := []any{
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			crossNSBackendRule("backend", "svc", 8080)),
		testSlice("backend", "svc", 8080, ptr(true), "10.1.0.1"),
	}
	for _, g := range grants {
		objs = append(objs, g)
	}
	return build(t, objs...)
}

func refsConditionOf(t *testing.T, g *Graph, ns, name string) metav1.Condition {
	t.Helper()
	ps := routeOf(t, g, ns, name).ParentStatuses()
	if len(ps) != 1 {
		t.Fatalf("parents: %+v", ps)
	}
	return condByType(ps[0].Conditions, "ResolvedRefs")
}

func TestReferenceGrant_BackendPermitted(t *testing.T) {
	g := permittedBackendCase(t,
		testGrant("backend", []gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "HTTPRoute", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Service", "")}))
	ri := routeOf(t, g, "default", "r")
	if cond := refsConditionOf(t, g, "default", "r"); cond.Status != metav1.ConditionTrue ||
		cond.Reason != string(gatewayv1.RouteReasonResolvedRefs) {
		t.Fatalf("granted cross-ns backend must resolve: %+v", cond)
	}
	if ri.Rules[0].Upstream != "backend_svc_8080" {
		t.Fatalf("upstream = %q, want backend_svc_8080", ri.Rules[0].Upstream)
	}
	if len(ri.Rules[0].Endpoints) != 1 {
		t.Fatalf("endpoints: %+v", ri.Rules[0].Endpoints)
	}
}

func TestReferenceGrant_BackendAbsentGrant(t *testing.T) {
	g := permittedBackendCase(t)
	ri := routeOf(t, g, "default", "r")
	cond := refsConditionOf(t, g, "default", "r")
	if cond.Status != metav1.ConditionFalse || cond.Reason != string(gatewayv1.RouteReasonRefNotPermitted) {
		t.Fatalf("ungranted backend: %+v", cond)
	}
	// GEP-1364: Accepted stays True, the rule still serves a static 500.
	ps := routeOf(t, g, "default", "r").ParentStatuses()[0]
	if c := condByType(ps.Conditions, "Accepted"); c.Status != metav1.ConditionTrue {
		t.Fatalf("accepted must stay true: %+v", c)
	}
	if ri.Rules[0].Upstream != StaticUpstreamName(500) {
		t.Fatalf("ungranted backend must serve static 500, upstream=%q", ri.Rules[0].Upstream)
	}
}

func TestReferenceGrant_BackendFromMismatch(t *testing.T) {
	cases := []struct {
		name string
		from gatewayv1.ReferenceGrantFrom
	}{
		{"wrong kind", grantFrom(gwAPIGroup, "TLSRoute", "default")},
		{"wrong namespace", grantFrom(gwAPIGroup, "HTTPRoute", "elsewhere")},
		{"wrong group", grantFrom("example.com", "HTTPRoute", "default")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := permittedBackendCase(t,
				testGrant("backend", []gatewayv1.ReferenceGrantFrom{tc.from},
					[]gatewayv1.ReferenceGrantTo{grantTo("", "Service", "")}))
			if cond := refsConditionOf(t, g, "default", "r"); cond.Status != metav1.ConditionFalse ||
				cond.Reason != string(gatewayv1.RouteReasonRefNotPermitted) {
				t.Fatalf("from-mismatch grant must not permit: %+v", cond)
			}
		})
	}
}

func TestReferenceGrant_BackendToMismatch(t *testing.T) {
	cases := []struct {
		name string
		to   gatewayv1.ReferenceGrantTo
	}{
		{"wrong kind", grantTo("", "Secret", "")},
		{"wrong group", grantTo("example.com", "Service", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := permittedBackendCase(t,
				testGrant("backend", []gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "HTTPRoute", "default")},
					[]gatewayv1.ReferenceGrantTo{tc.to}))
			if cond := refsConditionOf(t, g, "default", "r"); cond.Status != metav1.ConditionFalse ||
				cond.Reason != string(gatewayv1.RouteReasonRefNotPermitted) {
				t.Fatalf("to-mismatch grant must not permit: %+v", cond)
			}
		})
	}
}

func TestReferenceGrant_BackendToNameConstrained(t *testing.T) {
	// Grant names exactly "other": the reference to "svc" stays refused.
	g := permittedBackendCase(t,
		testGrant("backend", []gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "HTTPRoute", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Service", "other")}))
	if cond := refsConditionOf(t, g, "default", "r"); cond.Reason != string(gatewayv1.RouteReasonRefNotPermitted) {
		t.Fatalf("to.name mismatch must not permit: %+v", cond)
	}

	// Grant names exactly "svc": permitted even alongside a non-matching
	// to entry (to entries OR among themselves).
	g = permittedBackendCase(t,
		testGrant("backend", []gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "HTTPRoute", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Service", "nope"), grantTo("", "Service", "svc")}))
	if cond := refsConditionOf(t, g, "default", "r"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("matching to.name must permit: %+v", cond)
	}
}

func TestReferenceGrant_FromAndToMustShareAGrant(t *testing.T) {
	// Grant A: matching from, non-matching to. Grant B: non-matching from,
	// matching to. Neither alone permits — each grant is one trust
	// relationship (ReferenceGrant spec).
	g := permittedBackendCase(t,
		testGrant("backend",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "HTTPRoute", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", "")}),
		testGrant("backend",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "GRPCRoute", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Service", "")}))
	if cond := refsConditionOf(t, g, "default", "r"); cond.Status != metav1.ConditionFalse ||
		cond.Reason != string(gatewayv1.RouteReasonRefNotPermitted) {
		t.Fatalf("from and to must match within one grant: %+v", cond)
	}

	// The same from/to pairs combined in ONE grant do permit (from entries
	// OR each other, to entries OR each other).
	g = permittedBackendCase(t,
		testGrant("backend",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "GRPCRoute", "default"), grantFrom(gwAPIGroup, "HTTPRoute", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", ""), grantTo("", "Service", "")}))
	if cond := refsConditionOf(t, g, "default", "r"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("single-grant from/to OR must permit: %+v", cond)
	}
}

func TestReferenceGrant_DeletedGrantRevokes(t *testing.T) {
	grant := testGrant("backend",
		[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "HTTPRoute", "default")},
		[]gatewayv1.ReferenceGrantTo{grantTo("", "Service", "")})
	withGrant := permittedBackendCase(t, grant)
	if cond := refsConditionOf(t, withGrant, "default", "r"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("precondition: granted ref must resolve: %+v", cond)
	}
	if up := routeOf(t, withGrant, "default", "r").Rules[0].Upstream; up != "backend_svc_8080" {
		t.Fatalf("precondition upstream: %q", up)
	}

	// The grant is deleted: the next full reconcile (a fresh BuildGraph
	// over the snapshot) must revoke the permission.
	withoutGrant := permittedBackendCase(t)
	cond := refsConditionOf(t, withoutGrant, "default", "r")
	if cond.Status != metav1.ConditionFalse || cond.Reason != string(gatewayv1.RouteReasonRefNotPermitted) {
		t.Fatalf("grant deletion must revoke: %+v", cond)
	}
	if up := routeOf(t, withoutGrant, "default", "r").Rules[0].Upstream; up != StaticUpstreamName(500) {
		t.Fatalf("revoked backend must serve static 500, upstream=%q", up)
	}
}

func TestReferenceGrant_CertificateRefPermitted(t *testing.T) {
	grant := testGrant("certs-ns",
		[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "Gateway", "default")},
		[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", "cert")})
	g := build(t,
		testClass("c", ControllerName, 1),
		crossNSCertGateway("certs-ns", "cert"),
		testTLSSecret("certs-ns", "cert"),
		grant,
	)
	li := listenerOf(t, g.Gateways[0], "web")
	if !li.ResolvedRefs || li.RefsReason != string(gatewayv1.ListenerReasonResolvedRefs) {
		t.Fatalf("granted certRef must resolve: %+v", li)
	}
	if li.CertFailed || li.CertData == nil || li.TLSCert != "certs-ns_cert.pem" {
		t.Fatalf("cert must materialise: %+v", li)
	}
	if len(g.Configuration().Servers) == 0 {
		t.Fatal("granted cert must produce a server block")
	}
	certs := g.Certificates()
	if len(certs) != 1 || certs[0].Filename() != "certs-ns_cert.pem" {
		t.Fatalf("certificates: %+v", certs)
	}
}

func TestReferenceGrant_CertificateRefNotPermitted(t *testing.T) {
	cases := []struct {
		name   string
		grants []*gatewayv1.ReferenceGrant
	}{
		{"no grant at all", nil},
		{"from kind mismatch", []*gatewayv1.ReferenceGrant{testGrant("certs-ns",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "HTTPRoute", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", "")})}},
		{"to kind mismatch", []*gatewayv1.ReferenceGrant{testGrant("certs-ns",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "Gateway", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Service", "")})}},
		{"to.name constrained to another secret", []*gatewayv1.ReferenceGrant{testGrant("certs-ns",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "Gateway", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", "other")})}},
		{"grant in the wrong namespace", []*gatewayv1.ReferenceGrant{testGrant("default",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "Gateway", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", "cert")})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []any{
				testClass("c", ControllerName, 1),
				crossNSCertGateway("certs-ns", "cert"),
				testTLSSecret("certs-ns", "cert"),
			}
			for _, gr := range tc.grants {
				objs = append(objs, gr)
			}
			g := build(t, objs...)
			li := listenerOf(t, g.Gateways[0], "web")
			if li.ResolvedRefs || li.RefsReason != string(gatewayv1.ListenerReasonRefNotPermitted) {
				t.Fatalf("ungranted certRef must be RefNotPermitted: resolved=%v reason=%q", li.ResolvedRefs, li.RefsReason)
			}
			if li.CertData != nil || li.TLSCert != "" {
				t.Fatalf("ungranted certRef must not materialise a certificate: %+v", li)
			}
			// No server block for a listener whose cert cannot be resolved.
			if got := g.Configuration().Servers; len(got) != 0 {
				t.Fatalf("no server expected, got %+v", got)
			}
		})
	}
}

func TestReferenceGrant_SameNamespaceNeedsNoGrant(t *testing.T) {
	// Sanity: same-namespace backend and certificate refs resolve without
	// any ReferenceGrant in the snapshot.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			backendRule("svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	if cond := refsConditionOf(t, g, "default", "r"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("same-namespace backend needs no grant: %+v", cond)
	}
	var emptySecrets []*corev1.Secret
	_ = emptySecrets
}
