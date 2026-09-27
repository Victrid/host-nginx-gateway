package provider

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/errs"
)

func TestConfiguration_ProtocolListenMapping(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert"),
		testGateway("default", "gw", "c", 1,
			plainListener("http", 80, host("a.example.com")),
			tlsListener("https", 443, host("b.example.com"), gatewayv1.HTTPSProtocolType, nil, "cert"),
			tlsListener("tls", 8443, host("c.example.com"), gatewayv1.TLSProtocolType, nil, "cert"),
		),
	)
	cfg := g.Configuration()
	// Per protocol-socket: only the named (route/listener-claimed) block —
	// the controller emits no synthetic default block.
	if len(cfg.Servers) != 3 {
		t.Fatalf("servers: %+v", cfg.Servers)
	}
	for _, s := range cfg.Servers {
		if s.Hostname == "" {
			t.Fatalf("no synthetic default block may be emitted, got: %+v", cfg.Servers)
		}
	}
	byHostname := map[string]*contract.Server{}
	for _, s := range cfg.Servers {
		byHostname[s.Hostname] = s
	}
	http := byHostname["a.example.com"]
	if len(http.Listens) != 1 || http.Listens[0] != (contract.Listen{Port: 80}) {
		t.Fatalf("http listens: %+v", http.Listens)
	}
	if http.TLSCert != "" {
		t.Fatalf("http server must not have TLS: %q", http.TLSCert)
	}
	https := byHostname["b.example.com"]
	if !reflect.DeepEqual(https.Listens, []contract.Listen{{Port: 443, SSL: true, HTTP2: true}}) {
		t.Fatalf("https listens: %+v", https.Listens)
	}
	if https.TLSCert != "default_cert.pem" {
		t.Fatalf("https TLSCert: %q", https.TLSCert)
	}
	tls := byHostname["c.example.com"]
	if !reflect.DeepEqual(tls.Listens, []contract.Listen{{Port: 8443, SSL: true}}) {
		t.Fatalf("tls listens (ssl, no http2): %+v", tls.Listens)
	}
}

func TestConfiguration_ListenAddressesAnnotation(t *testing.T) {
	gw := testGateway("default", "gw", "c", 1, plainListener("web", 8080, nil))
	gw.Annotations = map[string]string{ListenAddressesAnnotation: "192.168.1.10,[::] , "}
	g := build(t, testClass("c", ControllerName, 1), gw)
	cfg := g.Configuration()
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers: %+v", cfg.Servers)
	}
	want := []contract.Listen{
		{Port: 8080, Address: "192.168.1.10"},
		{Port: 8080, Address: "[::]"},
	}
	if !reflect.DeepEqual(cfg.Servers[0].Listens, want) {
		t.Fatalf("listens = %+v, want %+v (annotation replaces the wildcard)", cfg.Servers[0].Listens, want)
	}
}

func TestConfiguration_LocationsAndUpstreams(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "zeta", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "svc", 8080),
			pathBackendRule(gatewayv1.PathMatchExact, "/exact", "svc", 8080),
		),
		testRoute("default", "alpha", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "other", 8080), // duplicate path: alpha wins (alphabetical route order)
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "other", 8080),
		),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "other", 8080, ptr(false), "10.0.0.9"),
	)
	cfg := g.Configuration()
	srv := cfg.Servers[0]
	// Deterministic first-wins on duplicate paths: routes are processed in
	// (namespace, name) order, so alpha wins /api; locations sorted by Path
	// ("/" sorts before "=").
	var paths []string
	for _, l := range srv.Locations {
		paths = append(paths, l.Path)
	}
	if !reflect.DeepEqual(paths, []string{"/", "/api/", "= /api", "= /exact"}) {
		t.Fatalf("location paths = %v", paths)
	}
	byPath := map[string]*contract.Location{}
	for _, l := range srv.Locations {
		byPath[l.Path] = l
	}
	if byPath["/api/"].Upstream != "default_other_8080" || byPath["= /api"].Upstream != "default_other_8080" {
		t.Fatalf("duplicate /api must keep the first (alpha) upstream: %+v", byPath["/api/"])
	}
	if byPath["/"].Upstream != "default_other_8080" {
		t.Fatalf("alpha root location upstream: %+v", byPath["/"])
	}
	// Path specificity beats route order for the exact location (GEP-722:
	// most specific path match wins regardless of attachment order).
	if byPath["= /exact"].Upstream != "default_svc_8080" {
		t.Fatalf("zeta exact location upstream: %+v", byPath["= /exact"])
	}
	if len(cfg.Upstreams) != 2 ||
		cfg.Upstreams[0].Name != "default_other_8080" || cfg.Upstreams[1].Name != "default_svc_8080" {
		t.Fatalf("upstreams (sorted by name): %+v", cfg.Upstreams)
	}
	for _, ep := range cfg.Upstreams[1].Endpoints {
		if ep.IP == "10.0.0.9" && ep.Ready {
			t.Fatal("not-ready endpoint must keep Ready=false")
		}
	}
}

func TestConfiguration_Deterministic(t *testing.T) {
	objs := []any{
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert"),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1", "10.0.0.2"),
	}
	a := build(t, objs...).Configuration()
	b := build(t, objs...).Configuration()
	if a.String() != b.String() {
		t.Fatalf("configuration must be deterministic:\n%s\nvs\n%s", a, b)
	}
}

func TestCertificates(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert"),
		testGateway("default", "gw1", "c", 1, tlsListener("a", 443, nil, gatewayv1.HTTPSProtocolType, nil, "cert")),
		testGateway("default", "gw2", "c", 1, tlsListener("b", 8443, nil, gatewayv1.HTTPSProtocolType, nil, "cert")),
		// missing secret: no cert desired
		testGateway("default", "gw3", "c", 1, tlsListener("c", 9443, nil, gatewayv1.HTTPSProtocolType, nil, "ghost")),
	)
	certs := g.Certificates()
	if len(certs) != 1 {
		t.Fatalf("expected 1 deduped cert, got %+v", certs)
	}
	if certs[0].Filename() != "default_cert.pem" || certs[0].Namespace != "default" || certs[0].Name != "cert" {
		t.Fatalf("cert: %+v", certs[0])
	}
}

func TestGatewayConditions_Matrix(t *testing.T) {
	mk := func(validListeners bool, apply ApplyResult) []metav1.Condition {
		g := build(t, testClass("c", ControllerName, 1),
			testGateway("default", "gw", "c", 7, plainListener("web", 80, nil)))
		gw := g.Gateways[0]
		if !validListeners {
			gw.Listeners[0].Valid = false
			gw.Listeners[0].InvalidReason = string(gatewayv1.ListenerReasonInvalid)
		}
		return gw.Conditions(apply)
	}
	get := func(conds []metav1.Condition, ctype string) metav1.Condition { return condByType(conds, ctype) }

	// success + valid
	c := get(mk(true, ApplyResultFromError(nil)), "Accepted")
	if c.Status != metav1.ConditionTrue || c.Reason != string(gatewayv1.GatewayReasonAccepted) || c.ObservedGeneration != 7 {
		t.Fatalf("accepted: %+v", c)
	}
	if c := get(mk(true, ApplyResultFromError(nil)), "Programmed"); c.Status != metav1.ConditionTrue ||
		c.Reason != string(gatewayv1.GatewayReasonProgrammed) {
		t.Fatalf("programmed: %+v", c)
	}
	// invalid listeners
	c = get(mk(false, ApplyResultFromError(nil)), "Accepted")
	if c.Status != metav1.ConditionFalse || c.Reason != string(gatewayv1.GatewayReasonListenersNotValid) {
		t.Fatalf("invalid listeners accepted: %+v", c)
	}
	if c := get(mk(false, ApplyResultFromError(nil)), "Programmed"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.GatewayReasonListenersNotValid) {
		t.Fatalf("invalid listeners programmed: %+v", c)
	}
	// nginx down → Pending
	if c := get(mk(true, ApplyResultFromError(errs.NginxNotRunning("pid file missing", nil))), "Programmed"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.GatewayReasonPending) {
		t.Fatalf("nginx down: %+v", c)
	}
	// reload failure → Invalid (§3.4)
	if c := get(mk(true, ApplyResultFromError(errs.Reload("nginx: [emerg] bad directive", nil))), "Programmed"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.GatewayReasonInvalid) {
		t.Fatalf("reload fail: %+v", c)
	}
	// include missing → Accepted False/Invalid (§2/S2)
	conds := mk(true, ApplyResultFromError(errs.IncludeMissing("/etc/nginx/conf.d/k8s-gw", nil)))
	if c := get(conds, "Accepted"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.GatewayReasonInvalid) {
		t.Fatalf("include missing accepted: %+v", c)
	}
	if c := get(conds, "Programmed"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.GatewayReasonInvalid) {
		t.Fatalf("include missing programmed: %+v", c)
	}
}

func TestListenerStatuses_Matrix(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert"),
		testGateway("default", "gw", "c", 3,
			plainListener("plain", 80, nil),
			gatewayv1.Listener{Name: "bad", Protocol: gatewayv1.HTTPProtocolType}, // port missing
			tlsListener("nocert", 443, nil, gatewayv1.HTTPSProtocolType, nil, "ghost"),
		),
		testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
	)
	gw := g.Gateways[0]
	// Simulate a conflict on "plain".
	gw.Listeners[0].Conflicted = true
	gw.Listeners[0].ConflictReason = string(gatewayv1.ListenerReasonHostnameConflict)
	gw.Listeners[0].ConflictMsg = "conflict"

	ls := gw.ListenerStatuses(ApplyResultFromError(nil))
	if len(ls) != 3 {
		t.Fatalf("listener statuses: %+v", ls)
	}
	byName := map[string]gatewayv1.ListenerStatus{}
	for _, s := range ls {
		byName[string(s.Name)] = s
	}
	// conflicted but valid listener
	plain := byName["plain"]
	if c := condByType(plain.Conditions, "Conflicted"); c.Status != metav1.ConditionTrue ||
		c.Reason != string(gatewayv1.ListenerReasonHostnameConflict) {
		t.Fatalf("plain conflicted: %+v", c)
	}
	if c := condByType(plain.Conditions, "Programmed"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.ListenerReasonPending) {
		t.Fatalf("conflicted listener programmed: %+v", c)
	}
	if plain.AttachedRoutes != 1 {
		t.Fatalf("plain attachedRoutes = %d, want 1", plain.AttachedRoutes)
	}
	// invalid listener
	bad := byName["bad"]
	if c := condByType(bad.Conditions, "Accepted"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.ListenerReasonInvalid) || c.ObservedGeneration != 3 {
		t.Fatalf("bad accepted: %+v", c)
	}
	if c := condByType(bad.Conditions, "Programmed"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.ListenerReasonInvalid) {
		t.Fatalf("bad programmed: %+v", c)
	}
	// missing certificate: ResolvedRefs False/InvalidCertificateRef, server without TLS
	nocert := byName["nocert"]
	if c := condByType(nocert.Conditions, "ResolvedRefs"); c.Status != metav1.ConditionFalse ||
		c.Reason != string(gatewayv1.ListenerReasonInvalidCertificateRef) {
		t.Fatalf("nocert resolvedrefs: %+v", c)
	}
	if c := condByType(nocert.Conditions, "Accepted"); c.Status != metav1.ConditionTrue {
		t.Fatalf("nocert accepted: %+v", c)
	}
	cfg := g.Configuration()
	for _, s := range cfg.Servers {
		for _, l := range s.Listens {
			if l.Port == 443 && l.SSL {
				t.Fatal("unresolvable cert must emit server without ssl")
			}
		}
	}
	// SupportedKinds
	if len(plain.SupportedKinds) != 1 || plain.SupportedKinds[0].Kind != "HTTPRoute" {
		t.Fatalf("supportedKinds: %+v", plain.SupportedKinds)
	}
}

func TestParentStatuses_GEP1364(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 4, nil, []gatewayv1.ParentReference{gwParent("gw")}, backendRule("ghost", 8080)),
	)
	ps := routeOf(t, g, "default", "r").ParentStatuses()
	if len(ps) != 1 {
		t.Fatalf("parents: %+v", ps)
	}
	if ps[0].ControllerName != ControllerName {
		t.Fatalf("controllerName: %q", ps[0].ControllerName)
	}
	acc := condByType(ps[0].Conditions, "Accepted")
	refs := condByType(ps[0].Conditions, "ResolvedRefs")
	if acc.Status != metav1.ConditionTrue || acc.Reason != string(gatewayv1.RouteReasonAccepted) || acc.ObservedGeneration != 4 {
		t.Fatalf("accepted: %+v", acc)
	}
	if refs.Status != metav1.ConditionFalse || refs.Reason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Fatalf("resolvedrefs: %+v", refs)
	}
}

func TestRuleTimeoutsConversion(t *testing.T) {
	req := gatewayv1.Duration("30s")
	back := gatewayv1.Duration("1m")
	rule := gatewayv1.HTTPRouteRule{
		Timeouts:    &gatewayv1.HTTPRouteTimeouts{Request: &req, BackendRequest: &back},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendOf("svc", 8080)},
	}
	got := ruleTimeouts(rule)
	if got == nil || got.Read != "30s" || got.Connect != "1m" || got.Send != "1m" {
		t.Fatalf("timeouts: %+v", got)
	}
	if bad := ruleTimeouts(gatewayv1.HTTPRouteRule{
		Timeouts: &gatewayv1.HTTPRouteTimeouts{Request: ptr(gatewayv1.Duration("500ms"))},
	}); bad != nil {
		t.Fatalf("sub-second durations are invalid GEP-1742 values: %+v", bad)
	}
}
