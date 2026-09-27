// Extra-file materialisation + snippet placeholder substitution tests
// (DESIGN-multinode-addresses.md §5): the applier materialises
// Configuration.ExtraFiles under <conf-dir>/files/ BEFORE publishing,
// substitutes "@<key>@" with the absolute materialised path, and fails
// fast (Programmed=False class, nothing written) when a placeholder
// matches no extra file.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/dataplane"
	"github.com/Victrid/HostNginxGateway/internal/errs"
)

func snippetConfig(snippet, locSnippet string, files ...*contract.ExtraFile) *contract.Configuration {
	c := testConfig()
	c.ExtraFiles = files
	for _, s := range c.Servers {
		s.RawServerSnippet = snippet
		for _, l := range s.Locations {
			l.RawSnippet = locSnippet
		}
	}
	return c
}

func newSnippetApplier(t *testing.T) (*DataplaneApplier, *Metrics, string) {
	t.Helper()
	applier, metrics, confDir := newTestApplier(t, &fakeNginx{})
	applier.Files = dataplane.NewFilesManager(confDir)
	return applier, metrics, confDir
}

func TestApply_ExtraFilesMaterialisedAndPlaceholdersSubstituted(t *testing.T) {
	applier, _, confDir := newSnippetApplier(t)
	lua := &contract.ExtraFile{Path: "files/default_lua/app.lua", Content: []byte("-- lua body")}
	chain := &contract.ExtraFile{Path: "files/default_chain/ca.crt", Content: []byte("CA")}

	cfg := snippetConfig(
		"    ssl_trusted_certificate @ca.crt@;",
		"        content_by_lua_file @app.lua@;",
		lua, chain,
	)
	if err := applier.Apply(context.Background(), cfg, testCerts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Both files materialised under <conf-dir>/files/<ns>_<name>/<key>.
	for _, f := range []*contract.ExtraFile{lua, chain} {
		b, err := os.ReadFile(filepath.Join(confDir, f.Path))
		if err != nil || string(b) != string(f.Content) {
			t.Fatalf("extra file %s = %q, %v", f.Path, b, err)
		}
	}

	// The rendered config references the ABSOLUTE materialised paths.
	conf, err := os.ReadFile(filepath.Join(confDir, globalConfName+".conf"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(conf)
	for _, want := range []string{
		"ssl_trusted_certificate " + filepath.Join(confDir, "files/default_chain/ca.crt") + ";",
		"content_by_lua_file " + filepath.Join(confDir, "files/default_lua/app.lua") + ";",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered conf missing %q\ngot:\n%s", want, s)
		}
	}
	if strings.Contains(s, "@app.lua@") || strings.Contains(s, "@ca.crt@") {
		t.Errorf("unsubstituted placeholder left in config:\n%s", s)
	}
}

func TestApply_UnknownPlaceholderFailsFast(t *testing.T) {
	applier, metrics, confDir := newSnippetApplier(t)
	cfg := snippetConfig(
		"    sub_filter_types text/css;",
		"        content_by_lua_file @missing.lua@;",
		&contract.ExtraFile{Path: "files/default_lua/app.lua", Content: []byte("x")},
	)
	err := applier.Apply(context.Background(), cfg, testCerts())
	if err == nil {
		t.Fatal("Apply succeeded with an unresolvable placeholder")
	}
	// Programmed=False class (the nginx -t / user-config class), never
	// Accepted=False.
	if !errors.Is(err, errs.ErrReload) {
		t.Fatalf("Apply err = %v, want errs.ErrReload class", err)
	}
	if !strings.Contains(err.Error(), "@missing.lua@") {
		t.Fatalf("error must name the missing placeholder: %v", err)
	}
	// Fail-fast: NOTHING was written (no conf file, no reload counted as
	// a publish failure — the failure predates the publisher).
	if _, statErr := os.Stat(filepath.Join(confDir, globalConfName+".conf")); !os.IsNotExist(statErr) {
		t.Fatalf("broken config was written despite fail-fast")
	}
	if got := metrics.ReloadFailures.Load(); got != 0 {
		t.Errorf("reload failures = %d, want 0 (pre-publish failure)", got)
	}
	// The desired extra file IS already materialised (Ensure ran first —
	// harmless, cleaned up by the next successful apply).
	if _, statErr := os.Stat(filepath.Join(confDir, "files/default_lua/app.lua")); statErr != nil {
		t.Fatalf("extra file not materialised before publish: %v", statErr)
	}
}

func TestApply_ServerSnippetUnknownPlaceholderFailsFast(t *testing.T) {
	applier, _, _ := newSnippetApplier(t)
	cfg := snippetConfig("    root @webroot@;", "", nil...)
	err := applier.Apply(context.Background(), cfg, testCerts())
	if err == nil || !errors.Is(err, errs.ErrReload) || !strings.Contains(err.Error(), "@webroot@") {
		t.Fatalf("Apply err = %v, want ErrReload naming @webroot@", err)
	}
}

func TestApply_ExtraFileOrphanCleanup(t *testing.T) {
	applier, _, confDir := newSnippetApplier(t)
	withFile := snippetConfig("", "",
		&contract.ExtraFile{Path: "files/default_lua/app.lua", Content: []byte("v1")})
	if err := applier.Apply(context.Background(), withFile, testCerts()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(confDir, "files/default_lua/app.lua")); err != nil {
		t.Fatalf("file missing after first apply: %v", err)
	}
	// Next apply drops the extra file: the orphan (and its emptied dir)
	// disappear after the successful publish.
	without := snippetConfig("", "")
	if err := applier.Apply(context.Background(), without, testCerts()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(confDir, "files/default_lua/app.lua")); !os.IsNotExist(err) {
		t.Fatalf("orphan extra file survived cleanup")
	}
	if _, err := os.Stat(filepath.Join(confDir, "files/default_lua")); !os.IsNotExist(err) {
		t.Fatalf("emptied extra-file dir survived cleanup")
	}
}

func TestApply_PrepareDoesNotMutateInput(t *testing.T) {
	applier, _, _ := newSnippetApplier(t)
	cfg := snippetConfig("    root @webroot@;", "",
		&contract.ExtraFile{Path: "files/default_cfg/webroot", Content: []byte("/var/www")})
	prepared, err := applier.prepare(cfg)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := prepared.Servers[0].RawServerSnippet; got != "    root "+filepath.Join(applier.ConfDir, "files/default_cfg/webroot")+";" {
		t.Fatalf("substituted snippet = %q", got)
	}
	if got := cfg.Servers[0].RawServerSnippet; got != "    root @webroot@;" {
		t.Fatalf("input configuration mutated: %q", got)
	}
}
