package nodeaddrs

import (
	"testing"
)

func TestSet_Contains(t *testing.T) {
	s := NewSet("192.0.2.10", "2001:db8::1", "[2001:db8::2]", "192.0.2.10", "not-an-ip", "127.0.0.1", "fe80::1")
	cases := []struct {
		addr string
		want bool
	}{
		// Explicit members (any spelling normalizes).
		{"192.0.2.10", true},
		{" 192.0.2.10 ", true},
		{"2001:db8::1", true},
		{"[2001:db8::2]", true},
		{"2001:db8::2", true},
		// The whole 127/8 loopback range is implicitly local — this is
		// what keeps the conformance harness's auto-assigned binds
		// (127.0.0.8 … 127.0.0.239) owned on every node.
		{"127.0.0.1", true},
		{"127.0.0.8", true},
		{"127.255.255.254", true},
		{"::1", true},
		// Non-members.
		{"198.51.100.7", false},
		{"fd00::99", false},
		{"0.0.0.0", false},
		{"garbage", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := s.Contains(tc.addr); got != tc.want {
			t.Errorf("Contains(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
	// Nil set: loopback is still local (a property of the OS, not of the
	// fingerprint); explicit membership owns nothing. Callers treat a nil
	// set as "no fingerprint" and skip filtering entirely.
	var nilSet *Set
	if nilSet.Contains("192.0.2.10") {
		t.Error("nil set must not own explicit addresses")
	}
	if !nilSet.Contains("127.0.0.9") {
		t.Error("the 127/8 loopback pool is local on every Linux node, fingerprint or not")
	}
}

func TestSet_NormalizationAndOrder(t *testing.T) {
	s := NewSet("10.0.0.2", "10.0.0.1", "10.0.0.2")
	want := []string{"10.0.0.1", "10.0.0.2"}
	got := s.Addresses()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Addresses() = %v, want %v (deduped, sorted)", got, want)
	}
	// Loopback and non-global-unicast inputs are not stored (loopback is
	// implicit, link-local never bindable).
	s2 := NewSet("127.0.0.5", "fe80::1", "ff02::1", "0.0.0.0", "192.0.2.1")
	if a := s2.Addresses(); len(a) != 1 || a[0] != "192.0.2.1" {
		t.Fatalf("Addresses() = %v, want [192.0.2.1]", a)
	}
}

func TestSet_EqualAndFingerprint(t *testing.T) {
	a := NewSet("10.0.0.1", "10.0.0.2")
	b := NewSet("10.0.0.2", "10.0.0.1") // order-insensitive
	c := NewSet("10.0.0.1")
	if !a.Equal(b) {
		t.Fatal("same membership must be Equal regardless of input order")
	}
	if a.Equal(c) {
		t.Fatal("different membership must not be Equal")
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("fingerprint must be stable under input order")
	}
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatal("different sets must have different fingerprints")
	}
}

func TestRenderBindForm(t *testing.T) {
	cases := map[string]string{
		"192.0.2.1":      "192.0.2.1",
		" 192.0.2.1 ":    "192.0.2.1",
		"fd00::1":        "[fd00::1]",
		"[fd00::1]":      "[fd00::1]",
		"::1":            "[::1]",
		"not-an-ip":      "",
		"":               "",
		"192.0.2.1:8080": "", // host:port is not a bare address
	}
	for in, want := range cases {
		if got := RenderBindForm(in); got != want {
			t.Errorf("RenderBindForm(%q) = %q, want %q", in, got, want)
		}
	}
}
