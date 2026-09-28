// Cluster-proxy mapping lifecycle (DESIGN-cluster-proxy.md §0.4/§3).
// Same pattern as certs.go / files.go:
//   - the provider serialises the mapping into
//     contract.Configuration.ProxyMapping (deterministically sorted JSON);
//   - the applier materialises it as <sockets-dir>/proxy.json with the
//     atomic tmp+rename write BEFORE publishing the referencing config;
//   - "cleanup" here is exactly the atomic replace — proxy.json is the
//     ONLY file this manager ever owns in the directory. The *.sock
//     files living beside it belong to the sidecar (it binds and unlinks
//     them) and are NEVER touched by the controller, in either
//     direction. Likewise the manager never deletes proxy.json itself:
//     in sidecar mode it is always rewritten (an empty backend set
//     writes "[]" so the sidecar unbinds leftovers), and in direct mode
//     the manager is not wired at all (byte-identical legacy behavior).
package dataplane

import (
	"fmt"
	"os"
	"path/filepath"
)

// ProxyMapFileName is the single-file contract with the cluster-proxy
// sidecar (DESIGN-cluster-proxy.md §3): a JSON array of
// {"listen":"<ns>_<svc>_<port>.sock","dial":"<clusterIP>:<port>"}.
const ProxyMapFileName = "proxy.json"

// ProxyMapManager materialises contract.Configuration.ProxyMapping into
// the shared sockets directory (hostPath /run/hng-proxy, tmpfs). The
// sidecar watches the directory and diffs the file by (listen, dial).
type ProxyMapManager struct {
	// Dir is the shared sockets directory (controller writes proxy.json,
	// sidecar binds *.sock). No default — an empty Dir is a wiring error.
	Dir string
	// FileMode applied to proxy.json. Default 0o644 (the sidecar only
	// needs read; 0640 would also work — both containers run as root).
	FileMode os.FileMode
}

// NewProxyMapManager returns a ProxyMapManager over dir.
func NewProxyMapManager(dir string) *ProxyMapManager {
	return &ProxyMapManager{Dir: dir, FileMode: 0o644}
}

// Sync atomically writes the mapping content to <Dir>/proxy.json. The
// tmp+rename write means the sidecar only ever observes the complete old
// or complete new file. content is contract.Configuration.ProxyMapping —
// callers pass "[]" (never nil) in sidecar mode.
func (m *ProxyMapManager) Sync(content []byte) error {
	if m.Dir == "" {
		return fmt.Errorf("dataplane: proxymap: empty sockets dir")
	}
	if len(content) == 0 {
		return fmt.Errorf("dataplane: proxymap: nil mapping content (sidecar mode always writes at least \"[]\")")
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return fmt.Errorf("dataplane: proxymap: mkdir: %w", err)
	}
	mode := m.FileMode
	if mode == 0 {
		mode = 0o644
	}
	if err := atomicWrite(m.Path(), content, mode); err != nil {
		return fmt.Errorf("dataplane: proxymap: write %s: %w", m.Path(), err)
	}
	return nil
}

// Path returns the absolute proxy.json path (<Dir>/proxy.json).
func (m *ProxyMapManager) Path() string {
	return filepath.Join(m.Dir, ProxyMapFileName)
}
