// Unit tests for the nginx process boundary (DESIGN.md §6).
//
// Probe tests use the current process's pid (always alive); Reload tests
// rely on the fake nginx script from validator_test.go.

package dataplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestNginx_ProbeReadsPIDFileAndKills(t *testing.T) {
	tmp := t.TempDir()
	pidPath := filepath.Join(tmp, "nginx.pid")
	// Use our own pid (always alive).
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	n := &NginxClientOS{PIDFile: pidPath}
	if err := n.Probe(); err != nil {
		t.Fatalf("expected Probe to succeed for live pid: %v", err)
	}
}

func TestNginx_ProbeFailsIfPIDDead(t *testing.T) {
	tmp := t.TempDir()
	pidPath := filepath.Join(tmp, "nginx.pid")
	// Pick a pid that almost certainly does not exist.
	// Use a very high number that the kernel will reject with ESRCH.
	deadPID := 999_999_999
	for {
		if err := syscall.Kill(deadPID, 0); err != nil {
			break
		}
		deadPID++
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID)), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	n := &NginxClientOS{PIDFile: pidPath}
	if err := n.Probe(); err == nil {
		t.Fatalf("expected Probe to fail for dead pid")
	}
}

func TestNginx_ProbeFailsOnMissingPIDFile(t *testing.T) {
	n := &NginxClientOS{PIDFile: "/nonexistent/nginx.pid"}
	err := n.Probe()
	if err == nil {
		t.Fatalf("expected error for missing pid file")
	}
}

func TestNginx_ProbeFailsOnEmptyPIDFile(t *testing.T) {
	tmp := t.TempDir()
	pidPath := filepath.Join(tmp, "nginx.pid")
	if err := os.WriteFile(pidPath, []byte("   \n"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	n := &NginxClientOS{PIDFile: pidPath}
	if err := n.Probe(); err == nil {
		t.Fatalf("expected error for empty pid file")
	}
}

func TestNginx_ProbeFailsOnBadPIDContent(t *testing.T) {
	tmp := t.TempDir()
	pidPath := filepath.Join(tmp, "nginx.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	n := &NginxClientOS{PIDFile: pidPath}
	if err := n.Probe(); err == nil {
		t.Fatalf("expected error for bad pid content")
	}
}

func TestNginx_ReloadCallsBinary(t *testing.T) {
	tmp := t.TempDir()
	bin := writeFakeNginx(t, tmp)
	logPath := filepath.Join(tmp, "nginx.log")
	t.Setenv("FAKE_NGINX_LOG", logPath)

	n := &NginxClientOS{Binary: bin, Commander: directExecCommander}
	if err := n.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !contains(logBytes, "-s") || !contains(logBytes, "reload") {
		t.Errorf("expected fake-nginx to be called with -s reload, log=%q", logBytes)
	}
}

func TestNginx_ReloadWrapsStderr(t *testing.T) {
	tmp := t.TempDir()
	bin := writeFakeNginx(t, tmp)
	logPath := filepath.Join(tmp, "nginx.log")
	t.Setenv("FAKE_NGINX_LOG", logPath)
	t.Setenv("FAKE_NGINX_FAIL", "bind failed")

	n := &NginxClientOS{Binary: bin, Commander: directExecCommander}
	err := n.Reload(context.Background())
	if err == nil {
		t.Fatalf("expected error from reload")
	}
	if !contains([]byte(err.Error()), "bind failed") {
		t.Errorf("expected wrapped stderr in error, got %q", err.Error())
	}
}

func TestNginx_DefaultsApplied(t *testing.T) {
	n := NewOSNginx(NginxConfig{})
	if n.Binary != defaultBinary {
		t.Errorf("expected default binary %q, got %q", defaultBinary, n.Binary)
	}
	if n.PIDFile != defaultPIDFile {
		t.Errorf("expected default pid file %q, got %q", defaultPIDFile, n.PIDFile)
	}
}

// Regression (found by e2e/run-daemonset.sh DS-1): NewOSNginx used to drop
// NginxConfig.Commander, silently reverting the DaemonSet (nsenter) form to
// direct execs that fail with ENOENT inside the pod.
func TestNginx_NewOSNginxCarriesCommander(t *testing.T) {
	called := false
	var gotName string
	var gotArgs []string
	cfg := NginxConfig{
		Commander: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			called = true
			gotName, gotArgs = name, args
			return exec.CommandContext(ctx, "true")
		},
	}
	n := NewOSNginx(cfg)
	if n.Commander == nil {
		t.Fatalf("NewOSNginx dropped NginxConfig.Commander")
	}
	if err := n.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !called || gotName != defaultBinary {
		t.Errorf("commander not used for reload: called=%v name=%q", called, gotName)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "-s" {
		t.Errorf("unexpected reload args: %v", gotArgs)
	}
}

func contains(b []byte, sub string) bool {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}
