// Host-namespace probing for the DaemonSet form: the controller pod's
// own net.Interfaces() would report the POD's addresses, but ownership
// must be decided against the NODE's addresses — the ones the host nginx
// can actually bind (all nginx -t / reload execs already run via
// `nsenter -t 1 -n -m` into the host network namespace,
// DESIGN.md §8.1).
//
// CommandProbe re-executes the controller's own binary via
// `nsenter -t 1 -n -- <self> PrintAddressesFlag`:
//
//   - -n only (no -m): the child keeps the POD's mount namespace, so the
//     binary path resolves inside the image, while net.Interfaces()
//     enumerates the HOST's interfaces (network namespace is per-process
//     and /proc-free — plain Go stdlib works);
//   - the hidden flag is handled in cmd/host-nginx-gateway/main() BEFORE
//     flag parsing, prints one address per line and exits 0.
package nodeaddrs

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PrintAddressesFlag is the hidden CLI flag the probe child runs with.
const PrintAddressesFlag = "--print-node-addresses"

// CommandProbe returns a probe function that enumerates the network
// namespace of targetPID (1 = host) by re-executing the controller
// binary at selfPath with PrintAddressesFlag. Fails when nsenter, the
// binary or the flag handling is unavailable — callers fall back to
// single-node semantics (no fingerprint).
func CommandProbe(targetPID int, selfPath string) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		cmd := exec.CommandContext(ctx, "nsenter",
			"-t", strconv.Itoa(targetPID), "-n", "--",
			selfPath, PrintAddressesFlag)
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				return nil, fmt.Errorf("nodeaddrs: %s %s: %v: %s",
					PrintAddressesFlag, selfPath, err, strings.TrimSpace(string(ee.Stderr)))
			}
			return nil, fmt.Errorf("nodeaddrs: %s %s: %w", PrintAddressesFlag, selfPath, err)
		}
		var addrs []string
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				addrs = append(addrs, line)
			}
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("nodeaddrs: %s %s produced no addresses", PrintAddressesFlag, selfPath)
		}
		return addrs, nil
	}
}
