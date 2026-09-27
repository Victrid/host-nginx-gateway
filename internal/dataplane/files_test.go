// FilesManager tests (DESIGN-multinode-addresses.md §5): materialisation
// of extra files under <conf-dir>/files/<ns>_<name>/<key> with atomic
// tmp+rename writes, orphan cleanup (files + emptied dirs), and the
// path-validation guard keeping writes inside the files/ subtree.
package dataplane

import (
	"os"
	"path/filepath"
	"testing"
)

func fileSet(paths ...string) []File {
	out := make([]File, 0, len(paths))
	for _, p := range paths {
		out = append(out, File{Path: p, Content: []byte("content of " + p)})
	}
	return out
}

func TestFiles_EnsureMaterialisesNestedPaths(t *testing.T) {
	root := t.TempDir()
	m := NewFilesManager(root)
	desired := fileSet(
		"files/default_lua/app.lua",
		"files/default_lua/lib/util.lua",
		"files/default_chain/ca.crt",
	)
	if err := m.Ensure(desired); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, f := range desired {
		b, err := os.ReadFile(filepath.Join(root, f.Path))
		if err != nil {
			t.Fatalf("file %s not materialised: %v", f.Path, err)
		}
		if string(b) != "content of "+f.Path {
			t.Fatalf("file %s content = %q", f.Path, b)
		}
		info, err := os.Stat(filepath.Join(root, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("file %s mode = %v, want 0640", f.Path, info.Mode().Perm())
		}
	}
}

func TestFiles_EnsureRewritesAtomically(t *testing.T) {
	root := t.TempDir()
	m := NewFilesManager(root)
	if err := m.Ensure(fileSet("files/ns_ref/key")); err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure([]File{{Path: "files/ns_ref/key", Content: []byte("v2")}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "files/ns_ref/key"))
	if err != nil || string(b) != "v2" {
		t.Fatalf("rewrite failed: %q %v", b, err)
	}
	// No tmp leftovers.
	got, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "files/ns_ref/key" {
		t.Fatalf("List() = %v", got)
	}
}

func TestFiles_CleanupRemovesOrphansAndEmptyDirs(t *testing.T) {
	root := t.TempDir()
	m := NewFilesManager(root)
	first := fileSet(
		"files/ns_a/k1",
		"files/ns_a/k2",
		"files/ns_b/k1",
	)
	if err := m.Ensure(first); err != nil {
		t.Fatal(err)
	}
	// Desired set drops ns_a entirely and one key of ns_b.
	keep := fileSet("files/ns_b/k1")
	removed, err := m.Cleanup(keep)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want the two ns_a files", removed)
	}
	for _, p := range removed {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s still present", p)
		}
	}
	// The emptied ns_a directory is gone; ns_b and its file stay.
	if _, err := os.Stat(filepath.Join(root, "files/ns_a")); !os.IsNotExist(err) {
		t.Fatalf("emptied dir files/ns_a still present")
	}
	if _, err := os.Stat(filepath.Join(root, "files/ns_b/k1")); err != nil {
		t.Fatalf("desired file removed: %v", err)
	}
	got, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "files/ns_b/k1" {
		t.Fatalf("List() after cleanup = %v", got)
	}
}

func TestFiles_CleanupKeepsForeignFilesOutsideSubtree(t *testing.T) {
	root := t.TempDir()
	m := NewFilesManager(root)
	if err := m.Ensure(fileSet("files/ns/k")); err != nil {
		t.Fatal(err)
	}
	// A file directly under the owned dir (not in files/) is not ours.
	foreign := filepath.Join(root, "00-global.conf")
	if err := os.WriteFile(foreign, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cleanup(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file removed by extra-file cleanup: %v", err)
	}
}

func TestFiles_EnsureRejectsBadPaths(t *testing.T) {
	root := t.TempDir()
	m := NewFilesManager(root)
	for _, bad := range []string{
		"../escape",
		"files/../../escape",
		"/etc/passwd",
		"certs/ns_tls.pem", // outside files/
		"files//double",
		"files/./dot",
		"",
	} {
		if err := m.Ensure(fileSet(bad)); err == nil {
			t.Fatalf("path %q accepted", bad)
		}
		if _, err := os.Stat(filepath.Join(root, "escape")); err == nil {
			t.Fatalf("path %q escaped the root", bad)
		}
	}
}

func TestFiles_CleanupMissingDirIsNoop(t *testing.T) {
	m := NewFilesManager(t.TempDir())
	removed, err := m.Cleanup(fileSet("files/ns/k"))
	if err != nil || removed != nil {
		t.Fatalf("Cleanup on missing dir = %v, %v", removed, err)
	}
}
