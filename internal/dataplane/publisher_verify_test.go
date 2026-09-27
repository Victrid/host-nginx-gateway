// Unit tests for the reload-effect verification (DESIGN.md §5.2, round 5):
// `nginx -s reload` exits 0 even when the NEW workers fail their binds and
// the master silently keeps serving the previous configuration. After a
// reload that adds listen sockets, the publisher tails the error log for
// bind failures and rolls back when one appears — EXCEPT EADDRINUSE (98)
// lines for sockets the previously-applied config already listens on: the
// running master hands those over via inherited-fd reuse, so they do not
// mean the reload was rejected (the nsenter -n fix makes exactly those
// lines observable, since `nginx -t`/reload now run against the host's
// real addresses and ports). Unassignable addresses (99) always roll back.
package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// bindFailNginxClient simulates the "reload signal blindness": Reload
// exits 0, then the (new workers') bind failure appears in the error log.
type bindFailNginxClient struct {
	fakeNginxClient
	logPath        string
	logLine        string
	appendOnReload int // 1-based reload count that appends the failure
}

func (f *bindFailNginxClient) Reload(ctx context.Context) error {
	if err := f.fakeNginxClient.Reload(ctx); err != nil {
		return err
	}
	if f.logPath != "" && f.logLine != "" && f.reloadCnt == f.appendOnReload {
		fh, err := os.OpenFile(f.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer fh.Close()
		_, _ = fh.WriteString(f.logLine + "\n")
	}
	return nil
}

func cfgWithListen(port int) *contract.Configuration {
	cfg := basicCfg()
	cfg.Servers[0].Listens = []contract.Listen{{Port: port}}
	return cfg
}

func TestPublisher_VerifyReloadDetectsBindFailureAndRollsBack(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	logPath := filepath.Join(tmp, "error.log")
	if err := os.WriteFile(logPath, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nginx := &bindFailNginxClient{
		logPath:        logPath,
		logLine:        `2026/09/23 10:00:00 [emerg] 123#123: bind() to 127.0.0.8:8081 failed (98: Address already in use)`,
		appendOnReload: 2, // the second reload call = the failed new-listen one
	}
	pub, err := NewPublisher(PublisherOptions{
		OutputDir:         dir,
		Validator:         val,
		Nginx:             nginx,
		ErrorLogPath:      logPath,
		VerifyReloadDelay: 400 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	// Establish a first good config (port 8080) so the rollback path has a
	// .prev to restore.
	if _, err := pub.Publish(context.Background(), "default_app", cfgWithListen(8080)); err != nil {
		t.Fatalf("base publish: %v", err)
	}
	baseReloads := nginx.reloadCnt

	// Second publish adds a NEW listen (8081); the reload is signalled OK
	// but the bind fails → verification must catch it and roll back.
	// (EADDRINUSE on a genuinely NEW socket — e.g. a foreign process holds
	// 8081 — is a real rejection, unlike 98 on a master-held socket below.)
	_, err = pub.Publish(context.Background(), "default_app", cfgWithListen(8081))
	if !errors.Is(err, ErrReload) {
		t.Fatalf("bind failure must surface as ErrReload, got %v", err)
	}
	if !strings.Contains(err.Error(), "bind() to 127.0.0.8:8081 failed") {
		t.Fatalf("the bind error must be in the message: %v", err)
	}
	// The failed reload + the rollback reload (the .prev config is restored
	// and reloaded).
	if nginx.reloadCnt != baseReloads+2 {
		t.Fatalf("expected failed reload + rollback reload, got %d (base %d)", nginx.reloadCnt, baseReloads)
	}
	// The applied hash must still be the BASE config's: a retry of the
	// failed desired state re-applies (not a no-op), the base config no-ops.
	hash8081 := Hash(RenderedForTest(t, cfgWithListen(8081)))
	if h, _ := pub.Applied("default_app"); h == hash8081 {
		t.Fatal("failed config must not be recorded as applied")
	}
	base := cfgWithListen(8080)
	cnt := nginx.reloadCnt
	if _, err := pub.Publish(context.Background(), "default_app", base); err != nil {
		t.Fatalf("base re-publish: %v", err)
	}
	if nginx.reloadCnt != cnt {
		t.Fatal("base config re-publish must no-op (hash unchanged)")
	}
	retry := cfgWithListen(8081)
	cnt = nginx.reloadCnt
	nginx.appendOnReload = cnt + 1 // the retry's own reload fails again
	_, err = pub.Publish(context.Background(), "default_app", retry)
	if !errors.Is(err, ErrReload) {
		t.Fatalf("retry of the failed config must re-apply (not no-op): %v", err)
	}
	if nginx.reloadCnt <= cnt {
		t.Fatalf("retry must reload (failed reload + rollback), got %d → %d", cnt, nginx.reloadCnt)
	}
}

// RenderedForTest exposes the deterministic renderer to tests in this file.
func RenderedForTest(t *testing.T, cfg *contract.Configuration) []byte {
	t.Helper()
	b, err := Render(cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return b
}

func TestPublisher_VerifyReloadPassesWhenBindsSucceed(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	logPath := filepath.Join(tmp, "error.log")
	if err := os.WriteFile(logPath, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nginx := &fakeNginxClient{}
	pub, err := NewPublisher(PublisherOptions{
		OutputDir:         dir,
		Validator:         val,
		Nginx:             nginx,
		ErrorLogPath:      logPath,
		VerifyReloadDelay: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	res, err := pub.Publish(context.Background(), "default_app", cfgWithListen(8081))
	if err != nil {
		t.Fatalf("clean reload must succeed: %v", err)
	}
	if !res.Reloaded || nginx.reloadCnt != 1 {
		t.Fatalf("single clean reload expected, got %d", nginx.reloadCnt)
	}
	if _, ok := pub.Applied("default_app"); !ok {
		t.Fatal("success must be recorded as applied")
	}
}

func TestPublisher_NoNewListensSkipsVerificationWait(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	logPath := filepath.Join(tmp, "error.log")
	if err := os.WriteFile(logPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	nginx := &fakeNginxClient{}
	pub, err := NewPublisher(PublisherOptions{
		OutputDir:         dir,
		Validator:         val,
		Nginx:             nginx,
		ErrorLogPath:      logPath,
		VerifyReloadDelay: 400 * time.Millisecond, // would stall the test if waited
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	// First publish establishes the listen set (verifies, fast because the
	// log stays clean), second publish changes only locations (same listen
	// set → verification skipped entirely).
	if _, err := pub.Publish(context.Background(), "default_app", cfgWithListen(8081)); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	cfg2 := cfgWithListen(8081)
	cfg2.Servers[0].Locations = []*contract.Location{{Path: "/v2", Upstream: "ns_svc_80"}}
	start := time.Now()
	if _, err := pub.Publish(context.Background(), "default_app", cfg2); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("no-new-listen publish must skip the verification window, took %s", elapsed)
	}
}

func TestPublisher_MissingErrorLogSkipsVerification(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	nginx := &fakeNginxClient{}
	pub, err := NewPublisher(PublisherOptions{
		OutputDir:         dir,
		Validator:         val,
		Nginx:             nginx,
		ErrorLogPath:      filepath.Join(tmp, "does-not-exist.log"),
		VerifyReloadDelay: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if _, err := pub.Publish(context.Background(), "default_app", cfgWithListen(8081)); err != nil {
		t.Fatalf("verification must be best-effort: %v", err)
	}
}

func TestFindBindFailure(t *testing.T) {
	inUse := "start\n2026/09/23 [emerg] bind() to 0.0.0.0:443 failed (98: Address already in use)\n"
	// EADDRINUSE (98) on a socket the previously-applied config holds is
	// benign — the running master hands the socket over on reload.
	held := socketTextForms([]string{":443"})
	if line, ok := findBindFailure([]byte(inUse), held); ok {
		t.Fatalf("98 on a held socket must be tolerated, got %q", line)
	}
	// The same line is a rejection when no config holds the socket (foreign
	// holder, or the very first publish).
	if _, ok := findBindFailure([]byte(inUse), nil); !ok {
		t.Fatal("98 on a non-held socket must fail")
	}
	// Unassignable addresses (99) fail even when the address is held.
	nine9 := "2026/09/23 [emerg] bind() to [2001:db8::1]:443 failed (99: Cannot assign requested address)\n"
	line, ok := findBindFailure([]byte(nine9), socketTextForms([]string{"[2001:db8::1]:443"}))
	if !ok || !strings.Contains(line, "99: Cannot assign requested address") {
		t.Fatalf("unassignable address must fail even when held: %q %v", line, ok)
	}
	if _, ok := findBindFailure([]byte("all fine\nupstream ready\n"), nil); ok {
		t.Fatal("false positive on clean log")
	}
	if _, ok := findBindFailure(nil, nil); ok {
		t.Fatal("empty log must not trip")
	}
}

func TestSocketTextForms(t *testing.T) {
	got := socketTextForms([]string{":8080", "0.0.0.0:8081", "[::]:8082", "192.168.1.10:8083"})
	want := map[string]struct{}{
		"0.0.0.0:8080":      {},
		"*:8080":            {},
		"0.0.0.0:8081":      {},
		"*:8081":            {},
		"[::]:8082":         {},
		"192.168.1.10:8083": {},
	}
	if len(got) != len(want) {
		t.Fatalf("socketTextForms = %v, want %v", got, want)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("socketTextForms missing %q: %v", k, got)
		}
	}
	if socketTextForms(nil) != nil {
		t.Fatal("nil keys must map to nil")
	}
}

// TestPublisher_VerifyToleratesEADDRINUSEOnHeldSockets: a reload that adds a
// new socket (8081) while KEEPING an existing one (8080) must not be rolled
// back because the log shows `bind() to 0.0.0.0:8080 failed (98 …)` for the
// socket the master already holds — nginx hands held sockets over on reload
// (inherited-fd reuse), so that line is noise, not a rejection.
func TestPublisher_VerifyToleratesEADDRINUSEOnHeldSockets(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	logPath := filepath.Join(tmp, "error.log")
	if err := os.WriteFile(logPath, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nginx := &bindFailNginxClient{
		logPath:        logPath,
		logLine:        `2026/09/23 10:00:00 [emerg] 123#123: bind() to 0.0.0.0:8080 failed (98: Address already in use)`,
		appendOnReload: 2, // the second reload call = the verified new-listen one
	}
	pub, err := NewPublisher(PublisherOptions{
		OutputDir:         dir,
		Validator:         val,
		Nginx:             nginx,
		ErrorLogPath:      logPath,
		VerifyReloadDelay: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	twoListens := func() *contract.Configuration {
		cfg := basicCfg()
		cfg.Servers[0].Listens = []contract.Listen{{Port: 8080}, {Port: 8081}}
		return cfg
	}
	if _, err := pub.Publish(context.Background(), "default_app", cfgWithListen(8080)); err != nil {
		t.Fatalf("base publish: %v", err)
	}
	res, err := pub.Publish(context.Background(), "default_app", twoListens())
	if err != nil {
		t.Fatalf("98 on a master-held socket must not roll back: %v", err)
	}
	if !res.Reloaded {
		t.Fatal("the new-listen publish must be recorded as reloaded")
	}
	if _, ok := pub.Applied("default_app"); !ok {
		t.Fatal("success must be recorded as applied")
	}
}

// TestPublisher_VerifyRollsBackOnUnassignableAddress: errno 99
// (EADDRNOTAVAIL — the address does not exist on the host, e.g. a node-global
// IPv6 gone missing) is ALWAYS a rejection, held or not.
func TestPublisher_VerifyRollsBackOnUnassignableAddress(t *testing.T) {
	val, tmp := newValidatorForTest(t)
	dir := filepath.Join(tmp, "out")
	logPath := filepath.Join(tmp, "error.log")
	if err := os.WriteFile(logPath, []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nginx := &bindFailNginxClient{
		logPath:        logPath,
		logLine:        `2026/09/23 10:00:00 [emerg] 123#123: bind() to 2001:db8::1:8081 failed (99: Cannot assign requested address)`,
		appendOnReload: 2,
	}
	pub, err := NewPublisher(PublisherOptions{
		OutputDir:         dir,
		Validator:         val,
		Nginx:             nginx,
		ErrorLogPath:      logPath,
		VerifyReloadDelay: 400 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if _, err := pub.Publish(context.Background(), "default_app", cfgWithListen(8080)); err != nil {
		t.Fatalf("base publish: %v", err)
	}
	twoListens := func() *contract.Configuration {
		cfg := basicCfg()
		cfg.Servers[0].Listens = []contract.Listen{{Port: 8080}, {Port: 8081}}
		return cfg
	}
	_, err = pub.Publish(context.Background(), "default_app", twoListens())
	if !errors.Is(err, ErrReload) {
		t.Fatalf("99 must surface as ErrReload, got %v", err)
	}
	if !strings.Contains(err.Error(), "(99: Cannot assign requested address)") {
		t.Fatalf("the 99 bind error must be in the message: %v", err)
	}
}
