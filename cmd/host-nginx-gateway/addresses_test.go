// Tests for the status.addresses source resolution (DESIGN.md §3.4):
// --publish-addresses flag, HNG_NODE_IP downward-API env (DaemonSet form)
// and interface detection fallback.
package main

import (
	"reflect"
	"testing"
)

func TestParseAddressList(t *testing.T) {
	cases := map[string][]string{
		"":                    nil,
		"   ":                 nil,
		"127.0.0.1":           {"127.0.0.1"},
		"10.0.0.1, 10.0.0.2 ": {"10.0.0.1", "10.0.0.2"},
		"1.2.3.4,,5.6.7.8":    {"1.2.3.4", "5.6.7.8"},
	}
	for in, want := range cases {
		if got := parseAddressList(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("parseAddressList(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDetectPublishAddresses_EnvWins(t *testing.T) {
	t.Setenv("HNG_NODE_IP", "192.0.2.7")
	got := detectPublishAddresses()
	if len(got) != 1 || got[0] != "192.0.2.7" {
		t.Fatalf("HNG_NODE_IP must be the DaemonSet source: %v", got)
	}
}

func TestDetectPublishAddresses_InterfaceFallback(t *testing.T) {
	t.Setenv("HNG_NODE_IP", "")
	got := detectPublishAddresses()
	// Environment-dependent: on a host without any non-loopback global
	// address the result is legitimately empty; otherwise it must be a
	// single parseable address that is not loopback.
	if len(got) == 0 {
		return
	}
	if len(got) != 1 || got[0] == "" || got[0] == "127.0.0.1" {
		t.Fatalf("detected address must be the primary non-loopback IP: %v", got)
	}
}
