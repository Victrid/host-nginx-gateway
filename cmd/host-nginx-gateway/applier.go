// DataplaneApplier: the cmd-side adapter implementing provider.Applier
// (internal/provider/manager.go seam) on top of internal/dataplane.
//
// Per Apply (one full sync):
//  1. probe nginx (pid file + kill -0, DESIGN.md §6) — not running wraps
//     errs.ErrNginxNotRunning so the provider maps Programmed=False (Pending);
//  2. sync TLS certificates into <conf-dir>/certs/ (DESIGN.md §5.3);
//     2b. materialise extra files into <conf-dir>/files/
//     (DESIGN-multinode-addresses.md §5) and substitute snippet
//     "@<key>@" placeholders — an unresolvable placeholder fails here,
//     before anything is written (fail-fast);
//  3. publish the SINGLE global contract.Configuration as
//     <conf-dir>/00-global.conf via dataplane.Publisher (validate via the
//     temporary-main-config `nginx -t` method, §5.1, then rollback-capable
//     reload, §5.2); orphan cleanup (certs + files) runs only after a
//     successful publish;
//  4. classify every failure into the internal/errs taxonomy so the
//     provider's errs.StatusReason mapping produces the §3.4 reasons.
package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	logr "github.com/go-logr/logr"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/dataplane"
	"github.com/Victrid/HostNginxGateway/internal/errs"
	"github.com/Victrid/HostNginxGateway/internal/provider"
)

// globalConfName is the basename (without .conf) of the one generated
// configuration file.
//
// File-layout choice: the provider's translate.go emits ONE global
// contract.Configuration whose Upstreams are shared across all Gateways
// ("emitted once per upstream regardless of how many Servers reference
// them"). Splitting per Gateway into <ns>-<name>.conf files (DESIGN.md §2's
// original sketch) would duplicate those shared upstream blocks across
// files and fail `nginx -t` with "upstream ... duplicate". The upstreams
// therefore dictate a single file; "00-global" sorts first in the include
// glob so upstream definitions precede the (only) server-bearing file when
// operators add hand-written includes later. DESIGN.md §2's per-Gateway
// layout is deferred until the IR carries per-Gateway upstream ownership.
const globalConfName = "00-global"

// DataplaneApplier implements provider.Applier.
type DataplaneApplier struct {
	// ConfDir is the owned directory (--nginx-conf-dir); the single
	// generated file, certs/ and files/ live under it.
	ConfDir string
	// Nginx probes/reloads the host nginx master.
	Nginx dataplane.NginxClient
	// Certs materialises the desired certificate set under ConfDir/certs.
	Certs *dataplane.CertsManager
	// Files materialises the extra-files escape-hatch set under
	// ConfDir/files (DESIGN-multinode-addresses.md §5). Nil skips the
	// step (tests without extra files).
	Files *dataplane.FilesManager
	// Proxy materialises the cluster-proxy mapping (proxy.json) into the
	// shared sockets dir (DESIGN-cluster-proxy.md §0.4) when sidecar mode
	// is on. Nil (direct mode) never touches the sockets dir — the *.sock
	// files in it belong to the sidecar, in both modes.
	Proxy *dataplane.ProxyMapManager
	// Publisher renders, validates (nginx -t) and reloads.
	Publisher *dataplane.Publisher
	// Metrics receives the apply counters (nil-safe).
	Metrics *Metrics
	// Log receives per-apply diagnostics.
	Log logr.Logger
	// ErrorLogPath overrides the owned error log used for the http-context
	// error_log directive and the reload-effect verification. Empty
	// derives <ConfDir>/error.log; the literal value "off" disables both.
	ErrorLogPath string
}

// Apply implements provider.Applier. The returned error is always either
// nil or one of the internal/errs typed errors (or an unclassified error
// the provider reports as the safe internal-error fallback).
func (a *DataplaneApplier) Apply(ctx context.Context, cfg *contract.Configuration, certs []provider.Certificate) error {
	if a.Metrics != nil {
		a.Metrics.Reconciles.Add(1)
	}

	// 1. Liveness probe (DESIGN.md §6): without a live master there is
	// nothing to reload — skip the dataplane entirely and let the provider
	// report Programmed=False (Pending).
	if err := a.Nginx.Probe(); err != nil {
		return errs.NginxNotRunning("nginx liveness probe failed (pid file + kill -0)", err)
	}

	// 2. Certificates (DESIGN.md §5.3): provider.Certificate and
	// dataplane.Cert agree on the deterministic <ns>_<name>.pem filename,
	// which is exactly the basename the provider writes into
	// Server.TLSCert.
	//
	// Ordering (E2E finding): only ADD/refresh certs here. Orphan deletion
	// must happen AFTER a successful publish — deleting a cert that the
	// still-on-disk config references poisons the include-glob `nginx -t`
	// validation and deadlocks Programmed=False (Invalid). A stale pem that
	// outlives a failed publish is harmless and is cleaned by the next
	// successful apply.
	desired := make([]dataplane.Cert, 0, len(certs))
	for _, c := range certs {
		desired = append(desired, dataplane.Cert{Namespace: c.Namespace, Name: c.Name, Data: c.Data})
	}
	if err := a.Certs.Ensure(desired); err != nil {
		// Not a user-input problem and not a reload: the dataplane cannot
		// be programmed without its certificates, so classify as
		// Programmed=False (Invalid) via the ErrReload class.
		return errs.Reload("syncing TLS certificates to "+a.Certs.CertsDir, err)
	}

	// 2b. Extra files (DESIGN-multinode-addresses.md §5): materialise the
	// desired set BEFORE publishing — the rendered config references the
	// files by absolute path, so a missing file would fail `nginx -t` and
	// (worse) pass validation against a stale one. Orphan deletion runs
	// after the publish succeeded (same ordering contract as certs).
	var extraFiles []dataplane.File
	if a.Files != nil {
		extraFiles = make([]dataplane.File, 0, len(cfg.ExtraFiles))
		for _, ef := range cfg.ExtraFiles {
			extraFiles = append(extraFiles, dataplane.File{Path: ef.Path, Content: ef.Content})
		}
		if err := a.Files.Ensure(extraFiles); err != nil {
			return errs.Reload("syncing extra files under "+filepath.Join(a.ConfDir, "files"), err)
		}
	}

	// 2c. Cluster-proxy mapping (DESIGN-cluster-proxy.md §3): write
	// proxy.json BEFORE publishing — the sidecar binds new sockets from
	// it immediately while the still-running nginx config keeps using the
	// old sockets (nginx -t does not check unix socket existence, so
	// there is no ordering hazard in either direction). "Cleanup" is the
	// atomic replace itself: proxy.json is the only file the controller
	// ever owns in that dir — the *.sock files belong to the sidecar and
	// are never touched here.
	if a.Proxy != nil {
		if err := a.Proxy.Sync(cfg.ProxyMapping); err != nil {
			return errs.Reload("syncing cluster-proxy mapping to "+a.Proxy.Dir, err)
		}
	}

	// 3. Publish the single global configuration (cert paths absolutised,
	// snippet placeholders substituted — a placeholder that resolves
	// against no extra file fails HERE, before anything is written).
	prepared, err := a.prepare(cfg)
	if err != nil {
		return err
	}
	published, err := a.Publisher.Publish(ctx, globalConfName, prepared)
	if err != nil {
		if a.Metrics != nil {
			a.Metrics.ReloadFailures.Add(1)
		}
		return a.classify(err)
	}
	// 4. Publish succeeded: the running config no longer references the
	// removed certificates — safe to clean up orphans now.
	if orphans, err := a.Certs.Cleanup(desired); err != nil {
		a.Log.Error(err, "certificate orphan cleanup failed (will retry next sync)")
	} else if len(orphans) > 0 {
		a.Log.Info("removed orphan certificates", "orphans", orphans)
	}
	if a.Files != nil {
		if orphans, err := a.Files.Cleanup(extraFiles); err != nil {
			a.Log.Error(err, "extra-file orphan cleanup failed (will retry next sync)")
		} else if len(orphans) > 0 {
			a.Log.Info("removed orphan extra files", "orphans", orphans)
		}
	}
	if a.Metrics != nil {
		a.Metrics.ReloadSuccesses.Add(1)
	}
	hash := published.Hash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	a.Log.Info("configuration applied",
		"file", published.File,
		"reloaded", published.Reloaded,
		"hash", hash,
		"servers", len(cfg.Servers),
		"upstreams", len(cfg.Upstreams))
	return nil
}

// withCertPaths returns a shallow-cloned Configuration whose Servers'
// TLSCert fields point at the materialised files. The provider emits the
// bare deterministic filename ("ns_tls.pem"); nginx resolves relative
// ssl_certificate paths against its prefix (/etc/nginx), not the owned
// conf dir, so the rendered directive must carry the concrete path
// <conf-dir>/certs/<basename>. It also pins the http-context error_log to
// a file inside the owned dir so the reload-effect verification (§5.2)
// reads bind failures from a log the controller controls.
func (a *DataplaneApplier) withCertPaths(cfg *contract.Configuration) *contract.Configuration {
	if cfg == nil {
		return nil
	}
	out := &contract.Configuration{
		Upstreams:       cfg.Upstreams,
		Maps:            cfg.Maps,
		SplitClients:    cfg.SplitClients,
		ErrorLog:        a.errorLogPath(),
		ProxySocketsDir: cfg.ProxySocketsDir, // shared upstreams carry Socket names; the renderer needs the dir
	}
	for _, s := range cfg.Servers {
		if s == nil {
			continue
		}
		cp := *s // shallow copy: only TLSCert is adjusted
		if cp.TLSCert != "" {
			cp.TLSCert = filepath.Join(a.ConfDir, "certs", filepath.Base(cp.TLSCert))
		}
		out.Servers = append(out.Servers, &cp)
	}
	return out
}

// prepare returns a clone of cfg ready for rendering: TLSCert fields point
// at the materialised certificate files (withCertPaths) and every snippet
// "@<key>@" placeholder is replaced with the absolute materialised path of
// the matching extra file (<ConfDir>/<ExtraFiles.Path>).
//
// Placeholder semantics (DESIGN-multinode-addresses.md §5): keys resolve
// against the union of the configuration's extra files by file BASENAME;
// when the same key occurs in several refs, the lexicographically first
// materialised path wins (ExtraFiles is sorted by path — deterministic).
// A placeholder matching no extra file is a render-time error: the sync
// FAILS FAST through the Programmed=False path and nothing is written.
//
// DESIGN CHOICE — substitution lives here (the apply lane), not in the
// provider: only the applier knows --nginx-conf-dir, so only it can emit
// the absolute path the way it already does for ssl_certificate. The
// provider carries the raw snippet and the ordered ExtraFiles set instead.
var snippetPlaceholder = regexp.MustCompile(`@([^@]+)@`)

func (a *DataplaneApplier) prepare(cfg *contract.Configuration) (*contract.Configuration, error) {
	out := a.withCertPaths(cfg)
	if out == nil {
		return nil, nil
	}
	keyPaths := make(map[string]string, len(cfg.ExtraFiles))
	for _, ef := range cfg.ExtraFiles {
		key := filepath.Base(ef.Path)
		if _, dup := keyPaths[key]; !dup {
			keyPaths[key] = filepath.Join(a.ConfDir, ef.Path)
		}
	}
	if len(keyPaths) == 0 {
		// No extra files: any placeholder in a snippet is an error, but
		// only snippets can contain one — skip the cloning work when none.
		hasSnippet := false
		for _, s := range out.Servers {
			if s.RawServerSnippet != "" {
				hasSnippet = true
				break
			}
			for _, l := range s.Locations {
				if l.RawSnippet != "" {
					hasSnippet = true
					break
				}
			}
		}
		if !hasSnippet {
			return out, nil
		}
	}
	substitute := func(owner, raw string) (string, error) {
		var missing string
		replaced := snippetPlaceholder.ReplaceAllStringFunc(raw, func(m string) string {
			if p, ok := keyPaths[m[1:len(m)-1]]; ok {
				return p
			}
			if missing == "" {
				missing = m[1 : len(m)-1]
			}
			return m
		})
		if missing != "" {
			return "", errs.Reload(
				fmt.Sprintf("resolving nginx snippet placeholders of %s: @%s@ matches no extra file (hng.victrid.dev/extra-files)", owner, missing),
				fmt.Errorf("unknown placeholder @%s@", missing))
		}
		return replaced, nil
	}
	for _, s := range out.Servers {
		if s.RawServerSnippet != "" {
			replaced, err := substitute("server block "+s.Hostname, s.RawServerSnippet)
			if err != nil {
				return nil, err
			}
			s.RawServerSnippet = replaced
		}
		for i, loc := range s.Locations {
			if loc.RawSnippet == "" {
				continue
			}
			replaced, err := substitute("location "+loc.Path, loc.RawSnippet)
			if err != nil {
				return nil, err
			}
			// Locations are shared with the input configuration
			// (withCertPaths only clones the server structs) — copy
			// before mutating.
			lc := *loc
			lc.RawSnippet = replaced
			s.Locations[i] = &lc
		}
	}
	return out, nil
}

// errorLogPath is the owned error log for reload-effect verification
// (<conf-dir>/error.log). An explicit "--nginx-error-log=off" disables it.
func (a *DataplaneApplier) errorLogPath() string {
	if a.ErrorLogPath != "" && a.ErrorLogPath == "off" {
		return ""
	}
	if a.ErrorLogPath != "" {
		return a.ErrorLogPath
	}
	return filepath.Join(a.ConfDir, "error.log")
}

// classify maps a dataplane error onto the internal/errs taxonomy so
// provider's errs.StatusReason produces the DESIGN.md §3.4 reasons.
//
// The publisher wraps validator failures without a sentinel (it only
// carries dataplane.ErrReload / dataplane.ErrNginxNotRunning /
// dataplane.ErrIncludeMissing as sentinels), so the nginx -t failure path
// is recognized via the publisher's stable "publish: validate:" wrap
// prefix (publisher.go applyRendered step 2). §3.4 classifies nginx -t
// failures as Programmed=False (Invalid) — the errs.ErrReload class.
func (a *DataplaneApplier) classify(err error) error {
	switch {
	case errors.Is(err, dataplane.ErrNginxNotRunning):
		// nginx died between the probe and the reload: §3.4 Pending.
		return errs.NginxNotRunning("nginx became unreachable during apply", err)
	case errors.Is(err, dataplane.ErrReload):
		return errs.Reload("nginx configuration apply failed", err)
	case errors.Is(err, dataplane.ErrIncludeMissing):
		// The message must tell the operator the exact line to add
		// (DESIGN.md §2: "message 说明需添加的 include 行").
		return errs.IncludeMissing(
			fmt.Sprintf("%s — add \"include %s/*.conf;\" to the http block of your nginx.conf", a.ConfDir, a.ConfDir), err)
	case strings.Contains(err.Error(), "publish: validate:"):
		return errs.Reload("nginx -t validation failed", err)
	default:
		// Render/IO surprises: left unclassified on purpose — the provider
		// maps unknown errors to Programmed=False (Invalid) with an
		// "internal error" message, never to a false Programmed=True.
		return fmt.Errorf("dataplane apply: %w", err)
	}
}
