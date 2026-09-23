// Commander: the exec seam between the controller and the host nginx
// binary (DESIGN.md §6 / §8.1 "DaemonSet 部署形态").
//
// The controller runs as a DaemonSet pod on the node (hostPID: true,
// host /etc/nginx mounted read-write) and re-enters the HOST's mount
// namespace for every nginx invocation, so the HOST's nginx binary,
// modules and config files are used for both `nginx -t` and
// `nginx -s reload`. The -c temp config and -p prefix are host paths
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
// `nsenter -t <targetPid> -m --`, i.e. run the command inside the mount
// namespace of the host's PID 1. Only the mount namespace is entered —
// user and PID namespaces stay the pod's, so signal permissions (CAP_KILL
// for `nginx -s reload`) are governed by the pod's securityContext.
func NsenterCommander(targetPid int) Commander {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		full := append([]string{"-t", strconv.Itoa(targetPid), "-m", "--", name}, args...)
		return exec.CommandContext(ctx, "nsenter", full...)
	}
}
