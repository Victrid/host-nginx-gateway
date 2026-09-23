// Unit tests for Gateway status.addresses reporting (Gateway API v1,
// GatewayStatus.addresses: "the network addresses that have been assigned
// to the Gateway", DESIGN.md §3.4) and for the cross-Gateway loopback
// auto-assignment that feeds it.
package provider

import (
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestStatusAddresses_Precedence(t *testing.T) {
	annotated := testGateway("default", "gw", "c", 1, plainListener("web", 80, host("a.com")))
	annotated.Annotations = map[string]string{PublishAddressesAnnotation: "203.0.113.7"}
	plain := testGateway("default", "gw3", "c", 1, plainListener("web", 80, host("c.com")))

	cases := []struct {
		gw       *gatewayv1.Gateway
		autoAddr string
		explicit []string
		fallback []string
		want     []string
	}{
		// 1. The Gateway annotation wins over everything.
		{annotated, "", []string{"198.51.100.1"}, []string{"192.0.2.9"}, []string{"203.0.113.7"}},
		// 2. Then the explicit --publish-addresses list.
		{plain, "", []string{"198.51.100.1"}, []string{"192.0.2.9"}, []string{"198.51.100.1"}},
		// 3. Then the auto-assigned loopback (the truth of where it binds).
		{plain, "127.0.0.42", nil, []string{"192.0.2.9"}, []string{"127.0.0.42"}},
		// 4. Then the fallback (detected node IP).
		{plain, "", nil, []string{"192.0.2.9"}, []string{"192.0.2.9"}},
		// 5. Nothing known → leave status untouched (nil).
		{plain, "", nil, nil, nil},
	}
	for i, tc := range cases {
		info := &GatewayInfo{Resource: tc.gw, AutoAddress: tc.autoAddr}
		got := info.StatusAddresses(tc.explicit, tc.fallback)
		if tc.want == nil {
			if got != nil {
				t.Fatalf("case %d: expected nil, got %+v", i, got)
			}
			continue
		}
		if len(got) != len(tc.want) {
			t.Fatalf("case %d: got %+v", i, got)
		}
		for j := range got {
			if got[j].Value != tc.want[j] || got[j].Type == nil || *got[j].Type != gatewayv1.IPAddressType {
				t.Fatalf("case %d entry %d: %+v", i, j, got[j])
			}
		}
	}
}

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

func TestAutoAssign_ListenAddressesAnnotationWins(t *testing.T) {
	gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, nil))
	gw.Annotations = map[string]string{ListenAddressesAnnotation: "192.168.1.10"}
	other := testGateway("default", "other", "c", 1, plainListener("web", 80, nil))
	g := build(t, testClass("c", ControllerName, 1), gw, other)
	annotated := gatewayOf(t, g, "default", "gw")
	if annotated.AutoAddress != "" {
		t.Fatalf("explicitly annotated gateway must keep its binds, got %q", annotated.AutoAddress)
	}
	if li := listenerOf(t, annotated, "web"); len(li.Addresses) != 1 || li.Addresses[0] != "192.168.1.10" {
		t.Fatalf("annotation binds: %+v", li.Addresses)
	}
	// The wildcard peer is separated onto its own loopback instead of
	// conflicting with the annotated gateway.
	if li := listenerOf(t, gatewayOf(t, g, "default", "other"), "web"); li.Conflicted {
		t.Fatalf("peer must be auto-separated, got conflicted: %s", li.ConflictMsg)
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
