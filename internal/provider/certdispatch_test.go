package provider

import (
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func serverByHostname(t *testing.T, cfg *contract.Configuration, hostname string) *contract.Server {
	t.Helper()
	for _, s := range cfg.Servers {
		if s.Hostname == hostname {
			return s
		}
	}
	t.Fatalf("no server block for hostname %q in: %v", hostname, cfg.Servers)
	return nil
}

// TestConfiguration_PerGroupCerts_TwoGateways reproduces the reported
// defect: two Gateways in different namespaces with different TLS Secrets
// sharing one socket (same explicit listen-addresses bind, same port,
// disjoint hostnames — not a §3.2 conflict) must each render their OWN
// certificate, not the first-processed listener's everywhere.
func TestConfiguration_PerGroupCerts_TwoGateways(t *testing.T) {
	argocd := testGateway("argocd", "main", "c", 1,
		tlsListener("https", 443, host("argocd.example.com"), gatewayv1.HTTPSProtocolType, nil, "argocd-tls-secret"))
	argocd.Annotations = map[string]string{ListenAddressesAnnotation: "100.64.0.5"}
	headlamp := testGateway("kube-system", "headlamp", "c", 1,
		tlsListener("https", 443, host("headlamp.example.com"), gatewayv1.HTTPSProtocolType, nil, "headlamp-tls-secret"))
	headlamp.Annotations = map[string]string{ListenAddressesAnnotation: "100.64.0.5"}
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("argocd", "argocd-tls-secret"),
		testTLSSecret("kube-system", "headlamp-tls-secret"),
		argocd, headlamp,
		// a route under argocd's listener exercises the attachment path
		testRoute("argocd", "route", 1, nil,
			[]gatewayv1.ParentReference{gwParentSection("main", "https")},
			backendRule("argocd-svc", 8080)),
		testSlice("argocd", "argocd-svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers: %+v", cfg.Servers)
	}
	if got := serverByHostname(t, cfg, "argocd.example.com").TLSCert; got != "argocd_argocd-tls-secret.pem" {
		t.Fatalf("argocd block cert = %q, want argocd_argocd-tls-secret.pem", got)
	}
	if got := serverByHostname(t, cfg, "headlamp.example.com").TLSCert; got != "kube-system_headlamp-tls-secret.pem" {
		t.Fatalf("headlamp block cert = %q, want kube-system_headlamp-tls-secret.pem", got)
	}
	// Both materialized certificates are still desired (Graph.Certificates
	// was already correct; the rendered blocks now point at both).
	certs := g.Certificates()
	if len(certs) != 2 {
		t.Fatalf("certificates: %+v", certs)
	}
	for _, li := range []*ListenerInfo{
		listenerOf(t, gatewayOf(t, g, "argocd", "main"), "https"),
		listenerOf(t, gatewayOf(t, g, "kube-system", "headlamp"), "https"),
	} {
		if li.Conflicted {
			t.Fatalf("listener %q must not be conflicted: %s", li.Spec.Name, li.ConflictMsg)
		}
	}
}

// TestConfiguration_PerGroupCerts_SameGateway: one Gateway, two HTTPS
// listeners on the same port with disjoint hostnames and different Secrets
// — each block carries its own basename.
func TestConfiguration_PerGroupCerts_SameGateway(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert-a"),
		testTLSSecret("default", "cert-b"),
		testGateway("default", "gw", "c", 1,
			tlsListener("a", 443, host("a.example.com"), gatewayv1.HTTPSProtocolType, nil, "cert-a"),
			tlsListener("b", 443, host("b.example.com"), gatewayv1.HTTPSProtocolType, nil, "cert-b"),
		),
	)
	cfg := g.Configuration()
	if got := serverByHostname(t, cfg, "a.example.com").TLSCert; got != "default_cert-a.pem" {
		t.Fatalf("a block cert = %q, want default_cert-a.pem", got)
	}
	if got := serverByHostname(t, cfg, "b.example.com").TLSCert; got != "default_cert-b.pem" {
		t.Fatalf("b block cert = %q, want default_cert-b.pem", got)
	}
}

// TestConfiguration_PerGroupCerts_CrossNS: two listeners on one socket
// whose certificateRefs point into different namespaces via ReferenceGrants
// — each block renders the cert of its own (cross-namespace) Secret.
func TestConfiguration_PerGroupCerts_CrossNS(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1,
			tlsListener("a", 443, host("a.example.com"), gatewayv1.HTTPSProtocolType, ptr(gatewayv1.Namespace("certs-a")), "cert-a"),
			tlsListener("b", 443, host("b.example.com"), gatewayv1.HTTPSProtocolType, ptr(gatewayv1.Namespace("certs-b")), "cert-b"),
		),
		testTLSSecret("certs-a", "cert-a"),
		testTLSSecret("certs-b", "cert-b"),
		testGrant("certs-a",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "Gateway", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", "cert-a")}),
		testGrant("certs-b",
			[]gatewayv1.ReferenceGrantFrom{grantFrom(gwAPIGroup, "Gateway", "default")},
			[]gatewayv1.ReferenceGrantTo{grantTo("", "Secret", "cert-b")}),
	)
	cfg := g.Configuration()
	if got := serverByHostname(t, cfg, "a.example.com").TLSCert; got != "certs-a_cert-a.pem" {
		t.Fatalf("a block cert = %q, want certs-a_cert-a.pem", got)
	}
	if got := serverByHostname(t, cfg, "b.example.com").TLSCert; got != "certs-b_cert-b.pem" {
		t.Fatalf("b block cert = %q, want certs-b_cert-b.pem", got)
	}
}

// TestConfiguration_CertMismatchConflict: the backstop. An exact listener
// and a covering wildcard listener are distinguishable per §3.2 (no
// conflict there), but a route hostname refining to the exact host makes
// BOTH claim the same effective hostname group with different certificates.
// Both listeners must be Conflicted (HostnameConflict, mismatch named) and
// neither may leak a server block.
func TestConfiguration_CertMismatchConflict(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert-a"),
		testTLSSecret("default", "cert-b"),
		testGateway("default", "gw", "c", 1,
			tlsListener("exact", 443, host("svc.example.com"), gatewayv1.HTTPSProtocolType, nil, "cert-a"),
			tlsListener("wild", 443, host("*.example.com"), gatewayv1.HTTPSProtocolType, nil, "cert-b"),
		),
		testRoute("default", "r", 1, []gatewayv1.Hostname{"svc.example.com"},
			[]gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	gw := gatewayOf(t, g, "default", "gw")
	exact := listenerOf(t, gw, "exact")
	wild := listenerOf(t, gw, "wild")
	for _, li := range []*ListenerInfo{exact, wild} {
		if !li.Conflicted || li.ConflictReason != string(gatewayv1.ListenerReasonHostnameConflict) {
			t.Fatalf("listener %q: got Conflicted=%v reason=%q, want HostnameConflict",
				li.Spec.Name, li.Conflicted, li.ConflictReason)
		}
	}
	for _, li := range []*ListenerInfo{exact, wild} {
		if !strings.Contains(li.ConflictMsg, "default_cert-a.pem") ||
			!strings.Contains(li.ConflictMsg, "default_cert-b.pem") {
			t.Fatalf("listener %q conflict message must name the cert mismatch: %q",
				li.Spec.Name, li.ConflictMsg)
		}
	}
	if got := cfg.Servers; len(got) != 0 {
		t.Fatalf("no server blocks may be generated for cert-conflicted listeners: %+v", got)
	}
}

// TestConfiguration_PerGroupCerts_SingleCertUnchanged: single-certificate
// setups are unchanged — every block on the socket (including the
// catch-all default block of a hostname-less listener) carries the one cert.
func TestConfiguration_PerGroupCerts_SingleCertUnchanged(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testTLSSecret("default", "cert"),
		testGateway("default", "gw", "c", 1,
			tlsListener("all", 443, nil, gatewayv1.HTTPSProtocolType, nil, "cert"),
			tlsListener("a", 443, host("a.example.com"), gatewayv1.HTTPSProtocolType, nil, "cert"),
		),
		testRoute("default", "r", 1, nil,
			[]gatewayv1.ParentReference{gwParentSection("gw", "a")}, backendRule("svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	cfg := g.Configuration()
	if len(cfg.Servers) != 2 || cfg.Servers[0].Hostname != "" {
		// catch-all default block first, named block second
		t.Fatalf("servers: %+v", cfg.Servers)
	}
	for _, s := range cfg.Servers {
		if s.TLSCert != "default_cert.pem" {
			t.Fatalf("block %q cert = %q, want default_cert.pem", s.Hostname, s.TLSCert)
		}
	}
	for _, li := range gatewayOf(t, g, "default", "gw").Listeners {
		if li.Conflicted {
			t.Fatalf("listener %q must not be conflicted: %s", li.Spec.Name, li.ConflictMsg)
		}
	}
}
