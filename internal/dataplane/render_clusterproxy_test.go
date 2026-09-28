// Cluster-proxy rendering tests (DESIGN-cluster-proxy.md §0.1/§0.2):
// endpoints with a Socket name render as
// `server unix:<ProxySocketsDir>/<Socket> [weight=N] [down];`; socket-less
// endpoints render the legacy `server <ip>:<port>` form byte-identically.
package dataplane

import (
	"os"
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func writeStub(path string) error {
	return os.WriteFile(path, []byte("stub"), 0o644)
}

func readAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func listDirNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out, nil
}

func osStat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func renderUpstreams(t *testing.T, cfg *contract.Configuration) string {
	t.Helper()
	out, err := Render(cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(out)
}

func TestRender_UnixUpstreamOnAndOff(t *testing.T) {
	base := &contract.Configuration{
		ProxySocketsDir: "/run/hng-proxy",
		Upstreams: []*contract.Upstream{{
			Name: "apps_web_80",
			Endpoints: []contract.Endpoint{
				{IP: "10.43.1.10", Port: 80, Ready: true, Socket: "apps_web_80.sock"},
				{IP: "10.43.1.20", Port: 8080, Ready: true, Weight: 3, Socket: "apps_api_8080.sock"},
			},
		}},
	}
	out := renderUpstreams(t, base)
	for _, want := range []string{
		"server unix:/run/hng-proxy/apps_web_80.sock;",
		"server unix:/run/hng-proxy/apps_api_8080.sock weight=3;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "10.43.1.10:80") {
		t.Fatalf("socket endpoint must not render its IP:port fallback:\n%s", out)
	}

	// Off: same endpoints WITHOUT Socket names render the legacy form.
	direct := &contract.Configuration{
		Upstreams: []*contract.Upstream{{
			Name: "apps_web_8080",
			Endpoints: []contract.Endpoint{
				{IP: "10.42.0.5", Port: 8080, Ready: true},
				{IP: "10.42.0.6", Port: 8080, Ready: false, Weight: 2},
			},
		}},
	}
	out = renderUpstreams(t, direct)
	for _, want := range []string{
		"server 10.42.0.5:8080;",
		"server 10.42.0.6:8080 weight=2 down;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("direct rendering missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unix:") {
		t.Fatalf("direct rendering must not contain unix: lines:\n%s", out)
	}
}

// TestRender_SocketDownSuffixPreserved: the down suffix logic is shared
// by both forms (a not-ready socket endpoint keeps `down`).
func TestRender_SocketDownSuffixPreserved(t *testing.T) {
	cfg := &contract.Configuration{
		ProxySocketsDir: "/run/hng-proxy",
		Upstreams: []*contract.Upstream{{
			Name: "apps_web_80",
			Endpoints: []contract.Endpoint{
				{IP: "10.43.1.10", Port: 80, Socket: "apps_web_80.sock"}, // Ready=false
			},
		}},
	}
	if out := renderUpstreams(t, cfg); !strings.Contains(out,
		"server unix:/run/hng-proxy/apps_web_80.sock down;") {
		t.Fatalf("down socket line missing:\n%s", out)
	}
}

// TestProxyMapManager_SyncAtomicAndScoped: proxy.json is written with
// the atomic tmp+rename pattern, exactly the requested bytes; no other
// files in the directory are created or removed by the manager (the
// *.sock files belong to the sidecar).
func TestProxyMapManager_SyncAtomicAndScoped(t *testing.T) {
	dir := t.TempDir()
	m := NewProxyMapManager(dir)

	// A pre-existing sidecar socket and an unrelated file: both must
	// survive every Sync.
	staleSock := dir + "/apps_web_80.sock"
	foreign := dir + "/operator-note.txt"
	if err := writeStub(staleSock); err != nil {
		t.Fatal(err)
	}
	if err := writeStub(foreign); err != nil {
		t.Fatal(err)
	}

	if err := m.Sync([]byte(`[{"listen":"apps_web_80.sock","dial":"10.43.1.10:80"}]`)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, err := readAll(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[{"listen":"apps_web_80.sock","dial":"10.43.1.10:80"}]` {
		t.Fatalf("proxy.json content: %s", got)
	}

	// Replacement is atomic (no tmp-* leftovers afterwards) and idempotent.
	if err := m.Sync([]byte(`[]`)); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if got, _ := readAll(m.Path()); string(got) != `[]` {
		t.Fatalf("proxy.json after second sync: %s", got)
	}
	entries, err := listDirNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if strings.HasPrefix(name, "proxy.json.tmp") {
			t.Fatalf("atomic write left a tmp file behind: %s", name)
		}
	}
	if _, err := osStat(staleSock); err != nil {
		t.Fatalf("sidecar socket must never be touched by the manager: %v", err)
	}
	if _, err := osStat(foreign); err != nil {
		t.Fatalf("unrelated files must never be touched: %v", err)
	}
}

// TestProxyMapManager_RejectsNilContent: sidecar mode always writes at
// least "[]" — a nil mapping is a wiring bug, not an empty set.
func TestProxyMapManager_RejectsNilContent(t *testing.T) {
	m := NewProxyMapManager(t.TempDir())
	if err := m.Sync(nil); err == nil {
		t.Fatal("nil mapping content must be rejected")
	}
	if err := m.Sync([]byte("")); err == nil {
		t.Fatal("empty mapping content must be rejected")
	}
}
