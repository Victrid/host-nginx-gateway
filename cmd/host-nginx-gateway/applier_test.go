// Tests for DataplaneApplier (the provider.Applier seam):
//   - probe failure → errs.ErrNginxNotRunning (Programmed=False / Pending);
//   - happy path → certs materialised, single global conf published with
//     absolute cert paths, reload issued, metrics bumped;
//   - nginx -t failure and reload failure → errs.ErrReload
//     (Programmed=False / Invalid).
//
// The real nginx binary is replaced by a fake shell script (same trick as
// internal/dataplane's validator tests); the NginxClient is a hand-rolled
// fake because dataplane's test double is package-private.
package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	logr "github.com/go-logr/logr"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/dataplane"
	"github.com/Victrid/HostNginxGateway/internal/errs"
	"github.com/Victrid/HostNginxGateway/internal/provider"
)

// fakeNginx mirrors dataplane.NginxClient for cmd tests.
type fakeNginx struct {
	probeErr  error
	reloadErr error
	probes    int
	reloads   int
}

// discardLogger returns a logr.Logger that swallows output (zap's
// controller-runtime wrapper with a nil sink would panic; the test log
// backend writes to os.Stderr only via t.Log when verbose — a dev-mode
// quiet logger is simpler).
func discardLogger() logr.Logger {
	return logr.Discard()
}

func (f *fakeNginx) Probe() error { f.probes++; return f.probeErr }
func (f *fakeNginx) Reload(_ context.Context) error {
	f.reloads++
	return f.reloadErr
}

// writeFakeNginxBinary scripts an nginx replacement honouring FAKE_NGINX_FAIL
// for -t / -s invocations (mirrors internal/dataplane's test helper).
func writeFakeNginxBinary(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "-t" ] || [ "$1" = "-s" ]; then
  if [ -n "$FAKE_NGINX_FAIL" ]; then
    echo "$FAKE_NGINX_FAIL" >&2
    exit 1
  fi
  echo "nginx: configuration test successful" >&2
fi
exit 0
`
	path := filepath.Join(dir, "fake-nginx")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nginx: %v", err)
	}
	return path
}

func newTestApplier(t *testing.T, nginx *fakeNginx) (*DataplaneApplier, *Metrics, string) {
	t.Helper()
	tmp := t.TempDir()
	bin := writeFakeNginxBinary(t, tmp)

	// Main config for the temporary-main-config validation (§5.1).
	mainPath := filepath.Join(tmp, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte("events {}\nhttp {}\n"), 0o644); err != nil {
		t.Fatalf("write main config: %v", err)
	}

	confDir := filepath.Join(tmp, "k8s-gw")
	publisher, err := dataplane.NewPublisher(dataplane.PublisherOptions{
		OutputDir: confDir,
		Validator: &dataplane.Validator{
			NginxBinary:    bin,
			MainConfigPath: mainPath,
			Prefix:         tmp,
			// Tests run outside a pod (no CAP_SYS_ADMIN): inject a plain
			// exec Commander instead of the nsenter production default.
			Commander: func(ctx context.Context, name string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, name, args...)
			},
		},
		Nginx: nginx,
	})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}

	metrics := &Metrics{}
	return &DataplaneApplier{
		ConfDir:   confDir,
		Nginx:     nginx,
		Certs:     dataplane.NewCertsManager(filepath.Join(confDir, "certs")),
		Publisher: publisher,
		Metrics:   metrics,
		Log:       discardLogger(),
	}, metrics, confDir
}

func testConfig() *contract.Configuration {
	return &contract.Configuration{
		Upstreams: []*contract.Upstream{{
			Name:      "ns_svc_80",
			Endpoints: []contract.Endpoint{{IP: "10.0.0.1", Port: 80, Ready: true}},
		}},
		Servers: []*contract.Server{{
			Hostname: "example.com",
			Listens:  []contract.Listen{{Port: 80, SSL: true}},
			TLSCert:  "ns_tls.pem",
			Locations: []*contract.Location{
				{Path: "/", Upstream: "ns_svc_80"},
				{Path: "/missing", Upstream: "hng_static_500"},
			},
		}},
	}
}

func testCerts() []provider.Certificate {
	return []provider.Certificate{{
		Namespace: "ns",
		Name:      "tls",
		Data:      []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
	}}
}

func TestApply_NginxNotRunning(t *testing.T) {
	applier, _, _ := newTestApplier(t, &fakeNginx{probeErr: errors.New("no pid file")})
	err := applier.Apply(context.Background(), testConfig(), testCerts())
	if !errors.Is(err, errs.ErrNginxNotRunning) {
		t.Fatalf("Apply err = %v, want errs.ErrNginxNotRunning", err)
	}
}

func TestApply_HappyPath(t *testing.T) {
	nginx := &fakeNginx{}
	applier, metrics, confDir := newTestApplier(t, nginx)

	if err := applier.Apply(context.Background(), testConfig(), testCerts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Certificate materialised under <conf-dir>/certs/ with the basename
	// from Server.TLSCert.
	certPath := filepath.Join(confDir, "certs", "ns_tls.pem")
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert not materialised at %s: %v", certPath, err)
	}

	// Single global config published.
	conf, err := os.ReadFile(filepath.Join(confDir, globalConfName+".conf"))
	if err != nil {
		t.Fatalf("global conf not written: %v", err)
	}
	s := string(conf)
	for _, want := range []string{
		"ssl_certificate " + certPath + ";",
		"ssl_certificate_key " + certPath + ";",
		"proxy_pass http://ns_svc_80;",
		"return 500;", // static marker location
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered conf missing %q\ngot:\n%s", want, s)
		}
	}

	// Reload issued exactly once; metrics reflect one sync + one success.
	if nginx.reloads != 1 {
		t.Errorf("reloads = %d, want 1", nginx.reloads)
	}
	if got := metrics.Reconciles.Load(); got != 1 {
		t.Errorf("reconciles = %d, want 1", got)
	}
	if got := metrics.ReloadSuccesses.Load(); got != 1 {
		t.Errorf("successes = %d, want 1", got)
	}
	if got := metrics.ReloadFailures.Load(); got != 0 {
		t.Errorf("failures = %d, want 0", got)
	}
}

func TestApply_NoOpSecondSyncStillCountsSuccess(t *testing.T) {
	nginx := &fakeNginx{}
	applier, metrics, _ := newTestApplier(t, nginx)
	ctx := context.Background()
	if err := applier.Apply(ctx, testConfig(), testCerts()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Second sync with identical input hits the applied-hash no-op (S6);
	// it is still a successful apply.
	if err := applier.Apply(ctx, testConfig(), testCerts()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := metrics.ReloadSuccesses.Load(); got != 2 {
		t.Errorf("successes = %d, want 2", got)
	}
}

func TestApply_ValidationFailureClassifiedAsReload(t *testing.T) {
	nginx := &fakeNginx{}
	applier, metrics, _ := newTestApplier(t, nginx)
	t.Setenv("FAKE_NGINX_FAIL", "emerg: broken config")
	err := applier.Apply(context.Background(), testConfig(), testCerts())
	if !errors.Is(err, errs.ErrReload) {
		t.Fatalf("Apply err = %v, want errs.ErrReload (nginx -t failure)", err)
	}
	if got := metrics.ReloadFailures.Load(); got != 1 {
		t.Errorf("failures = %d, want 1", got)
	}
}

func TestApply_ReloadFailureClassifiedAsReload(t *testing.T) {
	nginx := &fakeNginx{reloadErr: errors.New("reload: operation not permitted")}
	applier, _, _ := newTestApplier(t, nginx)
	err := applier.Apply(context.Background(), testConfig(), testCerts())
	if !errors.Is(err, errs.ErrReload) {
		t.Fatalf("Apply err = %v, want errs.ErrReload", err)
	}
}

func TestApply_CertSyncFailureIsTyped(t *testing.T) {
	nginx := &fakeNginx{}
	applier, _, _ := newTestApplier(t, nginx)
	// A cert with no namespace cannot be materialised (dataplane.CertsManager
	// rejects it) — must surface as a typed error, never a silent success.
	certs := []provider.Certificate{{Namespace: "", Name: "tls", Data: []byte("x")}}
	err := applier.Apply(context.Background(), testConfig(), certs)
	if err == nil {
		t.Fatal("Apply succeeded with an unmaterialisable certificate")
	}
	if !errors.Is(err, errs.ErrReload) {
		t.Fatalf("Apply err = %v, want errs.ErrReload class", err)
	}
}

func TestWithCertPaths(t *testing.T) {
	a := &DataplaneApplier{ConfDir: "/etc/nginx/conf.d/k8s-gw"}

	cfg := &contract.Configuration{
		Upstreams: []*contract.Upstream{{Name: "u"}},
		Servers: []*contract.Server{
			{Hostname: "tls.example.com", TLSCert: "ns_tls.pem"},
			{Hostname: "plain.example.com"},
		},
	}
	got := a.withCertPaths(cfg)

	// Upstreams shared untouched; TLS path rewritten; plain server untouched.
	if len(got.Upstreams) != 1 || got.Upstreams[0] != cfg.Upstreams[0] {
		t.Errorf("upstreams not preserved")
	}
	if want := "/etc/nginx/conf.d/k8s-gw/certs/ns_tls.pem"; got.Servers[0].TLSCert != want {
		t.Errorf("TLSCert = %q, want %q", got.Servers[0].TLSCert, want)
	}
	if got.Servers[1].TLSCert != "" {
		t.Errorf("plain server TLSCert = %q, want empty", got.Servers[1].TLSCert)
	}
	// The input must not be mutated.
	if cfg.Servers[0].TLSCert != "ns_tls.pem" {
		t.Errorf("input cfg mutated: TLSCert = %q", cfg.Servers[0].TLSCert)
	}
	// Nil-safe.
	if a.withCertPaths(nil) != nil {
		t.Errorf("withCertPaths(nil) != nil")
	}
}
