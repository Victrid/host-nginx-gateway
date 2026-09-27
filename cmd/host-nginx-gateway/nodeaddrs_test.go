// Tests for the hidden node-address probe flag (internal/nodeaddrs):
// the prober re-executes the controller binary through
// `nsenter -t 1 -n -- <self> --print-node-addresses` to enumerate the
// HOST network namespace. These tests verify (a) the flag path of the
// real binary and (b) the CommandProbe parsing seam against a stub
// nsenter-compatible wrapper.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/nodeaddrs"
)

// TestPrintNodeAddressesFlag builds the controller binary and runs the
// hidden flag: it must exit 0 and print one parseable address per line
// (the addresses of the CURRENT — test process — network namespace).
func TestPrintNodeAddressesFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the controller binary")
	}
	bin := filepath.Join(t.TempDir(), "hng-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, nodeaddrs.PrintAddressesFlag).Output()
	if err != nil {
		t.Fatalf("%s: %v", nodeaddrs.PrintAddressesFlag, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no addresses printed: %q", out)
	}
	for _, l := range lines {
		if l == "" || nodeaddrs.RenderBindForm(l) == "" {
			t.Fatalf("unparseable address line %q in %q", l, out)
		}
	}
}

// TestCommandProbe_ParsesProbeOutput pins the CommandProbe contract
// against a stub "nsenter" that fakes the host namespace view: the probe
// must return exactly the child's printed addresses.
func TestCommandProbe_ParsesProbeOutput(t *testing.T) {
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "nsenter")
	script := "#!/bin/sh\n# stub nsenter: ignore -t/-n/self, emit a fixed host view\nprintf '192.0.2.10\\n2001:db8::1\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	probe := nodeaddrs.CommandProbe(1, "/nonexistent/self")
	addrs, err := probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := nodeaddrs.NewSet(addrs...)
	if !s.Contains("192.0.2.10") || !s.Contains("2001:db8::1") || s.Contains("198.51.100.7") {
		t.Fatalf("probe addresses = %v", addrs)
	}
}
