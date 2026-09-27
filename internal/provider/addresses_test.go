// Unit tests for the multinode address model (DESIGN-multinode-addresses.md
// §2/§3/§4): spec.addresses as binding intent, node-fingerprint
// intersection (owned / not-owned / wildcard / partial / Hostname-type
// rejection), the status.addresses derivation from rendered listens, and
// the cross-Gateway loopback auto-assignment that feeds it.
package provider

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/nodeaddrs"
)

// specAddrs builds GatewaySpecAddress values of the given type.
func specAddrs(t gatewayv1.AddressType, values ...string) []gatewayv1.GatewaySpecAddress {
	out := make([]gatewayv1.GatewaySpecAddress, 0, len(values))
	for _, v := range values {
		tt := t
		out = append(out, gatewayv1.GatewaySpecAddress{Type: &tt, Value: v})
	}
	return out
}

// ipAddrs is the shorthand for IPAddress-type spec.addresses.
func ipAddrs(values ...string) []gatewayv1.GatewaySpecAddress {
	return specAddrs(gatewayv1.IPAddressType, values...)
}

// addrGateway builds a one-listener Gateway carrying spec.addresses.
func addrGateway(ns, name string, addrs []gatewayv1.GatewaySpecAddress) *gatewayv1.Gateway {
	gw := testGateway(ns, name, "c", 1, plainListener("web", 80, nil))
	gw.Spec.Addresses = addrs
	return gw
}

// buildWithNode is build() with GraphOptions (the node fingerprint).
func buildWithNode(t *testing.T, owned *nodeaddrs.Set, objs ...any) *Graph {
	t.Helper()
	res := &Resources{}
	addObjects(t, res, objs...)
	return BuildGraph(res, GraphOptions{NodeAddresses: owned})
}

// ---------------------------------------------------------------------------
// spec.addresses parsing (binding intent)
// ---------------------------------------------------------------------------

func TestSpecAddresses_Parsing(t *testing.T) {
	gw := addrGateway("default", "gw", ipAddrs("192.168.1.10", "[fd00::1]", " 10.0.0.5 ", "192.168.1.10"))
	g := build(t, testClass("c", ControllerName, 1), gw)
	li := listenerOf(t, gatewayOf(t, g, "default", "gw"), "web")
	want := []string{"192.168.1.10", "[fd00::1]", "10.0.0.5"} // spec order, deduped, listen form
	if !reflect.DeepEqual(li.Addresses, want) {
		t.Fatalf("addresses = %v, want %v", li.Addresses, want)
	}
	if !li.Owned {
		t.Fatal("no fingerprint (nil set): everything is owned")
	}
}

func TestSpecAddresses_HostnameTypeRejected(t *testing.T) {
	gw := addrGateway("default", "gw", specAddrs(gatewayv1.HostnameAddressType, "gw.example.com"))
	g := build(t, testClass("c", ControllerName, 1), gw)
	li := listenerOf(t, gatewayOf(t, g, "default", "gw"), "web")
	if li.Valid {
		t.Fatal("Hostname-type spec.addresses must invalidate the listener (no silent ignore)")
	}
	if li.InvalidReason != string(gatewayv1.ListenerReasonInvalid) {
		t.Fatalf("reason = %q, want Invalid", li.InvalidReason)
	}
	if !strings.Contains(li.InvalidMsg, "Hostname") || !strings.Contains(li.InvalidMsg, "IPAddress") {
		t.Fatalf("message must name the type problem: %q", li.InvalidMsg)
	}
	// The invalid listener IS ours to report: Accepted=False surfaces in
	// the listener conditions (GWA spec: invalid addresses MUST be
	// indicated in conditions).
	ls := gatewayOf(t, g, "default", "gw").ListenerStatuses(applyOK())
	if len(ls) != 1 {
		t.Fatalf("listener statuses = %+v", ls)
	}
	if got := condByType(ls[0].Conditions, "Accepted"); got.Status != metav1.ConditionFalse ||
		got.Reason != string(gatewayv1.ListenerReasonInvalid) {
		t.Fatalf("accepted = %+v", got)
	}
	// Mixed valid + Hostname entries: the whole bind intent is refused.
	mixed := addrGateway("default", "mixed", append(ipAddrs("192.168.1.10"), specAddrs(gatewayv1.HostnameAddressType, "gw.example.com")...))
	g2 := build(t, testClass("c", ControllerName, 1), mixed)
	if li := listenerOf(t, gatewayOf(t, g2, "default", "mixed"), "web"); li.Valid {
		t.Fatal("mixed address types must invalidate the listener")
	}
}

func TestSpecAddresses_InvalidIPValueRejected(t *testing.T) {
	gw := addrGateway("default", "gw", ipAddrs("not-an-ip"))
	g := build(t, testClass("c", ControllerName, 1), gw)
	li := listenerOf(t, gatewayOf(t, g, "default", "gw"), "web")
	if li.Valid || li.InvalidReason != string(gatewayv1.ListenerReasonInvalid) {
		t.Fatalf("invalid IP value must reject the listener: %+v", li)
	}
	if !strings.Contains(li.InvalidMsg, "not-an-ip") {
		t.Fatalf("message must name the bad value: %q", li.InvalidMsg)
	}
}

// ---------------------------------------------------------------------------
// Address-intersection matrix (owned / not-owned / wildcard / partial)
// ---------------------------------------------------------------------------

func TestNodeFilter_IntersectionMatrix(t *testing.T) {
	nodeA := nodeaddrs.NewSet("192.0.2.10", "2001:db8::1")
	cases := []struct {
		name        string
		addrs       []gatewayv1.GatewaySpecAddress
		owned       bool
		wantBinds   []string
		wantSkipped []string
	}{
		{"owned full", ipAddrs("192.0.2.10"), true, []string{"192.0.2.10"}, nil},
		{"owned v6 bracket form", ipAddrs("[2001:db8::1]"), true, []string{"[2001:db8::1]"}, nil},
		{"not owned", ipAddrs("198.51.100.7"), false, nil, []string{"198.51.100.7"}},
		{"partial: render intersection only", ipAddrs("192.0.2.10", "198.51.100.7"), true,
			[]string{"192.0.2.10"}, nil},
		{"wildcard: no spec.addresses", nil, true, nil, nil},
		{"loopback always owned", ipAddrs("127.0.0.77"), true, []string{"127.0.0.77"}, nil},
		{"::1 always owned", ipAddrs("::1"), true, []string{"[::1]"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := buildWithNode(t, nodeA, testClass("c", ControllerName, 1), addrGateway("default", "gw", tc.addrs))
			li := listenerOf(t, gatewayOf(t, g, "default", "gw"), "web")
			if li.Owned != tc.owned {
				t.Fatalf("owned = %v, want %v", li.Owned, tc.owned)
			}
			if !reflect.DeepEqual(li.Addresses, tc.wantBinds) {
				t.Fatalf("effective binds = %v, want %v", li.Addresses, tc.wantBinds)
			}
			if !reflect.DeepEqual(li.SkippedAddresses, tc.wantSkipped) {
				t.Fatalf("skipped addresses = %v, want %v", li.SkippedAddresses, tc.wantSkipped)
			}
		})
	}
}

func TestNodeFilter_NotOwnedListenerRendersNothing(t *testing.T) {
	nodeA := nodeaddrs.NewSet("192.0.2.10")
	route := testRoute("default", "r", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
		pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080))
	g := buildWithNode(t, nodeA,
		testClass("c", ControllerName, 1),
		addrGateway("default", "gw", ipAddrs("198.51.100.7")),
		route,
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	)
	if servers := g.Configuration().Servers; len(servers) != 0 {
		t.Fatalf("not-owned listener must render no server block, got %d", len(servers))
	}
	gw := gatewayOf(t, g, "default", "gw")
	if gw.OwnedHere() {
		t.Fatal("gateway owning zero listeners must not be owned here")
	}
	if ls := gw.ListenerStatuses(applyOK()); len(ls) != 0 {
		t.Fatalf("not-owned listener must produce no status entry, got %+v", ls)
	}
	// The route still attaches at spec level (its status is not
	// node-sharded in the MVP) — but nothing renders for it here.
	if li := listenerOf(t, gw, "web"); len(li.Attachments) != 1 {
		t.Fatalf("route attachment is spec-level: %+v", li.Attachments)
	}
}

func TestNodeFilter_PartialIntersectionRendersIntersectionOnly(t *testing.T) {
	nodeA := nodeaddrs.NewSet("10.0.0.1")
	g := buildWithNode(t, nodeA,
		testClass("c", ControllerName, 1),
		addrGateway("default", "gw", ipAddrs("192.0.2.10", "10.0.0.1")),
	)
	cfg := g.Configuration()
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers: %+v", cfg.Servers)
	}
	want := []contract.Listen{{Port: 80, Address: "10.0.0.1"}}
	if !reflect.DeepEqual(cfg.Servers[0].Listens, want) {
		t.Fatalf("listens = %+v, want %+v (intersection only)", cfg.Servers[0].Listens, want)
	}
}

func TestNodeFilter_WildcardOwnedOnEveryNode(t *testing.T) {
	// A wildcard listener is owned even by a node whose fingerprint has
	// no overlap with anything — every node serves the wildcard.
	weirdNode := nodeaddrs.NewSet("203.0.113.99")
	g := buildWithNode(t, weirdNode, testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)))
	li := listenerOf(t, gatewayOf(t, g, "default", "gw"), "web")
	if !li.Owned || len(li.Addresses) != 0 {
		t.Fatalf("wildcard listener must stay wildcard-owned: owned=%v addrs=%v", li.Owned, li.Addresses)
	}
	if len(g.Configuration().Servers) != 1 {
		t.Fatal("wildcard listener must render its (empty) server block")
	}
}

// The conformance harness's auto-assigned loopback binds (127.0.0.N) must
// stay owned on every node: the whole 127/8 is implicitly local.
func TestNodeFilter_LoopbackAutoAssignPreserved(t *testing.T) {
	nodeA := nodeaddrs.NewSet("192.0.2.10")
	g := buildWithNode(t, nodeA,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw1", "c", 1, plainListener("web", 80, nil)),
		testGateway("default", "gw2", "c", 1, plainListener("web", 80, nil)),
	)
	for _, name := range []string{"gw1", "gw2"} {
		gw := gatewayOf(t, g, "default", name)
		li := listenerOf(t, gw, "web")
		if !li.Owned || len(li.Addresses) != 1 || !strings.HasPrefix(li.Addresses[0], "127.0.0.") {
			t.Fatalf("%s auto-assign broken under node filter: owned=%v addrs=%v", name, li.Owned, li.Addresses)
		}
		if !gw.OwnedHere() {
			t.Fatalf("%s must stay owned (status writes continue)", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Effective-bindset conflicts (§4 of the multinode design)
// ---------------------------------------------------------------------------

func TestConflicts_CrossNodeDisjointEffectiveAddressesCoexist(t *testing.T) {
	// Two Gateways, same port+hostname, addresses pinned to different
	// nodes. On THIS node (holding 192.0.2.10) the first renders, the
	// second is skipped — no conflict, no cross-node status pollution.
	nodeA := nodeaddrs.NewSet("192.0.2.10")
	g := buildWithNode(t, nodeA,
		testClass("c", ControllerName, 1),
		addrGateway("default", "gwa", ipAddrs("192.0.2.10")),
		addrGateway("default", "gwb", ipAddrs("198.51.100.7")),
	)
	for _, name := range []string{"gwa", "gwb"} {
		if li := listenerOf(t, gatewayOf(t, g, "default", name), "web"); li.Conflicted {
			t.Fatalf("%s must not conflict across nodes: %s", name, li.ConflictMsg)
		}
	}
	if !gatewayOf(t, g, "default", "gwa").OwnedHere() {
		t.Fatal("gwa is owned here")
	}
	if gatewayOf(t, g, "default", "gwb").OwnedHere() {
		t.Fatal("gwb belongs to another node — no status writes from here")
	}
	// Same-node conflict rules are unchanged: two owned listeners with
	// overlapping effective binds and equal hostnames still conflict.
	collide := buildWithNode(t, nodeA,
		testClass("c", ControllerName, 1),
		addrGateway("default", "gw1", ipAddrs("192.0.2.10")),
		addrGateway("default", "gw2", ipAddrs("192.0.2.10", "10.0.0.1")),
	)
	for _, name := range []string{"gw1", "gw2"} {
		li := listenerOf(t, gatewayOf(t, collide, "default", name), "web")
		if !li.Conflicted || li.ConflictReason != string(gatewayv1.ListenerReasonHostnameConflict) {
			t.Fatalf("%s same-node overlapping binds must conflict: %+v", name, li)
		}
	}
}

// ---------------------------------------------------------------------------
// status.addresses derivation (§3)
// ---------------------------------------------------------------------------

func TestStatusAddresses_Derivation(t *testing.T) {
	plain := testGateway("default", "gw", "c", 1, plainListener("web", 80, host("c.com")))
	auto := testGateway("default", "auto", "c", 1, plainListener("web", 80, nil))

	cases := []struct {
		name     string
		info     func() *GatewayInfo
		explicit []string
		fallback []string
		want     []string
	}{
		{
			// 1. --publish-addresses stays the external override.
			name: "explicit flag wins",
			info: func() *GatewayInfo {
				return &GatewayInfo{Resource: plain}
			},
			explicit: []string{"198.51.100.1"},
			fallback: []string{"192.0.2.9"},
			want:     []string{"198.51.100.1"},
		},
		{
			// 2. spec.addresses owned → the rendered binds.
			name: "rendered binds",
			info: func() *GatewayInfo {
				gw := addrGateway("default", "pinned", ipAddrs("192.0.2.10", "10.0.0.1"))
				return &GatewayInfo{Resource: gw, Listeners: []*ListenerInfo{{
					Gateway: nil, Spec: gw.Spec.Listeners[0], Valid: true, Owned: true,
					Addresses: []string{"192.0.2.10"},
				}}}
			},
			fallback: []string{"192.0.2.9"},
			want:     []string{"192.0.2.10"},
		},
		{
			// 3. Auto-assigned loopback: the rendered (bound) address.
			name: "auto loopback mirrors the bind",
			info: func() *GatewayInfo {
				g := build(t, testClass("c", ControllerName, 1), auto,
					testGateway("default", "other", "c", 1, plainListener("web", 80, nil)))
				return gatewayOf(t, g, "default", "auto")
			},
			want: nil, // filled below from AutoAddress
		},
		{
			// 4. Wildcard bind → the fallback (node primary IP).
			name: "wildcard falls back to node IP",
			info: func() *GatewayInfo {
				return &GatewayInfo{Resource: plain, Listeners: []*ListenerInfo{{
					Gateway: nil, Spec: plain.Spec.Listeners[0], Valid: true, Owned: true,
				}}}
			},
			fallback: []string{"192.0.2.9"},
			want:     []string{"192.0.2.9"},
		},
		{
			// 5. Nothing owned/rendered → leave status untouched.
			name: "empty stays empty",
			info: func() *GatewayInfo {
				return &GatewayInfo{Resource: plain, Listeners: []*ListenerInfo{{
					Gateway: nil, Spec: plain.Spec.Listeners[0], Valid: true, Owned: false,
				}}}
			},
			want: nil,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := tc.info()
			got := info.StatusAddresses(tc.explicit, tc.fallback)
			want := tc.want
			if want == nil && tc.name == "auto loopback mirrors the bind" {
				if info.AutoAddress == "" {
					t.Fatal("auto address missing")
				}
				want = []string{info.AutoAddress}
			}
			if want == nil {
				if got != nil {
					t.Fatalf("case %d: expected nil, got %+v", i, got)
				}
				return
			}
			if len(got) != len(want) {
				t.Fatalf("case %d: got %+v, want %v", i, got, want)
			}
			for j := range got {
				if got[j].Value != want[j] || got[j].Type == nil || *got[j].Type != gatewayv1.IPAddressType {
					t.Fatalf("case %d entry %d: %+v (want %s)", i, j, got[j], want[j])
				}
			}
		})
	}
}

func TestStatusAddresses_WritePathIntegration(t *testing.T) {
	// End-to-end inside the graph: indistinct gateways end up with
	// status.addresses equal to their auto-assigned (bound) addresses.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw1", "c", 1, plainListener("web", 80, host("example.com"))),
		testGateway("default", "gw2", "c", 1, plainListener("web", 80, host("example.com"))),
	)
	for _, name := range []string{"gw1", "gw2"} {
		gw := gatewayOf(t, g, "default", name)
		addrs := gw.StatusAddresses(nil, nil)
		if len(addrs) != 1 || addrs[0].Value != gw.AutoAddress {
			t.Fatalf("%s status.addresses %+v must mirror the auto-assigned bind %q",
				name, addrs, gw.AutoAddress)
		}
	}
}

func TestRemovedAnnotations_HaveNoEffect(t *testing.T) {
	// v0.3.0 cut the listen-addresses / publish-addresses annotations:
	// neither spelling influences binds, status.addresses or warnings.
	gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, nil))
	gw.Annotations = map[string]string{
		"hng.victrid.dev/listen-addresses":     "192.168.1.10",
		"hng.victrid.dev/publish-addresses":    "203.0.113.9",
		"gateway.host-nginx/listen-addresses":  "10.0.0.1",
		"gateway.host-nginx/publish-addresses": "198.51.100.1",
	}
	g := build(t, testClass("c", ControllerName, 1), gw)
	info := gatewayOf(t, g, "default", "gw")
	if li := listenerOf(t, info, "web"); len(li.Addresses) != 0 {
		t.Fatalf("listen-addresses annotation must be ignored, got binds %v", li.Addresses)
	}
	if addrs := info.StatusAddresses(nil, nil); addrs != nil {
		t.Fatalf("publish-addresses annotation must be ignored, got %+v", addrs)
	}
	for _, w := range g.Warnings {
		if strings.Contains(w, "listen-addresses") || strings.Contains(w, "publish-addresses") {
			t.Fatalf("removed annotations must not produce warnings: %v", g.Warnings)
		}
	}
}

// ---------------------------------------------------------------------------
// Auto-assignment stability (unchanged behavior under the new model)
// ---------------------------------------------------------------------------

func TestAutoAssign_StableUnderUnrelatedChanges(t *testing.T) {
	mk := func(extra ...*gatewayv1.Gateway) []any {
		objs := []any{testClass("c", ControllerName, 1),
			testGateway("default", "gw-a", "c", 1, plainListener("web", 80, nil)),
			testGateway("default", "gw-b", "c", 1, plainListener("web", 80, nil)),
		}
		for _, e := range extra {
			objs = append(objs, e)
		}
		return objs
	}
	base := build(t, mk()...)
	addrA := gatewayOf(t, base, "default", "gw-a").AutoAddress
	addrB := gatewayOf(t, base, "default", "gw-b").AutoAddress
	if addrA == "" || addrB == "" || addrA == addrB {
		t.Fatalf("indistinct gateways must receive distinct addresses: %q %q", addrA, addrB)
	}
	if !strings.HasPrefix(addrA, "127.0.0.") {
		t.Fatalf("loopback pool expected, got %q", addrA)
	}

	// Adding a third gateway must NOT shift the existing assignments
	// (bind churn mid-flight would break in-flight traffic).
	extended := build(t, mk(testGateway("default", "gw-c", "c", 1, plainListener("web", 80, nil)))...)
	if got := gatewayOf(t, extended, "default", "gw-a").AutoAddress; got != addrA {
		t.Fatalf("gw-a address shifted: %q → %q", addrA, got)
	}
	if got := gatewayOf(t, extended, "default", "gw-b").AutoAddress; got != addrB {
		t.Fatalf("gw-b address shifted: %q → %q", addrB, got)
	}
	// The new gateway gets its own distinct address.
	if got := gatewayOf(t, extended, "default", "gw-c").AutoAddress; got == "" || got == addrA || got == addrB {
		t.Fatalf("gw-c address %q", got)
	}

	// Removing gw-b keeps gw-a stable, too.
	without := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw-a", "c", 1, plainListener("web", 80, nil)),
		testGateway("default", "gw-c", "c", 1, plainListener("web", 80, nil)),
	)
	if got := gatewayOf(t, without, "default", "gw-a").AutoAddress; got != addrA {
		t.Fatalf("gw-a address shifted after deletion: %q → %q", addrA, got)
	}
}

func TestAutoAssign_SpecAddressesWin(t *testing.T) {
	gw := addrGateway("default", "gw", ipAddrs("192.168.1.10"))
	other := testGateway("default", "other", "c", 1, plainListener("web", 80, nil))
	g := build(t, testClass("c", ControllerName, 1), gw, other)
	pinned := gatewayOf(t, g, "default", "gw")
	if pinned.AutoAddress != "" {
		t.Fatalf("explicitly pinned gateway must keep its binds, got %q", pinned.AutoAddress)
	}
	if li := listenerOf(t, pinned, "web"); len(li.Addresses) != 1 || li.Addresses[0] != "192.168.1.10" {
		t.Fatalf("spec binds: %+v", li.Addresses)
	}
	// The wildcard peer is separated onto its own loopback instead of
	// conflicting with the pinned gateway.
	if li := listenerOf(t, gatewayOf(t, g, "default", "other"), "web"); li.Conflicted {
		t.Fatalf("peer must be auto-separated, got conflicted: %s", li.ConflictMsg)
	}
}

// applyOK is the success ApplyResult used by status-shape assertions.
func applyOK() ApplyResult {
	return ApplyResultFromError(nil)
}
