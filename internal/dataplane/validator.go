// Validator: DESIGN.md §5.1 - temporary-main-config validation.
// nginx -t -c accepts a single config file, not a directory; testing only the
// generated <gw>.conf in isolation would not exercise include resolution.
// We therefore:
//  1. copy the user's nginx.conf to a temp file;
//  2. detect whether the (already-working) config already includes the
//     managed dir -- either explicitly (e.g. "/etc/nginx/conf.d/k8s-gw/*.conf")
//     or via a wildcard ("/etc/nginx/conf.d/*.conf");
//  3. if not, inject "include /etc/nginx/conf.d/k8s-gw/*.conf;" into the
//     http block of the temp copy ONLY;
//  4. run `<binary> -t -c <tmp> -p <prefix>`.
//
// The user's /etc/nginx/nginx.conf is NEVER touched (DESIGN.md §2).
package dataplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ValidatorResult reports what happened during a validation run. The caller
// (publisher) inspects AlreadyIncluded to decide whether the user's main
// config already pulls in our managed dir (DESIGN.md §2 / S2).
type ValidatorResult struct {
	// AlreadyIncluded is true when the user's nginx.conf already references
	// the managed directory (via an explicit k8s-gw include, the bare
	// conf.d/*.conf wildcard, or any equivalent path matching). When true,
	// the validator does NOT inject any include into the temp copy.
	AlreadyIncluded bool
	// Injected is true when we added an include line into the temp copy.
	Injected bool
	// Stderr is nginx's stderr (trimmed). Empty on success.
	Stderr string
	// OK is true when nginx -t exited 0.
	OK bool
}

// ErrIncludeMissing indicates the validator detected that no include of the
// managed dir is present AND `--require-include=true` was set; the caller
// is expected to map this to Accepted=False (Invalid) per DESIGN.md §2.
var ErrIncludeMissing = errors.New("dataplane: managed dir not included in main config")

// includeTarget is the directory the gateway writes into; include directives
// must resolve (after path normalisation) to a pattern inside this dir.
// It is a var so tests can override it, but production code never mutates it.
var includeTarget = "/etc/nginx/conf.d/k8s-gw"

// includePatternGlob is the pattern we inject when the user's config does
// not already cover our directory. nginx evaluates globs at startup.
const includePatternGlob = "/etc/nginx/conf.d/k8s-gw/*.conf"

// includeDirectiveRe captures "include <args>;" tokens. nginx allows several
// shapes: `include file;`, `include /abs/*.conf;`, `include /a /b;` (multiple
// patterns on one directive, no comma). For our detection we tokenise each
// include directive and inspect every glob.
var includeDirectiveRe = regexp.MustCompile(`(?m)^\s*include\s+([^;]+);`)

// httpBlockRe finds the top-level http { ... } block in an nginx config.
// We do a single balanced scan: find `http {` and match braces until depth 0.
var httpBlockRe = regexp.MustCompile(`(?ms)^(\s*)http\s*\{`)

// injectGeneratedInclude rewrites a COPY of the user's main config so that
// every include pattern covering the managed dir points at the specific
// generated file being validated instead. This validates exactly the
// would-be directory state:
//
//   - the OLD published file (about to be replaced) is NOT included — it
//     must not be able to fail the validation of its own replacement (an
//     E2E finding: a stale 00-global.conf referencing a since-deleted
//     certificate pem deadlocked Programmed=False forever);
//   - hand-written user files under the managed dir are not validated
//     either (DESIGN.md §2: non-owned files are only logged; their
//     validity is the operator's business).
//
// When no covering include exists the plain injection path is used.
func injectGeneratedInclude(mainConf []byte, generatedPath string) ([]byte, error) {
	target := filepath.Clean(includeTarget)
	out := includeDirectiveRe.ReplaceAllFunc(mainConf, func(match []byte) []byte {
		m := includeDirectiveRe.FindSubmatch(match)
		arg := strings.TrimSpace(string(m[1]))
		var kept []string
		covered := false
		for _, tok := range strings.Fields(arg) {
			if includePatternCovers(tok, target) {
				covered = true
				continue // swap this pattern for the generated file
			}
			kept = append(kept, tok)
		}
		if !covered {
			return match
		}
		newArg := generatedPath
		if len(kept) > 0 {
			newArg = strings.Join(kept, " ") + " " + generatedPath
		}
		return []byte("include " + newArg + ";")
	})
	if !includeDirectiveIncludes(out, generatedPath) {
		// No covering include existed (or none survived): inject one
		// pointing directly at the generated file.
		return injectIncludeIntoHTTP(out, generatedPath)
	}
	return out, nil
}

// includeDirectiveIncludes reports whether any include directive in conf
// references path exactly.
func includeDirectiveIncludes(conf []byte, path string) bool {
	for _, m := range includeDirectiveRe.FindAllSubmatch(conf, -1) {
		for _, tok := range strings.Fields(string(m[1])) {
			if tok == path {
				return true
			}
		}
	}
	return false
}

// InjectInclude produces a copy of the user's nginx.conf (read from mainConf)
// with `include /etc/nginx/conf.d/k8s-gw/*.conf;` injected into the http block.
// It returns the in-memory copy; callers are responsible for writing it to a
// temp file before invoking nginx -t.
//
// If the user's config already references the managed dir (via explicit path
// or a conf.d wildcard), this function returns the input unchanged and
// alreadyIncluded=true.
//
// injectIfMissing=false short-circuits the injection; it is used when the
// caller wants to confirm the validator's detection alone (tests).
func InjectInclude(mainConf []byte, injectIfMissing bool) (out []byte, alreadyIncluded bool, err error) {
	if len(bytes.TrimSpace(mainConf)) == 0 {
		return nil, false, fmt.Errorf("dataplane: empty main config")
	}
	alreadyIncluded = configIncludesTarget(mainConf)
	if alreadyIncluded || !injectIfMissing {
		return append([]byte(nil), mainConf...), alreadyIncluded, nil
	}
	injected, err := injectIncludeIntoHTTP(mainConf, includePatternGlob)
	if err != nil {
		return nil, false, err
	}
	return injected, false, nil
}

// nginxPrefix is the canonical install prefix nginx uses when resolving
// relative include paths (e.g. "conf.d/k8s-gw/*.conf" becomes
// "/etc/nginx/conf.d/k8s-gw/*.conf"). We use it to expand relative include
// patterns during detection; the validator runs with `-p /etc/nginx` so
// this matches production behaviour.
const nginxPrefix = "/etc/nginx"

// configIncludesTarget returns true if mainConf has any include directive
// that, after path normalisation, would resolve into includeTarget.
//
// Accepted patterns (nginx include globs do NOT recurse into
// subdirectories — verified against nginx 1.30 during E2E, where even `-p`
// is ignored for relative include resolution when `-c` is used):
//
//   - "/etc/nginx/conf.d/k8s-gw/*.conf"    (explicit glob inside the dir)
//   - "/etc/nginx/conf.d/k8s-gw/00-*.conf" (any glob inside the dir)
//   - "conf.d/k8s-gw/*.conf"               (relative variant, evaluated
//     against nginxPrefix)
//
// NOT accepted (they leave the managed dir ineffective at runtime):
//
//   - "/etc/nginx/conf.d/*.conf" — matches only files directly in conf.d,
//     never conf.d/k8s-gw/*; treating it as coverage would skip injection,
//     skip validation of the generated config, and let RequireInclude
//     falsely pass while nothing is actually served (DESIGN.md §2 / S2).
//   - "/etc/nginx/conf.d/k8s-gw.conf" — a sibling FILE does not include
//     the directory's contents.
func configIncludesTarget(mainConf []byte) bool {
	target := filepath.Clean(includeTarget)
	matches := includeDirectiveRe.FindAllSubmatch(mainConf, -1)
	for _, m := range matches {
		arg := strings.TrimSpace(string(m[1]))
		// An include directive may list several patterns; split on whitespace.
		for _, pat := range strings.Fields(arg) {
			if includePatternCovers(pat, target) {
				return true
			}
		}
	}
	return false
}

// absolutise resolves an include pattern against nginxPrefix if it is
// relative. nginx evaluates includes relative to its prefix directory
// (typically /etc/nginx), so a pattern like "conf.d/k8s-gw/*.conf" should
// be treated as "/etc/nginx/conf.d/k8s-gw/*.conf" for matching purposes.
func absolutise(pat string) string {
	if filepath.IsAbs(pat) {
		return filepath.Clean(pat)
	}
	return filepath.Clean(nginxPrefix + "/" + pat)
}

// includePatternCovers returns true if `pat` resolves to the managed dir
// itself or to a glob inside it — the only forms whose expansion can match
// files under includeTarget, given nginx's non-recursive globs.
func includePatternCovers(pat, target string) bool {
	if pat == "" {
		return false
	}
	cleaned := absolutise(pat)
	return cleaned == target || strings.HasPrefix(cleaned, target+"/")
}

// injectIncludeIntoHTTP inserts the include directive immediately after the
// opening brace of the top-level http block. If no http block is found the
// function returns an error: a config without an http block cannot be
// validated against our server/upstream snippets anyway.
func injectIncludeIntoHTTP(mainConf []byte, includeLine string) ([]byte, error) {
	loc := httpBlockRe.FindIndex(mainConf)
	if loc == nil {
		return nil, fmt.Errorf("dataplane: no http block found in main config")
	}
	// loc[1] is the byte right after the opening "{". We insert at loc[1].
	insertAt := loc[1]
	prefix := mainConf[:insertAt]
	suffix := mainConf[insertAt:]

	// Trim leading whitespace/newlines from suffix so the include line lands
	// at column 0; nginx is whitespace-tolerant but keep the file readable.
	suffixTrimmed := bytes.TrimLeft(suffix, " \t")
	leadingWS := suffix[:len(suffix)-len(suffixTrimmed)]

	var buf bytes.Buffer
	buf.Write(prefix)
	buf.WriteByte('\n')
	buf.WriteString("    include " + includeLine + ";\n")
	buf.Write(leadingWS)
	buf.Write(suffixTrimmed)
	return buf.Bytes(), nil
}

// Validator wraps the inputs needed to call nginx -t. It is constructed by
// the publisher and is also usable directly from tests.
type Validator struct {
	// NginxBinary is the path to the nginx executable (DESIGN.md §5.4 / N5).
	NginxBinary string
	// MainConfigPath is the path to the user's /etc/nginx/nginx.conf.
	MainConfigPath string
	// Prefix is the nginx prefix directory passed via -p.
	Prefix string
	// RequireInclude, when true, makes Validate return ErrIncludeMissing if
	// the main config already includes the managed dir. Production code
	// leaves this false; it exists for diagnostic use (tests use it to
	// negative-test the detector).
	RequireInclude bool
	// Commander builds the exec.Cmd for `nginx -t`. Nil means
	// NsenterCommander(1) (the DaemonSet host-namespace default); tests
	// inject fakes so the HOST's nginx validates the config.
	Commander Commander
}

// commander returns the configured Commander or the nsenter default.
func (v *Validator) commander() Commander {
	if v.Commander != nil {
		return v.Commander
	}
	return NsenterCommander(1)
}

// Validate implements DESIGN.md §5.1's temporary-main-config method.
// It returns a ValidatorResult and an error: the error is non-nil only
// for unexpected infrastructure failures (missing binary, copy failure);
// a config failure manifests as OK=false plus Stderr populated.
func (v *Validator) Validate(ctx context.Context, generatedConfPath string) (ValidatorResult, error) {
	res := ValidatorResult{}
	if v.NginxBinary == "" {
		return res, fmt.Errorf("dataplane: validator: nginx binary not set")
	}
	if v.MainConfigPath == "" {
		return res, fmt.Errorf("dataplane: validator: main config path not set")
	}

	// 1. Read the user's main config.
	main, err := os.ReadFile(v.MainConfigPath)
	if err != nil {
		return res, fmt.Errorf("dataplane: read main config: %w", err)
	}

	// 2. Detect / inject. On the publish path (generatedConfPath != "") the
	// covering includes are redirected at the generated tmp file so the
	// previous published file cannot poison the validation of its own
	// replacement.
	alreadyIncluded := configIncludesTarget(main)
	if alreadyIncluded {
		res.AlreadyIncluded = true
		// Skip injection entirely; pass the user's file through unchanged.
	}
	var injectedMain []byte
	if generatedConfPath != "" {
		injectedMain, err = injectGeneratedInclude(main, generatedConfPath)
		if err != nil {
			return res, err
		}
		res.Injected = !alreadyIncluded
	} else {
		var aiFromInject bool
		injectedMain, aiFromInject, err = InjectInclude(main, !alreadyIncluded)
		if err != nil {
			return res, err
		}
		// InjectInclude always returns the right alreadyIncluded value, but
		// we trust our prior detect because it is single-source-of-truth:
		// the validator's AlreadyIncluded flag and Injected flag must agree
		// with the user's config, not with a re-evaluation.
		_ = aiFromInject
		if !alreadyIncluded {
			res.Injected = true
		}
	}

	// 3. Write the temp copy NEXT TO the user's main config, not into /tmp:
	// nginx (verified on 1.30) resolves relative include paths in a `-c`
	// config against that config file's own directory and ignores `-p` for
	// this resolution, so a temp copy in /tmp breaks includes like
	// `include mime.types;`. A fallback to the system temp dir keeps the
	// validator usable (with degraded semantics) when the nginx dir is not
	// writable.
	tmpDir := filepath.Dir(v.MainConfigPath)
	tmp, err := os.CreateTemp(tmpDir, "hng-nginx-test-*.conf")
	if err != nil {
		tmp, err = os.CreateTemp("", "hng-nginx-test-*.conf")
		if err != nil {
			return res, fmt.Errorf("dataplane: create temp main config: %w", err)
		}
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := io.Copy(tmp, bytes.NewReader(injectedMain)); err != nil {
		_ = tmp.Close()
		return res, fmt.Errorf("dataplane: write temp main config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return res, fmt.Errorf("dataplane: close temp main config: %w", err)
	}

	// 4. nginx -t -c <tmp> -p <prefix>.
	args := []string{"-t"}
	if v.Prefix != "" {
		args = append(args, "-p", v.Prefix)
	}
	args = append(args, "-c", tmpPath)

	cmd := v.commander()(ctx, v.NginxBinary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	runErr := cmd.Run()
	res.Stderr = strings.TrimSpace(stderr.String())
	if runErr == nil {
		res.OK = true
		if v.RequireInclude && !res.AlreadyIncluded {
			return res, ErrIncludeMissing
		}
		return res, nil
	}
	// nginx -t exit non-zero: surface as OK=false plus an error wrapping the
	// stderr so callers can put it in Programmed=False message (DESIGN.md §3.4).
	if res.Stderr != "" {
		return res, fmt.Errorf("dataplane: nginx -t: %w: %s", runErr, res.Stderr)
	}
	return res, fmt.Errorf("dataplane: nginx -t: %w", runErr)
}
