// Package reachability implements the startup ClusterIP-reachability
// probe (DESIGN-cluster-proxy.md §5/§0.11): the K8s spec does not
// guarantee that a HOST network namespace can reach Service ClusterIPs
// (k3s happens to allow it; eBPF CNIs / NetworkPolicies may not). The
// controller probes once at startup by dialing the apiserver Service
// (KUBERNETES_SERVICE_HOST:KUBERNETES_SERVICE_PORT — a ClusterIP-backed
// address every cluster has) FROM THE HOST network namespace, re-using
// the nsenter self re-exec machinery of internal/nodeaddrs.
//
// The probe is informational only: unreachable + proxy mode off logs a
// warning with enablement guidance; no behavior change, no status
// conditions (design §0.11 — no auto-switching).
package reachability

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProbeFlag is the hidden CLI flag the probe child runs with. The child
// dials KUBERNETES_SERVICE_HOST:KUBERNETES_SERVICE_PORT in its CURRENT
// network namespace (the parent runs it inside the host netns via
// `nsenter -t 1 -n`) and exits 0 (reachable) / 1 (unreachable).
const ProbeFlag = "--probe-clusterip-reachability"

// dialTimeout bounds one probe dial. ClusterIP DNAT answers SYN
// immediately when the path exists; 3s is generous without slowing
// startup.
const dialTimeout = 3 * time.Second

// Child implements the probe child: dial the apiserver Service address
// from the CURRENT network namespace using the pod's env vars (nsenter
// preserves the environment). Returns nil when reachable.
func Child() error {
	host := envOr("KUBERNETES_SERVICE_HOST", "")
	port := envOr("KUBERNETES_SERVICE_PORT", "443")
	if host == "" {
		return fmt.Errorf("reachability: KUBERNETES_SERVICE_HOST is unset")
	}
	addr := net.JoinHostPort(host, port)
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("reachability: dial %s: %w", addr, err)
	}
	_ = conn.Close()
	return nil
}

// CommandProbe returns a probe function that dials the apiserver Service
// address from targetPID's network namespace (1 = the host) by
// re-executing the controller binary at selfPath with ProbeFlag —
// exactly the internal/nodeaddrs.CommandProbe pattern (the child keeps
// the pod's mount namespace via `-n` only, so the binary path resolves
// inside the image while the dial observes the HOST's routing).
//
// The returned function reports (reachable, error): error is a probe
// MACHINERY failure (nsenter/binary missing — treat as "unknown", never
// as "unreachable"); reachable=false with nil error is a clean,
// authoritative "the host netns cannot reach ClusterIPs".
func CommandProbe(ctx context.Context, targetPID int, selfPath string) (reachable bool, err error) {
	cmd := exec.CommandContext(ctx, "nsenter",
		"-t", strconv.Itoa(targetPID), "-n", "--",
		selfPath, ProbeFlag)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() == 1 {
				// The child ran and could not dial: an authoritative
				// "unreachable" answer, not a machinery failure.
				return false, nil
			}
			return false, fmt.Errorf("reachability: %s %s: %v: %s",
				ProbeFlag, selfPath, err, strings.TrimSpace(string(out)))
		}
		return false, fmt.Errorf("reachability: %s %s: %w", ProbeFlag, selfPath, err)
	}
	return true, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
