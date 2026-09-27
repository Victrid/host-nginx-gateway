// Unit tests for Publisher (DESIGN.md §5.2) and the applied-hash no-op (S6).
// The fake nginx script (see validator_test.go) doubles as the reload binary
// in these tests.

package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// fakeNginxClient is a test double for NginxClient. It is exported as a type
// alias so the publisher tests can construct one.
type fakeNginxClient struct {
	probeErr   error
	reloadErr  error
	reloadCnt  int
	probeCnt   int
	lastReload context.Context
}

func (f *fakeNginxClient) Probe() error {
	f.probeCnt++
	return f.probeErr
}

func (f *fakeNginxClient) Reload(ctx context.Context) error {
	f.reloadCnt++
	f.lastReload = ctx
	return f.reloadErr
}

func newValidatorForTest(t *testing.T) (*Validator, string) {
	t.Helper()
	tmp := t.TempDir()
	bin := writeFakeNginx(t, tmp)
	mainPath := filepath.Join(tmp, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte("http {}\n"), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	t.Setenv("FAKE_NGINX_LOG", filepath.Join(tmp, "nginx.log"))
	return &Validator{
		NginxBinary:    bin,
		MainConfigPath: mainPath,
		Prefix:         tmp,
		Commander:      directExecCommander,
	}, tmp
}

func basicCfg() *contract.Configuration {
	return &contract.Configuration{
		Upstreams: []*contract.Upstream{{
			Name: "ns_svc_80",
			Endpoints: []contract.Endpoint{
				{IP: "10.0.0.1", Port: 80, Ready: true},
			},
		}},
		Servers: []*contract.Server{{
			Hostname: "example.com",
			Listens:  []contract.Listen{{Port: 80}},
			Locations: []*contract.Location{{
				Path: "/", Upstream: "ns_svc_80",
			}},
		}},
	}
}

func TestPublisher_SuccessPath(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx:     &fakeNginxClient{},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	res, err := pub.Publish(context.Background(), "default_app", basicCfg())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Reloaded {
		t.Errorf("expected Reloaded=true on first publish")
	}
	if res.Hash == "" {
		t.Errorf("expected non-empty hash")
	}
	// The config file should exist on disk.
	path := filepath.Join(dir, "default_app.conf")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
	}
	// And the .prev file should be cleaned up.
	if _, err := os.Stat(path + ".prev"); err == nil {
		t.Errorf("expected .prev to be cleaned up after success")
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Errorf("expected .tmp to be cleaned up after rename")
	}
}

func TestPublisher_AppliedHashNoOp(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx:     &fakeNginxClient{},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	// First publish: should reload.
	res1, err := pub.Publish(context.Background(), "default_app", basicCfg())
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if !res1.Reloaded {
		t.Fatalf("expected first publish to reload")
	}
	// Second publish with same content: should NO-OP (skip reload).
	res2, err := pub.Publish(context.Background(), "default_app", basicCfg())
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if res2.Reloaded {
		t.Errorf("expected second publish to skip reload (S6 no-op)")
	}
	if res2.Hash != res1.Hash {
		t.Errorf("hash mismatch between publishes")
	}
	nginx := pub.opts.Nginx.(*fakeNginxClient)
	if nginx.reloadCnt != 1 {
		t.Errorf("expected exactly 1 reload, got %d", nginx.reloadCnt)
	}
}

func TestPublisher_ContentChangeTriggersReload(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx:     &fakeNginxClient{},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	cfg := basicCfg()
	if _, err := pub.Publish(context.Background(), "default_app", cfg); err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	// Change content (e.g., add a second endpoint) -> hash changes -> reload.
	cfg.Upstreams[0].Endpoints = append(cfg.Upstreams[0].Endpoints,
		contract.Endpoint{IP: "10.0.0.2", Port: 80, Ready: true})
	res, err := pub.Publish(context.Background(), "default_app", cfg)
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if !res.Reloaded {
		t.Errorf("expected reload after content change")
	}
	nginx := pub.opts.Nginx.(*fakeNginxClient)
	if nginx.reloadCnt != 2 {
		t.Errorf("expected 2 reloads, got %d", nginx.reloadCnt)
	}
}

func TestPublisher_ReloadFailureRollsBack(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx: &fakeNginxClient{
			reloadErr: errors.New("reload-fail-once"),
		},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	// First publish: will fail at reload -> rollback (no .prev existed, so
	// the final config gets deleted).
	_, err = pub.Publish(context.Background(), "default_app", basicCfg())
	if err == nil {
		t.Fatalf("expected error on reload failure")
	}
	if !errors.Is(err, ErrReload) {
		t.Fatalf("expected ErrReload in chain, got %v", err)
	}
	path := filepath.Join(dir, "default_app.conf")
	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected broken .conf to be removed after rollback")
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Errorf("expected .tmp to be cleaned up")
	}
}

func TestPublisher_ReloadFailureRestoresPrev(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx:     &fakeNginxClient{},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	// First publish: succeeds, .prev is cleaned up, hash stored.
	if _, err := pub.Publish(context.Background(), "default_app", basicCfg()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	originalBytes, _ := os.ReadFile(filepath.Join(dir, "default_app.conf"))

	// Force next reload to fail: change content AND swap nginx client to one
	// that fails on reload.
	pub.opts.Nginx = &fakeNginxClient{reloadErr: errors.New("boom")}

	cfg2 := basicCfg()
	cfg2.Servers[0].Hostname = "changed.example.com"
	_, err = pub.Publish(context.Background(), "default_app", cfg2)
	if err == nil || !errors.Is(err, ErrReload) {
		t.Fatalf("expected ErrReload, got %v", err)
	}

	// .prev file should have been consumed during rollback; final .conf
	// should be the original (since the failed reload was rolled back).
	path := filepath.Join(dir, "default_app.conf")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final conf after rollback: %v", err)
	}
	if string(got) != string(originalBytes) {
		t.Errorf("expected final conf to match original after rollback")
	}
	if _, err := os.Stat(path + ".prev"); err == nil {
		t.Errorf("expected .prev to be consumed during rollback")
	}
}

func TestPublisher_ProbeBeforeReloadFails(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx: &fakeNginxClient{
			probeErr: ErrNginxNotRunning,
		},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	_, err = pub.Publish(context.Background(), "default_app", basicCfg())
	if err == nil {
		t.Fatalf("expected error when nginx not running")
	}
	if !errors.Is(err, ErrNginxNotRunning) {
		t.Fatalf("expected ErrNginxNotRunning in chain, got %v", err)
	}
	nginx := pub.opts.Nginx.(*fakeNginxClient)
	if nginx.reloadCnt != 0 {
		t.Errorf("expected 0 reloads when probe fails, got %d", nginx.reloadCnt)
	}
}

func TestPublisher_ValidationFailureNoFileWritten(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	// Force fake-nginx to fail so validator rejects the rendered config.
	t.Setenv("FAKE_NGINX_FAIL", "bad config")

	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx:     &fakeNginxClient{},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	_, err = pub.Publish(context.Background(), "default_app", basicCfg())
	if err == nil {
		t.Fatalf("expected validation failure")
	}
	if !strings.Contains(err.Error(), "validate") {
		t.Errorf("expected validate error in chain, got %v", err)
	}
	// No file should have been left behind.
	if _, err := os.Stat(filepath.Join(dir, "default_app.conf")); err == nil {
		t.Errorf("expected no final conf when validation fails")
	}
}

func TestPublisher_ForgetClearsHash(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	pub, err := NewPublisher(PublisherOptions{
		OutputDir: dir,
		Validator: val,
		Nginx:     &fakeNginxClient{},
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if _, err := pub.Publish(context.Background(), "default_app", basicCfg()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	h1, ok := pub.Applied("default_app")
	if !ok || h1 == "" {
		t.Fatalf("expected Applied hash to be recorded")
	}
	pub.Forget("default_app")
	if _, ok := pub.Applied("default_app"); ok {
		t.Errorf("expected Applied hash to be cleared after Forget")
	}
}
