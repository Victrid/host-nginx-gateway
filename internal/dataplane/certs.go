// Certificate lifecycle: DESIGN.md §5.3 / B6.
//   - deterministic filenames: <namespace>_<name>.pem
//   - atomic rename write (tmp + rename)
//   - permission 0640
//   - orphan cleanup: delete files in CertsDir not present in the desired set
package dataplane

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Cert is a single certificate to be materialised under the gateway's
// managed dir. The bytes are PEM-encoded (concatenated cert + key as is
// conventional; the publisher does not need to parse them — nginx trusts
// the format because the upstream Secret store ships PEM).
type Cert struct {
	// Namespace and Name identify the Kubernetes Secret whose data this
	// cert corresponds to; the filename is derived as
	// "<Namespace>_<Name>.pem" (B6 / DESIGN.md §5.3).
	Namespace string
	Name      string
	// Data is the PEM-encoded certificate body.
	Data []byte
}

// Filename returns the canonical file name for this Cert (B6).
func (c Cert) Filename() string {
	return certFilename(c.Namespace, c.Name)
}

// certFilename centralises the filename derivation so every consumer (write,
// orphan-scan, tests) agrees on the format.
func certFilename(namespace, name string) string {
	return namespace + "_" + name + ".pem"
}

// CertsManager materialises a set of certificates into a directory and
// removes orphans (B6). It does not call nginx directly — the publisher
// reloads after the cert set has been updated so both the .conf and the
// .pem files see the same reload.
type CertsManager struct {
	// CertsDir is where the .pem files live; defaults to
	// /etc/nginx/conf.d/k8s-gw/certs.
	CertsDir string
	// FileMode is the permission applied to written files. Default 0o640
	// (DESIGN.md §5.3).
	FileMode os.FileMode
}

// NewCertsManager returns a CertsManager with defaults filled in.
func NewCertsManager(certsDir string) *CertsManager {
	if certsDir == "" {
		certsDir = "/etc/nginx/conf.d/k8s-gw/certs"
	}
	return &CertsManager{CertsDir: certsDir, FileMode: 0o640}
}

// Sync ensures that exactly the desired set of Cert entries exist under
// CertsDir with the right content, atomic-write mode 0640, and deletes
// any orphan .pem files that are not in the desired set.
//
// Returns the list of orphan files deleted (sorted) for diagnostics and
// logging. The function is safe to call on a non-existent dir: it creates
// CertsDir if missing.
//
// NOTE on ordering (E2E finding): within one apply the caller should use
// Ensure before publishing config that may reference NEW certs, and
// Cleanup only AFTER the publish succeeded — deleting a cert the previous
// (still on disk) config references poisons the include-glob validation
// and deadlocks Programmed=False/Invalid. Sync does both at once and is
// kept for callers that publish nothing.
func (m *CertsManager) Sync(desired []Cert) ([]string, error) {
	if err := m.Ensure(desired); err != nil {
		return nil, err
	}
	return m.Cleanup(desired)
}

// Ensure materialises the desired certs (atomic write, 0640) without
// removing anything. Safe to call before the referencing config is
// published: adding files can never break the running config.
func (m *CertsManager) Ensure(desired []Cert) error {
	if m.CertsDir == "" {
		return fmt.Errorf("dataplane: certs: empty certs dir")
	}
	if err := os.MkdirAll(m.CertsDir, 0o750); err != nil {
		return fmt.Errorf("dataplane: certs: mkdir: %w", err)
	}
	mode := m.FileMode
	if mode == 0 {
		mode = 0o640
	}
	for _, c := range desired {
		if c.Namespace == "" || c.Name == "" {
			return fmt.Errorf("dataplane: certs: cert with empty namespace/name")
		}
		fp := filepath.Join(m.CertsDir, c.Filename())
		if err := atomicWrite(fp, c.Data, mode); err != nil {
			return fmt.Errorf("dataplane: certs: write %s: %w", fp, err)
		}
	}
	return nil
}

// Cleanup removes .pem files under CertsDir that are not in the desired
// set (DESIGN.md §5.3 orphan cleanup) and returns the removed names,
// sorted. Call it only after the configuration that no longer references
// those certs has been successfully published.
func (m *CertsManager) Cleanup(desired []Cert) ([]string, error) {
	if m.CertsDir == "" {
		return nil, fmt.Errorf("dataplane: certs: empty certs dir")
	}
	want := make(map[string]struct{}, len(desired))
	for _, c := range desired {
		want[c.Filename()] = struct{}{}
	}

	entries, err := os.ReadDir(m.CertsDir)
	if err != nil {
		return nil, fmt.Errorf("dataplane: certs: readdir: %w", err)
	}
	var orphans []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".pem") {
			// Not ours; leave it alone (DESIGN.md §2: only own this dir,
			// don't aggressively touch unrelated files).
			continue
		}
		if _, ok := want[name]; ok {
			continue
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)

	for _, name := range orphans {
		fp := filepath.Join(m.CertsDir, name)
		if err := os.Remove(fp); err != nil {
			return orphans, fmt.Errorf("dataplane: certs: remove orphan %s: %w", fp, err)
		}
	}
	return orphans, nil
}

// List returns the current set of cert filenames under CertsDir. The result
// is sorted. Used by tests; the reconciler has no direct use for it.
func (m *CertsManager) List() ([]string, error) {
	entries, err := os.ReadDir(m.CertsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".pem") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
