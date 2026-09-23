// Unit tests for CertsManager (DESIGN.md §5.3 / B6).

package dataplane

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCerts_FilenameFormat(t *testing.T) {
	c := Cert{Namespace: "default", Name: "tls"}
	if got, want := c.Filename(), "default_tls.pem"; got != want {
		t.Errorf("Filename() = %q, want %q", got, want)
	}
}

func TestCerts_SyncWritesAndCleansOrphans(t *testing.T) {
	dir := t.TempDir()
	// Pre-create two orphans that are not in the desired set.
	if err := os.WriteFile(filepath.Join(dir, "old_one.pem"), []byte("stale"), 0o640); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old_two.pem"), []byte("stale"), 0o640); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	m := NewCertsManager(dir)
	desired := []Cert{
		{Namespace: "default", Name: "tls", Data: []byte("pem-bytes-1")},
		{Namespace: "kube", Name: "ingress", Data: []byte("pem-bytes-2")},
	}
	orphans, err := m.Sync(desired)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	wantOrphans := []string{"old_one.pem", "old_two.pem"}
	if !equalStringSlices(orphans, wantOrphans) {
		t.Errorf("orphans = %v, want %v", orphans, wantOrphans)
	}

	// Desired files exist with right content + mode.
	for _, c := range desired {
		path := filepath.Join(dir, c.Filename())
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != string(c.Data) {
			t.Errorf("content mismatch for %s: %q vs %q", path, got, c.Data)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Errorf("expected mode 0640 for %s, got %v", path, info.Mode().Perm())
		}
	}

	// Orphans removed.
	for _, name := range wantOrphans {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("expected orphan %s to be removed", name)
		}
	}
}

func TestCerts_LeavesNonPemFilesAlone(t *testing.T) {
	dir := t.TempDir()
	// A non-pem file under the certs dir is not our concern (DESIGN.md §2:
	// only own this dir, don't touch unrelated files).
	stray := filepath.Join(dir, "stray.txt")
	if err := os.WriteFile(stray, []byte("user-data"), 0o640); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	m := NewCertsManager(dir)
	desired := []Cert{
		{Namespace: "default", Name: "tls", Data: []byte("pem")},
	}
	orphans, err := m.Sync(desired)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("expected 0 orphans (stray.txt is not .pem), got %v", orphans)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("expected stray.txt to remain; got %v", err)
	}
}

func TestCerts_UpdateInPlace(t *testing.T) {
	dir := t.TempDir()
	m := NewCertsManager(dir)
	c1 := Cert{Namespace: "default", Name: "tls", Data: []byte("v1")}
	if _, err := m.Sync([]Cert{c1}); err != nil {
		t.Fatalf("Sync v1: %v", err)
	}
	c1.Data = []byte("v2")
	if _, err := m.Sync([]Cert{c1}); err != nil {
		t.Fatalf("Sync v2: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "default_tls.pem"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("expected content v2, got %q", got)
	}
}

func TestCerts_EmptyDesiredCleansAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.pem"), []byte("a"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.pem"), []byte("b"), 0o640); err != nil {
		t.Fatal(err)
	}
	m := NewCertsManager(dir)
	orphans, err := m.Sync(nil)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	sort.Strings(orphans)
	want := []string{"a.pem", "b.pem"}
	if !equalStringSlices(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
}

func TestCerts_RejectsEmptyNamespaceOrName(t *testing.T) {
	dir := t.TempDir()
	m := NewCertsManager(dir)
	_, err := m.Sync([]Cert{{Namespace: "", Name: "tls", Data: []byte("x")}})
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("expected namespace error, got %v", err)
	}
	_, err = m.Sync([]Cert{{Namespace: "default", Name: "", Data: []byte("x")}})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("expected name error, got %v", err)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
