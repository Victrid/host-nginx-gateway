// Location construction (§3.3): entry lists → contract.Location, including
// the map-driven dispatch tree for method/header/query matching, the
// RequestRedirect and URLRewrite translations and the header-modifier
// mappings. Everything here is pure and deterministic: map names derive
// from a deterministic allocation counter.
package provider

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// redirectCaptureName is the named regex capture used by guarded redirects
// and prefix rewrites to carry the unmatched path remainder.
const redirectCaptureName = "hng_r"

// dispatchBuilder allocates map blocks for one Configuration render. Names
// are deterministic: the monotonic counter's sequence follows the
// deterministic single-pass translation order.
type dispatchBuilder struct {
	cfg     *contract.Configuration
	counter int
}

// locationFor builds the location for one path spec from its viable,
// precedence-ordered cases.
//
// Semantics:
//   - the highest-precedence entry decides the location's kind: a
//     RequestRedirect entry turns the whole location into a redirect;
//   - a DIRECT case (path spec == the location matcher) that is
//     unconstrained matches every request in the location, so all later
//     cases are shadowed and truncated (first-rule-wins);
//   - static-marker entries (GEP-1364) are emitted as static returns when
//     they end up alone, and dropped from dispatch case lists otherwise
//     (a failed backend must not capture traffic other rules can serve);
//   - if more than one case remains (or the single case carries non-path
//     matchers), the location dispatches through a map tree whose leaves
//     are upstream names; "" means "no rule matched" and the renderer
//     answers 404.
func (d *dispatchBuilder) locationFor(path string, cases []*entry) *contract.Location {
	if len(cases) == 0 {
		return nil
	}
	head := cases[0]
	if head.rule.RequestRedirect != nil {
		return d.redirectLocation(path, head)
	}
	// A static-marker entry (GEP-1364: unresolvable/absent backends → 500)
	// in the HEAD position owns the location outright — its rule matches
	// these requests first, so they answer 500 (later viable cases are
	// shadowed). Markers in lower-precedence positions are dropped: a
	// failed backend must not capture traffic other rules can serve
	// (documented deviation). Redirect entries shadowed by a proxy case are
	// dropped likewise.
	var kept []*entry
	for i, e := range cases {
		if e.rule.RequestRedirect != nil {
			continue
		}
		if IsStaticUpstream(e.upstream) && i > 0 {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		return d.staticLocation(path, head)
	}
	if IsStaticUpstream(kept[0].upstream) {
		return d.staticLocation(path, kept[0])
	}
	// Shadow truncation (direct cases only): an unconstrained case whose
	// spec is the location matcher itself matches every request of the
	// location, so all later cases are shadowed (first-match-wins).
	for i, c := range kept {
		if c.path == path && len(c.constraints) == 0 {
			kept = kept[:i+1]
			break
		}
	}

	loc := &contract.Location{Path: path}
	if e := kept[0]; e.rule.URLRewrite != nil {
		applyURLRewrite(loc, e)
	}
	d.applyHeaderModifiers(loc, kept[0].rule)
	d.applyBackendRefHeaders(loc, kept)
	loc.Mirrors = d.mergeMirrors(kept)
	loc.Upstream = d.dispatchVar(path, kept)
	loc.Timeouts = kept[0].rule.Timeouts
	return loc
}

// staticLocation renders a single-entry (or fully shadowed) location.
func (d *dispatchBuilder) staticLocation(path string, e *entry) *contract.Location {
	loc := &contract.Location{Path: path, Upstream: e.upstream, Timeouts: e.rule.Timeouts}
	if e.rule.URLRewrite != nil {
		applyURLRewrite(loc, e)
	}
	d.applyHeaderModifiers(loc, e.rule)
	d.applyBackendRefHeaders(loc, []*entry{e})
	// Mirrors on a static-marker location are omitted: the static return
	// executes in the rewrite phase, before nginx's content-phase mirror
	// subrequests could fire — the directives would be dead config.
	if !IsStaticUpstream(e.upstream) {
		loc.Mirrors = d.mergeMirrors([]*entry{e})
	}
	return loc
}

// mergeMirrors collects the RequestMirror targets of a location's reachable
// cases. nginx `mirror` directives are location-wide (there is no per-case
// mirroring), so every target of every reachable case fires for any request
// the location serves; when the same mirror TARGET appears in several cases
// (e.g. the same backend mirrored by two rules sharing the location), the
// HIGHEST percentage's internal location wins (NGF behavior — one `mirror`
// directive per target per location). Deterministic: sorted by path; ties
// keep the first (precedence-ordered) case's path.
func (d *dispatchBuilder) mergeMirrors(cases []*entry) []contract.Mirror {
	if len(cases) == 0 {
		return nil
	}
	type picked struct {
		path string
		pct  float64
	}
	byTarget := map[string]picked{}
	for _, e := range cases {
		for _, m := range e.rule.RequestMirrors {
			cur, ok := byTarget[m.Upstream]
			if !ok || m.Percent > cur.pct {
				byTarget[m.Upstream] = picked{path: m.Path, pct: m.Percent}
			}
		}
	}
	if len(byTarget) == 0 {
		return nil
	}
	out := make([]contract.Mirror, 0, len(byTarget))
	for _, p := range byTarget {
		out = append(out, contract.Mirror{Path: p.path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// applyBackendRefHeaders puts the backendRef-level RequestHeaderModifier
// (§3.3 BackendRequestHeaderModification) on the location when — and only
// when — every request served by the location reaches the SAME rule with
// exactly one active backend. The spec scopes these filters to requests
// "being forwarded to the backend defined here"; a shared dispatch location
// (multiple rules or upstreams behind one map tree) cannot honor that
// per-request, so the modifier is only applied in the unambiguous case.
func (d *dispatchBuilder) applyBackendRefHeaders(loc *contract.Location, kept []*entry) {
	if len(kept) == 0 {
		return
	}
	rule := kept[0].rule
	for _, e := range kept {
		if e.rule != rule {
			return
		}
	}
	if len(rule.Backends) != 1 || rule.Backends[0].RequestHeaderModifier == nil {
		return
	}
	// The backendRef-level modifier is applied AFTER the rule-level one
	// (GEP-1310): same-name entries are replaced, new names appended.
	for _, h := range d.requestHeaderEntries(rule.Backends[0].RequestHeaderModifier) {
		replaced := false
		for i, rh := range loc.RequestHeaders {
			if strings.EqualFold(rh.Name, h.Name) {
				loc.RequestHeaders[i] = h
				replaced = true
				break
			}
		}
		if !replaced {
			loc.RequestHeaders = append(loc.RequestHeaders, h)
		}
	}
}

// specViableIn reports whether a path spec can match at least one request
// that nginx routes into the location matcher loc, AND is not claimed by a
// more specific location of its own inside the same block. Used to select
// the dispatch cases of a location (fall-through inside a hostname block):
//
//   - an exact location admits only its own spec and prefix specs covering
//     it (the location's single URI);
//   - a prefix location admits prefix specs that COVER it entirely; exact
//     and nested-prefix specs are claimed by their own more-specific
//     locations, so they never fall through here.
//
// All viable cases are therefore DIRECT: they apply to every request of
// the location and need no $uri guard.
func specViableIn(spec, loc string) bool {
	if spec == loc {
		return true
	}
	locPrefix, locIsPrefix := "", isPrefixLocation(loc)
	if locIsPrefix {
		locPrefix = pathPrefixOf(loc)
	}
	locExact, locIsExact := exactPathOf(loc)
	if locIsExact {
		if ep, ok := exactPathOf(spec); ok {
			return ep == locExact
		}
		pp, ok := prefixSpecOf(spec)
		return ok && prefixMatches(pp, locExact)
	}
	if locIsPrefix {
		if _, ok := exactPathOf(spec); ok {
			return false
		}
		pp, ok := prefixSpecOf(spec)
		return ok && prefixMatches(pp, locPrefix)
	}
	return false
}

// prefixSpecOf extracts the base path of a "/x/" prefix spec ("/" → "/").
func prefixSpecOf(spec string) (string, bool) {
	if !isPrefixLocation(spec) {
		return "", false
	}
	return pathPrefixOf(spec), true
}

// prefixMatches reports whether the prefix matcher q matches the literal
// URI u with Gateway API segment semantics ("/x" matches "/x" and
// "/x/…"; "/" matches everything).
func prefixMatches(q, u string) bool {
	if q == "/" {
		return true
	}
	return u == q || strings.HasPrefix(u, q+"/")
}

// dispatchVar returns the proxy_pass upstream expression for a case list:
// either a plain upstream name (single, unconstrained case) or "$variable"
// resolving through a generated map tree. The tree inputs are, in order,
// the method, headers and query constraints — each level's exact keys are
// checked case-insensitively by nginx (the Gateway API header-Exact
// semantics) and "~" regex keys in declaration order (RegularExpression,
// case-sensitive). The leaf value is the highest-precedence matching
// case's upstream; the "" value means no case matched (the renderer
// answers 404).
func (d *dispatchBuilder) dispatchVar(path string, cases []*entry) string {
	if len(cases) == 1 && len(cases[0].constraints) == 0 {
		return cases[0].upstream
	}
	return d.buildTree(cases, dispatchInputs(cases))
}

// dispatchInputs collects the deduplicated, deterministically ordered
// constraint inputs for a case list.
func dispatchInputs(cases []*entry) []MatchConstraint {
	seen := map[MatchConstraint]struct{}{}
	var out []MatchConstraint
	for _, e := range cases {
		for _, c := range e.constraints {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// buildTree emits (at most) one map for the first input and recurses.
// The returned string is the map VARIABLE ("$name") when a map was
// allocated, an upstream name at a leaf, or "" when no case matched.
func (d *dispatchBuilder) buildTree(cases []*entry, inputs []MatchConstraint) string {
	if len(cases) == 0 {
		return "" // no case: caller's "" → 404 guard
	}
	if len(inputs) == 0 || !anyConstrains(cases, inputs) {
		return cases[0].upstream
	}
	input := inputs[0]
	rest := inputs[1:]

	d.counter++
	// Short deterministic variable names: nginx's default
	// variables_hash_bucket_size (64 bytes) rejects long names, and short
	// names keep the variables hash small. Allocation order follows the
	// deterministic single-pass translation, so names are stable.
	name := fmt.Sprintf("hng_s%d", d.counter)
	mb := &contract.MapBlock{Name: name, Source: sourceOf(input)}

	// Branch cases PRESERVE the cases order (it encodes GEP-722 precedence):
	// a branch holds the cases constraining this input with the branch's
	// key PLUS the cases that do not constrain this input at all (they match
	// any value), both in their original relative order. Exact keys are
	// emitted first (nginx checks exact keys before regex keys), sorted for
	// determinism; then "~" regex keys in first-seen (case precedence)
	// order; then the default branch.
	keyOf := func(c *entry) (exact string, regex string, constrained bool) {
		ct, ok := constraintOf(c, input)
		if !ok {
			return "", "", false
		}
		if ct.Type == "Exact" {
			return ct.Value, "", true
		}
		return "", ct.Value, true
	}
	exactVals := map[string][]*entry{}
	regexKeys := map[string][]*entry{}
	var regexOrder []string
	var unconstrained []*entry
	for _, c := range cases {
		exact, regex, constrained := keyOf(c)
		switch {
		case !constrained:
			unconstrained = append(unconstrained, c)
		case exact != "":
			exactVals[exact] = append(exactVals[exact], c)
		default:
			if _, seen := regexKeys[regex]; !seen {
				regexOrder = append(regexOrder, regex)
			}
			regexKeys[regex] = append(regexKeys[regex], c)
		}
	}
	mergeBranch := func(key string) []*entry {
		var sub []*entry
		for _, c := range cases {
			exact, regex, constrained := keyOf(c)
			if !constrained || (exact != "" && exact == key) || (regex != "" && regex == key) {
				sub = append(sub, c)
			}
		}
		return sub
	}

	vals := make([]string, 0, len(exactVals))
	for v := range exactVals {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	for _, v := range vals {
		mb.Entries = append(mb.Entries, contract.MapEntry{
			Key:   v,
			Value: d.buildTree(mergeBranch(v), rest),
		})
	}
	for _, r := range regexOrder {
		mb.Entries = append(mb.Entries, contract.MapEntry{
			Key:   "~" + r,
			Value: d.buildTree(mergeBranch(r), rest),
		})
	}
	mb.Entries = append(mb.Entries, contract.MapEntry{
		Key:   "",
		Value: d.buildTree(unconstrained, rest),
	})

	d.cfg.Maps = append(d.cfg.Maps, mb)
	return "$" + name
}

// anyConstrains reports whether any case constrains any of the remaining
// inputs — when not, the first case is the answer and no map is needed.
func anyConstrains(cases []*entry, inputs []MatchConstraint) bool {
	for _, c := range cases {
		for _, in := range inputs {
			if _, ok := constraintOf(c, in); ok {
				return true
			}
		}
	}
	return false
}

// constraintOf returns the case's matcher for the given input, if any.
func constraintOf(e *entry, in MatchConstraint) (MatchConstraint, bool) {
	for _, c := range e.constraints {
		if c.Kind == in.Kind && c.Name == in.Name {
			return c, true
		}
	}
	return MatchConstraint{}, false
}

// sourceOf renders the nginx variable a constraint input reads from.
func sourceOf(in MatchConstraint) string {
	switch in.Kind {
	case constraintMethod:
		return "$request_method"
	case constraintHeader:
		return "$http_" + in.Name
	case constraintQuery:
		return "$arg_" + in.Name
	default:
		return "$request_method"
	}
}

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

// redirectLocation builds a static/guarded redirect location from a rule's
// RequestRedirect filter (GEP-726).
//
// Target construction: scheme (literal or preserved via $scheme), host
// (literal or preserved via $host), port (omitted when it is the scheme's
// default), then the path:
//
//   - no path modifier            → $request_uri (preserve path+query);
//   - ReplaceFullPath N           → N (query preserved via $is_args$args);
//   - ReplacePrefixMatch N, exact location "= P"   → N;
//   - ReplacePrefixMatch N, prefix location "P/"  → guarded
//     `if ($uri ~ ^P/(?<hng_r>.*)$) { return … N/$hng_r … }` carrying the
//     unmatched remainder (303/307/308 cannot use nginx's rewrite-redirect
//     flags, so the guarded-return form is used uniformly).
func (d *dispatchBuilder) redirectLocation(path string, e *entry) *contract.Location {
	rr := e.rule.RequestRedirect
	code := 301
	if rr.StatusCode != nil {
		switch *rr.StatusCode {
		case 301, 302, 303, 307, 308:
			code = *rr.StatusCode
		}
	}
	scheme := "$scheme"
	if rr.Scheme != nil {
		scheme = *rr.Scheme
	}
	host := "$host"
	if rr.Hostname != nil {
		host = string(*rr.Hostname)
	}
	prefix := scheme + "://" + host
	if rr.Port != nil {
		p := int(*rr.Port)
		defaultPort := (scheme == "http" && p == 80) || (scheme == "https" && p == 443)
		if !defaultPort {
			prefix += ":" + strconv.Itoa(p)
		}
	}

	loc := &contract.Location{Path: path}
	switch {
	case rr.Path == nil:
		loc.Redirect = &contract.Redirect{Code: code, URL: prefix + "$request_uri"}
	case rr.Path.ReplaceFullPath != nil:
		n := strings.TrimSuffix(*rr.Path.ReplaceFullPath, "/")
		if n == "" {
			n = "/"
		}
		loc.Redirect = &contract.Redirect{Code: code, URL: prefix + n + "$is_args$args"}
	case rr.Path.ReplacePrefixMatch != nil:
		n := strings.TrimSuffix(*rr.Path.ReplacePrefixMatch, "/")
		if _, ok := exactPathOf(path); ok {
			// Exact twin ("= P"): the matched prefix IS the whole path.
			loc.Redirect = &contract.Redirect{Code: code, URL: prefix + n + "$is_args$args"}
			return loc
		}
		// Prefix location ("P/"): capture the remainder after the matched
		// prefix and re-attach it to the replacement.
		p := e.rule.PathPrefix
		match := "^" + regexp.QuoteMeta(p) + "/(?<" + redirectCaptureName + ">.*)$"
		loc.RedirectIf = &contract.RedirectIf{
			Match: match,
			Code:  code,
			URL:   prefix + n + "/$" + redirectCaptureName + "$is_args$args",
		}
	}
	return loc
}

// applyURLRewrite applies a rule's URLRewrite filter (GEP-726) to a proxying
// location: hostname replaces the proxied Host; ReplaceFullPath/ReplacePrefixMatch
// emit nginx rewrites (query preserved automatically by nginx unless the
// replacement contains "?").
func applyURLRewrite(loc *contract.Location, e *entry) {
	uw := e.rule.URLRewrite
	if uw == nil {
		return
	}
	if uw.Hostname != nil {
		loc.ProxyHost = string(*uw.Hostname)
	}
	if uw.Path == nil {
		return
	}
	switch {
	case uw.Path.ReplaceFullPath != nil:
		n := *uw.Path.ReplaceFullPath
		loc.Rewrite = "^ " + n + " break"
	case uw.Path.ReplacePrefixMatch != nil:
		n := strings.TrimSuffix(*uw.Path.ReplacePrefixMatch, "/")
		if _, ok := exactPathOf(loc.Path); ok {
			loc.Rewrite = "^ " + n + " break"
			return
		}
		p := e.rule.PathPrefix
		loc.Rewrite = "^" + regexp.QuoteMeta(p) + "/(?<" + redirectCaptureName + ">.*)$ " + n + "/$" + redirectCaptureName + " break"
	}
}

// applyHeaderModifiers maps a rule's RequestHeaderModifier /
// ResponseHeaderModifier (GEP-1310) onto nginx directives:
//
//   - request set    → proxy_set_header Name value (replaces the inbound
//     header);
//   - request add    → proxy_set_header Name $hng_app_… where a generated
//     map appends to the inbound value when present ("in, V") and emits the
//     bare value when the inbound header is absent — true GEP add
//     semantics;
//   - request remove → proxy_set_header Name "" (nginx suppresses a header
//     whose value is empty);
//   - response add   → add_header Name value always;
//   - response set   → proxy_hide_header + add_header (suppress the
//     backend's value, then add ours);
//   - response remove → proxy_hide_header.
func (d *dispatchBuilder) applyHeaderModifiers(loc *contract.Location, rule *RuleInfo) {
	if m := rule.RequestHeaderModifier; m != nil {
		loc.RequestHeaders = append(loc.RequestHeaders, d.requestHeaderEntries(m)...)
	}
	if m := rule.ResponseHeaderModifier; m != nil {
		for _, h := range m.Set {
			loc.HideHeaders = append(loc.HideHeaders, string(h.Name))
			loc.ResponseHeaders = append(loc.ResponseHeaders, contract.Header{Name: string(h.Name), Value: h.Value})
		}
		for _, h := range m.Add {
			loc.ResponseHeaders = append(loc.ResponseHeaders, contract.Header{Name: string(h.Name), Value: h.Value})
		}
		for _, n := range m.Remove {
			loc.HideHeaders = append(loc.HideHeaders, n)
		}
	}
}

// requestHeaderEntries converts a request header modifier (GEP-1310) into
// proxy-header entries:
//
//	set    → literal value (replaces the inbound header);
//	add    → $hng_app_… variable where a generated map appends to the
//	         inbound value when present ("in, V") and emits the bare value
//	         when the inbound header is absent — true GEP add semantics;
//	remove → empty value (nginx suppresses a header whose value is empty).
//
// Shared by the rule-level modifier, the backendRef-level modifier
// (BackendRequestHeaderModification) and the mirrored copies of rule-level
// modifiers on internal mirror locations.
func (d *dispatchBuilder) requestHeaderEntries(m *gatewayv1.HTTPHeaderFilter) []contract.Header {
	var out []contract.Header
	for _, h := range m.Set {
		out = append(out, contract.Header{Name: string(h.Name), Value: h.Value})
	}
	for _, h := range m.Add {
		out = append(out, contract.Header{
			Name:  string(h.Name),
			Value: d.appendMap(headerVarName(string(h.Name)), h.Value),
		})
	}
	for _, n := range m.Remove {
		out = append(out, contract.Header{Name: n, Value: ""})
	}
	return out
}

// appendMap allocates the map implementing request-header add semantics
// for one (header, value) pair: inbound value present → "inbound, value";
// absent → "value". Returns the result variable (with $).
func (d *dispatchBuilder) appendMap(headerVar, value string) string {
	name := "hng_app_" + headerVar + "_" + strconv.FormatUint(uint64(fnv32a(value)), 16)
	for _, m := range d.cfg.Maps {
		if m.Name == name {
			return "$" + name
		}
	}
	d.cfg.Maps = append(d.cfg.Maps, &contract.MapBlock{
		Name:   name,
		Source: "$http_" + headerVar,
		Entries: []contract.MapEntry{
			{Key: "", Value: value},
			// Gateway API add semantics join multiple values with a
			// single comma (no space) — the conformance suite compares
			// the echoed header byte-for-byte.
			{Key: "default", Value: "$http_" + headerVar + "," + value},
		},
	})
	return "$" + name
}

// ---------------------------------------------------------------------------
// Path spec helpers
// ---------------------------------------------------------------------------

// isPrefixLocation reports whether the location matcher is a prefix match
// ("/x/" form; "/" is the root prefix).
func isPrefixLocation(path string) bool {
	return !strings.HasPrefix(path, "= ") && strings.HasSuffix(path, "/")
}

// exactPathOf extracts the literal path from an "= /x" exact location
// matcher.
func exactPathOf(path string) (string, bool) {
	if !strings.HasPrefix(path, "= ") {
		return "", false
	}
	return strings.TrimPrefix(path, "= "), true
}

// pathPrefixOf returns a prefix location's base path ("/x/" → "/x", "/" →
// "/").
func pathPrefixOf(path string) string {
	if path == "/" {
		return "/"
	}
	return strings.TrimSuffix(path, "/")
}
