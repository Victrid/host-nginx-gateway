// Commander: the exec seam between the controller and the host nginx
// binary (DESIGN.md §6 / §8.1 "DaemonSet 部署形态").
//
// The controller runs as a DaemonSet pod on the node (hostPID: true,
// host /etc/nginx mounted read-write) and re-enters the HOST's network and
// mount namespaces for every nginx invocation, so the HOST's nginx binary,
// modules and config files are used for both `nginx -t` and
// `nginx -s reload`, and validation sees the HOST's addresses (a `listen`
// on a host-specific address must be bindable for `nginx -t` to pass).
// The -c temp config and -p prefix are host paths
// already (they live under the mounted /etc/nginx).
//
// Tests inject fake Commanders through the same seam.
package dataplane

import (
	"context"
	"os/exec"
	"strconv"
)

// Commander builds the *exec.Cmd for one host-binary invocation. Callers
// own the returned command (buffers, Run).
type Commander func(ctx context.Context, name string, args ...string) *exec.Cmd

// NsenterCommander returns a Commander that prefixes every invocation with
// `nsenter -t <targetPid> -n -m --`, i.e. run the command inside the host's
// NETWORK and MOUNT namespaces. The network namespace is required as well as
// the mount one: `nginx -t` probes every `listen` directive with a real
// bind(), so an address that only exists on the host (e.g. the node's global
// IPv6) must be visible to the validation process — in the pod's network
// namespace that bind fails with `Cannot assign requested address` (errno 99)
// even though the config is perfectly valid on the host. EADDRINUSE (errno
// 98) against ports the running master already holds is tolerated by nginx in
// test mode, so validating against a live master stays green.
// User and PID namespaces stay the pod's, so signal permissions (CAP_KILL
// for `nginx -s reload`) are governed by the pod's securityContext.
func NsenterCommander(targetPid int) Commander {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		full := append([]string{"-t", strconv.Itoa(targetPid), "-n", "-m", "--", name}, args...)
		return exec.CommandContext(ctx, "nsenter", full...)
	}
}
