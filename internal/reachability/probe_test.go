// Tests for the ClusterIP reachability probe child (the nsenter'd half
// is machinery — see internal/nodeaddrs precedent — and is not
// unit-testable without host namespaces).
package reachability

import (
	"net"
	"testing"
)

func TestChild_ReachableAndUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", portOf(t, l))
	if err := Child(); err != nil {
		t.Fatalf("reachable dial must succeed: %v", err)
	}

	// A port with nothing behind it: refused → unreachable.
	t.Setenv("KUBERNETES_SERVICE_PORT", freePort(t))
	if err := Child(); err == nil {
		t.Fatalf("unreachable dial must fail")
	}
}

func TestChild_MissingEnv(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if err := Child(); err == nil {
		t.Fatalf("missing KUBERNETES_SERVICE_HOST must fail the probe")
	}
}

func portOf(t *testing.T, l net.Listener) string {
	t.Helper()
	_, p, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// freePort returns a currently-unused port (listen :0, close).
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return portOf(t, l)
}
