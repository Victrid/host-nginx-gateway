// Cluster-proxy applier tests (DESIGN-cluster-proxy.md §0.4): proxy.json
// is materialised into the shared sockets dir with the atomic-write
// pattern, BEFORE the publish; the controller never touches *.sock files;
// direct mode (Proxy manager nil) leaves the sockets dir alone entirely.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/dataplane"
)

// TestApply_ProxyMappingMaterialisedBeforePublish: a sidecar-mode Apply
// writes <sockets-dir>/proxy.json with the exact provider bytes, before
// the nginx config publish happens (the published conf references the
// sockets by path — validation via nginx -t does not require them to
// exist, but the sidecar must see the mapping as soon as possible).
func TestApply_ProxyMappingMaterialisedBeforePublish(t *testing.T) {
	nginx := &fakeNginx{}
	applier, _, _ := newTestApplier(t, nginx)
	sockDir := t.TempDir()
	applier.Proxy = dataplane.NewProxyMapManager(sockDir)

	cfg := testConfig()
	cfg.ProxySocketsDir = sockDir
	cfg.ProxyMapping = []byte(`[{"listen":"ns_svc_80.sock","dial":"10.43.1.10:80"}]`)
	// The upstream endpoint carries the socket name: the rendered config
	// must reference <sockDir>/ns_svc_80.sock.
	cfg.Upstreams[0].Endpoints[0].Socket = "ns_svc_80.sock"

	if err := applier.Apply(context.Background(), cfg, testCerts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(sockDir, "proxy.json"))
	if err != nil {
		t.Fatalf("proxy.json not materialised: %v", err)
	}
	if string(data) != string(cfg.ProxyMapping) {
		t.Fatalf("proxy.json bytes: got %s, want %s", data, cfg.ProxyMapping)
	}
	// The published conf renders the unix upstream with the shared dir.
	conf, err := os.ReadFile(filepath.Join(applier.ConfDir, "00-global.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "server unix:"+sockDir+"/ns_svc_80.sock;") {
		t.Fatalf("published conf missing unix upstream line:\n%s", conf)
	}
}

// TestApply_ProxyMappingNeverTouchesSocks: pre-existing *.sock files in
// the shared dir (bound by the sidecar) survive every apply — the
// controller owns proxy.json ONLY.
func TestApply_ProxyMappingNeverTouchesSocks(t *testing.T) {
	applier, _, _ := newTestApplier(t, &fakeNginx{})
	sockDir := t.TempDir()
	applier.Proxy = dataplane.NewProxyMapManager(sockDir)

	sock := filepath.Join(sockDir, "ns_svc_80.sock")
	if err := os.WriteFile(sock, []byte("sidecar owns me"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.ProxyMapping = []byte(`[]`)
	if err := applier.Apply(context.Background(), cfg, testCerts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("controller must never remove sidecar sockets: %v", err)
	}

	// Shrink to empty mapping: proxy.json is REWRITTEN as "[]" (the
	// sidecar unbinds), the socket file is still not our business.
	data, err := os.ReadFile(filepath.Join(sockDir, "proxy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]" {
		t.Fatalf("proxy.json after empty sync: %s", data)
	}
}

// TestApply_DirectModeNeverTouchesSocketsDir: Proxy manager nil (flag
// absent) — no proxy.json is written even when a stale one sits in the
// directory from a previous sidecar-mode deployment.
func TestApply_DirectModeNeverTouchesSocketsDir(t *testing.T) {
	applier, _, _ := newTestApplier(t, &fakeNginx{})
	sockDir := t.TempDir()
	stale := filepath.Join(sockDir, "proxy.json")
	if err := os.WriteFile(stale, []byte(`[{"listen":"x.sock","dial":"1.2.3.4:80"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applier.Apply(context.Background(), testConfig(), testCerts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("direct mode must leave the sockets dir untouched: %v", err)
	}
	if string(data) != `[{"listen":"x.sock","dial":"1.2.3.4:80"}]` {
		t.Fatalf("direct mode must not rewrite proxy.json: %s", data)
	}
}

// TestApply_ProxySyncFailureClassified: a sockets dir that cannot be
// created fails the apply through the errs.Reload class
// (Programmed=False/Invalid) — the mapping is a dataplane prerequisite.
func TestApply_ProxySyncFailureClassified(t *testing.T) {
	applier, _, _ := newTestApplier(t, &fakeNginx{})
	// A regular FILE where the directory should be created: MkdirAll fails.
	blocker := t.TempDir()
	blockerPath := filepath.Join(blocker, "blocked")
	if err := os.WriteFile(blockerPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	applier.Proxy = dataplane.NewProxyMapManager(filepath.Join(blockerPath, "sub"))

	cfg := testConfig()
	cfg.ProxyMapping = []byte(`[]`)
	if err := applier.Apply(context.Background(), cfg, testCerts()); err == nil {
		t.Fatal("Apply must fail when the proxy mapping cannot be materialised")
	}
}
