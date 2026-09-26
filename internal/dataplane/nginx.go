// Process boundary: DESIGN.md §6.
// The controller does NOT start or supervise the nginx master. It only
// (a) probes liveness by reading the pid file + syscall.Kill(pid, 0),
// (b) issues `nginx -s reload` (the publisher owns whether to call it).
package dataplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrNginxNotRunning is returned by Probe when nginx is not detected alive.
// Callers should map this to Programmed=False (Pending) per DESIGN.md §6.
var ErrNginxNotRunning = errors.New("dataplane: nginx is not running")

// NginxClient is the interface to the host nginx process. It is split out
// from the publisher so unit tests can inject a fake (see nginx_fake_test.go
// and the publisher tests in publisher_test.go).
type NginxClient interface {
	// Probe returns nil if nginx is running, ErrNginxNotRunning otherwise.
	// It must not parse `nginx -s reload` exit codes (semantics flagged ambiguous
	// in DESIGN.md §6); the pid file + kill(pid, 0) check is authoritative.
	Probe() error
	// Reload asks the running nginx master to reload configuration.
	// The returned error wraps the captured stderr so callers can include it
	// in the Programmed=False message per DESIGN.md §3.4 / N6.
	Reload(ctx context.Context) error
}

// NginxConfig configures the default NginxClient.
type NginxConfig struct {
	// Binary is the path to the nginx executable (DESIGN.md §5.4 / N5).
	// Defaults to "/usr/sbin/nginx" when empty. In nsenter (DaemonSet)
	// mode this is the HOST's nginx path — the wrapper re-enters the host
	// network + mount namespaces before exec (see exec.go).
	Binary string
	// PIDFile is the path to nginx's pid file (DESIGN.md §6).
	// Defaults to "/run/nginx.pid" when empty. In the DaemonSet form the
	// host's /run is mounted read-only at /host/run (see the chart), so
	// pods typically pass --nginx-pid=/host/run/nginx.pid.
	PIDFile string
	// Commander builds the exec.Cmd for `nginx -s reload`. Nil means
	// NsenterCommander(1) (the DaemonSet host-namespace default).
	Commander Commander
}

// commander returns the configured Commander or the nsenter default.
func (n *NginxClientOS) commander() Commander {
	if n.Commander != nil {
		return n.Commander
	}
	return NsenterCommander(1)
}

// defaultBinary and defaultPIDFile are the documented defaults; tests may
// override via NginxConfig.
const (
	defaultBinary  = "/usr/sbin/nginx"
	defaultPIDFile = "/run/nginx.pid"
)

// NginxClientOS is the default NginxClient implementation: it probes via the
// pid file + syscall.Kill(pid, 0), and issues `<binary> -s reload` on Reload.
// Both Binary and PIDFile are exposed as struct fields (rather than via a
// constructor only) so unit tests can inject a fake shell script.
type NginxClientOS struct {
	Binary  string
	PIDFile string
	// Commander routes the reload exec; nil means direct (see exec.go).
	Commander Commander
}

// NewOSNginx returns an NginxClientOS honoring cfg; empty fields take
// defaults. Production code typically uses this; tests construct the struct
// directly with their own paths.
func NewOSNginx(cfg NginxConfig) *NginxClientOS {
	b := cfg.Binary
	if b == "" {
		b = defaultBinary
	}
	p := cfg.PIDFile
	if p == "" {
		p = defaultPIDFile
	}
	// Commander MUST be carried over: dropping it silently reverts the
	// DaemonSet (nsenter) form to direct execs, which fail with ENOENT
	// inside the pod (found by e2e/run-daemonset.sh DS-1).
	return &NginxClientOS{Binary: b, PIDFile: p, Commander: cfg.Commander}
}

// Probe reads the pid file and issues kill(pid, 0). The call does not
// signal the process (signal 0 is the standard "check liveness" idiom).
// Any error reading the pid file or signaling the pid maps to
// ErrNginxNotRunning so callers can branch on a single sentinel.
func (n *NginxClientOS) Probe() error {
	pidPath := n.PIDFile
	if pidPath == "" {
		pidPath = defaultPIDFile
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("%w: read pid file %s: %v", ErrNginxNotRunning, pidPath, err)
	}
	pidStr := strings.TrimSpace(string(data))
	if pidStr == "" {
		return fmt.Errorf("%w: empty pid file %s", ErrNginxNotRunning, pidPath)
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return fmt.Errorf("%w: bad pid in %s: %v", ErrNginxNotRunning, pidPath, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		// EPERM means the target process exists but is not signalable
		// (e.g. a root nginx master probed by the non-root controller user
		// of DESIGN.md §8): liveness is still established — the process is
		// there. A subsequent reload will surface its own permission error
		// if signaling is genuinely impossible.
		if errors.Is(err, syscall.EPERM) {
			return nil
		}
		return fmt.Errorf("%w: kill(%d, 0): %v", ErrNginxNotRunning, pid, err)
	}
	return nil
}

// Reload executes `<binary> -s reload` and returns a combined error + stderr
// if it fails. The publisher wraps the returned error to compose the
// ErrReload state (DESIGN.md §5.2).
func (n *NginxClientOS) Reload(ctx context.Context) error {
	bin := n.Binary
	if bin == "" {
		bin = defaultBinary
	}
	cmd := n.commander()(ctx, bin, "-s", "reload")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("dataplane: nginx -s reload: %w", err)
		}
		return fmt.Errorf("dataplane: nginx -s reload: %w: %s", err, msg)
	}
	return nil
}
