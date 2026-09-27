// Extra-file lifecycle (DESIGN-multinode-addresses.md §5, the
// --dangerously-allow-extra-files escape hatch). Same pattern as certs.go
// (DESIGN.md §5.3 / B6):
//   - deterministic layout: <owned-conf-dir>/files/<ns>_<name>/<key>
//   - atomic rename write (tmp + rename)
//   - permission 0640
//   - orphan cleanup: files under files/ not present in the desired set
//     are deleted (after a successful publish), then emptied directories
//     are removed bottom-up. Everything OUTSIDE the files/ subtree of the
//     owned dir is untouched.
package dataplane

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filesSubdir is the subtree of the owned conf dir the FilesManager owns.
const filesSubdir = "files"

// File is one extra file to materialise. Path is RELATIVE to the owned
// conf dir and produced by the provider as "files/<ns>_<name>/<key>"
// (contract.ExtraFile.Path) — the manager only accepts paths inside the
// files/ subtree so a malformed IR can never write elsewhere.
type File struct {
	Path    string
	Content []byte
}

// FilesManager materialises a set of extra files under
// <conf-dir>/files/<ns>_<name>/<key> and removes orphans. It does not call
// nginx — the publisher reloads after the file set has been updated so the
// .conf and the referenced files see the same reload.
type FilesManager struct {
	// RootDir is the owned conf dir (--nginx-conf-dir); files live under
	// RootDir/files. Defaults to /etc/nginx/conf.d/k8s-gw.
	RootDir string
	// FileMode is the permission applied to written files. Default 0o640.
	FileMode os.FileMode
}

// NewFilesManager returns a FilesManager with defaults filled in.
func NewFilesManager(rootDir string) *FilesManager {
	if rootDir == "" {
		rootDir = "/etc/nginx/conf.d/k8s-gw"
	}
	return &FilesManager{RootDir: rootDir, FileMode: 0o640}
}

// validatePath checks one desired path: it must be relative, clean, inside
// the files/ subtree and free of traversal segments. Returns the absolute
// path under RootDir.
func (m *FilesManager) validatePath(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || rel != filepath.Clean(rel) {
		return "", fmt.Errorf("dataplane: files: invalid relative path %q", rel)
	}
	if !strings.HasPrefix(rel, filesSubdir+"/") {
		return "", fmt.Errorf("dataplane: files: path %q is outside the %s/ subtree", rel, filesSubdir)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("dataplane: files: path %q contains a traversal segment", rel)
		}
	}
	return filepath.Join(m.RootDir, rel), nil
}

// Ensure materialises the desired files (atomic write, 0640) without
// removing anything. Safe to call before the referencing config is
// published: adding files can never break the running config.
func (m *FilesManager) Ensure(desired []File) error {
	if m.RootDir == "" {
		return fmt.Errorf("dataplane: files: empty root dir")
	}
	mode := m.FileMode
	if mode == 0 {
		mode = 0o640
	}
	for _, f := range desired {
		abs, err := m.validatePath(f.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return fmt.Errorf("dataplane: files: mkdir: %w", err)
		}
		if err := atomicWrite(abs, f.Content, mode); err != nil {
			return fmt.Errorf("dataplane: files: write %s: %w", abs, err)
		}
	}
	return nil
}

// Cleanup removes files under <RootDir>/files that are not in the desired
// set, then removes directories the cleanup emptied (bottom-up, so
// "<ns>_<name>/" dirs of fully-dropped refs disappear). Returns the
// removed paths relative to RootDir, sorted. Call it only after the
// configuration that no longer references those files has been
// successfully published (same ordering contract as CertsManager).
func (m *FilesManager) Cleanup(desired []File) ([]string, error) {
	root := filepath.Join(m.RootDir, filesSubdir)
	want := make(map[string]struct{}, len(desired))
	for _, f := range desired {
		abs, err := m.validatePath(f.Path)
		if err != nil {
			return nil, err
		}
		want[abs] = struct{}{}
	}

	// Collect the whole files/ subtree (missing dir = nothing to do).
	var all []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return fs.SkipAll
			}
			return err
		}
		if !d.IsDir() {
			all = append(all, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("dataplane: files: walk: %w", err)
	}

	// Orphan files first…
	var orphans []string
	for _, p := range all {
		if _, ok := want[p]; !ok {
			orphans = append(orphans, p)
		}
	}
	sort.Strings(orphans)
	for _, p := range orphans {
		if err := os.Remove(p); err != nil {
			return nil, fmt.Errorf("dataplane: files: remove orphan %s: %w", p, err)
		}
	}

	// …then emptied directories, deepest first.
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == root {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if empty, err := dirIsEmpty(d); err == nil && empty {
			if err := os.Remove(d); err != nil {
				return nil, fmt.Errorf("dataplane: files: remove empty dir %s: %w", d, err)
			}
		}
	}

	// Report relative to the owned dir, matching the desired-set form.
	if len(orphans) == 0 {
		return nil, nil
	}
	rel := make([]string, len(orphans))
	for i, p := range orphans {
		if r, err := filepath.Rel(m.RootDir, p); err == nil {
			rel[i] = r
		} else {
			rel[i] = p
		}
	}
	return rel, nil
}

// dirIsEmpty reports whether dir holds no entries.
func dirIsEmpty(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(ents) == 0, nil
}

// List returns the current set of files under <RootDir>/files (relative to
// RootDir, sorted). Used by tests.
func (m *FilesManager) List() ([]string, error) {
	root := filepath.Join(m.RootDir, filesSubdir)
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if r, err := filepath.Rel(m.RootDir, path); err == nil {
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
