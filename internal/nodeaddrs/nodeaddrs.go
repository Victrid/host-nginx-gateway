// Package nodeaddrs maintains this node's address fingerprint for the
// multinode ownership model (DESIGN-multinode-addresses.md §2):
//
//   - Set is the immutable fingerprint: the node's global-unicast v4/v6
//     addresses plus the SYNTHETIC rule that the whole 127/8 loopback
//     range and ::1 are local (Linux binds any 127/8 address without
//     setup — that is what backs the controller's per-Gateway loopback
//     auto-assignment pool);
//   - Enumerate lists the addresses of the CURRENT network namespace;
//   - Prober turns enumeration into a debounced, periodically refreshed
//     fingerprint whose changes fire full reconciles.
//
// DaemonSet caveat: the controller pod does not run with hostNetwork, so
// Enumerate inside the pod sees the POD's addresses, not the node's the
// host nginx binds with. CommandProbe therefore re-executes the
// controller's own binary through `nsenter -t 1 -n` (host network
// namespace, pod mount namespace — the binary stays resolvable) with the
// PrintAddressesFlag hidden flag; that process runs Enumerate against the
// HOST's interfaces. See cmd/host-nginx-gateway/main.go.
package nodeaddrs

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// Set is an immutable node address fingerprint: the sorted, normalized
// global-unicast addresses of one node. The zero Set (and a nil *Set)
// contains no addresses; callers treat a missing fingerprint as
// "ownership unknown / own everything" (single-node semantics).
//
// The 127/8 loopback range and ::1 are implicit members of every Set
// (Contains reports them as local regardless of what enumeration saw).
type Set struct {
	addrs []string // sorted, unique, canonical (ip.String()) form
	index map[string]struct{}
}

// NewSet normalizes raw addresses (bracketed v6 accepted) into a Set.
// Invalid entries are skipped. Loopback and non-global-unicast entries
// are not stored (they are either implicit or never bindable intent).
func NewSet(addrs ...string) *Set {
	s := &Set{index: map[string]struct{}{}}
	for _, raw := range addrs {
		ip := net.ParseIP(strings.TrimSpace(stripBrackets(raw)))
		if ip == nil {
			continue
		}
		if !ip.IsGlobalUnicast() {
			continue // loopback is implicit; link-local/multicast never bindable
		}
		key := ip.String()
		if _, dup := s.index[key]; dup {
			continue
		}
		s.index[key] = struct{}{}
		s.addrs = append(s.addrs, key)
	}
	sort.Strings(s.addrs)
	return s
}

// stripBrackets removes one level of "[...]" around an address.
func stripBrackets(a string) string {
	if strings.HasPrefix(a, "[") && strings.HasSuffix(a, "]") {
		return a[1 : len(a)-1]
	}
	return a
}

// Contains reports whether addr is owned by this node: an explicit member
// of the fingerprint, or any address of the implicit 127/8 + ::1 loopback
// pool. Unparseable addresses are never owned.
func (s *Set) Contains(addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(stripBrackets(addr)))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true // the whole 127/8 range and ::1 are local on Linux
	}
	if s == nil {
		return false
	}
	_, ok := s.index[ip.String()]
	return ok
}

// Addresses returns the sorted canonical global-unicast addresses of the
// set (no loopback members — those are implicit).
func (s *Set) Addresses() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.addrs...)
}

// Equal compares two sets by membership.
func (s *Set) Equal(other *Set) bool {
	a, b := s.Addresses(), other.Addresses()
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Fingerprint returns a short stable hash of the address set for logs.
func (s *Set) Fingerprint() string {
	h := uint32(2166136261)
	for _, a := range s.Addresses() {
		for i := 0; i < len(a); i++ {
			h ^= uint32(a[i])
			h *= 16777619
		}
		h ^= ','
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}

// Enumerate lists the global-unicast IPv4/IPv6 addresses of every up
// interface in the CURRENT network namespace (DESIGN-multinode-
// addresses.md §2 "节点地址指纹"). Loopback is excluded — Set treats
// the whole 127/8 + ::1 pool as implicitly local.
func Enumerate() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("nodeaddrs: listing interfaces: %w", err)
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
				ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out, nil
}

// renderBindForm canonicalizes an IP into the nginx `listen` directive
// form: plain for IPv4, bracketed for IPv6. Used by consumers that turn
// spec.addresses values into bind addresses.
func renderBindForm(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return "[" + ip.String() + "]"
}

// RenderBindForm normalizes any parseable address string into the nginx
// listen form ("" when unparseable).
func RenderBindForm(addr string) string {
	ip := net.ParseIP(strings.TrimSpace(stripBrackets(addr)))
	if ip == nil {
		return ""
	}
	return renderBindForm(ip)
}
