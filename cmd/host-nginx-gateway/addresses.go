// status.addresses sources for the controller process (DESIGN.md §3.4):
// --publish-addresses (explicit), HNG_NODE_IP (DaemonSet downward-API env),
// then interface detection of the node's primary IP.
package main

import (
	"net"
	"os"
	"strings"
)

// parseAddressList splits a comma-separated address list ("" → nil).
func parseAddressList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// detectPublishAddresses resolves the address to advertise when the
// operator did not pass --publish-addresses:
//
//  1. HNG_NODE_IP — set by the DaemonSet chart from the downward API
//     (status.hostIP), because inside a pod the interface route sees the
//     POD's network, not the node's;
//  2. the interface route default — the primary non-loopback IPv4 (or
//     first global unicast address) of the host (non-pod runs, e.g.
//     developer testing outside a DaemonSet).
//
// An empty result means "not determinable" — status.addresses stays
// untouched rather than advertising a wrong address.
func detectPublishAddresses() []string {
	if v := strings.TrimSpace(os.Getenv("HNG_NODE_IP")); v != "" {
		return parseAddressList(v)
	}
	if ip := primaryInterfaceIP(); ip != "" {
		return []string{ip}
	}
	return nil
}

// primaryInterfaceIP picks the source address a connection to a public
// destination would use (no packets are sent — routing table lookup only).
// Falls back to the first global unicast address of any non-loopback,
// interface.
func primaryInterfaceIP() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer c.Close()
		if addr, ok := c.LocalAddr().(*net.UDPAddr); ok && !addr.IP.IsLoopback() {
			return addr.IP.String()
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			if ip.IsLoopback() || !ip.IsGlobalUnicast() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return ""
}
