// Unit tests for Validator (DESIGN.md §5.1) and the underlying include-detection
// helpers. We do not require a real nginx binary; a fake shell script is written
// to t.TempDir() that simulates pass / fail / injected-include behaviour.

package dataplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// directExecCommander is the test-injected Commander used where the old
// direct-exec default was exercised: it execs the binary in this process's
// own namespaces (no nsenter — the tests run outside a pod and lack
// CAP_SYS_ADMIN). Production wiring injects NsenterCommander(1).
func directExecCommander(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// writeFakeNginx creates a small shell script that emulates nginx -t for tests.
// The script:
//   - on call with -t: exits 0 and prints "nginx: configuration test successful"
//     to stderr, unless FAKE_NGINX_FAIL is set to a non-empty value, in which
//     case it exits 1 and prints the value of FAKE_NGINX_FAIL.
//   - records its invocation in FAKE_NGINX_LOG (one line per invocation).
func writeFakeNginx(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
LOG="$FAKE_NGINX_LOG"
echo "$*" >> "$LOG"
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

func TestValidator_IncludesAlreadyExplicit(t *testing.T) {
	main := []byte(`
http {
    include /etc/nginx/conf.d/k8s-gw/*.conf;
}
`)
	if !configIncludesTarget(main) {
		t.Fatalf("expected include detection to match explicit pattern")
	}
}

func TestValidator_IncludesConfDWildcard(t *testing.T) {
	// nginx include globs do NOT recurse: a bare "conf.d/*.conf" wildcard
	// matches files directly in conf.d only, never conf.d/k8s-gw/*.conf
	// (verified against nginx 1.30 during E2E). It must NOT count as
	// coverage — otherwise injection, validation and RequireInclude all
	// falsely pass while the running master never loads our files.
	main := []byte(`
http {
    include /etc/nginx/conf.d/*.conf;
}
`)
	if configIncludesTarget(main) {
		t.Fatalf("conf.d/*.conf wildcard must not count as covering k8s-gw/")
	}
}

func TestValidator_IncludesSiblingFileNotCovered(t *testing.T) {
	// A sibling FILE named k8s-gw.conf does not include the directory's
	// contents.
	main := []byte("http {\n    include /etc/nginx/conf.d/k8s-gw.conf;\n}\n")
	if configIncludesTarget(main) {
		t.Fatalf("k8s-gw.conf sibling file must not count as coverage")
	}
}

func TestValidator_IncludesRelativePath(t *testing.T) {
	main := []byte(`
http {
    include conf.d/k8s-gw/*.conf;
}
`)
	if !configIncludesTarget(main) {
		t.Fatalf("expected include detection to match relative k8s-gw path")
	}
}

func TestValidator_IncludesMissing(t *testing.T) {
	main := []byte(`
http {
    include mime.types;
    include fastcgi_params;
}
`)
	if configIncludesTarget(main) {
		t.Fatalf("expected include detection to NOT match unrelated paths")
	}
}

func TestValidator_InjectIncludeIntoHTTP(t *testing.T) {
	main := []byte("http {\n    server { listen 80; }\n}\n")
	out, already, err := InjectInclude(main, true)
	if err != nil {
		t.Fatalf("InjectInclude: %v", err)
	}
	if already {
		t.Fatalf("expected injected, not already-included")
	}
	if !strings.Contains(string(out), "include /etc/nginx/conf.d/k8s-gw/*.conf;") {
		t.Fatalf("expected injected include line, got:\n%s", out)
	}
	if !strings.HasPrefix(string(out), "http {") {
		t.Fatalf("output should still begin with http block, got:\n%s", out)
	}
}

func TestValidator_NoInjectionWhenAlreadyIncluded(t *testing.T) {
	main := []byte("http {\n    include /etc/nginx/conf.d/k8s-gw/*.conf;\n}\n")
	out, already, err := InjectInclude(main, true)
	if err != nil {
		t.Fatalf("InjectInclude: %v", err)
	}
	if !already {
		t.Fatalf("expected already-included flag")
	}
	if string(out) != string(main) {
		t.Fatalf("expected unchanged output, got:\n%s", out)
	}
}

func TestValidator_NoHTTPBlockError(t *testing.T) {
	// A config without an http block is invalid; we surface that as an
	// error so the caller knows the file is not usable as a main config.
	_, _, err := InjectInclude([]byte("events {}\n"), true)
	if err == nil {
		t.Fatalf("expected error when http block missing")
	}
}

func TestInjectGeneratedIncludeIsolatesOldFile(t *testing.T) {
	// The publish path must validate ONLY the generated file: the covering
	// glob (which at runtime would also pick up the old published file)
	// is redirected at the explicit tmp path.
	main := []byte("http {\n    include /etc/nginx/conf.d/k8s-gw/*.conf;\n}\n")
	out, err := injectGeneratedInclude(main, "/etc/nginx/conf.d/k8s-gw/00-global.conf.tmp")
	if err != nil {
		t.Fatalf("injectGeneratedInclude: %v", err)
	}
	if strings.Contains(string(out), "k8s-gw/*.conf") {
		t.Fatalf("covering glob must be replaced, got:\n%s", out)
	}
	if !strings.Contains(string(out), "include /etc/nginx/conf.d/k8s-gw/00-global.conf.tmp;") {
		t.Fatalf("expected explicit include of the generated file, got:\n%s", out)
	}
}

func TestInjectGeneratedIncludeKeepsUnrelatedTokens(t *testing.T) {
	main := []byte("http {\n    include mime.types /etc/nginx/conf.d/k8s-gw/*.conf;\n}\n")
	out, err := injectGeneratedInclude(main, "/dir/x.tmp")
	if err != nil {
		t.Fatalf("injectGeneratedInclude: %v", err)
	}
	if !strings.Contains(string(out), "include mime.types /dir/x.tmp;") {
		t.Fatalf("unrelated tokens must survive the rewrite, got:\n%s", out)
	}
}

func TestInjectGeneratedIncludeNoCoveringInclude(t *testing.T) {
	// Without any covering include, one pointing at the generated file is
	// injected into the http block.
	main := []byte("http {\n    include mime.types;\n}\n")
	out, err := injectGeneratedInclude(main, "/dir/x.tmp")
	if err != nil {
		t.Fatalf("injectGeneratedInclude: %v", err)
	}
	if !strings.Contains(string(out), "include /dir/x.tmp;") {
		t.Fatalf("expected injected include, got:\n%s", out)
	}
}

func TestValidator_ValidatePasses(t *testing.T) {
	tmp := t.TempDir()
	bin := writeFakeNginx(t, tmp)
	logPath := filepath.Join(tmp, "nginx.log")
	mainPath := filepath.Join(tmp, "nginx.conf")
	// Plain http with no include; the validator will inject.
	if err := os.WriteFile(mainPath, []byte("http {}\n"), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	t.Setenv("FAKE_NGINX_LOG", logPath)
	t.Setenv("FAKE_NGINX_FAIL", "")

	v := &Validator{
		NginxBinary:    bin,
		MainConfigPath: mainPath,
		Prefix:         tmp,
		Commander:      directExecCommander,
	}
	res, err := v.Validate(context.Background(), mainPath)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK=true; stderr=%q", res.Stderr)
	}
	if res.Injected != true {
		t.Fatalf("expected Injected=true (no include in main config)")
	}
	if res.AlreadyIncluded {
		t.Fatalf("expected AlreadyIncluded=false")
	}
	logBytes, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logBytes), "-t") {
		t.Fatalf("expected fake nginx to be called with -t, log=%q", logBytes)
	}
}

func TestValidator_ValidateFailsReturnsStderr(t *testing.T) {
	tmp := t.TempDir()
	bin := writeFakeNginx(t, tmp)
	logPath := filepath.Join(tmp, "nginx.log")
	mainPath := filepath.Join(tmp, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte("http {}\n"), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	t.Setenv("FAKE_NGINX_LOG", logPath)
	t.Setenv("FAKE_NGINX_FAIL", "syntax error on line 3")

	v := &Validator{
		NginxBinary:    bin,
		MainConfigPath: mainPath,
		Prefix:         tmp,
		Commander:      directExecCommander,
	}
	res, err := v.Validate(context.Background(), mainPath)
	if err == nil {
		t.Fatalf("expected error on validation failure")
	}
	if res.OK {
		t.Fatalf("expected OK=false on validation failure")
	}
	if !strings.Contains(res.Stderr, "syntax error") {
		t.Fatalf("expected stderr captured; got %q", res.Stderr)
	}
}

func TestValidator_ValidateAlreadyIncluded(t *testing.T) {
	tmp := t.TempDir()
	bin := writeFakeNginx(t, tmp)
	logPath := filepath.Join(tmp, "nginx.log")
	mainPath := filepath.Join(tmp, "nginx.conf")
	// The user's main config already covers our managed dir via an explicit
	// k8s-gw glob; the validator must NOT inject a duplicate include.
	main := []byte("http {\n    include /etc/nginx/conf.d/k8s-gw/*.conf;\n}\n")
	if err := os.WriteFile(mainPath, main, 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	t.Setenv("FAKE_NGINX_LOG", logPath)

	v := &Validator{
		NginxBinary:    bin,
		MainConfigPath: mainPath,
		Prefix:         tmp,
		Commander:      directExecCommander,
	}
	res, err := v.Validate(context.Background(), mainPath)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK=true; stderr=%q", res.Stderr)
	}
	if !res.AlreadyIncluded {
		t.Fatalf("expected AlreadyIncluded=true")
	}
	if res.Injected {
		t.Fatalf("expected Injected=false (no double include)")
	}
}
