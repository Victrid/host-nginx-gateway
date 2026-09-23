// Translation layer: Graph → contract.Configuration / certificates (the
// dataplane seam) and Graph → Gateway API status shapes (the status writer
// seam). Everything here is pure; the only clock/user-input boundary is the
// LastTransitionTime stamping done by internal/status at write time.
package provider

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/errs"
)

// ---------------------------------------------------------------------------
// Dataplane translation
// ---------------------------------------------------------------------------

// entry is one (rule match, path-spec) contribution to a server block's
// location set. seq is a global deterministic sequence number (Gateway order
// × listener order × attachment order × rule order × match order × path
// order) that decides precedence everywhere: within a hostname group, within
// a merged block and within a dispatch case list. upstream is the resolved
// upstream name for the rule (service upstream, rule-private weighted
// upstream, or a static marker). constraints are the MATCH's non-path
// constraints (each match is one dispatch case — OR across matches).
type entry struct {
	seq         int
	rule        *RuleInfo
	match       int
	path        string
	upstream    string
	constraints []MatchConstraint
}

// socketBuild accumulates one nginx socket's server blocks. The socket key
// is the canonical listen set; every listener (across Gateways) whose
// listen set normalizes to the same key contributes its hostname groups to
// this builder — nginx multiplexes a socket's blocks by server_name.
type socketBuild struct {
	listens []contract.Listen
	tlsCert string
	// groups maps effective hostname → entries.
	groups map[string][]*entry
	// seen dedupes (rule, match, path) per hostname: the same rule match
	// reaching the same (socket, hostname) through several listeners
	// contributes once, while DIFFERENT rules claiming the same path must
	// all survive (they become dispatch cases).
	seen  map[string]map[*RuleInfo]map[int]map[string]struct{}
	order []string // hostname first-seen order (fallback determinism)
	// hasCatchAll records whether any listener on this socket contributes
	// a catch-all ("") group — the socket's default server.
	hasCatchAll bool
}

func (s *socketBuild) group(h string) []*entry { return s.groups[h] }

// ensure registers a (possibly empty) hostname group so an attachment-less
// listener still renders its empty server block under its hostname.
func (s *socketBuild) ensure(hostname string) {
	if s.groups == nil {
		s.groups = map[string][]*entry{}
		s.seen = map[string]map[*RuleInfo]map[int]map[string]struct{}{}
	}
	if _, ok := s.groups[hostname]; !ok {
		s.seen[hostname] = map[*RuleInfo]map[int]map[string]struct{}{}
		s.order = append(s.order, hostname)
		s.groups[hostname] = nil
	}
}

func (s *socketBuild) add(hostname, path string, e *entry) {
	s.ensure(hostname)
	byMatch, ok := s.seen[hostname][e.rule]
	if !ok {
		byMatch = map[int]map[string]struct{}{}
		s.seen[hostname][e.rule] = byMatch
	}
	if byMatch[e.match] == nil {
		byMatch[e.match] = map[string]struct{}{}
	}
	if _, dup := byMatch[e.match][path]; dup {
		return
	}
	byMatch[e.match][path] = struct{}{}
	s.groups[hostname] = append(s.groups[hostname], e)
}

// Configuration translates the graph into the locked IR (DESIGN.md §5.4):
// per-socket server blocks with GEP-722 hostname-precedence dispatch,
// deterministic ordering.
//
// The translation is socket-centric: all listeners whose (post-annotation)
// listen sets are equal share one socket, and nginx multiplexes their
// server blocks by server_name. For every socket the translator:
//
//  1. collects location entries per effective hostname (per-route hostname
//     dispatch, Gateway API v1 HTTPRoute spec.hostnames; routes without
//     hostnames inherit the listener hostname);
//  2. merges broader-hostname groups into narrower-hostname blocks in
//     GEP-722 precedence order (exact > more-specific wildcard >
//     less-specific wildcard > catch-all) so that a request matching an
//     exact-hostname route WITHOUT a matching path falls through to a
//     broader route's matching path;
//  3. emits the socket's default server: the merged catch-all block when a
//     catch-all group exists (spec: it matches every host), otherwise a
//     synthetic empty block so unmatched hosts answer 404 instead of
//     leaking into any route.
//
// Location.Upstream values with the StaticUpstreamPrefix are markers: the
// renderer emits `return <code>;` for them and Configuration.Upstreams
// deliberately contains NO entry for them (an empty upstream block would
// fail `nginx -t`).
func (g *Graph) Configuration() *contract.Configuration {
	cfg := &contract.Configuration{}
	upstreams := map[string]map[string]contract.Endpoint{}
	sockets := map[string]*socketBuild{}
	seq := 0

	// The dispatcher owns deterministic map/split naming for the whole
	// render; create it before the mirror pre-pass so mirrored copies of
	// rule-level `add` header modifiers allocate their append maps in the
	// same deterministic sequence.
	dispatcher := &dispatchBuilder{cfg: cfg}

	// Mirror pre-pass (§3.3 RequestMirror): build every rule's internal
	// mirror location once (keyed by the mirror path), register the mirror
	// upstreams (shared with the main-backend upstream generation) and
	// compute the split_clients blocks backing percentage gates. The blocks
	// are attached only after the server loop confirms which gates survived
	// location merging (attachSplitClients below).
	mirrorIR, mirrorSplits := g.mirrorLocations(dispatcher, upstreams)

	for _, gw := range g.Gateways {
		if _, _, rejected := gw.rejection(); rejected {
			continue // rejected Gateway: no server blocks (InvalidParameters)
		}
		for _, li := range gw.Listeners {
			if !li.Valid || li.Conflicted {
				continue // §3.2: conflicted listeners get no server block
			}
			if li.CertFailed && li.CertData == nil {
				continue // no resolvable certificate material: nothing to serve
			}

			ssl := li.TLSCert != ""
			base := contract.Listen{
				Port:  int(li.Spec.Port),
				SSL:   ssl,
				HTTP2: ssl && li.Spec.Protocol == gatewayv1.HTTPSProtocolType,
			}
			// Without the listen-addresses annotation (or auto-assignment):
			// the bare wildcard `listen <port>` line. With it, ONLY the
			// annotated addresses are bound — the wildcard is replaced
			// (§3.1; this is what makes §3.2's disjoint-bind coexistence
			// meaningful).
			listens := []contract.Listen(nil)
			if len(li.Addresses) == 0 {
				listens = append(listens, base)
			}
			for _, addr := range li.Addresses {
				extra := base
				extra.Address = addr
				if !containsListen(listens, extra) {
					listens = append(listens, extra)
				}
			}
			key := socketKeyOf(listens)
			sb := sockets[key]
			if sb == nil {
				sb = &socketBuild{listens: listens, tlsCert: li.TLSCert}
				sockets[key] = sb
			} else if sb.tlsCert == "" {
				sb.tlsCert = li.TLSCert
			}

			// A listener with no attachments still renders its (empty)
			// server block under the listener hostname — behaviour and
			// status (Programmed) are unchanged by the grouping. With
			// attachments, groups are created lazily from each
			// attachment's effective hostnames so no spurious empty block
			// appears next to them.
			lg := map[string][]*entry{}
			seenInListener := map[string]map[*RuleInfo]map[int]map[string]struct{}{}
			addLocal := func(h, path string, e *entry) {
				if seenInListener[h] == nil {
					seenInListener[h] = map[*RuleInfo]map[int]map[string]struct{}{}
				}
				if seenInListener[h][e.rule] == nil {
					seenInListener[h][e.rule] = map[int]map[string]struct{}{}
				}
				if seenInListener[h][e.rule][e.match] == nil {
					seenInListener[h][e.rule][e.match] = map[string]struct{}{}
				}
				if _, dup := seenInListener[h][e.rule][e.match][path]; dup {
					return
				}
				seenInListener[h][e.rule][e.match][path] = struct{}{}
				lg[h] = append(lg[h], e)
			}
			if len(li.Attachments) == 0 {
				h := hostnameString(li.Spec.Hostname)
				lg[h] = nil
				if h == "" {
					sb.hasCatchAll = true
				}
			} else {
				hasCA := false
				for _, att := range li.Attachments {
					for _, eff := range hostnameIntersection(att.Route.Resource.Spec.Hostnames, li.Spec.Hostname) {
						bh, ok := refineHostname(li.Spec.Hostname, eff)
						if !ok {
							// The listener's claim and the route hostname
							// share NO common host (e.g. an apex listener
							// with a wildcard route): the attachment counts
							// for status but serves no requests.
							continue
						}
						if bh == "" {
							hasCA = true
						}
						for _, rule := range att.Route.Rules {
							if !rule.Valid {
								continue
							}
							up, _ := g.ruleUpstream(gw, att.Route, rule, upstreams)
							for mi, paths := range rule.MatchPaths {
								cases := rule.MatchCases[mi]
								for _, path := range paths {
									seq++
									addLocal(bh, path, &entry{
										seq: seq, rule: rule, match: mi, path: path,
										upstream: up, constraints: cases,
									})
								}
							}
						}
					}
				}
				if hasCA {
					sb.hasCatchAll = true
				}
			}

			// Listener-scoped GEP-722 precedence merge: broader-hostname
			// groups of THIS listener fold into narrower ones; the
			// listener's catch-all never absorbs other claims and never
			// leaks across listeners (requests match at most one listener).
			for h, entries := range mergeListenerGroups(lg) {
				sb.ensure(h)
				for _, e := range entries {
					sb.add(h, e.path, e)
				}
			}
		}
	}

	// Emit sockets in deterministic (lexicographic socket-key) order.
	keys := make([]string, 0, len(sockets))
	for k := range sockets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		sb := sockets[k]
		for _, hostname := range socketBlockOrder(sb) {
			entries := sb.groups[hostname]
			srv := &contract.Server{
				Hostname: hostname,
				TLSCert:  sb.tlsCert,
				Listens:  sb.listens,
			}
			srv.Locations = buildLocations(entries, dispatcher)
			// Mirror subrequests resolve within their own server block, so
			// every server whose locations mirror to a target carries its
			// own copy of the internal mirror location (once per server).
			srv.Locations = attachMirrorLocations(srv.Locations, mirrorIR)
			sort.Slice(srv.Locations, func(a, b int) bool {
				return srv.Locations[a].Path < srv.Locations[b].Path
			})
			cfg.Servers = append(cfg.Servers, srv)
		}
	}

	// Emit the split_clients blocks of the gates that survived the
	// per-server location merging.
	attachSplitClients(cfg, mirrorIR, mirrorSplits)

	for name, set := range upstreams {
		u := &contract.Upstream{Name: name}
		for _, ep := range set {
			u.Endpoints = append(u.Endpoints, ep)
		}
		sort.Slice(u.Endpoints, func(a, b int) bool {
			if u.Endpoints[a].IP != u.Endpoints[b].IP {
				return u.Endpoints[a].IP < u.Endpoints[b].IP
			}
			return u.Endpoints[a].Port < u.Endpoints[b].Port
		})
		cfg.Upstreams = append(cfg.Upstreams, u)
	}
	sort.Slice(cfg.Upstreams, func(a, b int) bool { return cfg.Upstreams[a].Name < cfg.Upstreams[b].Name })
	sort.Slice(cfg.Maps, func(a, b int) bool { return cfg.Maps[a].Name < cfg.Maps[b].Name })
	sort.Slice(cfg.SplitClients, func(a, b int) bool { return cfg.SplitClients[a].Name < cfg.SplitClients[b].Name })
	return cfg
}

// mirrorLocations pre-builds the internal mirror locations for every valid
// rule's resolved RequestMirror targets (§3.3), keyed by mirror path:
//
//	location = <path> {
//	    internal;
//	    if ($hng_sc_x = "") { return 204; }   # only when 0 < percent < 100
//	    proxy_pass http://<mirror upstream>$request_uri;
//	    proxy_set_header …                    # rule-level request modifiers
//	}
//
// It also (a) registers each mirror target's upstream in the shared
// upstream set — mirror upstreams reuse the existing ns_svc_port generation
// (the RequestMirror backendRef carries no weight) — and (b) computes the
// split_clients blocks backing percentage gates (keyed on $request_id,
// per-request random sampling — the NGF mechanism). The blocks are attached
// to the configuration only after the server loop confirms which mirror
// locations survived location merging (attachSplitClients). 100% / unset
// mirrors get no split_clients and no gate; 0% mirrors are dropped at
// resolution time and never reach here.
//
// The SAME mirror target appearing in several filters of one rule maps to
// ONE path (the path hash covers route + rule + target), so the max
// percentage across those filters wins here too — the same NGF max-merge
// the per-location merge applies.
func (g *Graph) mirrorLocations(d *dispatchBuilder,
	upstreams map[string]map[string]contract.Endpoint) (map[string]*contract.Location, map[string]*contract.SplitClients) {
	out := map[string]*contract.Location{}
	maxPct := map[string]float64{}

	for _, r := range g.Routes {
		for _, rule := range r.Rules {
			if !rule.Valid {
				continue
			}
			for _, m := range rule.RequestMirrors {
				if _, seen := out[m.Path]; !seen {
					set, ok := upstreams[m.Upstream]
					if !ok {
						set = map[string]contract.Endpoint{}
						upstreams[m.Upstream] = set
					}
					for _, ep := range m.Endpoints {
						k := fmt.Sprintf("%s:%d", ep.IP, ep.Port)
						if _, dup := set[k]; !dup {
							set[k] = ep
						}
					}
					loc := &contract.Location{
						Path:     "= " + m.Path,
						Internal: true,
						Upstream: m.Upstream,
						// The mirror subrequest URI is the internal path; the
						// original client request URI (path + query) must be
						// restored for the mirrored request.
						ProxyPassURI: "$request_uri",
					}
					// The rule-level request header modifier travels with the
					// mirrored copy (NGF mechanism: the mirror route carries
					// the rule's non-mirror request filters).
					if hm := rule.RequestHeaderModifier; hm != nil {
						loc.RequestHeaders = append(loc.RequestHeaders, d.requestHeaderEntries(hm)...)
					}
					out[m.Path] = loc
				}
				if m.Percent > maxPct[m.Path] {
					maxPct[m.Path] = m.Percent
				}
			}
		}
	}

	splits := map[string]*contract.SplitClients{}
	for path, loc := range out {
		pct := maxPct[path]
		if pct > 0 && pct < 100 {
			varName := mirrorSplitVarName(path, pct)
			loc.MirrorGate = varName
			if _, ok := splits[varName]; !ok {
				splits[varName] = &contract.SplitClients{
					Name:   varName,
					Source: "$request_id",
					Entries: []contract.SplitEntry{
						{Percent: fmt.Sprintf("%.2f", pct), Value: path},
						{Percent: "*", Value: ""},
					},
				}
			}
		}
	}
	return out, splits
}

// attachSplitClients emits the split_clients blocks of the gates that
// survived location merging: a block is emitted only when its mirror
// location is actually referenced by an emitted business location, so
// locations shadowed by higher-precedence rules leave no dead splits.
func attachSplitClients(cfg *contract.Configuration,
	mirrorIR map[string]*contract.Location, splits map[string]*contract.SplitClients) {
	if len(splits) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, srv := range cfg.Servers {
		for _, loc := range srv.Locations {
			for _, m := range loc.Mirrors {
				ml, ok := mirrorIR[m.Path]
				if !ok || ml.MirrorGate == "" || seen[ml.MirrorGate] {
					continue
				}
				sc, ok := splits[ml.MirrorGate]
				if !ok {
					continue // defensive
				}
				seen[ml.MirrorGate] = true
				cfg.SplitClients = append(cfg.SplitClients, sc)
			}
		}
	}
}

// mirrorSplitVarName derives the deterministic split_clients result variable
// for one percentage-gated mirror, keyed by (mirror path, percentage) so the
// same gated mirror referenced from several locations or server blocks
// shares one block (NGF deduplicates split_clients by variable name too).
func mirrorSplitVarName(path string, pct float64) string {
	return fmt.Sprintf("hng_sc_%x", fnv32a(fmt.Sprintf("%s|%.2f", path, pct)))
}

// attachMirrorLocations appends the internal mirror locations referenced by
// the server's business locations, once per server.
func attachMirrorLocations(locs []*contract.Location,
	mirrorIR map[string]*contract.Location) []*contract.Location {
	var referenced []string
	for _, loc := range locs {
		for _, m := range loc.Mirrors {
			referenced = append(referenced, m.Path)
		}
	}
	if len(referenced) == 0 {
		return locs
	}
	out := append([]*contract.Location(nil), locs...)
	seen := map[string]bool{}
	for _, p := range referenced {
		if seen[p] {
			continue
		}
		ml, ok := mirrorIR[p]
		if !ok {
			continue // defensive: every mirror path is pre-built
		}
		seen[p] = true
		out = append(out, ml)
	}
	return out
}

// ruleUpstream returns the upstream name and endpoint set a rule's
// locations point at, registering the upstream (once per name) in the
// shared set. Rules with multiple active backends get a rule-private
// combined upstream carrying per-endpoint weights (backendRef weights,
// §3.3); single-backend rules reuse the deterministic service upstream.
func (g *Graph) ruleUpstream(gw *GatewayInfo, route *RouteInfo, rule *RuleInfo,
	upstreams map[string]map[string]contract.Endpoint) (string, []contract.Endpoint) {
	// GEP-1364: a rule with ANY unresolvable backendRef answers the static
	// 500 for its matching requests (the existing single-backend behavior,
	// generalized) — nginx cannot mix "500" into a weighted upstream.
	for _, b := range rule.Backends {
		if IsStaticUpstream(b.Upstream) {
			return StaticUpstreamName(500), nil
		}
	}
	if len(rule.Backends) > 1 {
		name := weightedUpstreamName(gw, route, rule)
		if _, ok := upstreams[name]; !ok {
			set := map[string]contract.Endpoint{}
			for _, b := range rule.Backends {
				for _, ep := range b.Endpoints {
					wep := ep
					wep.Weight = int(b.Weight)
					k := wep.IP + ":" + strconv.Itoa(wep.Port)
					if _, dup := set[k]; !dup {
						set[k] = wep
					}
				}
			}
			upstreams[name] = set
		}
		return name, nil
	}
	if !IsStaticUpstream(rule.Upstream) {
		set, ok := upstreams[rule.Upstream]
		if !ok {
			set = map[string]contract.Endpoint{}
			upstreams[rule.Upstream] = set
		}
		for _, ep := range rule.Endpoints {
			k := fmt.Sprintf("%s:%d", ep.IP, ep.Port)
			if _, dup := set[k]; !dup {
				set[k] = ep
			}
		}
	}
	return rule.Upstream, rule.Endpoints
}

// weightedUpstreamName derives the deterministic rule-private upstream name
// for a weighted multi-backend rule.
func weightedUpstreamName(gw *GatewayInfo, route *RouteInfo, rule *RuleInfo) string {
	var sig strings.Builder
	sig.WriteString(gw.Resource.Namespace + "/" + gw.Resource.Name + "/" + route.Resource.Name)
	sig.WriteString("/" + strconv.Itoa(rule.Index))
	for _, b := range rule.Backends {
		sig.WriteString("|" + b.Upstream + ":" + strconv.Itoa(int(b.Weight)))
	}
	return "hng_wr_" + strconv.FormatUint(uint64(fnv32a(sig.String())), 16)
}

// mergeListenerGroups folds ONE listener's hostname groups into the final
// per-block entry sets, applying the GEP-722 precedence within the
// listener's claim:
//
//   - exact-hostname groups absorb the entries of every wildcard group
//     covering them, and of the catch-all group, in precedence order (a
//     request matching an exact-hostname route WITHOUT a matching path
//     falls through to a broader route's matching path);
//   - wildcard groups absorb entries from broader wildcards and the
//     catch-all group the same way;
//   - the catch-all group ("") keeps only its own entries: it is the
//     default server for hosts NO route claims, so broader routes must not
//     reach into it, and it must not absorb named routes' entries either.
//
// The merge is scoped to ONE listener: requests match at most one listener
// (Gateway API v1 "General Listener behavior"), so groups never cross
// listeners. The returned map always contains every input group key.
func mergeListenerGroups(lg map[string][]*entry) map[string][]*entry {
	names := make([]string, 0, len(lg))
	for name := range lg {
		names = append(names, name)
	}
	// Deterministic order: "" first, then lexical.
	sort.Slice(names, func(a, b int) bool {
		if (names[a] == "") != (names[b] == "") {
			return names[a] == ""
		}
		return names[a] < names[b]
	})

	var wildcards []string
	for _, n := range names {
		if strings.HasPrefix(n, "*.") {
			wildcards = append(wildcards, n)
		}
	}
	// More-specific wildcards contribute their entries before broader ones
	// so a shared path resolves to the narrower route.
	sort.Slice(wildcards, func(a, b int) bool {
		al, bl := strings.Count(wildcards[a], "."), strings.Count(wildcards[b], ".")
		if al != bl {
			return al > bl
		}
		return wildcards[a] < wildcards[b]
	})

	catchAll := lg[""]

	out := make(map[string][]*entry, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		cases := append([]*entry(nil), lg[n]...)
		for _, w := range wildcards {
			if w == n || !wildcardCovers(w, n) {
				continue
			}
			cases = append(cases, lg[w]...)
		}
		cases = append(cases, catchAll...)
		out[n] = dedupeEntries(cases)
	}
	out[""] = catchAll
	return out
}

// wildcardCovers reports whether every request Host matched by wildcard
// pattern w is also matched by the hostname-group n — i.e. n's block must
// carry w's entries so requests that nginx routes to n can fall through to
// w's routes by path. n may be an exact hostname or a (narrower) wildcard.
func wildcardCovers(w, n string) bool {
	if !strings.HasPrefix(w, "*.") {
		return false
	}
	wb := strings.TrimPrefix(w, "*.")
	if strings.HasPrefix(n, "*.") {
		nb := strings.TrimPrefix(n, "*.")
		// Every host of *.nb is a subdomain of nb; it matches *.wb iff nb
		// ends with .wb (or equals it — same group, excluded by caller).
		return strings.HasSuffix("."+nb, "."+wb)
	}
	return hostnameMatches(w, n)
}

// socketBlockOrder returns the server-block hostnames for a socket in
// emission order: the default server first, then named hostnames lexically.
// The default server is the merged catch-all block when the socket has one
// (spec: a hostname-less route matches every host of its listener);
// otherwise a synthetic empty block is emitted so unmatched hosts answer
// 404 instead of leaking into any route (nginx default server = first
// block listed for the socket).
func socketBlockOrder(sb *socketBuild) []string {
	names := make([]string, 0, len(sb.groups))
	for name := range sb.groups {
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	final := make([]string, 0, len(names)+1)
	if sb.hasCatchAll {
		final = append(final, "")
	} else if len(names) > 0 {
		// No route claims unmatched hosts: an empty default block answers
		// 404 (spec: unknown hosts must not leak into a route).
		sb.ensure("")
		final = append(final, "")
	}
	final = append(final, names...)
	return final
}

// refineHostname computes the nginx server_name for an attachment: the
// hosts that match BOTH the listener hostname H and the route's effective
// hostname E. When that common claim is empty (an exact/apex listener with
// a wildcard route whose coverage excludes the apex), the attachment
// serves no requests and produces no block (ok=false). Otherwise the block
// name is the more specific of the two:
//
//	H == E                    → H
//	H covers E (wildcard)     → E   (all E hosts lie within H's claim)
//	E covers H (wildcard and
//	           E matches H)   → H   (H is the exact, narrower claim)
//	no common host            → not ok
func refineHostname(listener *gatewayv1.Hostname, eff gatewayv1.Hostname) (string, bool) {
	if listener == nil {
		return string(eff), true
	}
	h := normHostname(string(*listener))
	e := normHostname(string(eff))
	switch {
	case e == h:
		return h, true
	case hostnameMatches(h, e):
		return e, true
	case hostnameMatches(e, h):
		return h, true
	default:
		return "", false
	}
}

// dedupeEntries removes entries duplicated by the merge (the same rule
// MATCH can contribute to a hostname through several listeners), keeping
// the first (highest-precedence) occurrence. Matches are distinct cases
// and are all preserved.
func dedupeEntries(in []*entry) []*entry {
	seen := map[*RuleInfo]map[int]map[string]struct{}{}
	out := make([]*entry, 0, len(in))
	for _, e := range in {
		byMatch, ok := seen[e.rule]
		if !ok {
			byMatch = map[int]map[string]struct{}{}
			seen[e.rule] = byMatch
		}
		if byMatch[e.match] == nil {
			byMatch[e.match] = map[string]struct{}{}
		}
		if _, dup := byMatch[e.match][e.path]; dup {
			continue
		}
		byMatch[e.match][e.path] = struct{}{}
		out = append(out, e)
	}
	return out
}

// buildLocations converts a merged entry list into contract locations: one
// location per distinct path spec, whose dispatch CASES are all entries in
// the block whose path spec can match a request reaching that location
// (specViableIn). This realizes GEP-722 fall-through INSIDE a hostname
// block: a request routed to a more specific location whose rule does not
// match (e.g. wrong method) falls through to a broader-path rule.
func buildLocations(entries []*entry, d *dispatchBuilder) []*contract.Location {
	byPath := map[string][]*entry{}
	var paths []string
	for _, e := range entries {
		if _, ok := byPath[e.path]; !ok {
			paths = append(paths, e.path)
		}
		byPath[e.path] = append(byPath[e.path], e)
	}
	sort.Strings(paths)

	var out []*contract.Location
	for _, p := range paths {
		// Viable cases in PRECEDENCE ORDER (GEP-993/GEP-722):
		//  1. the location's OWN path spec first (path specificity),
		//  2. then matches with MORE constraints (a more specific match is
		//     higher precedence),
		//  3. then attachment order (seq).
		// Foreign covering specs (broader paths / broader hostnames merged
		// in) come after the location's own matcher: the location itself is
		// the more specific path claim for the requests it receives.
		var own, foreign []*entry
		seenCase := map[*RuleInfo]map[int]map[string]struct{}{}
		for _, e := range entries {
			if !specViableIn(e.path, p) {
				continue
			}
			byMatch, ok := seenCase[e.rule]
			if !ok {
				byMatch = map[int]map[string]struct{}{}
				seenCase[e.rule] = byMatch
			}
			if byMatch[e.match] == nil {
				byMatch[e.match] = map[string]struct{}{}
			}
			if _, dup := byMatch[e.match][e.path]; dup {
				continue
			}
			byMatch[e.match][e.path] = struct{}{}
			if e.path == p {
				own = append(own, e)
			} else {
				foreign = append(foreign, e)
			}
		}
		bySpecificity := func(l []*entry) {
			sort.SliceStable(l, func(a, b int) bool {
				return len(l[a].constraints) > len(l[b].constraints)
			})
		}
		bySpecificity(own)
		bySpecificity(foreign)
		viable := append(own, foreign...)
		if loc := d.locationFor(p, viable); loc != nil {
			out = append(out, loc)
		}
	}
	return out
}

func socketKeyOf(listens []contract.Listen) string {
	sorted := append([]contract.Listen(nil), listens...)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Address != sorted[b].Address {
			return sorted[a].Address < sorted[b].Address
		}
		if sorted[a].Port != sorted[b].Port {
			return sorted[a].Port < sorted[b].Port
		}
		if sorted[a].SSL != sorted[b].SSL {
			return !sorted[a].SSL
		}
		return !sorted[a].HTTP2 && sorted[b].HTTP2
	})
	var b strings.Builder
	for _, l := range sorted {
		fmt.Fprintf(&b, "%s|%d|%t|%t;", l.Address, l.Port, l.SSL, l.HTTP2)
	}
	return b.String()
}

func hostnameString(h *gatewayv1.Hostname) string {
	if h == nil {
		return "" // catch-all: renderer emits server_name _
	}
	return string(*h)
}

func containsListen(listens []contract.Listen, l contract.Listen) bool {
	for _, x := range listens {
		if x == l {
			return true
		}
	}
	return false
}

// Certificates returns the desired certificate set (DESIGN.md §5.3),
// deduplicated by deterministic filename, sorted.
func (g *Graph) Certificates() []Certificate {
	seen := map[string]Certificate{}
	for _, gw := range g.Gateways {
		for _, li := range gw.Listeners {
			if li.CertData == nil {
				continue
			}
			c := Certificate{
				Namespace: li.CertSecretNamespace,
				Name:      li.CertSecretName,
				Data:      li.CertData,
			}
			seen[c.Filename()] = c
		}
	}
	out := make([]Certificate, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename() < out[j].Filename() })
	return out
}

// ---------------------------------------------------------------------------
// Apply result → Programmed/Acpected classification (DESIGN.md §3.4)
// ---------------------------------------------------------------------------

// ApplyResult is the dataplane outcome for one full sync, already classified
// through internal/errs so the provider stays free of dataplane types.
type ApplyResult struct {
	// Applied is true when the configuration was rendered, validated and
	// reloaded successfully.
	Applied bool
	// Reason is the Programmed condition reason (Programmed | Pending |
	// Invalid) when Applied is false.
	Reason string
	// Message accompanies the Programmed condition.
	Message string
	// AcceptedReason, when non-empty, forces Gateway Accepted=False with
	// this reason (Invalid) — the include-missing and validation classes.
	AcceptedReason  string
	AcceptedMessage string
}

// ApplyResultFromError classifies an Applier error via the errs taxonomy.
// A nil error is the success case.
func ApplyResultFromError(err error) ApplyResult {
	if err == nil {
		return ApplyResult{
			Applied: true,
			Reason:  string(gatewayv1.GatewayReasonProgrammed),
			Message: "configuration generated and applied (nginx reloaded)",
		}
	}
	m := errs.StatusReason(err)
	r := ApplyResult{Reason: string(m.Reason), Message: m.Message}
	if m.Type == errs.ConditionAccepted && m.Status == errs.StatusFalse {
		r.AcceptedReason, r.AcceptedMessage = string(m.Reason), m.Message
	}
	return r
}

// ---------------------------------------------------------------------------
// Status shapes (three layers, DESIGN.md §3.4)
// ---------------------------------------------------------------------------

func cond(gen int64, ctype, status, reason, msg string) metav1.Condition {
	return metav1.Condition{
		Type:               ctype,
		Status:             metav1.ConditionStatus(status),
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: gen,
	}
}

// AcceptedCondition is the GatewayClass status condition (layer 1).
func (c *ClassInfo) AcceptedCondition() metav1.Condition {
	if !c.Accepted {
		return cond(c.Resource.Generation,
			string(gatewayv1.GatewayClassConditionStatusAccepted),
			string(metav1.ConditionFalse),
			c.AcceptedReason,
			c.Message)
	}
	msg := "GatewayClass accepted by " + string(ControllerName)
	if c.Message != "" {
		msg += "; " + c.Message
	}
	return cond(c.Resource.Generation,
		string(gatewayv1.GatewayClassConditionStatusAccepted),
		string(metav1.ConditionTrue),
		string(gatewayv1.GatewayClassReasonAccepted),
		msg)
}

// rejection reports why the Gateway as a whole is rejected (class not
// accepted or unsupported infrastructure parametersRef).
func (gw *GatewayInfo) rejection() (reason, msg string, rejected bool) {
	if !gw.Class.Accepted {
		return gw.Class.AcceptedReason, gw.Class.Message, true
	}
	if gw.Rejected {
		return gw.RejectReason, gw.RejectMsg, true
	}
	return "", "", false
}

// StatusAddresses computes the Gateway's status.addresses (Gateway API v1:
// "the network addresses that have been assigned to the Gateway", DESIGN.md
// §3.4). Precedence:
//
//  1. the Gateway's gateway.host-nginx/publish-addresses annotation;
//  2. the operator's explicit --publish-addresses list;
//  3. the auto-assigned loopback bind (cross-Gateway listener separation);
//  4. the caller-provided default (flag default: the node's primary IP).
//
// An empty result means "nothing authoritative known" — the status writer
// leaves any existing addresses untouched.
func (gw *GatewayInfo) StatusAddresses(explicit, fallback []string) []gatewayv1.GatewayStatusAddress {
	addrs := publishList(gw.Resource.Annotations[PublishAddressesAnnotation])
	if len(addrs) == 0 {
		addrs = explicit
	}
	if len(addrs) == 0 && gw.AutoAddress != "" {
		addrs = []string{gw.AutoAddress}
	}
	if len(addrs) == 0 {
		addrs = fallback
	}
	if len(addrs) == 0 {
		return nil
	}
	ipType := gatewayv1.IPAddressType
	out := make([]gatewayv1.GatewayStatusAddress, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, gatewayv1.GatewayStatusAddress{Type: &ipType, Value: a})
	}
	return out
}

// publishList parses a comma-separated address list (annotation form).
func publishList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// AnyListenerInvalid reports whether any listener of the Gateway is invalid.
func (gw *GatewayInfo) AnyListenerInvalid() bool {
	for _, l := range gw.Listeners {
		if !l.Valid {
			return true
		}
	}
	return false
}

// Conditions assembles the Gateway-level conditions (layer 2a): Accepted and
// Programmed per the §3.4 table:
//
//	Accepted   False/Invalid           include missing or validation failure (errs)
//	Accepted   False/ListenersNotValid any listener invalid
//	Accepted   True/Accepted           otherwise
//	Programmed True/Programmed         applied + reloaded
//	Programmed False/Pending           nginx master not running (§6)
//	Programmed False/Invalid           nginx -t / reload failure (N6 stderr in message)
//	Programmed False/ListenersNotValid applied, but invalid listeners hold it back
func (gw *GatewayInfo) Conditions(apply ApplyResult) []metav1.Condition {
	gen := gw.Resource.Generation

	// A rejected Gateway (invalid GatewayClass, unsupported infrastructure
	// parametersRef) is rejected wholesale (Gateway API: Accepted=False).
	if reason, msg, rejected := gw.rejection(); rejected {
		return []metav1.Condition{
			cond(gen, string(gatewayv1.GatewayConditionAccepted),
				string(metav1.ConditionFalse), reason, msg),
			cond(gen, string(gatewayv1.GatewayConditionProgrammed),
				string(metav1.ConditionFalse),
				string(gatewayv1.GatewayReasonPending),
				"Gateway is rejected; no configuration is generated"),
		}
	}

	anyValid, anyInvalid := false, false
	for _, l := range gw.Listeners {
		if l.Valid {
			anyValid = true
		} else {
			anyInvalid = true
		}
	}

	var accepted metav1.Condition
	switch {
	case apply.AcceptedReason != "":
		accepted = cond(gen, string(gatewayv1.GatewayConditionAccepted),
			string(metav1.ConditionFalse), apply.AcceptedReason, apply.AcceptedMessage)
	case !anyValid:
		accepted = cond(gen, string(gatewayv1.GatewayConditionAccepted),
			string(metav1.ConditionFalse),
			string(gatewayv1.GatewayReasonListenersNotValid),
			"all listeners are invalid; see status.listeners[].conditions")
	case anyInvalid:
		// At least one listener is accepted: the Gateway is Accepted=True,
		// with ListenersNotValid documenting the partial invalidity
		// (Gateway API conformance: GatewayListenerUnsupportedProtocol).
		accepted = cond(gen, string(gatewayv1.GatewayConditionAccepted),
			string(metav1.ConditionTrue),
			string(gatewayv1.GatewayReasonListenersNotValid),
			"one or more listeners are invalid; see status.listeners[].conditions")
	default:
		accepted = cond(gen, string(gatewayv1.GatewayConditionAccepted),
			string(metav1.ConditionTrue),
			string(gatewayv1.GatewayReasonAccepted),
			"Gateway accepted by "+string(ControllerName))
	}

	var programmed metav1.Condition
	switch {
	case !apply.Applied:
		programmed = cond(gen, string(gatewayv1.GatewayConditionProgrammed),
			string(metav1.ConditionFalse), apply.Reason, apply.Message)
	case anyInvalid:
		programmed = cond(gen, string(gatewayv1.GatewayConditionProgrammed),
			string(metav1.ConditionFalse),
			string(gatewayv1.GatewayReasonListenersNotValid),
			"configuration applied, but one or more listeners are invalid")
	default:
		programmed = cond(gen, string(gatewayv1.GatewayConditionProgrammed),
			string(metav1.ConditionTrue),
			string(gatewayv1.GatewayReasonProgrammed),
			apply.Message)
	}
	return []metav1.Condition{accepted, programmed}
}

// ListenerStatuses assembles status.listeners[] (layer 2b): one entry per
// spec listener with Accepted / Conflicted / ResolvedRefs / Programmed.
//
//	Accepted      True/Accepted                  valid listener
//	Accepted      False/Invalid                  port missing, tls misuse
//	Accepted      False/UnsupportedProtocol      unknown protocol, Passthrough
//	Conflicted    False/NoConflicts              no §3.2 conflict
//	Conflicted    True/HostnameConflict          overlapping port+hostname
//	Conflicted    True/ProtocolConflict          same port, differing protocols
//	ResolvedRefs  True/ResolvedRefs              certificates resolved
//	ResolvedRefs  False/RefNotPermitted          cross-ns certificateRef (§3.5)
//	ResolvedRefs  False/InvalidCertificateRef    missing/invalid TLS Secret
//	Programmed    True/Programmed                server block generated & applied
//	Programmed    False/Invalid                  invalid listener or apply failure
//	Programmed    False/Pending                  conflicted listener or nginx down
func (gw *GatewayInfo) ListenerStatuses(apply ApplyResult) []gatewayv1.ListenerStatus {
	gen := gw.Resource.Generation
	out := make([]gatewayv1.ListenerStatus, 0, len(gw.Listeners))
	for _, li := range gw.Listeners {
		ls := gatewayv1.ListenerStatus{Name: li.Spec.Name}
		if _, msg, rejected := gw.rejection(); rejected {
			// Listener under a rejected Gateway: not accepted, nothing
			// will be programmed for it.
			ls.Conditions = append(ls.Conditions,
				cond(gen, string(gatewayv1.ListenerConditionAccepted),
					string(metav1.ConditionFalse),
					string(gatewayv1.ListenerReasonInvalid),
					"Gateway is rejected: "+msg),
				cond(gen, string(gatewayv1.ListenerConditionProgrammed),
					string(metav1.ConditionFalse),
					string(gatewayv1.ListenerReasonPending),
					"Gateway is rejected"))
			out = append(out, ls)
			continue
		}
		ls.SupportedKinds = li.SupportedKinds
		ls.AttachedRoutes = int32(li.attachedRouteCount())

		// Accepted
		if li.Valid {
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionAccepted),
				string(metav1.ConditionTrue),
				string(gatewayv1.ListenerReasonAccepted), ""))
		} else {
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionAccepted),
				string(metav1.ConditionFalse), li.InvalidReason, li.InvalidMsg))
		}
		// Conflicted
		if li.Conflicted {
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionConflicted),
				string(metav1.ConditionTrue), li.ConflictReason, li.ConflictMsg))
		} else {
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionConflicted),
				string(metav1.ConditionFalse),
				string(gatewayv1.ListenerReasonNoConflicts), ""))
		}
		// ResolvedRefs (certificateRefs)
		status, reason := string(metav1.ConditionTrue), li.RefsReason
		msg := li.RefsMsg
		if !li.ResolvedRefs {
			status = string(metav1.ConditionFalse)
		} else {
			msg = ""
		}
		ls.Conditions = append(ls.Conditions, cond(gen,
			string(gatewayv1.ListenerConditionResolvedRefs), status, reason, msg))
		// Programmed
		switch {
		case !li.Valid:
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionProgrammed),
				string(metav1.ConditionFalse),
				string(gatewayv1.ListenerReasonInvalid), li.InvalidMsg))
		case li.Conflicted:
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionProgrammed),
				string(metav1.ConditionFalse),
				string(gatewayv1.ListenerReasonPending),
				li.ConflictMsg+" — no server configuration generated"))
		case li.CertFailed && li.CertData == nil:
			// Unresolvable certificateRefs: no server block was generated,
			// so the listener cannot be considered programmed.
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionProgrammed),
				string(metav1.ConditionFalse),
				string(gatewayv1.ListenerReasonPending),
				li.RefsMsg+" — no server configuration generated"))
		case !apply.Applied:
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionProgrammed),
				string(metav1.ConditionFalse), apply.Reason, apply.Message))
		default:
			ls.Conditions = append(ls.Conditions, cond(gen,
				string(gatewayv1.ListenerConditionProgrammed),
				string(metav1.ConditionTrue),
				string(gatewayv1.ListenerReasonProgrammed), ""))
		}
		out = append(out, ls)
	}
	return out
}

// attachedRouteCount counts unique routes with Accepted=true attached to the
// listener (ListenerStatus.AttachedRoutes semantics).
func (li *ListenerInfo) attachedRouteCount() int {
	seen := map[*RouteInfo]struct{}{}
	for _, att := range li.Attachments {
		if att.Parent.Accepted {
			seen[att.Route] = struct{}{}
		}
	}
	return len(seen)
}

// ParentStatuses assembles status.parents[] (layer 3) with the GEP-1364
// table; entries whose parentRef was skipped (foreign controller) are
// omitted.
//
//	Accepted        True/Accepted                    attached to ≥1 listener, ≥1 valid rule
//	Accepted        False/InvalidKind                parentRef not a Gateway
//	Accepted        False/NoMatchingParent           Gateway/listener/port not found
//	Accepted        False/NotAllowedByListeners      no listener accepts HTTPRoute (protocol/kinds/namespaces —
//	                                                  including allowedRoutes from=Same rejecting cross-namespace routes)
//	Accepted        False/NoMatchingListenerHostname hostname intersection empty (S4)
//	Accepted        False/UnsupportedValue           no implementable rule
//	ResolvedRefs    True/ResolvedRefs                all backendRefs resolved
//	ResolvedRefs    False/BackendNotFound            service absent (GEP-1364; rule answers 500)
//	ResolvedRefs    False/RefNotPermitted            cross-namespace backendRef (§3.5)
//	ResolvedRefs    False/InvalidKind                backendRef not a Service
//	ResolvedRefs    False/UnsupportedValue           port missing/ambiguous
//	PartiallyInvalid True/UnsupportedValue           some rules dropped, ≥1 still serving (GEP-1748)
func (r *RouteInfo) ParentStatuses() []gatewayv1.RouteParentStatus {
	gen := r.Resource.Generation
	var out []gatewayv1.RouteParentStatus
	for _, pi := range r.Parents {
		if pi.Skip {
			continue
		}
		ps := gatewayv1.RouteParentStatus{
			ParentRef:      pi.Ref,
			ControllerName: ControllerName,
		}
		status := string(metav1.ConditionTrue)
		if !pi.Accepted {
			status = string(metav1.ConditionFalse)
		}
		msg := pi.Message
		if pi.Accepted {
			msg = ""
		}
		ps.Conditions = append(ps.Conditions, cond(gen,
			string(gatewayv1.RouteConditionAccepted), status, pi.Reason, msg))

		refStatus, refMsg := string(metav1.ConditionTrue), ""
		if !pi.ResolvedRefs {
			refStatus = string(metav1.ConditionFalse)
			refMsg = pi.RefsMsg
		}
		ps.Conditions = append(ps.Conditions, cond(gen,
			string(gatewayv1.RouteConditionResolvedRefs), refStatus, pi.RefsReason, refMsg))

		// PartiallyInvalid only when the route is accepted and at least one
		// rule survived (GEP-1748: never set for fully valid/invalid/not
		// accepted routes).
		if pi.Accepted && r.partiallyInvalid() {
			ps.Conditions = append(ps.Conditions, cond(gen,
				string(gatewayv1.RouteConditionPartiallyInvalid),
				string(metav1.ConditionTrue),
				string(gatewayv1.RouteReasonUnsupportedValue),
				r.partialInvalidMessage()))
		}
		out = append(out, ps)
	}
	return out
}

func (r *RouteInfo) partiallyInvalid() bool {
	anyValid, anyInvalid := false, false
	for _, rule := range r.Rules {
		if rule.Valid {
			anyValid = true
		} else {
			anyInvalid = true
		}
	}
	return anyValid && anyInvalid
}

func (r *RouteInfo) partialInvalidMessage() string {
	var dropped []int
	for _, rule := range r.Rules {
		if !rule.Valid {
			dropped = append(dropped, rule.Index)
		}
	}
	return fmt.Sprintf("rules %v are not implementable and answer nothing; the remaining rules serve traffic", dropped)
}
