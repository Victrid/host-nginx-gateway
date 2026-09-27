// Package dataplane renders nginx configuration from a locked IR contract
// (internal/contract) and applies it to a co-existing on-host nginx,
// preserving user-owned /etc/nginx/nginx.conf and only writing files under
// /etc/nginx/conf.d/k8s-gw/.
package dataplane

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// nginx.tmpl is the canonical, go:embed-baked template (DESIGN.md §5.4).
// External override is deliberately not implemented (N3 / YAGNI).
//
//go:embed nginx.tmpl
var nginxTemplate string

// funcs is the funcMap advertised by DESIGN.md §5.4. Each helper has a single
// responsibility and accepts a narrow value type so the template can stay small.
var funcs = template.FuncMap{
	"buildServerName": buildServerName,
	"buildListen":     buildListen,
	"buildLocation":   buildLocation,
	"buildProxyPass":  buildProxyPass,
	"buildTLS":        buildTLS,
	"mapKey":          mapKey,
	"mapValue":        mapValue,
	"splitKey":        splitKey,
}

// Render produces the rendered server configuration for a single Gateway
// from the locked IR contract (DESIGN.md §5.4 / S8). It is deterministic:
// same Configuration always yields the same bytes so the applied-hash
// no-op (DESIGN.md §1 / S6) is meaningful.
func Render(cfg *contract.Configuration) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("dataplane: nil configuration")
	}
	tpl, err := template.New("nginx").Funcs(funcs).Parse(nginxTemplate)
	if err != nil {
		return nil, fmt.Errorf("dataplane: parse template: %w", err)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, cfg); err != nil {
		return nil, fmt.Errorf("dataplane: execute template: %w", err)
	}
	return []byte(sb.String()), nil
}

// Hash returns a deterministic SHA-256 hex digest of the supplied bytes.
// Used by the publisher's no-op check (DESIGN.md §1 / S6).
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------- template helpers ----------
//
// Each helper returns a string of fully-indented nginx directive lines
// (terminated by ';' but no trailing newline, so the template can place
// them at the correct indentation level via the surrounding range bodies).

// buildServerName renders the value for the nginx `server_name` directive.
// Empty input returns the catch-all "_" so nginx accepts the server block.
func buildServerName(hostname string) string {
	if hostname == "" {
		return "_"
	}
	return hostname
}

// buildListen renders one full `listen ...;` directive (DESIGN.md §3.1):
//   - Address empty: "listen <port>".
//   - Address set:   "listen <address>:<port>".
//   - SSL:           appends " ssl".
//   - HTTP2:         appends " http2" (only meaningful with SSL; the upstream
//     pipeline is responsible for not setting http2 on plain HTTP).
func buildListen(l contract.Listen) string {
	var b strings.Builder
	b.WriteString("listen ")
	if l.Address != "" {
		b.WriteString(l.Address)
		b.WriteByte(':')
	}
	b.WriteString(strconv.Itoa(l.Port))
	if l.SSL {
		b.WriteString(" ssl")
	}
	if l.HTTP2 {
		b.WriteString(" http2")
	}
	b.WriteByte(';')
	return b.String()
}

// buildLocation renders the inner directives of a single `location` block,
// one per line. Each line is prefixed with eight spaces so it lines up with
// the `proxy_pass` line under the location's opening brace (the template
// emits "    location <Path> {" on a 4-space indented line).
//
// Directive order inside the block:
//  1. internal (internal-only locations: mirror targets, §3.3)
//  2. mirror-percentage gate (`if ($gate = "") { return 204; }` — the
//     split_clients result deciding whether the mirror subrequest proxies
//     or no-ops)
//  3. rewrite (URLRewrite)
//  4. guarded redirect (RequestRedirect + replacePrefixMatch in prefix
//     locations — the `if` must precede the static return below so the
//     prefix-carrying requests are answered with the rewritten remainder)
//  5. mirror directives (RequestMirror targets — emitted before proxying;
//     nginx fires the mirror subrequests and discards their responses)
//  6. static return (static-marker upstreams and RequestRedirect)
//  7. proxy_pass (static upstream or $variable for map dispatch)
//  8. proxy_set_header Host (preserved or rewritten), request header
//     modifications, proxy_hide_header, add_header … always, proxy timeouts.
//
// Static-marker upstreams (GEP-1364): when Upstream carries the
// staticUpstreamPrefix the provider has no matching entry in
// Configuration.Upstreams (an empty upstream block would fail `nginx -t`,
// see translate.go); the renderer must answer a static `return <code>;`
// instead of proxying. An empty Upstream is treated the same way — the
// locked contract (internal/contract) reserves "" for "static response /
// return directive".
func buildLocation(loc contract.Location) string {
	var lines []string
	if loc.Internal {
		lines = append(lines, "        internal;")
	}
	if loc.MirrorGate != "" {
		lines = append(lines,
			"        if ($"+loc.MirrorGate+" = \"\") {",
			"            return 204;",
			"        }")
	}
	if loc.Rewrite != "" {
		lines = append(lines, "        rewrite "+loc.Rewrite+";")
	}
	if loc.RedirectIf != nil {
		lines = append(lines,
			"        if ($uri ~ "+quoteNginx(loc.RedirectIf.Match)+") {",
			"            return "+strconv.Itoa(loc.RedirectIf.Code)+" "+loc.RedirectIf.URL+";",
			"        }")
	}
	for _, m := range loc.Mirrors {
		lines = append(lines, "        mirror "+m.Path+";")
	}
	if loc.Redirect != nil {
		lines = append(lines, "        return "+strconv.Itoa(loc.Redirect.Code)+" "+loc.Redirect.URL+";")
		return strings.Join(lines, "\n")
	}
	// A guarded redirect without a proxy target IS the whole location (a
	// RequestRedirect rule never proxies — CRD validation forbids combining
	// it with backendRefs).
	if loc.RedirectIf != nil && loc.Upstream == "" {
		return strings.Join(lines, "\n")
	}
	if code, ok := staticReturnCode(loc.Upstream); ok {
		lines = append(lines, "        return "+strconv.Itoa(code)+";")
		return strings.Join(lines, "\n")
	}
	// proxy_pass: either the upstream's named block or a "$variable" for
	// map-driven dispatch (the variable's value is matched against the
	// known upstream groups at runtime — no resolver needed). A dispatch
	// variable may legitimately resolve to "" (no rule matched this
	// combination of method/headers/query): the guard below answers 404
	// before proxy_pass would see an empty upstream name.
	if strings.HasPrefix(loc.Upstream, "$") {
		lines = append(lines,
			"        if ("+loc.Upstream+" = \"\") { return 404; }")
	}
	lines = append(lines, "        "+buildProxyPassURI(loc.Upstream, loc.ProxyPassURI))
	// WebSocket support: proxy over HTTP/1.1 and relay the hop-by-hop
	// Upgrade/Connection headers through the http-context
	// $connection_upgrade map (emitted once by the template). For plain
	// requests $http_upgrade is empty, so the Upgrade header is suppressed
	// and Connection collapses to "close" — nginx's default upstream
	// behavior, unchanged.
	lines = append(lines,
		"        proxy_http_version 1.1;",
		"        proxy_set_header Upgrade $http_upgrade;",
		"        proxy_set_header Connection $connection_upgrade;")
	// Preserve the client's Host header (Gateway API v1, HTTPRoute
	// spec.hostnames: "MUST forward this header unmodified to the
	// backend", absent applicable header-modification configuration).
	// Without this, nginx's proxy_pass sends the upstream group NAME as
	// the Host header, breaking Host-based route selection at the backend.
	// URLRewrite.hostname replaces it (ProxyHost).
	if loc.ProxyHost != "" {
		lines = append(lines, "        proxy_set_header Host "+quoteNginx(loc.ProxyHost)+";")
	} else {
		lines = append(lines, "        proxy_set_header Host $http_host;")
	}
	for _, h := range loc.RequestHeaders {
		lines = append(lines, "        proxy_set_header "+quoteNginx(h.Name)+" "+quoteNginx(h.Value)+";")
	}
	for _, n := range loc.HideHeaders {
		lines = append(lines, "        proxy_hide_header "+quoteNginx(n)+";")
	}
	for _, h := range loc.ResponseHeaders {
		lines = append(lines, "        add_header "+quoteNginx(h.Name)+" "+quoteNginx(h.Value)+" always;")
	}
	// Emit proxy timeouts only when set; nginx defaults are sensible but we
	// expose them explicitly so generated config is self-documenting. They
	// are meaningless for a static return, so the static branch above never
	// reaches this point.
	if loc.Timeouts != nil {
		t := loc.Timeouts
		if t.Connect != "" {
			lines = append(lines, "        proxy_connect_timeout "+t.Connect+";")
		}
		if t.Send != "" {
			lines = append(lines, "        proxy_send_timeout "+t.Send+";")
		}
		if t.Read != "" {
			lines = append(lines, "        proxy_read_timeout "+t.Read+";")
		}
	}
	return strings.Join(lines, "\n")
}

// quoteNginx renders a token as a double-quoted nginx string, escaping the
// characters nginx treats specially inside quotes (" and \). Used for map
// keys, header names/values and regex guards so arbitrary spec values
// cannot break out of the token.
func quoteNginx(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// mapKey renders one map entry key. Keys prefixed with "~" are nginx map
// regexes and are quoted verbatim (the tilde must stay outside the quotes
// for nginx to recognize the regex form); the bare keyword `default` is
// emitted unquoted (nginx reserves it as the fallback branch); plain keys
// are quoted strings (nginx matches plain map keys case-insensitively,
// which is exactly the Gateway API header-Exact semantics).
func mapKey(k string) string {
	switch {
	case k == "default":
		return "default"
	case strings.HasPrefix(k, "~"):
		return "~" + quoteNginx(strings.TrimPrefix(k, "~"))
	default:
		return quoteNginx(k)
	}
}

// mapValue renders one map entry value: an upstream name, a "$child"
// variable or "" (no matching rule). Quoting is always safe for nginx map
// values.
func mapValue(v string) string {
	return quoteNginx(v)
}

// buildProxyPass emits `proxy_pass http://<name>;` for a given upstream.
// The upstream name is treated as already-sanitized by the graph layer;
// we keep the prefix `http://` so TLS upstreams can be added later
// without a template change.
//
// Static-marker names (staticUpstreamPrefix) must never reach this helper —
// buildLocation routes them to a `return` directive — so no guard lives
// here; proxying a marker would reference an upstream block that
// deliberately does not exist and fail `nginx -t`.
func buildProxyPass(upstream string) string {
	return "proxy_pass http://" + upstream + ";"
}

// buildProxyPass emits `proxy_pass http://<upstream><uri>;`. uri is the
// contract Location.ProxyPassURI suffix (e.g. "$request_uri" for internal
// mirror locations, which must restore the original client request URI);
// empty for ordinary locations.
func buildProxyPassURI(upstream, uri string) string {
	return "proxy_pass http://" + upstream + uri + ";"
}

// splitKey renders one split_clients distribution key: the catch-all is the
// bare "*"; percentage buckets are two-decimal numbers ("12.50" → "12.50%").
func splitKey(p string) string {
	if p == "*" {
		return "*"
	}
	return p + "%"
}

// buildTLS renders the ssl_certificate / ssl_certificate_key pair for a
// single server block. An empty TLSCert (plain HTTP server) produces the
// empty string and the template omits the lines.
//
// Output lines are prefixed with four spaces so they line up with the
// `server_name` line emitted by the template body.
func buildTLS(certPath string) string {
	if certPath == "" {
		return ""
	}
	return "    ssl_certificate " + certPath + ";\n" +
		"    ssl_certificate_key " + certPath + ";"
}

// ---------------------------------------------------------------------------
// Static-marker upstreams (GEP-1364: unresolvable backends answer a fixed
// HTTP status code instead of proxying — DESIGN.md §3.4).
// ---------------------------------------------------------------------------

// staticUpstreamPrefix mirrors internal/provider's StaticUpstreamPrefix. It
// is redeclared locally instead of imported because internal/dataplane must
// stay free of Kubernetes dependencies (DESIGN.md §10 step 2: the dataplane
// is unit-testable against a fixed IR without a cluster). template_static_test.go
// locks the two constants together so drift fails the build's tests.
const staticUpstreamPrefix = "hng_static_"

// defaultStaticCode is the status code used when a static marker carries no
// usable numeric suffix ("hng_static_" or "hng_static_abc") and for empty
// Upstream values. The provider only ever emits hng_static_500 today.
const defaultStaticCode = 500

// IsStaticUpstream reports whether an upstream name is a static-response
// marker emitted by the provider for unresolvable backends. Marker names
// deliberately have NO entry in Configuration.Upstreams.
func IsStaticUpstream(name string) bool {
	return strings.HasPrefix(name, staticUpstreamPrefix)
}

// staticReturnCode extracts the HTTP status code encoded after the static
// prefix ("hng_static_503" → 503, true). An empty Upstream (the contract's
// "static response" placeholder) and markers with a missing or non-numeric
// suffix fall back to defaultStaticCode with ok=true, so the renderer always
// emits a valid `return <code>;`.
func staticReturnCode(upstream string) (code int, ok bool) {
	if upstream == "" {
		return defaultStaticCode, true
	}
	if !IsStaticUpstream(upstream) {
		return 0, false
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(upstream, staticUpstreamPrefix)); err == nil && n > 0 {
		return n, true
	}
	return defaultStaticCode, true
}
