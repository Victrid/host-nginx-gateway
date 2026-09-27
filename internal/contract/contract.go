// Package contract defines the locked Intermediate Representation (IR)
// between the K8s provider lane (graph → IR) and the dataplane lane
// (IR → nginx config). Per DESIGN.md §5.4 the shape is frozen — both
// lanes must extend through the same coordination step.
package contract

// Configuration is the full IR produced by one reconcile pass. It is the
// single object the dataplane renderer consumes to emit nginx config.
//
// Upstreams are listed alongside Servers so the template can render them in
// a stable order (upstreams first, then servers). nginx does not care
// about ordering, but determinism matters for the applied-hash no-op
// described in DESIGN.md §1 / S6.
type Configuration struct {
	// ErrorLog, when set, is emitted as an http-context `error_log`
	// directive at the top of the generated file. The controller points it
	// at a file inside its OWNED directory so the reload-effect
	// verification (§5.2) reads bind failures from a log it controls —
	// the host nginx may otherwise log to stderr/journal only. Empty omits
	// the directive.
	ErrorLog string `json:"errorLog,omitempty"`

	// Maps are the nginx `map` blocks (http context) backing per-location
	// upstream dispatch (method/header/query matching, §3.3). They are
	// emitted before upstreams so the rendered file reads top-down. nginx
	// does not care about ordering, but determinism matters for the
	// applied-hash no-op (DESIGN.md §1 / S6).
	Maps []*MapBlock `json:"maps,omitempty"`

	// SplitClients are the nginx `split_clients` blocks (http context)
	// implementing percentage-gated request mirroring (RequestMirror filter
	// with percent/fraction < 100, §3.3): the business location always fires
	// the `mirror` subrequest and the gate inside the internal mirror
	// location consults the split result to answer 204 instead of proxying.
	// Emitted after Maps, before Upstreams; sorted by Name for determinism.
	SplitClients []*SplitClients `json:"splitClients,omitempty"`

	// Servers are the per-Gateway server blocks to emit, one per (Gateway,
	// hostname) tuple. Servers must be sorted deterministically by the
	// producer so the rendered output is stable.
	Servers []*Server `json:"servers,omitempty"`

	// Upstreams are the upstream blocks referenced by Locations. They are
	// emitted once per upstream regardless of how many Servers reference
	// them.
	Upstreams []*Upstream `json:"upstreams,omitempty"`
}

// String returns a stable, human-readable summary of the configuration
// suitable for logs and event messages.
func (c *Configuration) String() string {
	if c == nil {
		return "Configuration(nil)"
	}
	return fmtConfiguration(c)
}

// Server is one nginx `server { ... }` block, keyed by Hostname.
type Server struct {
	// Hostname is the server_name value. An empty Hostname means "catch-all"
	// (listen-only) and produces `server_name "";` (or `_`, per the renderer).
	Hostname string `json:"hostname,omitempty"`

	// Listens is the set of listen directives for this server block. They
	// are emitted in order; the producer must deduplicate per (port, address).
	Listens []Listen `json:"listens,omitempty"`

	// TLSCert is the path (relative to nginxConfDir or absolute) of the PEM
	// file to reference via `ssl_certificate`. Empty means no TLS.
	TLSCert string `json:"tlsCert,omitempty"`

	// Locations are the location blocks for this server, sorted by Path by
	// the producer.
	Locations []*Location `json:"locations,omitempty"`
}

func (s *Server) String() string {
	if s == nil {
		return "Server(nil)"
	}
	return fmtServer(s)
}

// Listen is one `listen` directive plus its ssl/http2 modifiers. Multiple
// Listen entries with the same Port are permitted to express "listen on
// multiple addresses" (DESIGN.md §3.1 annotation behaviour).
type Listen struct {
	// Port is the TCP port number. Required (DESIGN.md §3.1, S1).
	Port int `json:"port"`

	// Address is the optional bind address ("", "0.0.0.0", "192.168.1.10",
	// "[::]"). Empty means "listen on all addresses" via the bare
	// `listen <port>` form.
	Address string `json:"address,omitempty"`

	// SSL enables `ssl` on the listen directive (TLS / HTTPS).
	SSL bool `json:"ssl,omitempty"`

	// HTTP2 enables `http2` on the listen directive.
	HTTP2 bool `json:"http2,omitempty"`
}

func (l Listen) String() string {
	return fmtListen(l)
}

// Location is one nginx `location` block. Path matching semantics are
// chosen by the producer (literal vs prefix vs regex) and embedded in Path
// itself (e.g. "^~/api" for regex). The renderer does not interpret Path.
type Location struct {
	// Path is the nginx location matcher, written exactly as it will appear
	// after `location` in the config (e.g. "/", "= /api", "~ \.png$").
	Path string `json:"path"`

	// Upstream is the name of an Upstream defined in Configuration.Upstreams.
	// A value starting with "$" selects the upstream at RUNTIME through the
	// named map variable (method/header/query dispatch, §3.3); nginx matches
	// the variable's value against the known upstream groups, so no resolver
	// is involved. Empty means no proxy_pass (static response / return).
	Upstream string `json:"upstream,omitempty"`

	// Rewrite is the body of an optional `rewrite` directive (pattern,
	// replacement and optional flag, e.g. `^/foo(/.*)$ /bar$1 break`).
	// Empty omits the directive. Used for URLRewrite (§3.3) and previously
	// for the bare-host redirect rule.
	Rewrite string `json:"rewrite,omitempty"`

	// Redirect, when set, turns the location into a static `return` redirect
	// (RequestRedirect filter, §3.3). Mutually exclusive with Upstream.
	Redirect *Redirect `json:"redirect,omitempty"`

	// RedirectIf, when set, emits a guarded `if ($uri ~ <Match>) { return … }`
	// inside the location. Used for prefix-preserving redirects in prefix
	// locations (the remainder after the matched prefix is captured).
	RedirectIf *RedirectIf `json:"redirectIf,omitempty"`

	// ProxyHost overrides the Host header sent to the backend
	// (URLRewrite.hostname, §3.3). Empty preserves the client Host
	// ($http_host).
	ProxyHost string `json:"proxyHost,omitempty"`

	// RequestHeaders are request header modifications applied before
	// proxying (RequestHeaderModifier, §3.3): set/add emit proxy_set_header,
	// remove emits proxy_set_header with an empty value (the nginx idiom
	// for suppressing a header towards the backend). On MIRROR locations
	// these carry the mirrored copy's modifiers (rule-level modifier per
	// the NGF mechanism); on per-backend dispatch they carry the
	// backendRef-level modifier (BackendRequestHeaderModification, §3.3).
	RequestHeaders []Header `json:"requestHeaders,omitempty"`

	// Mirrors are the RequestMirror targets of this location (§3.3): the
	// renderer emits one `mirror <Path>;` directive per entry. Each Path
	// points at an internal mirror location (Internal=true) rendered in the
	// same server block. Mirror directives fire before proxying; the
	// mirrored response is discarded by nginx. Mirror filters never change
	// the location's own request handling.
	Mirrors []Mirror `json:"mirrors,omitempty"`

	// Internal marks an internal-only location (`internal;` directive):
	// client requests matching it directly are answered 404 by nginx; only
	// mirror subrequests reach it. Used for request-mirror targets (§3.3).
	Internal bool `json:"internal,omitempty"`

	// MirrorGate, when set, names (without $) a split_clients result
	// variable consulted inside an internal mirror location: the renderer
	// emits `if ($<MirrorGate> = "") { return 204; }` before proxying, so a
	// percentage-gated mirror (percent/fraction < 100, §3.3) proxies only
	// the sampled share of subrequests. Empty means unconditional mirroring
	// (100% — no split_clients, no gate).
	MirrorGate string `json:"mirrorGate,omitempty"`

	// ProxyPassURI is appended verbatim to the proxy_pass target
	// (`proxy_pass http://<Upstream><ProxyPassURI>;`). Internal mirror
	// locations use "$request_uri": the subrequest URI ($uri) is the mirror
	// path, so the ORIGINAL client request URI (path + query) must be
	// restored explicitly (same reasoning as NGF internal locations).
	ProxyPassURI string `json:"proxyPassURI,omitempty"`

	// HideHeaders are response headers suppressed towards the client
	// (ResponseHeaderModifier remove, §3.3): proxy_hide_header.
	HideHeaders []string `json:"hideHeaders,omitempty"`

	// ResponseHeaders are response headers added towards the client
	// (ResponseHeaderModifier add/set, §3.3): add_header … always. For set,
	// the producer also lists the header in HideHeaders so the backend's
	// original value is suppressed first.
	ResponseHeaders []Header `json:"responseHeaders,omitempty"`

	// Timeouts optionally overrides the proxy_*_timeout values for this
	// location. Nil means "render with nginx defaults"; non-nil with empty
	// fields renders nothing for that field.
	Timeouts *Timeouts `json:"timeouts,omitempty"`
}

func (loc *Location) String() string {
	if loc == nil {
		return "Location(nil)"
	}
	return fmtLocation(loc)
}

// Redirect is a static `return <Code> <URL>;` directive (RequestRedirect).
// The URL may contain nginx variables ($host, $scheme, $is_args, …) — the
// renderer emits it verbatim.
type Redirect struct {
	// Code is the HTTP redirect status (301/302/303/307/308).
	Code int `json:"code"`
	// URL is the redirect target (absolute, possibly variable-bearing).
	URL string `json:"url"`
}

// RedirectIf is a guarded redirect emitted inside a location:
//
//	if ($uri ~ <Match>) {
//	    return <Code> <URL>;
//	}
//
// Match is an nginx location-style regex (without the leading ~). Used when
// a prefix path must be rewritten while preserving the unmatched remainder
// (RequestRedirect + replacePrefixMatch).
type RedirectIf struct {
	// Match is the regex applied to $uri (POSIX ERE, case-sensitive).
	Match string `json:"match"`
	// Code is the redirect status.
	Code int `json:"code"`
	// URL is the redirect target; may reference the regex capture (?<hng_r>…).
	URL string `json:"url"`
}

// Header is one name/value pair for request/response header modification.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MapBlock is one nginx `map` block used for per-location upstream
// selection (§3.3): map <Source> $<Name> { <entries> }.
type MapBlock struct {
	// Name is the result variable name (without $).
	Name string `json:"name"`
	// Source is the source variable (e.g. $request_method, $http_x_name).
	Source string `json:"source"`
	// Entries are the match/value pairs. A Key starting with "~" is an
	// nginx map regex (case-sensitive, evaluated after exact keys in
	// declaration order). Values are upstream names, "$child" variables or
	// "" (no matching rule → the location answers 404).
	Entries []MapEntry `json:"entries,omitempty"`
}

// MapEntry is one key/value line of a MapBlock.
type MapEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Mirror is one RequestMirror target attached to a business location
// (§3.3 RequestMirror filter). Path is the bare internal mirror location
// path (e.g. "/hng_mirror_1a2b3c4d"); the same path is rendered as an
// `location = <Path>` block (Internal=true) in every server that carries a
// location mirroring to it.
type Mirror struct {
	// Path is the internal mirror location path (bare, without "=").
	Path string `json:"path"`
}

// SplitClients is one nginx `split_clients` block (http context):
//
//	split_clients <Source> $<Name> { <Percent>% <Value>; * <Value>; }
//
// Used for percentage-gated request mirroring (§3.3): the result variable
// evaluates to the mirror location path for the sampled share of requests
// and "" otherwise; the internal mirror location's gate turns "" into a
// 204 no-op.
type SplitClients struct {
	// Name is the result variable name (without $).
	Name string `json:"name"`

	// Source is the source variable. Fixed to "$request_id" (a fresh random
	// value per request — the same keying NGF uses) so the sampling is
	// per-request and unbiased.
	Source string `json:"source"`

	// Entries are the distributions. Exactly one entry carries Percent="*"
	// (the catch-all). Percent values are formatted with two decimals
	// ("12.50") as required by nginx's 0.01% granularity.
	Entries []SplitEntry `json:"entries,omitempty"`
}

// SplitEntry is one distribution line of a SplitClients block.
type SplitEntry struct {
	// Percent is the distribution share: a two-decimal number ("12.50") or
	// "*" for the catch-all remainder.
	Percent string `json:"percent"`

	// Value is the result for this bucket: the mirror location path or ""
	// (skip mirroring).
	Value string `json:"value"`
}

// Timeouts captures the three proxy_*_timeout directives the controller
// exposes. Values are nginx duration strings ("30s", "1m", …); empty
// fields are omitted from the rendered config; nil Timeouts means "all
// nginx defaults". The provider lane is responsible for converting
// HTTPRoute timeouts into these strings.
type Timeouts struct {
	// Connect is the proxy_connect_timeout value.
	Connect string `json:"connect,omitempty"`
	// Send is the proxy_send_timeout value.
	Send string `json:"send,omitempty"`
	// Read is the proxy_read_timeout value.
	Read string `json:"read,omitempty"`
}

// Upstream is one nginx `upstream` block. Endpoints are the resolved
// EndpointSlice addresses; their Ready flag determines whether they are
// emitted as `server` (up) or `server down` lines.
type Upstream struct {
	// Name is the upstream name and must match the Upstream field on
	// referencing Locations. Deterministic naming: <ns>_<svc>_<port>
	// (DESIGN.md §4).
	Name string `json:"name"`

	// Endpoints is the set of resolved backend endpoints.
	Endpoints []Endpoint `json:"endpoints,omitempty"`
}

func (u *Upstream) String() string {
	if u == nil {
		return "Upstream(nil)"
	}
	return fmtUpstream(u)
}

// Endpoint is one resolved backend address.
type Endpoint struct {
	// IP is the IPv4 or IPv6 literal as carried by EndpointSlice. The
	// renderer does not perform DNS resolution.
	IP string `json:"ip"`

	// Port is the TCP port the backend listens on.
	Port int `json:"port"`

	// Ready mirrors EndpointSlice Conditions.Ready. When false the
	// renderer emits `server <ip>:<port> down;` to preserve health
	// awareness without dropping the entry (DESIGN.md §4).
	Ready bool `json:"ready,omitempty"`

	// Weight is the backendRef weight carried onto the upstream server line
	// (`weight=N`, backendRef weights §3.3). Zero/1 renders no weight
	// directive (nginx default).
	Weight int `json:"weight,omitempty"`
}

func (e Endpoint) String() string {
	return fmtEndpoint(e)
}
