// Package provider implements the Kubernetes side of HostNginxGateway:
// it watches Gateway API resources, builds the internal graph (IR) from a
// cache snapshot, translates it into the locked dataplane contract, and
// drives the three-layer status write-back.
//
// BuildGraph is a pure function over a Resources snapshot so the full
// Gateway API semantics (listener validity, §3.2 conflicts, §3.3 hostname
// intersection, §3.4 GEP-1364, §3.5 ReferenceGrant for cross-namespace
// backend/certificate refs with allowedRoutes-governed cross-namespace
// route attachment, §4 EndpointSlice backend resolution) are unit-testable
// without a cluster.
package provider

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/nodeaddrs"
)

const (
	// ControllerName is the gateway.controller value this implementation
	// claims (DESIGN.md §7: "本 controllerName 之下的资源"). GatewayClasses
	// with any other controllerName are ignored.
	ControllerName gatewayv1.GatewayController = "gateway.host-nginx/controller"

	// AnnotationPrefix is the namespace of the controller's own
	// annotations since v0.2.0 (DESIGN-multinode-addresses.md §5:
	// "全部自有注解统一为 hng.victrid.dev/<name>").
	AnnotationPrefix = "hng.victrid.dev/"

	// NOTE (v0.3.0): the pre-v0.2.0 `gateway.host-nginx/<name>` fallback
	// and the listen-addresses / publish-addresses annotations were
	// REMOVED. spec.addresses (IPAddress type) is the binding intent and
	// status.addresses derives from the rendered listens; see README.md
	// ("Breaking changes in v0.3.0").

	// ServerSnippetAnnotation injects raw nginx configuration inside every
	// server block rendered from the Gateway
	// (DESIGN-multinode-addresses.md §5 escape hatch). Honored only when
	// --dangerously-allow-nginx-snippets is set; NO legacy fallback.
	//
	//	hng.victrid.dev/server-snippet: "sub_filter_types text/css;"
	ServerSnippetAnnotation = AnnotationPrefix + "server-snippet"

	// LocationSnippetAnnotation (HTTPRoute metadata) injects raw nginx
	// configuration inside every location block generated from that
	// route's rules. Honored only when --dangerously-allow-nginx-snippets
	// is set; NO legacy fallback.
	//
	//	hng.victrid.dev/location-snippet: "proxy_buffering off;"
	LocationSnippetAnnotation = AnnotationPrefix + "location-snippet"

	// ExtraFilesAnnotation (Gateway metadata) is a comma-separated list of
	// same-namespace refs whose data keys are materialised under
	// <conf-dir>/files/<ns>_<name>/<key> (DESIGN-multinode-addresses.md
	// §5): "configmap:ns/name" and "secret:ns/name". Honored only when
	// --dangerously-allow-extra-files is set; NO legacy fallback.
	//
	//	hng.victrid.dev/extra-files: "configmap:default/lua,secret:default/chain"
	ExtraFilesAnnotation = AnnotationPrefix + "extra-files"

	// autoAssignBase / autoAssignSpan define the loopback pool used to give
	// indistinct cross-Gateway listeners distinct bind addresses
	// (127.0.0.8 … 127.0.0.239). The whole 127/8 loopback is routable on
	// Linux, so binding any address in the range needs no setup; the lower
	// IDs (…/8 base) avoid 127.0.0.1/2 which operators commonly use.
	autoAssignBase = 8
	autoAssignSpan = 232

	// StaticUpstreamPrefix marks Location.Upstream values that the renderer
	// MUST translate into a static `return <code>;` response instead of a
	// proxy_pass (GEP-1364: unresolvable backends answer HTTP 500).
	// Upstreams with this prefix are intentionally NOT emitted in
	// Configuration.Upstreams — see translate.go.
	StaticUpstreamPrefix = "hng_static_"

	// TLSSecretType is the only Secret type accepted for certificates.
	TLSSecretType corev1.SecretType = "kubernetes.io/tls"
)

// GraphOptions carries the operator's escape-hatch flags into BuildGraph
// (DESIGN-multinode-addresses.md §5a danger flags). Both default to false:
// snippet / extra-file annotations are then ignored with a recorded
// warning (surfaced as a controller log line by the reconciler — no
// status condition, the flags are the deliberate gate).
type GraphOptions struct {
	// AllowNginxSnippets enables the hng.victrid.dev/server-snippet and
	// hng.victrid.dev/location-snippet annotations.
	AllowNginxSnippets bool
	// AllowExtraFiles enables the hng.victrid.dev/extra-files annotation.
	AllowExtraFiles bool

	// NodeAddresses is THIS node's address fingerprint
	// (DESIGN-multinode-addresses.md §2). It intersects every listener's
	// spec.addresses bind intent: only the intersection is rendered and
	// reported; a non-wildcard listener whose intersection is empty is
	// not owned by this node (no server block, no listener status entry,
	// and — when the Gateway owns nothing here — no Gateway status
	// write). Nil means "no fingerprint" (single-node semantics: every
	// address is owned).
	NodeAddresses *nodeaddrs.Set
}

// StaticUpstreamName returns the marker upstream name for a static response
// code (e.g. hng_static_500).
func StaticUpstreamName(code int) string {
	return fmt.Sprintf("%s%d", StaticUpstreamPrefix, code)
}

// IsStaticUpstream reports whether an upstream name is a static-response
// marker (used by the renderer seam and tests).
func IsStaticUpstream(name string) bool {
	return strings.HasPrefix(name, StaticUpstreamPrefix)
}

// Certificate is one TLS certificate materialised by the dataplane under
// <confDir>/certs/<Namespace>_<Name>.pem (DESIGN.md §5.3). Data is the
// concatenation of tls.crt and tls.key — nginx accepts cert+key in a single
// PEM referenced by both ssl_certificate and ssl_certificate_key.
type Certificate struct {
	Namespace string
	Name      string
	Data      []byte
}

// Filename returns the deterministic cert file base name (DESIGN.md §5.3).
func (c Certificate) Filename() string {
	return c.Namespace + "_" + c.Name + ".pem"
}

// Resources is an immutable snapshot of the objects listed from the cache.
// All namespaces; graph-level filtering applies (MVP has no predicates,
// DESIGN.md §7 / S7).
type Resources struct {
	GatewayClasses []*gatewayv1.GatewayClass
	Gateways       []*gatewayv1.Gateway
	HTTPRoutes     []*gatewayv1.HTTPRoute
	Secrets        []*corev1.Secret // all secrets; TLS filtering happens during certificate resolution
	EndpointSlices []*discoveryv1.EndpointSlice
	// Services resolve backendRef.servicePort → target port for named /
	// multi-port Services (conformance: multi-port resolution).
	Services []*corev1.Service
	// Namespaces evaluate allowedRoutes.namespaces.from=Selector.
	Namespaces []*corev1.Namespace
	// ReferenceGrants gate cross-namespace backendRefs (HTTPRoute → Service)
	// and listener certificateRefs (Gateway → Secret) per the Gateway API
	// ReferenceGrant spec (§3.5).
	ReferenceGrants []*gatewayv1.ReferenceGrant
	// ConfigMaps feed the --dangerously-allow-extra-files escape hatch
	// (DESIGN-multinode-addresses.md §5: extra-files refs).
	ConfigMaps []*corev1.ConfigMap
}

// Graph is the resolved IR: ownership, listener validity, conflicts, route
// attachment and backend resolution, ready for translation into
// contract.Configuration and status shapes.
type Graph struct {
	Classes  []*ClassInfo
	Gateways []*GatewayInfo
	Routes   []*RouteInfo

	// Warnings are deterministic, human-readable advisories collected
	// while building the graph (legacy-annotation deprecations, escape-
	// hatch annotations ignored because their danger flag is off, skipped
	// extra-file refs). BuildGraph stays pure — it records instead of
	// logging; the reconciler turns these into controller log lines.
	Warnings []string
}

// warn records one advisory on the graph (deterministic order: the order
// BuildGraph noticed them).
func (g *Graph) warn(msg string) {
	g.Warnings = append(g.Warnings, msg)
}

// ClassInfo is an owned GatewayClass (spec.controllerName == ControllerName).
type ClassInfo struct {
	Resource *gatewayv1.GatewayClass
	// Accepted is false when the class spec is rejected by this controller
	// (currently: any spec.parametersRef — no parameters kind is supported).
	Accepted       bool
	AcceptedReason string // GatewayClass reason: Accepted | InvalidParameters
	// Message is appended to the Accepted condition; empty when fully clean.
	Message string
}

// GatewayInfo is a Gateway whose GatewayClass is owned by this controller.
type GatewayInfo struct {
	Resource  *gatewayv1.Gateway
	Class     *ClassInfo
	Listeners []*ListenerInfo

	// Rejected marks a Gateway rejected independently of its listeners
	// (currently: spec.infrastructure.parametersRef, which no implementation
	// exists for → Accepted=False/InvalidParameters).
	Rejected     bool
	RejectReason string
	RejectMsg    string

	// AutoAddress is the loopback bind address the controller assigned to
	// this Gateway so that its listeners are distinct from another
	// Gateway's indistinct listeners on the same port (Gateway API v1,
	// "Distinct Listeners": implementations merging Gateways onto one data
	// plane must keep the combined listener set distinct — the address is
	// part of that tuple). Empty when no assignment was needed.
	AutoAddress string

	// RawServerSnippet is the Gateway's hng.victrid.dev/server-snippet
	// value, populated only when --dangerously-allow-nginx-snippets is on
	// (DESIGN-multinode-addresses.md §5); copied onto every listener in
	// resolveListener and from the claiming listener onto the rendered
	// server blocks.
	RawServerSnippet string

	// ExtraFiles are the materialised extra-file entries resolved from
	// hng.victrid.dev/extra-files (only when
	// --dangerously-allow-extra-files is on): one entry per (ref, data
	// key), RelPath relative to the owned conf dir. Configuration()
	// copies them into contract.Configuration.ExtraFiles.
	ExtraFiles []ExtraFileEntry
}

// ExtraFileEntry is one resolved extra-files data key
// (DESIGN-multinode-addresses.md §5). RelPath is always
// "files/<ns>_<name>/<key>" — the path the applier materialises under the
// owned conf dir and the base of the absolute path snippet placeholders
// resolve to.
type ExtraFileEntry struct {
	RelPath string
	Content []byte
}

// ListenerInfo is the resolution result for one spec.listeners[] entry.
type ListenerInfo struct {
	Gateway *GatewayInfo
	Spec    gatewayv1.Listener
	Index   int // position in spec.listeners, for stable ordering

	// Valid is false when the listener spec is rejected (port missing,
	// unsupported protocol, invalid TLS usage). Invalid listeners produce no
	// server block, do not join conflict detection, and do not accept routes.
	Valid         bool
	InvalidReason string // metav1.Condition reason: Invalid | UnsupportedProtocol
	InvalidMsg    string

	// Conflicted is set by §3.2 conflict detection (both sides are marked,
	// no server block is generated for either).
	Conflicted     bool
	ConflictReason string // HostnameConflict | ProtocolConflict
	ConflictMsg    string

	// CertFailed marks certificateRefs resolution failures. With no cert
	// data at all the server block is suppressed and the listener stays
	// Programmed=False/Pending; when the Secret exists but its PEM is
	// malformed the (garbage) cert is still emitted so nginx -t fails
	// loudly (Programmed=False/Invalid, DESIGN.md §3.4).
	CertFailed bool

	// ResolvedRefs covers certificateRefs and allowedRoutes.kinds
	// (DESIGN.md §3.5).
	ResolvedRefs bool
	RefsReason   string // ResolvedRefs | RefNotPermitted | InvalidCertificateRef | InvalidRouteKinds
	RefsMsg      string

	// SupportedKinds lists the route kinds this listener accepts after
	// intersecting allowedRoutes.kinds with the supported set (mirrored into
	// status.listeners[].supportedKinds; nil for non-route protocols).
	SupportedKinds []gatewayv1.RouteGroupKind

	// Addresses are the listener's EFFECTIVE bind addresses in nginx
	// listen form (v4 plain, v6 bracketed): the Gateway's spec.addresses
	// (binding intent, DESIGN-multinode-addresses.md §2) intersected with
	// this node's address fingerprint by applyNodeFilter. Empty means the
	// wildcard `listen <port>` form. Not-owned listeners end up empty
	// here — see Owned.
	Addresses []string
	// Owned reports whether THIS node is responsible for the listener
	// (multinode ownership MVP, DESIGN-multinode-addresses.md §2/§3):
	// always true for invalid listeners (spec errors are ours to report
	// in status) and wildcard listeners (every node binds the wildcard);
	// false exactly when a non-wildcard listener's spec.addresses have an
	// empty intersection with the node fingerprint. Not-owned listeners
	// render nothing, get no listener status entry, and stay out of
	// conflict detection — another node owns them.
	Owned bool
	// SkippedAddresses records the pre-intersection bind intent of a
	// not-owned listener (Owned=false), for the ListenerSkippedOnNode
	// event message. Empty for owned listeners.
	SkippedAddresses []string
	// RawServerSnippet carries the Gateway's server-snippet annotation
	// through to the rendered server blocks (see GatewayInfo.RawServerSnippet).
	RawServerSnippet string
	// TLSCert is the deterministic cert filename ("" when TLS is absent or
	// unresolvable — the server is then emitted without ssl, §3.4).
	TLSCert string
	// CertSecretName is the referenced Secret's name (for Certificates()).
	CertSecretName string
	// CertSecretNamespace is the referenced Secret's namespace (differs
	// from the Gateway's namespace for permitted cross-namespace
	// certificateRefs, §3.5).
	CertSecretNamespace string
	CertData            []byte

	// Attachments are the routes attached to this listener, in deterministic
	// (namespace, name) route order.
	Attachments []*RouteAttachment
}

// RouteAttachment records one (route parentRef, listener) attachment.
type RouteAttachment struct {
	Route    *RouteInfo
	Parent   *RouteParentInfo
	Listener *ListenerInfo
}

// RouteInfo is one HTTPRoute with its per-parentRef resolution.
type RouteInfo struct {
	Resource *gatewayv1.HTTPRoute
	Parents  []*RouteParentInfo
	Rules    []*RuleInfo
}

// RouteParentInfo is the resolution of one spec.parentRefs[] entry.
type RouteParentInfo struct {
	Ref gatewayv1.ParentReference

	// Skip: no status is written for this parentRef (target Gateway belongs
	// to another controller — that controller owns its status).
	Skip bool

	Gateway  *GatewayInfo // nil when the Gateway could not be found
	Attached []*RouteAttachment
	Accepted bool
	Reason   string // standard v1 Accepted reason (or "Accepted")
	Message  string

	// Resolved* carry the route-wide backendRef resolution (GEP-1364).
	ResolvedRefs bool
	RefsReason   string // ResolvedRefs | RefNotPermitted | BackendNotFound | InvalidKind | UnsupportedValue
	RefsMsg      string
}

// RuleInfo is one HTTPRoute rule after feature filtering and backend
// resolution.
type RuleInfo struct {
	Index int
	// Valid rules are emitted. Invalid ("dropped") rules set PartiallyInvalid.
	Valid      bool
	InvalidMsg string
	// RawSnippet is the route's hng.victrid.dev/location-snippet value
	// (route-level: applies to every location generated from this rule),
	// populated only when --dangerously-allow-nginx-snippets is on
	// (DESIGN-multinode-addresses.md §5).
	RawSnippet string
	// Locations are nginx-ready location matchers ("= /x", "/x/", "/").
	Locations []string
	// Upstream is the rule's single-backend upstream: ns_svc_port or
	// StaticUpstreamName(500). For weighted multi-backend rules this is the
	// rule-private combined upstream (see Backends).
	Upstream string
	// Endpoints are the resolved EndpointSlice addresses for Upstream
	// (empty for static marker upstreams).
	Endpoints []contract.Endpoint
	// Backends are the rule's active (weight>0) backendRefs with their
	// weights. Length ≤ 1 means plain single-backend routing; > 1 means the
	// translator combines them into one weighted upstream.
	Backends []WeightedBackend
	// RequestMirrors are the rule's resolved RequestMirror filters (§3.3),
	// each with its normalized percentage and deterministic internal mirror
	// location path. Mirror backendRefs that failed to resolve are dropped
	// (route stays functional; the failure surfaces in ResolvedRefs per the
	// RequestMirror spec) and do not appear here.
	RequestMirrors []RequestMirror
	// Timeouts converted from HTTPRoute rule timeouts (nil = nginx defaults).
	Timeouts *contract.Timeouts

	// MatchCases holds each match's non-path constraints (method, headers,
	// query params). Matches are OR'd within a rule, so each match is a
	// separate dispatch case pointing at the rule's upstream.
	MatchCases [][]MatchConstraint
	// MatchPaths holds each match's nginx path specs (parallel to
	// MatchCases; a match without a path covers "/").
	MatchPaths [][]string
	// Constraints is the union of all matches' constraints (used for
	// capability checks, not dispatch).
	Constraints []MatchConstraint
	// PathPrefix is the single PathPrefix match value used with
	// ReplacePrefixMatch filters ("" otherwise; the CRD requires exactly one
	// PathPrefix match in that combination).
	PathPrefix string
	// RequestRedirect / URLRewrite / header-modifier filters (nil = absent).
	RequestRedirect        *gatewayv1.HTTPRequestRedirectFilter
	URLRewrite             *gatewayv1.HTTPURLRewriteFilter
	RequestHeaderModifier  *gatewayv1.HTTPHeaderFilter
	ResponseHeaderModifier *gatewayv1.HTTPHeaderFilter

	// refFailReason/Msg carry this rule's backendRef failure into the
	// route-wide ResolvedRefs aggregation (GEP-1364).
	refFailReason string
	refFailMsg    string
}

// WeightedBackend is one active backendRef of a rule with its weight.
type WeightedBackend struct {
	// Upstream is the backend's service upstream (ns_svc_port) or
	// StaticUpstreamName(500) on resolution failure.
	Upstream string
	// Weight is the backendRef weight (> 0).
	Weight int32
	// Endpoints are the resolved endpoints of this backend.
	Endpoints []contract.Endpoint
	// RequestHeaderModifier is the backendRef-level request header modifier
	// (BackendRequestHeaderModification, §3.3): applied only to requests
	// forwarded to THIS backend. Non-nil only on single-backend rules (the
	// multi-backend combined upstream cannot carry per-backend headers —
	// such rules are rejected in unsupportedRuleReason).
	RequestHeaderModifier *gatewayv1.HTTPHeaderFilter
}

// RequestMirror is one resolved RequestMirror filter (§3.3): the mirror
// target's upstream, its normalized percentage (fraction converted,
// unset = 100) and the deterministic internal mirror location path the
// translator renders for it.
type RequestMirror struct {
	// Upstream is the mirror target's service upstream (ns_svc_port).
	Upstream string
	// Path is the internal mirror location path (bare, no "="), deterministic
	// per (route, rule, target): "/hng_mirror_<hash>" (hng_ marker
	// convention).
	Path string
	// Percent is the normalized mirror share in [0, 100]. The translator
	// emits split_clients + a 204 gate strictly between 0 and 100; 100 (or
	// unset) mirrors unconditionally and 0 drops the mirror entirely.
	Percent float64
	// Endpoints are the resolved endpoints of the mirror target (reused
	// upstream generation; the RequestMirror backendRef carries no weight).
	Endpoints []contract.Endpoint
}

// MatchConstraint is one non-path HTTPRoute matcher, normalized for the
// translator's map-dispatch builder.
type MatchConstraint struct {
	// Kind is the constraint input: "method", "header" or "query".
	Kind string
	// Name is the header/query name ("" for method). Header names are
	// normalized to the nginx variable form (lowercase, '-' → '_').
	Name string
	// Type is "Exact" or "RegularExpression".
	Type string
	// Value is the expected value (regex body for RegularExpression).
	Value string
}

// ---------------------------------------------------------------------------
// Graph construction
// ---------------------------------------------------------------------------

// BuildGraph resolves a Resources snapshot into a Graph. It is pure:
// identical input always yields identical output (deterministic ordering
// throughout, required for the dataplane applied-hash no-op, DESIGN.md S6).
// Route attachment across namespaces is governed by the TARGET Gateway's
// per-listener allowedRoutes (Gateway API v1: allowedRoutes IS the
// authorization — no ReferenceGrant applies to Gateway-route attachment);
// ReferenceGrants gate only cross-namespace backendRefs and listener
// certificateRefs (§3.5). opts carries the danger flags gating the snippet
// / extra-file escape-hatch annotations (DESIGN-multinode-addresses.md §5).
func BuildGraph(res *Resources, opts GraphOptions) *Graph {
	g := &Graph{}

	// 1. Owned GatewayClasses.
	for _, gc := range res.GatewayClasses {
		if gc.Spec.ControllerName != ControllerName {
			continue
		}
		ci := &ClassInfo{Resource: gc, Accepted: true,
			AcceptedReason: string(gatewayv1.GatewayClassReasonAccepted)}
		if gc.Spec.ParametersRef != nil {
			// No parameters kind is supported (DESIGN.md §9 non-goal); a
			// parametersRef is therefore always unresolvable for us and the
			// class is rejected (Gateway API: Accepted=False/InvalidParameters).
			ci.Accepted = false
			ci.AcceptedReason = string(gatewayv1.GatewayClassReasonInvalidParameters)
			ci.Message = fmt.Sprintf(
				"spec.parametersRef %s/%s %q is not supported (no GatewayClass parameters are implemented)",
				string(gc.Spec.ParametersRef.Group), string(gc.Spec.ParametersRef.Kind),
				gc.Spec.ParametersRef.Name)
		}
		g.Classes = append(g.Classes, ci)
	}
	sort.Slice(g.Classes, func(i, j int) bool {
		return g.Classes[i].Resource.Name < g.Classes[j].Resource.Name
	})

	classByName := map[string]*ClassInfo{}
	for _, c := range g.Classes {
		classByName[c.Resource.Name] = c
	}

	// 2. Gateways owned through those classes. Gateways whose class is
	// missing or owned by another controller are not ours: no config, no
	// status (that controller — if any — owns them).
	for _, gw := range res.Gateways {
		info := &GatewayInfo{Resource: gw}
		if gw.Spec.Infrastructure != nil && gw.Spec.Infrastructure.ParametersRef != nil {
			// No infrastructure parameters are supported: the Gateway is
			// rejected (Gateway API: Accepted=False/InvalidParameters) and
			// gets no configuration, but its status is still ours to write.
			info.Rejected = true
			info.RejectReason = string(gatewayv1.GatewayReasonInvalidParameters)
			info.RejectMsg = fmt.Sprintf(
				"spec.infrastructure.parametersRef %s/%s %q is not supported (no infrastructure parameters are implemented)",
				gw.Spec.Infrastructure.ParametersRef.Group,
				gw.Spec.Infrastructure.ParametersRef.Kind,
				gw.Spec.Infrastructure.ParametersRef.Name)
		}
		class, ok := classByName[string(gw.Spec.GatewayClassName)]
		if !ok {
			continue
		}
		info.Class = class
		g.Gateways = append(g.Gateways, info)
	}
	sort.Slice(g.Gateways, func(i, j int) bool {
		a, b := g.Gateways[i].Resource, g.Gateways[j].Resource
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	slicesByService := indexEndpointSlices(res.EndpointSlices)
	secretsByKey := map[string]*corev1.Secret{}
	for _, s := range res.Secrets {
		secretsByKey[s.Namespace+"/"+s.Name] = s
	}
	configmapsByKey := map[string]*corev1.ConfigMap{}
	for _, cm := range res.ConfigMaps {
		configmapsByKey[cm.Namespace+"/"+cm.Name] = cm
	}
	servicesByKey := map[string]*corev1.Service{}
	for _, svc := range res.Services {
		servicesByKey[svc.Namespace+"/"+svc.Name] = svc
	}
	namespacesByName := map[string]*corev1.Namespace{}
	for _, ns := range res.Namespaces {
		namespacesByName[ns.Name] = ns
	}
	// ReferenceGrants are evaluated per TARGET namespace (the grant must
	// exist in the namespace of the referenced object, ReferenceGrant spec).
	grantsByNamespace := indexReferenceGrants(res.ReferenceGrants)

	// 2b. Per-Gateway escape-hatch annotations (§5a danger flags),
	// recorded as graph warnings — BuildGraph stays pure; the reconciler
	// logs them. (The listen-addresses / publish-addresses annotations
	// were REMOVED in v0.3.0: spec.addresses is the binding intent and
	// status.addresses derives from the rendered listens.)
	for _, gw := range g.Gateways {
		nsName := gw.Resource.Namespace + "/" + gw.Resource.Name
		if v := gw.Resource.Annotations[ServerSnippetAnnotation]; v != "" {
			if opts.AllowNginxSnippets {
				gw.RawServerSnippet = v
			} else {
				g.warn(fmt.Sprintf("Gateway %s: annotation %s is ignored (--dangerously-allow-nginx-snippets is off)",
					nsName, ServerSnippetAnnotation))
			}
		}
		if raw := gw.Resource.Annotations[ExtraFilesAnnotation]; raw != "" {
			if opts.AllowExtraFiles {
				resolveExtraFiles(gw, raw, configmapsByKey, secretsByKey, g.warn)
			} else {
				g.warn(fmt.Sprintf("Gateway %s: annotation %s is ignored (--dangerously-allow-extra-files is off)",
					nsName, ExtraFilesAnnotation))
			}
		}
	}

	// 3. Listener resolution.
	for _, gw := range g.Gateways {
		for i := range gw.Resource.Spec.Listeners {
			li := resolveListener(gw.Resource, i, secretsByKey, grantsByNamespace)
			li.Gateway = gw
			li.RawServerSnippet = gw.RawServerSnippet
			gw.Listeners = append(gw.Listeners, li)
		}
	}

	// 4. Node address filtering (DESIGN-multinode-addresses.md §2): each
	// listener's spec.addresses bind intent is intersected with this
	// node's fingerprint. Listeners with an empty intersection are not
	// owned here (another node serves them) — they render nothing, get
	// no status entry and stay out of the conflict machinery. A nil
	// fingerprint (probe unavailable, tests, single-node dev) owns
	// everything: the pre-v0.3.0 semantics.
	applyNodeFilter(g, opts.NodeAddresses)

	// 5. Cross-Gateway indistinct listeners: assign distinct per-Gateway
	// loopback bind addresses so the combined listener set (the controller
	// merges ALL Gateways into one nginx data plane) satisfies the Gateway
	// API "Distinct Listeners" rule via the address component of the
	// (address, port, hostname) tuple (DESIGN.md §3.2, §3.4).
	assignCrossGatewayAddresses(g)

	// 6. Conflict detection (§3.2) — valid, owned listeners only, over
	// the post-intersection effective bind sets (§4 of the multinode
	// design: listeners on different nodes with disjoint effective
	// addresses coexist legally). Same-Gateway indistinctness is always a
	// spec conflict; cross-Gateway pairs that remain indistinct after
	// address assignment (explicit overlapping spec.addresses) are marked
	// Conflicted on both sides.
	detectConflicts(g)

	// 7. HTTPRoutes in deterministic order so per-listener attachment (and
	// the resulting location ordering) is stable.
	routes := append([]*gatewayv1.HTTPRoute(nil), res.HTTPRoutes...)
	sort.Slice(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	gatewaysByKey := map[string]*GatewayInfo{}
	for _, gw := range g.Gateways {
		gatewaysByKey[gw.Resource.Namespace+"/"+gw.Resource.Name] = gw
	}
	for _, r := range routes {
		ri := &RouteInfo{Resource: r}
		if v := r.Annotations[LocationSnippetAnnotation]; v != "" && !opts.AllowNginxSnippets {
			g.warn(fmt.Sprintf("HTTPRoute %s/%s: annotation %s is ignored (--dangerously-allow-nginx-snippets is off)",
				r.Namespace, r.Name, LocationSnippetAnnotation))
		}
		resolveRoute(ri, gatewaysByKey, slicesByService, servicesByKey, namespacesByName, grantsByNamespace, opts)
		g.Routes = append(g.Routes, ri)
	}
	return g
}

// applyNodeFilter intersects every listener's spec.addresses bind intent
// with the node's address fingerprint (DESIGN-multinode-addresses.md §2):
//
//   - intersection non-empty → the listener binds ONLY the intersection
//     (li.Addresses becomes the effective, post-shard bind set);
//   - wildcard (no spec.addresses) → owned everywhere, binds unchanged;
//   - empty intersection → not owned by this node: no server block, no
//     listener status entry, excluded from conflict detection (another
//     node serves it). The original intent is kept in SkippedAddresses
//     for the ListenerSkippedOnNode event.
//
// Invalid listeners stay owned regardless of addresses: their spec error
// must surface in this node's status writes (and it is identical on every
// node, so the writes converge). A nil fingerprint owns everything
// (single-node semantics).
func applyNodeFilter(g *Graph, owned *nodeaddrs.Set) {
	if owned == nil {
		return
	}
	for _, gw := range g.Gateways {
		for _, li := range gw.Listeners {
			if !li.Valid || len(li.Addresses) == 0 {
				continue // invalid = ours to report; wildcard = owned everywhere
			}
			var keep []string
			for _, a := range li.Addresses {
				if owned.Contains(a) {
					keep = append(keep, a)
				}
			}
			if len(keep) == 0 {
				li.Owned = false
				li.SkippedAddresses = append([]string(nil), li.Addresses...)
			}
			li.Addresses = keep
		}
	}
}

// assignCrossGatewayAddresses gives Gateways whose listeners are indistinct
// from another Gateway's listeners (same port, overlapping hostnames,
// overlapping bind sets) distinct loopback bind addresses.
//
// Gateway API v1 (GatewaySpec doc, "Distinct Listeners"): the distinctness
// rules are defined over "a set of Listeners" rather than "Listeners in a
// single Gateway" BECAUSE "implementations MAY merge configuration from
// multiple Gateways onto a single data plane, and these rules also apply in
// that case". This controller merges every Gateway into one nginx instance,
// so the address becomes part of the distinctness tuple: each Gateway in an
// indistinct group receives its own 127.0.0.N bind (reported in
// status.addresses), which is exactly the per-Gateway address model the
// spec presumes.
//
// Assignment is deterministic AND stable: candidates come from a hash of
// the Gateway's namespace/name probed forward through the pool, so adding
// or removing an unrelated Gateway never shifts another Gateway's address
// mid-flight.
func assignCrossGatewayAddresses(g *Graph) {
	// Collect gateways that share an indistinct cross-Gateway listener pair.
	need := map[*GatewayInfo]struct{}{}
	for i := 0; i < len(g.Gateways); i++ {
		for j := i + 1; j < len(g.Gateways); j++ {
			ga, gb := g.Gateways[i], g.Gateways[j]
			if gatewaysIndistinct(ga, gb) {
				need[ga], need[gb] = struct{}{}, struct{}{}
			}
		}
	}
	if len(need) == 0 {
		return
	}

	// Gateway-level processing order: namespace/name.
	ordered := make([]*GatewayInfo, 0, len(need))
	for gw := range need {
		ordered = append(ordered, gw)
	}
	sort.Slice(ordered, func(a, b int) bool {
		x, y := ordered[a].Resource, ordered[b].Resource
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		return x.Name < y.Name
	})

	taken := map[string]struct{}{}
	for _, gw := range g.Gateways {
		for _, li := range gw.Listeners {
			if !li.Valid || !li.Owned {
				continue // not-owned listeners bind nothing here
			}
			for addr := range bindSet(li) {
				taken[addr] = struct{}{}
			}
		}
	}

	for _, gw := range ordered {
		// Explicit per-listener binds (spec.addresses) win; a Gateway
		// keeps them and only wildcard-bound Gateways are moved.
		annotated := false
		wildcardListener := false
		for _, li := range gw.Listeners {
			if !li.Valid || !li.Owned {
				continue
			}
			if len(li.Addresses) > 0 {
				annotated = true
			} else {
				wildcardListener = true
			}
		}
		if annotated || !wildcardListener {
			continue
		}
		addr := pickLoopback(gw.Resource.Namespace+"/"+gw.Resource.Name, taken)
		if addr == "" {
			continue // pool exhausted: leave unassigned; §3.2 marks the conflict
		}
		taken[addr] = struct{}{}
		gw.AutoAddress = addr
		for _, li := range gw.Listeners {
			if !li.Valid || !li.Owned || len(li.Addresses) > 0 {
				continue
			}
			li.Addresses = []string{addr}
		}
	}
}

// gatewaysIndistinct reports whether any valid, owned listener pair across
// the two Gateways is indistinct: same port, equivalent hostnames (equal
// or both empty), overlapping effective bind sets (i.e. they would fight
// over the same socket on THIS node).
func gatewaysIndistinct(a, b *GatewayInfo) bool {
	for _, la := range a.Listeners {
		if !la.Valid || !la.Owned {
			continue
		}
		for _, lb := range b.Listeners {
			if !lb.Valid || !lb.Owned {
				continue
			}
			if la.Spec.Port != lb.Spec.Port {
				continue
			}
			if !bindAddressesOverlap(la, lb) {
				continue
			}
			if !hostnamesEquivalent(la.Spec.Hostname, lb.Spec.Hostname) {
				continue
			}
			return true
		}
	}
	return false
}

// pickLoopback deterministically selects a free 127.0.0.N address for a
// Gateway. The first candidate is a hash of the gateway key; collisions are
// probed forward through the pool in gateway-key order (the caller iterates
// gateways sorted), so results are stable under unrelated changes.
func pickLoopback(gatewayKey string, taken map[string]struct{}) string {
	h := fnv32a(gatewayKey)
	for off := 0; off < autoAssignSpan; off++ {
		n := autoAssignBase + int((h+uint32(off))%autoAssignSpan)
		addr := fmt.Sprintf("127.0.0.%d", n)
		if _, used := taken[addr]; !used {
			return addr
		}
	}
	return ""
}

func fnv32a(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// indexReferenceGrants groups ReferenceGrants by their own namespace — the
// namespace whose objects they permit referencing INTO (ReferenceGrant spec:
// a grant applies to references to resources in ITS namespace).
func indexReferenceGrants(grants []*gatewayv1.ReferenceGrant) map[string][]*gatewayv1.ReferenceGrant {
	out := map[string][]*gatewayv1.ReferenceGrant{}
	for _, g := range grants {
		if g == nil {
			continue
		}
		out[g.Namespace] = append(out[g.Namespace], g)
	}
	return out
}

// referenceGrantPermits reports whether the grants living in the TARGET
// namespace permit one cross-namespace reference from (fromGroup, fromKind,
// fromNamespace) to (toGroup, toKind, toName). Per the ReferenceGrant spec,
// permission requires ONE grant whose spec.from contains a matching entry
// AND whose spec.to contains a matching entry — from entries OR among
// themselves, to entries OR among themselves, but a matching from and to
// must come from the SAME grant (each grant represents one trust
// relationship). An empty/absent to.name constrains to nothing (all names
// of the group/kind); a set to.name permits exactly that name.
// Group "" and "core" both denote the Kubernetes core API group.
func referenceGrantPermits(grants []*gatewayv1.ReferenceGrant,
	fromGroup, fromKind, fromNamespace, toGroup, toKind, toName string) bool {
	fg, tg := normAPIGroup(fromGroup), normAPIGroup(toGroup)
	for _, g := range grants {
		fromOK := false
		for _, f := range g.Spec.From {
			if normAPIGroup(string(f.Group)) == fg && string(f.Kind) == fromKind &&
				string(f.Namespace) == fromNamespace {
				fromOK = true
				break
			}
		}
		if !fromOK {
			continue
		}
		for _, to := range g.Spec.To {
			if normAPIGroup(string(to.Group)) != tg || string(to.Kind) != toKind {
				continue
			}
			if to.Name != nil && string(*to.Name) != toName {
				continue
			}
			return true
		}
	}
	return false
}

// normAPIGroup normalises the core API group spellings ("" and "core") to
// "" so grant and reference groups compare equal regardless of form.
func normAPIGroup(g string) string {
	if g == "core" {
		return ""
	}
	return g
}

// indexEndpointSlices maps "namespace/service" to its slices (label
// kubernetes.io/service-name, DESIGN.md §3.3 / S3).
func indexEndpointSlices(slices []*discoveryv1.EndpointSlice) map[string][]*discoveryv1.EndpointSlice {
	out := map[string][]*discoveryv1.EndpointSlice{}
	for _, s := range slices {
		svc, ok := s.Labels[discoveryv1.LabelServiceName]
		if !ok || svc == "" {
			continue
		}
		out[s.Namespace+"/"+svc] = append(out[s.Namespace+"/"+svc], s)
	}
	return out
}

// ---------------------------------------------------------------------------
// Listener resolution
// ---------------------------------------------------------------------------

func resolveListener(gw *gatewayv1.Gateway, index int, secrets map[string]*corev1.Secret,
	grants map[string][]*gatewayv1.ReferenceGrant) *ListenerInfo {
	src := gw.Spec.Listeners[index]
	li := &ListenerInfo{
		Spec:         src,
		Index:        index,
		Valid:        true,
		ResolvedRefs: true,
		RefsReason:   string(gatewayv1.ListenerReasonResolvedRefs),
		Addresses:    gatewayBindAddresses(gw.Spec.Addresses),
		Owned:        true,
	}
	invalid := func(reason, msg string) *ListenerInfo {
		li.Valid, li.InvalidReason, li.InvalidMsg = false, reason, msg
		return li
	}

	// Port is required in v1 (S1): a zero value means it was omitted (the Go
	// type is a bare int32, so "absent" deserialises as 0).
	if src.Port == 0 {
		return invalid(string(gatewayv1.ListenerReasonInvalid),
			"listener port is required (Gateway API v1, DESIGN.md S1)")
	}

	// spec.addresses is binding intent (DESIGN-multinode-addresses.md §2):
	// only IPAddress values are bindable. Invalid or non-bindable entries
	// MUST surface in the listener conditions (Gateway API spec: "invalid
	// or unavailable addresses ... indicate ... in conditions") — never a
	// silent ignore.
	if msg := invalidGatewayAddresses(gw.Spec.Addresses); msg != "" {
		return invalid(string(gatewayv1.ListenerReasonInvalid), msg)
	}

	switch src.Protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType, gatewayv1.TLSProtocolType:
	default:
		return invalid(string(gatewayv1.ListenerReasonUnsupportedProtocol),
			fmt.Sprintf("protocol %q is not supported (supported: HTTP, HTTPS, TLS)", src.Protocol))
	}

	// allowedRoutes.kinds analysis: SupportedKinds is the intersection of the
	// requested kinds with the kinds this listener can actually accept. Any
	// unknown/unsupported kind in the list sets ResolvedRefs=False
	// (InvalidRouteKinds) — the listener itself stays valid.
	if src.Protocol == gatewayv1.HTTPProtocolType || src.Protocol == gatewayv1.HTTPSProtocolType {
		grp := gatewayv1.Group("gateway.networking.k8s.io")
		supported := []gatewayv1.RouteGroupKind{{Group: &grp, Kind: "HTTPRoute"}}
		if kinds := routeKinds(src); len(kinds) > 0 {
			requested, invalid := filterRouteKinds(kinds, supported)
			li.SupportedKinds = requested
			if invalid {
				li.ResolvedRefs = false
				li.RefsReason = string(gatewayv1.ListenerReasonInvalidRouteKinds)
				li.RefsMsg = "allowedRoutes.kinds contains kind(s) not supported by this listener (supported: HTTPRoute)"
			}
		} else {
			li.SupportedKinds = supported
		}
	}

	if src.Protocol == gatewayv1.HTTPProtocolType {
		if src.TLS != nil {
			return invalid(string(gatewayv1.ListenerReasonInvalid),
				"tls must not be set for HTTP protocol listeners")
		}
		return li // plain HTTP listener
	}

	// HTTPS / TLS listeners require a TLS section (Terminate mode only).
	if src.TLS == nil {
		return invalid(string(gatewayv1.ListenerReasonInvalid),
			fmt.Sprintf("tls configuration is required for %s protocol listeners", src.Protocol))
	}
	mode := gatewayv1.TLSModeTerminate
	if src.TLS.Mode != nil {
		mode = *src.TLS.Mode
	}
	if mode != gatewayv1.TLSModeTerminate {
		return invalid(string(gatewayv1.ListenerReasonUnsupportedProtocol),
			fmt.Sprintf("tls mode %q is not supported (only Terminate)", mode))
	}

	// certificateRefs: same-namespace Secrets only (§3.5).
	if n := len(src.TLS.CertificateRefs); n == 0 {
		li.ResolvedRefs, li.RefsReason, li.RefsMsg =
			false, string(gatewayv1.ListenerReasonInvalidCertificateRef),
			"tls.certificateRefs must specify at least one Secret"
		li.CertFailed = true
		return li
	} else if n > 1 {
		li.RefsMsg = fmt.Sprintf("only the first of %d certificateRefs is used", n)
	}
	ref := src.TLS.CertificateRefs[0]
	if kind := string(deref(ref.Kind, "Secret")); kind != "Secret" {
		li.ResolvedRefs, li.RefsReason, li.RefsMsg = false,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("certificateRef kind must be Secret, got %q", kind)
		li.CertFailed = true
		return li
	}
	if group := deref(ref.Group, ""); group != "" && group != "core" {
		li.ResolvedRefs, li.RefsReason, li.RefsMsg = false,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("certificateRef group must be core (\"\"), got %q", group)
		return li
	}
	if ref.Namespace != nil && string(*ref.Namespace) != gw.Namespace {
		// §3.5 / ReferenceGrant spec: cross-namespace certificateRefs are
		// permitted only by a ReferenceGrant in the TARGET namespace whose
		// from matches the (Gateway, its namespace) and whose to matches
		// the core/Secret reference (to.name constrains when set).
		if !referenceGrantPermits(grants[string(*ref.Namespace)],
			"gateway.networking.k8s.io", "Gateway", gw.Namespace,
			"", "Secret", string(ref.Name)) {
			li.ResolvedRefs, li.RefsReason, li.RefsMsg = false,
				string(gatewayv1.ListenerReasonRefNotPermitted),
				fmt.Sprintf("cross-namespace certificateRef %s/%s is not permitted (no matching ReferenceGrant in %s)",
					*ref.Namespace, ref.Name, *ref.Namespace)
			li.CertFailed = true
			return li
		}
	}
	// The Secret is looked up in the REFERENCE's namespace: same-namespace
	// refs use the Gateway's namespace; cross-namespace refs (permitted by
	// a ReferenceGrant above) resolve in the target namespace (§3.5).
	certNS := gw.Namespace
	if ref.Namespace != nil && string(*ref.Namespace) != "" {
		certNS = string(*ref.Namespace)
	}
	secret, ok := secrets[certNS+"/"+string(ref.Name)]
	if !ok {
		li.ResolvedRefs, li.RefsReason, li.RefsMsg = false,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("certificate Secret %s/%s not found (no server configuration generated)",
				certNS, ref.Name)
		li.CertFailed = true
		return li
	}
	pem, msg := tlsPEM(secret)
	if msg != "" {
		li.ResolvedRefs, li.RefsReason, li.RefsMsg = false,
			string(gatewayv1.ListenerReasonInvalidCertificateRef), msg
		// Malformed-but-present PEM keeps flowing to the dataplane so
		// nginx -t rejects it (Programmed=False/Invalid, §3.4 rollback);
		// truly absent material leaves CertData nil (server suppressed).
		if pem != nil {
			li.TLSCert = secret.Namespace + "_" + secret.Name + ".pem"
			li.CertSecretNamespace = secret.Namespace
			li.CertSecretName = secret.Name
			li.CertData = pem
		} else {
			li.CertFailed = true
		}
		return li
	}
	li.TLSCert = secret.Namespace + "_" + secret.Name + ".pem"
	li.CertSecretNamespace = secret.Namespace
	li.CertSecretName = secret.Name
	li.CertData = pem
	return li
}

// routeKinds extracts allowedRoutes.kinds (nil when unset).
func routeKinds(l gatewayv1.Listener) []gatewayv1.RouteGroupKind {
	if l.AllowedRoutes == nil {
		return nil
	}
	return l.AllowedRoutes.Kinds
}

// filterRouteKinds intersects requested kinds with the supported set. It
// returns the requested entries that are supported and whether any entry was
// unknown/unsupported (InvalidRouteKinds).
func filterRouteKinds(requested, supported []gatewayv1.RouteGroupKind) (kept []gatewayv1.RouteGroupKind, sawInvalid bool) {
	for _, k := range requested {
		g := "gateway.networking.k8s.io"
		if k.Group != nil && *k.Group != "" {
			g = string(*k.Group)
		}
		ok := false
		for _, s := range supported {
			sg := "gateway.networking.k8s.io"
			if s.Group != nil && *s.Group != "" {
				sg = string(*s.Group)
			}
			if sg == g && strings.EqualFold(string(k.Kind), string(s.Kind)) {
				ok = true
				break
			}
		}
		if ok {
			kept = append(kept, k)
		} else {
			sawInvalid = true
		}
	}
	return kept, sawInvalid
}

// tlsPEM validates a TLS Secret and returns the concatenated crt+key PEM.
func tlsPEM(s *corev1.Secret) ([]byte, string) {
	if s.Type != TLSSecretType {
		return nil, fmt.Sprintf("certificate Secret %s/%s has type %q, want %q",
			s.Namespace, s.Name, s.Type, TLSSecretType)
	}
	crt, hasCrt := s.Data["tls.crt"]
	key, hasKey := s.Data["tls.key"]
	if !hasCrt || !hasKey || len(crt) == 0 || len(key) == 0 {
		return nil, fmt.Sprintf("certificate Secret %s/%s is missing tls.crt/tls.key data",
			s.Namespace, s.Name)
	}
	out := make([]byte, 0, len(crt)+1+len(key))
	out = append(out, crt...)
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, key...)
	if !looksLikePEMBlock(crt, "CERTIFICATE") || !looksLikeKeyPEM(key) {
		// Return the raw bytes anyway: the dataplane writes the cert and
		// nginx -t rejects it (loud failure path, DESIGN.md §3.4), while
		// ResolvedRefs=False/InvalidCertificateRef is reported here.
		return out, fmt.Sprintf("certificate Secret %s/%s does not contain valid CERTIFICATE / PRIVATE KEY PEM blocks",
			s.Namespace, s.Name)
	}
	return out, ""
}

// looksLikePEMBlock reports whether data contains a PEM BEGIN/END fence for
// the given block type (cheap sanity, not a full parse).
func looksLikePEMBlock(data []byte, blockType string) bool {
	begin := "-----BEGIN " + blockType + "-----"
	end := "-----END " + blockType + "-----"
	return bytes.Contains(data, []byte(begin)) && bytes.Contains(data, []byte(end))
}

// looksLikeKeyPEM accepts any of the PRIVATE KEY PEM variants (RSA / EC /
// PKCS8 / encrypted).
func looksLikeKeyPEM(data []byte) bool {
	return bytes.Contains(data, []byte("-----BEGIN")) &&
		bytes.Contains(data, []byte("PRIVATE KEY-----"))
}

// gatewayBindAddresses converts the Gateway's spec.addresses into bind
// addresses in nginx listen form (v4 plain, v6 bracketed), deduplicated
// in spec order. Entries that are not type IPAddress / not parseable IPs
// are DROPPED here — the listener-level error is produced separately by
// invalidGatewayAddresses so every listener of the Gateway carries the
// same condition. A nil/empty result means the wildcard bind.
func gatewayBindAddresses(specAddrs []gatewayv1.GatewaySpecAddress) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, a := range specAddrs {
		if t := deref(a.Type, gatewayv1.IPAddressType); t != gatewayv1.IPAddressType {
			continue
		}
		form := nodeaddrs.RenderBindForm(a.Value)
		if form == "" {
			continue
		}
		if _, dup := seen[form]; dup {
			continue
		}
		seen[form] = struct{}{}
		out = append(out, form)
	}
	return out
}

// invalidGatewayAddresses returns the listener-invalidating message for
// the Gateway's spec.addresses ("" when every entry is a bindable
// IPAddress). Only IPAddress is bindable (DESIGN-multinode-addresses.md
// §5: "仅 IPAddress 类型可作绑定意图；Hostname 类型不可绑定，按 GWA Spec
// 要求在 GatewayStatus.Conditions 中报告无效").
func invalidGatewayAddresses(specAddrs []gatewayv1.GatewaySpecAddress) string {
	for i := range specAddrs {
		a := &specAddrs[i]
		t := gatewayv1.IPAddressType
		if a.Type != nil {
			t = *a.Type
		}
		if t != gatewayv1.IPAddressType {
			return fmt.Sprintf(
				"spec.addresses[%d] (%s) has type %q; only IPAddress is bindable — set spec.addresses entries to type IPAddress to pin this Gateway to node addresses",
				i, a.Value, t)
		}
		if nodeaddrs.RenderBindForm(a.Value) == "" {
			return fmt.Sprintf("spec.addresses[%d] value %q is not a valid IP address", i, a.Value)
		}
	}
	return ""
}

// resolveExtraFiles resolves the hng.victrid.dev/extra-files annotation
// (DESIGN-multinode-addresses.md §5) into GatewayInfo.ExtraFiles entries.
// Refs are "configmap:ns/name" / "secret:ns/name", comma-separated;
// a ref without a namespace part defaults to the Gateway's namespace.
//
// Policy (flag-gated escape hatch, not a spec path — keep it simple):
//   - cross-namespace refs are DENIED (no ReferenceGrant machinery): a ref
//     whose namespace differs from the Gateway's is skipped with a warning;
//   - missing / wrong-kind / malformed refs are skipped with a warning;
//   - each surviving ref contributes one entry per data key
//     (ConfigMap data + binaryData, Secret data), keyed by
//     "files/<ns>_<name>/<key>"; keys containing "/" or equal to "." / ".."
//     are skipped (defense in depth — the API server already restricts
//     ConfigMap/Secret keys to [-._a-zA-Z0-9]+).
//
// Determinism: refs resolve in annotation order, keys in lexical order;
// duplicate paths (same ref listed twice) collapse to the first entry.
func resolveExtraFiles(gw *GatewayInfo, raw string,
	configmaps map[string]*corev1.ConfigMap, secrets map[string]*corev1.Secret,
	warn func(string)) {
	nsName := gw.Resource.Namespace + "/" + gw.Resource.Name
	seen := map[string]struct{}{}
	addKey := func(refNS, refName, key string, content []byte) {
		rel := "files/" + refNS + "_" + refName + "/" + key
		if _, dup := seen[rel]; dup {
			return
		}
		seen[rel] = struct{}{}
		gw.ExtraFiles = append(gw.ExtraFiles, ExtraFileEntry{RelPath: rel, Content: content})
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, ref, ok := strings.Cut(part, ":")
		if !ok || ref == "" {
			warn(fmt.Sprintf("Gateway %s: extra-files ref %q is malformed (want configmap:ns/name or secret:ns/name); skipped", nsName, part))
			continue
		}
		refNS, name := gw.Resource.Namespace, ref
		if r, rest, hasNS := strings.Cut(ref, "/"); hasNS {
			refNS, name = r, strings.TrimSpace(rest)
		}
		if refNS != gw.Resource.Namespace {
			warn(fmt.Sprintf("Gateway %s: extra-files ref %q is cross-namespace; only refs in %s are allowed; skipped", nsName, part, gw.Resource.Namespace))
			continue
		}
		if name == "" {
			warn(fmt.Sprintf("Gateway %s: extra-files ref %q has an empty name; skipped", nsName, part))
			continue
		}
		var keys []string
		contentOf := func(key string) ([]byte, bool) { return nil, false }
		switch kind {
		case "configmap":
			cm, found := configmaps[refNS+"/"+name]
			if !found {
				warn(fmt.Sprintf("Gateway %s: extra-files ref ConfigMap %s/%s not found; skipped", nsName, refNS, name))
				continue
			}
			for k := range cm.Data {
				keys = append(keys, k)
			}
			for k := range cm.BinaryData {
				if _, dup := cm.Data[k]; !dup {
					keys = append(keys, k)
				}
			}
			contentOf = func(key string) ([]byte, bool) {
				if v, ok := cm.Data[key]; ok {
					return []byte(v), true
				}
				v, ok := cm.BinaryData[key]
				return v, ok
			}
		case "secret":
			s, found := secrets[refNS+"/"+name]
			if !found {
				warn(fmt.Sprintf("Gateway %s: extra-files ref Secret %s/%s not found; skipped", nsName, refNS, name))
				continue
			}
			for k := range s.Data {
				keys = append(keys, k)
			}
			contentOf = func(key string) ([]byte, bool) {
				v, ok := s.Data[key]
				return v, ok
			}
		default:
			warn(fmt.Sprintf("Gateway %s: extra-files ref %q has unsupported kind %q (want configmap or secret); skipped", nsName, part, kind))
			continue
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.ContainsAny(k, "/\\\x00") || k == "." || k == ".." {
				warn(fmt.Sprintf("Gateway %s: extra-files key %q of %s %s/%s cannot be a path segment; skipped", nsName, k, kind, refNS, name))
				continue
			}
			if content, ok := contentOf(k); ok {
				addKey(refNS, name, k, content)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Conflict detection (§3.2)
// ---------------------------------------------------------------------------

// detectConflicts marks BOTH listeners of every conflicting pair; no
// deterministic winner is chosen (B2: no name-based tie-breaking, avoid
// silently overriding user intent). Only valid, OWNED listeners
// participate: not-owned listeners bind nothing on this node, so their
// would-be conflicts belong to the node that owns them.
func detectConflicts(g *Graph) {
	// Same Gateway: overlapping port+hostname (DESIGN.md §3.2).
	for _, gw := range g.Gateways {
		ls := gw.Listeners
		for i := 0; i < len(ls); i++ {
			for j := i + 1; j < len(ls); j++ {
				if !ls[i].Owned || !ls[j].Owned {
					continue
				}
				markConflict(ls[i], ls[j], sameGateway)
			}
		}
	}
	// Cross Gateway: overlapping address:port + server_name. server_name is
	// proxied by the listener hostname for the MVP (documented limitation:
	// a conflict is reported even when no route realises the hostname).
	for i := 0; i < len(g.Gateways); i++ {
		for j := i + 1; j < len(g.Gateways); j++ {
			for _, la := range g.Gateways[i].Listeners {
				for _, lb := range g.Gateways[j].Listeners {
					if !la.Owned || !lb.Owned {
						continue
					}
					markConflict(la, lb, crossGateway)
				}
			}
		}
	}
}

type conflictScope int

const (
	sameGateway conflictScope = iota
	crossGateway
)

func markConflict(a, b *ListenerInfo, scope conflictScope) {
	if !a.Valid || !b.Valid || !a.Owned || !b.Owned || a.Conflicted || b.Conflicted {
		return
	}
	if a.Spec.Port != b.Spec.Port {
		return
	}
	if scope == crossGateway && !bindAddressesOverlap(a, b) {
		return // disjoint listen addresses (§3.1 annotation): both may bind
	}
	if !hostnamesEquivalent(a.Spec.Hostname, b.Spec.Hostname) {
		return
	}
	what := "hostname"
	reason := string(gatewayv1.ListenerReasonHostnameConflict)
	if a.Spec.Protocol != b.Spec.Protocol {
		what = "protocol and hostname"
		reason = string(gatewayv1.ListenerReasonProtocolConflict)
	}
	a.Conflicted, a.ConflictReason = true, reason
	a.ConflictMsg = fmt.Sprintf("%s conflict on port %d with listener %q of Gateway %s/%s",
		what, a.Spec.Port, b.Spec.Name, b.Gateway.Resource.Namespace, b.Gateway.Resource.Name)
	b.Conflicted, b.ConflictReason = true, reason
	b.ConflictMsg = fmt.Sprintf("%s conflict on port %d with listener %q of Gateway %s/%s",
		what, b.Spec.Port, a.Spec.Name, a.Gateway.Resource.Namespace, a.Gateway.Resource.Name)
}

// bindAddressesOverlap reports whether two listeners' bind address sets
// intersect. The bare `listen <port>` form binds all addresses, so a set
// containing the wildcard (always present unless overridden) overlaps all.
func bindAddressesOverlap(a, b *ListenerInfo) bool {
	as, bs := bindSet(a), bindSet(b)
	for x := range as {
		if _, ok := bs[x]; ok {
			return true
		}
	}
	return false
}

// bindSet normalises the listener's EFFECTIVE bind addresses; "" is the
// wildcard. A listener without spec.addresses binds all addresses (the
// bare `listen <port>` line — the auto-assignment path writes its
// loopback into Addresses the same way). With spec.addresses, ONLY those
// addresses (post node-fingerprint intersection) are bound — this is
// what makes the cross-Gateway "address:port" conflict rule (§3.2) and
// the multinode disjoint-address coexistence meaningful: two listeners
// with disjoint bind sets may share a port.
func bindSet(l *ListenerInfo) map[string]struct{} {
	out := map[string]struct{}{}
	add := func(addr string) {
		addr = normalizeAddress(addr)
		if addr == "0.0.0.0" || addr == "::" {
			addr = ""
		}
		out[addr] = struct{}{}
	}
	if len(l.Addresses) == 0 {
		add("") // the default bare `listen <port>` line
		return out
	}
	for _, a := range l.Addresses {
		add(a)
	}
	return out
}

func normalizeAddress(a string) string {
	a = strings.TrimSpace(a)
	if strings.HasPrefix(a, "[") && strings.HasSuffix(a, "]") {
		a = a[1 : len(a)-1]
	}
	if ip := net.ParseIP(a); ip != nil {
		return ip.String()
	}
	return a
}

// ---------------------------------------------------------------------------
// Hostname matching (§3.3 / S4)
// ---------------------------------------------------------------------------

// hostnamesEquivalent reports whether two listener hostnames make claims
// that cannot be told apart by request matching: both unset (each claims
// every host) or equal after normalisation. Overlapping-but-distinct
// claims (exact vs covering wildcard; wildcards of different specificity)
// do NOT conflict — Gateway API v1 processes request hostnames from most
// to least specific, and nginx natively dispatches by most-specific
// server_name.
func hostnamesEquivalent(a, b *gatewayv1.Hostname) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return normHostname(string(*a)) == normHostname(string(*b))
}

// hostnamesOverlap reports whether two listener hostnames can match a common
// request Host (Gateway API Hostname matching: wildcard = suffix match). A
// nil hostname ("all hostnames") overlaps only another nil — a wildcard
// listener and a hostname'd listener on the same port coexist and nginx
// dispatches to the more specific server_name; identical or overlapping
// hostnames still conflict on both sides (B2: no deterministic tie-break).
func hostnamesOverlap(a, b *gatewayv1.Hostname) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	as, bs := normHostname(string(*a)), normHostname(string(*b))
	return hostnameMatches(as, bs) || hostnameMatches(bs, as)
}

// hostnameMatches reports whether name matches pattern per Gateway API
// Hostname semantics (Gateway API v1, HTTPRoute spec.hostnames doc): a
// wildcard label is a SUFFIX match on the remaining labels — `*.example.com`
// matches `test.example.com` and `foo.test.example.com` (any number of
// labels) but not the apex `example.com` itself. A specific pattern matches
// only the identical name.
func hostnameMatches(pattern, name string) bool {
	p, h := normHostname(pattern), normHostname(name)
	if p == h {
		return true
	}
	if strings.HasPrefix(p, "*.") {
		return strings.HasSuffix(h, p[1:]) // ".example.com"
	}
	return false
}

func normHostname(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// hostnameIntersection returns the route hostnames accepted by the listener.
// A nil result means "no intersection" (route not attached to that listener,
// S4). A non-nil result always has at least one element; "" is the catch-all
// marker used when either side is wildcard/empty.
//
// Intersection semantics (Gateway API v1, HTTPRoute spec.hostnames):
//   - listener hostname nil: every route hostname is kept verbatim;
//   - route hostnames nil: the route inherits the listener hostname;
//   - otherwise a route hostname h intersects listener hostname l when
//     h == l, the listener wildcard covers h (l=*.example.com matches
//     test.example.com and foo.test.example.com), or the route wildcard's
//     covered domain contains the listener hostname (route *.example.com
//     intersects listeners example.com and test.example.com, and the
//     narrower listener wildcard *.test.example.com — spec: "a Listener
//     with test.example.com ... matches HTTPRoutes that ... specified
//     *.example.com"). Non-matching route hostnames are ignored (spec:
//     "must not be considered for a match").
func hostnameIntersection(routeHostnames []gatewayv1.Hostname, listener *gatewayv1.Hostname) []gatewayv1.Hostname {
	if listener == nil {
		if len(routeHostnames) == 0 {
			return []gatewayv1.Hostname{""}
		}
		return routeHostnames
	}
	if len(routeHostnames) == 0 {
		return []gatewayv1.Hostname{*listener} // catch-all route adopts the listener hostname
	}
	l := normHostname(string(*listener))
	var out []gatewayv1.Hostname
	for _, h := range routeHostnames {
		hn := normHostname(string(h))
		if hostnameMatches(l, hn) { // listener (wildcard or exact) covers the route hostname
			out = append(out, h)
			continue
		}
		if strings.HasPrefix(hn, "*.") {
			base := strings.TrimPrefix(hn, "*.")
			// The route wildcard *.P intersects the listener when the
			// listener hostname lies within P's coverage: L == P, L is a
			// subdomain of P (exact listener), or L's own wildcard coverage
			// sits inside P (wildcard listener narrower than the route).
			// The route's own wildcard is the effective dispatch hostname.
			if l == base || strings.HasSuffix(l, "."+base) || hostnameMatches(l, base) {
				out = append(out, h)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Route resolution
// ---------------------------------------------------------------------------

func resolveRoute(ri *RouteInfo, gateways map[string]*GatewayInfo,
	slices map[string][]*discoveryv1.EndpointSlice, services map[string]*corev1.Service,
	namespaces map[string]*corev1.Namespace, grants map[string][]*gatewayv1.ReferenceGrant,
	opts GraphOptions) {
	route := ri.Resource

	// Rules first: the route-wide backendRef resolution (GEP-1364) feeds
	// every parent status. First failure in spec order wins.
	var (
		refFail           bool
		refReason, refMsg string
	)
	for i := range route.Spec.Rules {
		rule := resolveRule(i, route, slices, services, grants)
		ri.Rules = append(ri.Rules, rule)
		if rule.refFailReason != "" && !refFail {
			refFail, refReason, refMsg = true, rule.refFailReason, rule.refFailMsg
		}
	}

	// Route-level escape-hatch annotation (DESIGN-multinode-addresses.md
	// §5): applies to every location generated from this route's rules.
	// Flag-gated; the ignored-with-warning case is recorded by BuildGraph.
	if opts.AllowNginxSnippets {
		for _, rule := range ri.Rules {
			rule.RawSnippet = route.Annotations[LocationSnippetAnnotation]
		}
	}
	refsOK, refsReason, refsMsg := true, string(gatewayv1.RouteReasonResolvedRefs), ""
	if refFail {
		refsOK, refsReason, refsMsg = false, refReason, refMsg
	}

	// parentRefs in spec order.
	for _, ref := range route.Spec.ParentRefs {
		pi := resolveParentRef(ri, ref, gateways, namespaces)
		pi.ResolvedRefs, pi.RefsReason, pi.RefsMsg = refsOK, refsReason, refsMsg
		ri.Parents = append(ri.Parents, pi)

		if pi.Skip || !pi.Accepted {
			continue
		}
		// Accepted=True requires at least one implementable rule ("A Route
		// MUST be considered Accepted if at least one of its Rules is
		// implemented" — RouteParentStatus docs).
		anyValid := false
		for _, rule := range ri.Rules {
			if rule.Valid {
				anyValid = true
				break
			}
		}
		if !anyValid {
			pi.Accepted = false
			pi.Reason = string(gatewayv1.RouteReasonUnsupportedValue)
			pi.Message = "no implementable rules: every rule uses unsupported features (unsupported filter types, regex paths, backendRef-level filters, sessionPersistence) or has no active backendRefs"
			continue
		}
		// Register attachments on the listeners (routes arrive in
		// deterministic order, so attachment order is stable).
		for _, att := range pi.Attached {
			att.Listener.Attachments = append(att.Listener.Attachments, att)
		}
	}
}

// resolveParentRef resolves one parentRef against the owned Gateways.
func resolveParentRef(ri *RouteInfo, ref gatewayv1.ParentReference, gateways map[string]*GatewayInfo,
	namespaces map[string]*corev1.Namespace) *RouteParentInfo {
	route := ri.Resource
	pi := &RouteParentInfo{Ref: ref, Accepted: false}

	group := "gateway.networking.k8s.io"
	if ref.Group != nil && *ref.Group != "" {
		group = string(*ref.Group)
	}
	kind := "Gateway"
	if ref.Kind != nil && *ref.Kind != "" {
		kind = string(*ref.Kind)
	}
	if group != "gateway.networking.k8s.io" || kind != "Gateway" {
		pi.Reason = string(gatewayv1.RouteReasonInvalidKind)
		pi.Message = fmt.Sprintf("parentRef group/kind %s/%s is not supported (only Gateway)", group, kind)
		return pi
	}
	if ref.Name == "" {
		pi.Skip = true // cannot key a status entry without a name
		return pi
	}

	ns := route.Namespace
	if ref.Namespace != nil && *ref.Namespace != "" {
		ns = string(*ref.Namespace)
	}

	gw, ok := gateways[ns+"/"+string(ref.Name)]
	if !ok {
		// Either the Gateway does not exist, or its GatewayClass belongs to
		// another controller (BuildGraph already dropped those Gateways).
		// Distinguishing would require the foreign Gateway object, which we
		// deliberately do not own: report NoMatchingParent for a missing
		// Gateway, and stay silent when another controller owns it.
		pi.Reason = string(gatewayv1.RouteReasonNoMatchingParent)
		pi.Message = fmt.Sprintf("Gateway %s/%s not found", ns, ref.Name)
		return pi
	}
	if !gw.Class.Accepted {
		pi.Reason = string(gatewayv1.RouteReasonNoMatchingParent)
		pi.Message = fmt.Sprintf("GatewayClass %q is not accepted: %s",
			gw.Class.Resource.Name, gw.Class.Message)
		return pi
	}
	pi.Gateway = gw

	// Candidate listeners via sectionName / port.
	var candidates []*ListenerInfo
	if ref.SectionName != nil && *ref.SectionName != "" {
		for _, l := range gw.Listeners {
			if l.Spec.Name == *ref.SectionName {
				candidates = append(candidates, l)
				break
			}
		}
		if len(candidates) == 0 {
			pi.Reason = string(gatewayv1.RouteReasonNoMatchingParent)
			pi.Message = fmt.Sprintf("Gateway %s/%s has no listener %q", ns, ref.Name, *ref.SectionName)
			return pi
		}
	} else {
		candidates = gw.Listeners
	}
	if ref.Port != nil {
		var filtered []*ListenerInfo
		for _, l := range candidates {
			if l.Spec.Port == *ref.Port {
				filtered = append(filtered, l)
			}
		}
		if len(filtered) == 0 {
			pi.Reason = string(gatewayv1.RouteReasonNoMatchingParent)
			pi.Message = fmt.Sprintf("Gateway %s/%s has no listener on port %d", ns, ref.Name, *ref.Port)
			return pi
		}
		candidates = filtered
	}

	// Attachment eligibility: valid listener, HTTPRoute-accepting protocol,
	// allowedRoutes, then hostname intersection (S4). The most specific
	// standard reason for the total failure is reported.
	hostnameFail, allowedFail := false, false
	for _, l := range candidates {
		if !l.Valid {
			continue
		}
		if l.Spec.Protocol != gatewayv1.HTTPProtocolType && l.Spec.Protocol != gatewayv1.HTTPSProtocolType {
			allowedFail = true // TLS listeners do not accept HTTPRoute in this MVP
			continue
		}
		if allowed, _ := routeAllowed(l, route.Namespace, namespaces); !allowed {
			allowedFail = true
			continue
		}
		if hostnameIntersection(route.Spec.Hostnames, l.Spec.Hostname) == nil {
			hostnameFail = true
			continue
		}
		pi.Attached = append(pi.Attached, &RouteAttachment{Route: ri, Parent: pi, Listener: l})
	}

	// Cross-namespace attachment is governed ONLY by the target listener's
	// allowedRoutes (evaluated above per listener — Gateway API v1:
	// allowedRoutes IS the authorization for Gateway-route attachment; a
	// ReferenceGrant is neither required nor consulted for parentRefs).
	// from=Same already produced NotAllowedByListeners above for cross-ns
	// routes; from=All / matching Selector attach regardless of namespaces.
	// sectionName/port matching and the hostname intersection (S4) rules are
	// namespace-independent and unchanged.

	switch {
	case len(pi.Attached) > 0:
		pi.Accepted = true
		pi.Reason = string(gatewayv1.RouteReasonAccepted)
	case hostnameFail:
		pi.Reason = string(gatewayv1.RouteReasonNoMatchingListenerHostname)
		pi.Message = "route hostnames do not intersect any listener hostname (DESIGN.md §3.3 S4)"
	case allowedFail:
		pi.Reason = string(gatewayv1.RouteReasonNotAllowedByListeners)
		pi.Message = "no eligible listener accepts this HTTPRoute (listener protocol, allowedRoutes kinds or namespaces)"
	default:
		pi.Reason = string(gatewayv1.RouteReasonNoMatchingParent)
		pi.Message = "no valid listener matched this parentRef"
	}
	return pi
}

// routeAllowed evaluates listener.allowedRoutes for an HTTPRoute (Gateway
// API v1): from=All accepts any namespace, from=Same only the Gateway's own
// namespace, from=None rejects everything, from=Selector matches the ROUTE
// namespace's labels. allowedRoutes is a PER-LISTENER policy: the caller
// evaluates it per matched listener. Unset namespaces policy = the CRD
// default `{namespaces:{from:Same}}` (the API server materializes that
// default for every stored Gateway; synthetic objects get the same
// behaviour here).
func routeAllowed(l *ListenerInfo, routeNS string, namespaces map[string]*corev1.Namespace) (bool, string) {
	gwNS := l.Gateway.Resource.Namespace
	from := gatewayv1.NamespacesFromSame
	var selector *metav1.LabelSelector
	if ar := l.Spec.AllowedRoutes; ar != nil {
		if len(ar.Kinds) > 0 {
			ok := false
			for _, k := range ar.Kinds {
				g := "gateway.networking.k8s.io"
				if k.Group != nil && *k.Group != "" {
					g = string(*k.Group)
				}
				if g == "gateway.networking.k8s.io" && strings.EqualFold(string(k.Kind), "HTTPRoute") {
					ok = true
					break
				}
			}
			if !ok {
				return false, "allowedRoutes.kinds does not include HTTPRoute"
			}
		}
		if ns := ar.Namespaces; ns != nil {
			if ns.From != nil {
				from = *ns.From
			}
			selector = ns.Selector
		}
	}
	switch from {
	case gatewayv1.NamespacesFromAll:
		// any namespace may attach
	case gatewayv1.NamespacesFromSame:
		if routeNS != gwNS {
			return false, "allowedRoutes.namespaces.from=Same rejects cross-namespace routes"
		}
	case gatewayv1.NamespacesFromNone:
		return false, "allowedRoutes.namespaces.from=None rejects all routes"
	case gatewayv1.NamespacesFromSelector:
		if selector == nil {
			return false, "allowedRoutes.namespaces.from=Selector requires a selector"
		}
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return false, fmt.Sprintf("allowedRoutes.namespaces selector is invalid: %v", err)
		}
		target, ok := namespaces[routeNS]
		if !ok || !sel.Matches(labels.Set(target.Labels)) {
			return false, "allowedRoutes.namespaces.from=Selector does not match the route namespace"
		}
	default:
		return false, fmt.Sprintf("allowedRoutes.namespaces.from=%q is not recognized", from)
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// Rule + backend resolution (§4, GEP-1364)
// ---------------------------------------------------------------------------

func resolveRule(index int, route *gatewayv1.HTTPRoute, slices map[string][]*discoveryv1.EndpointSlice,
	services map[string]*corev1.Service, grants map[string][]*gatewayv1.ReferenceGrant) *RuleInfo {
	rule := route.Spec.Rules[index]
	ri := &RuleInfo{Index: index}

	if msg := unsupportedRuleReason(rule); msg != "" {
		ri.Valid, ri.InvalidMsg = false, msg
		return ri
	}

	// Filters (§3.3): RequestRedirect, URLRewrite, the two header modifiers
	// and RequestMirror are implemented; duplicates (except mirrors — the
	// spec allows multiple mirror filters per rule) and unsupported filter
	// types are rejected in unsupportedRuleReason. A redirect rule never
	// reaches the backend (CRD validation also forbids combining it with
	// backendRefs).
	var rr *gatewayv1.HTTPRequestRedirectFilter
	var uw *gatewayv1.HTTPURLRewriteFilter
	var rhm, shm *gatewayv1.HTTPHeaderFilter
	var mirrors []*gatewayv1.HTTPRequestMirrorFilter
	for i := range rule.Filters {
		f := &rule.Filters[i]
		switch f.Type {
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			rr = f.RequestRedirect
		case gatewayv1.HTTPRouteFilterURLRewrite:
			uw = f.URLRewrite
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			rhm = f.RequestHeaderModifier
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			shm = f.ResponseHeaderModifier
		case gatewayv1.HTTPRouteFilterRequestMirror:
			if f.RequestMirror == nil {
				ri.Valid, ri.InvalidMsg = false,
					fmt.Sprintf("filter[%d]: RequestMirror filter requires the requestMirror field", i)
				return ri
			}
			if _, ok := mirrorPercent(f.RequestMirror); !ok {
				ri.Valid, ri.InvalidMsg = false,
					fmt.Sprintf("filter[%d]: requestMirror percent/fraction is invalid (mutually exclusive; 0≤percent≤100; 0≤numerator≤denominator; denominator>0)", i)
				return ri
			}
			mirrors = append(mirrors, f.RequestMirror)
		}
	}
	ri.RequestRedirect = rr
	ri.URLRewrite = uw
	ri.RequestHeaderModifier = rhm
	ri.ResponseHeaderModifier = shm
	ri.PathPrefix = singlePathPrefix(rule)

	// Matches (§3.3): one dispatch case per match (OR across matches, AND
	// within a match), each with its own path specs so constrained matches
	// only apply where their path applies too.
	ri.MatchCases = make([][]MatchConstraint, len(rule.Matches))
	ri.MatchPaths = make([][]string, len(rule.Matches))
	for i := range rule.Matches {
		ri.MatchCases[i] = matchConstraintsOf(&rule.Matches[i])
		ri.MatchPaths[i] = pathSpecsOfMatch(&rule.Matches[i])
	}
	ri.Constraints = unionConstraints(ri.MatchCases)
	if len(rule.Matches) == 0 {
		// A rule without matches matches every request.
		ri.MatchCases = [][]MatchConstraint{nil}
		ri.MatchPaths = [][]string{{"/"}}
		ri.Constraints = nil
	}

	// Backends (§3.3): every active (weight>0) backendRef participates with
	// its weight; a rule whose backendRefs exist but are ALL weight 0
	// matches no traffic (spec: weight 0 = no requests routed) → the rule
	// is not implementable and falls through like a non-match.
	// Rules with NO backendRefs at all answer 500 (GEP-1364 static marker).
	active, zeroWanted := activeBackends(rule)
	if len(active) == 0 {
		if zeroWanted && len(rule.BackendRefs) > 0 {
			// All refs present but every weight 0: the rule must not route.
			ri.Valid, ri.InvalidMsg = false, "every backendRef has weight 0; the rule matches no traffic"
			return ri
		}
		if zeroWanted {
			// No backendRefs at all: GEP-1364 semantics — emit the rule
			// with the static-500 marker so matching requests answer 500
			// instead of falling through; ResolvedRefs stays healthy.
			ri.Upstream = StaticUpstreamName(500)
			ri.Locations = pathSpecs(rule)
			ri.Timeouts = ruleTimeouts(rule)
			ri.Valid = true
			return ri
		}
		ri.Valid, ri.InvalidMsg = false, "rule needs at least one active (weight>0) backendRef"
		return ri
	}

	var (
		refFail  bool
		failMsg  string
		failRsn  string
		backends []WeightedBackend
	)
	singleUpstream, singleEndpoints := "", []contract.Endpoint(nil)
	for i := range active {
		ref := active[i]
		upstream, _, endpoints, failReason, msg := resolveBackend(ref.BackendObjectReference, route.Namespace, slices, services, grants)
		w := int32(1)
		if ref.Weight != nil {
			w = *ref.Weight
		}
		wb := WeightedBackend{Upstream: upstream, Weight: w, Endpoints: endpoints}
		// backendRef-level filter (§3.3 BackendRequestHeaderModification):
		// at most one RequestHeaderModifier per ref (validated in
		// unsupportedRuleReason). Applies only to requests sent to THIS
		// backend; the translator puts it on the business location for
		// single-backend rules.
		for j := range ref.Filters {
			if ref.Filters[j].Type == gatewayv1.HTTPRouteFilterRequestHeaderModifier {
				wb.RequestHeaderModifier = ref.Filters[j].RequestHeaderModifier
			}
		}
		backends = append(backends, wb)
		if failReason != "" && !refFail {
			refFail, failRsn, failMsg = true, failReason, msg
		}
		if i == 0 {
			singleUpstream, singleEndpoints = upstream, endpoints
		}
	}
	// Mirrors (§3.3 RequestMirror): resolved against their own backendRef —
	// the route's normal handling is unaffected either way. Per the spec,
	// an unresolvable mirror backendRef drops the MIRROR (never configure
	// that backend) and reports ResolvedRefs=False with the failure reason;
	// the rule keeps serving its main backends (no static 500). A mirror
	// that resolves to zero endpoints (our internal static marker, GEP-1364)
	// is dropped silently — the ref resolved, there is just nothing to
	// mirror to.
	for _, m := range mirrors {
		pct, _ := mirrorPercent(m)
		if pct == 0 {
			// 0% mirrors no traffic: dropping the mirror is observably
			// identical to configuring a 0.00% split — and simpler.
			continue
		}
		upstream, _, endpoints, failReason, msg := resolveBackend(m.BackendRef, route.Namespace, slices, services, grants)
		if failReason != "" {
			if !refFail {
				refFail, failRsn, failMsg = true, failReason, msg
			}
			continue
		}
		if IsStaticUpstream(upstream) {
			continue
		}
		ri.RequestMirrors = append(ri.RequestMirrors, RequestMirror{
			Upstream:  upstream,
			Path:      mirrorRoutePath(route, index, upstream),
			Percent:   pct,
			Endpoints: endpoints,
		})
	}
	ri.Backends = backends
	ri.Upstream = singleUpstream
	ri.Endpoints = singleEndpoints
	ri.Locations = pathSpecs(rule)
	ri.Timeouts = ruleTimeouts(rule)
	ri.Valid = true
	// On failure the rule is still emitted: matching requests get an
	// immediate 500 from the static marker upstream instead of falling
	// through to another route (GEP-1364).
	ri.refFailReason, ri.refFailMsg = failRsn, failMsg
	return ri
}

// mirrorPercent normalizes a RequestMirror filter's percent/fraction pair
// into a percentage in [0, 100] (Gateway API v1 HTTPRequestMirrorFilter):
// percent and fraction are mutually exclusive; percent must be within
// [0, 100]; a fraction requires denominator > 0 and 0 ≤ numerator ≤
// denominator (denominator defaults to 100 when omitted). Unset → 100
// (mirror everything). ok=false marks a spec-invalid combination — the rule
// is dropped (PartiallyInvalid), consistent with other filter value errors.
func mirrorPercent(m *gatewayv1.HTTPRequestMirrorFilter) (pct float64, ok bool) {
	switch {
	case m.Percent != nil && m.Fraction != nil:
		return 0, false
	case m.Percent != nil:
		p := float64(*m.Percent)
		if p < 0 || p > 100 {
			return 0, false
		}
		return p, true
	case m.Fraction != nil:
		den := int32(100) // kubebuilder default when omitted
		if m.Fraction.Denominator != nil {
			den = *m.Fraction.Denominator
		}
		if den <= 0 || m.Fraction.Numerator < 0 || m.Fraction.Numerator > den {
			return 0, false
		}
		return float64(m.Fraction.Numerator) * 100 / float64(den), true
	default:
		return 100, true
	}
}

// mirrorRoutePath derives the deterministic internal mirror location path
// for one (route, rule, mirror target): "/hng_mirror_<hash>" (hng_ marker
// convention, unique per mirror target of a rule — two mirror filters
// pointing at the same backend within one rule share the path so their
// percentages merge by max, mirroring NGF's naming scheme).
func mirrorRoutePath(route *gatewayv1.HTTPRoute, ruleIdx int, upstream string) string {
	key := route.Namespace + "/" + route.Name + "/" + strconv.Itoa(ruleIdx) + "/" + upstream
	return "/hng_mirror_" + strconv.FormatUint(uint64(fnv32a(key)), 16)
}

// activeBackends returns the rule's active (weight>0) backendRefs in spec
// order. zeroWanted reports whether the rule deliberately has no active
// backends (empty list or all weights 0) as opposed to… well, the caller
// only distinguishes "none by intent" from "none because invalid" for the
// GEP-1364 marker decision.
func activeBackends(rule gatewayv1.HTTPRouteRule) (active []*gatewayv1.HTTPBackendRef, zeroWanted bool) {
	if len(rule.BackendRefs) == 0 {
		return nil, true
	}
	for i := range rule.BackendRefs {
		w := int32(1)
		if rule.BackendRefs[i].Weight != nil {
			w = *rule.BackendRefs[i].Weight
		}
		if w == 0 {
			continue
		}
		active = append(active, &rule.BackendRefs[i])
	}
	return active, len(active) == 0 // all weights zero → zeroWanted
}

// matchConstraintsOf extracts one match's non-path constraints (method,
// headers, query params). Within a match, semantics are AND — the
// translator's dispatch tree evaluates them as a chain.
func matchConstraintsOf(m *gatewayv1.HTTPRouteMatch) []MatchConstraint {
	var out []MatchConstraint
	add := func(c MatchConstraint) {
		for _, e := range out {
			if e == c {
				return
			}
		}
		out = append(out, c)
	}
	if m.Method != nil {
		add(MatchConstraint{Kind: constraintMethod, Type: "Exact", Value: string(*m.Method)})
	}
	for _, h := range m.Headers {
		name := headerVarName(string(h.Name))
		t := string(gatewayv1.HeaderMatchExact)
		if h.Type != nil {
			t = string(*h.Type)
		}
		add(MatchConstraint{Kind: constraintHeader, Name: name, Type: t, Value: h.Value})
	}
	for _, q := range m.QueryParams {
		name := headerVarName(string(q.Name))
		t := string(gatewayv1.QueryParamMatchExact)
		if q.Type != nil {
			t = string(*q.Type)
		}
		add(MatchConstraint{Kind: constraintQuery, Name: name, Type: t, Value: q.Value})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		if out[a].Name != out[b].Name {
			return out[a].Name < out[b].Name
		}
		if out[a].Type != out[b].Type {
			return out[a].Type < out[b].Type
		}
		return out[a].Value < out[b].Value
	})
	return out
}

// unionConstraints merges match constraint sets into a deterministic
// deduplicated union (used for capability checks).
func unionConstraints(cases [][]MatchConstraint) []MatchConstraint {
	var out []MatchConstraint
	seen := map[MatchConstraint]struct{}{}
	for _, cs := range cases {
		for _, c := range cs {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		if out[a].Name != out[b].Name {
			return out[a].Name < out[b].Name
		}
		if out[a].Type != out[b].Type {
			return out[a].Type < out[b].Type
		}
		return out[a].Value < out[b].Value
	})
	return out
}

const (
	constraintMethod = "method"
	constraintHeader = "header"
	constraintQuery  = "query"
)

// headerVarName converts an HTTP header/query-param name into the nginx
// variable component form: lowercase with '-' replaced by '_' ($http_x_name,
// $arg_x). Query param names use the same sanitization ($arg_ names are
// normalized by nginx the same way).
func headerVarName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

// singlePathPrefix returns the rule's single PathPrefix match value when the
// rule has exactly one match and that match is a PathPrefix (the CRD-gated
// precondition for replacePrefixMatch filters); "" otherwise.
func singlePathPrefix(rule gatewayv1.HTTPRouteRule) string {
	if len(rule.Matches) != 1 || rule.Matches[0].Path == nil {
		return ""
	}
	if rule.Matches[0].Path.Type != nil && *rule.Matches[0].Path.Type != gatewayv1.PathMatchPathPrefix {
		return ""
	}
	if rule.Matches[0].Path.Value == nil {
		return "/"
	}
	return *rule.Matches[0].Path.Value
}

// unsupportedRuleReason returns a human reason when the rule uses features
// outside the implemented subset ("" = supported). Subset: path matches
// (Exact / PathPrefix), method/header/query matches, backendRefs with
// weights, RequestRedirect / URLRewrite / RequestHeaderModifier /
// ResponseHeaderModifier / RequestMirror filters (multiple RequestMirror
// filters per rule are legal per spec), backendRef-level
// RequestHeaderModifier on single-backend rules, optional timeouts. Dropped
// features surface as PartiallyInvalid (GEP-1748).
func unsupportedRuleReason(rule gatewayv1.HTTPRouteRule) string {
	if rule.SessionPersistence != nil {
		return "sessionPersistence is not supported"
	}
	redirectCount, rewriteCount, reqHeaderCount, respHeaderCount := 0, 0, 0, 0
	for i := range rule.Filters {
		switch rule.Filters[i].Type {
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			redirectCount++
		case gatewayv1.HTTPRouteFilterURLRewrite:
			rewriteCount++
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			reqHeaderCount++
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			respHeaderCount++
		case gatewayv1.HTTPRouteFilterRequestMirror:
			// Multiple mirror filters per rule are explicitly allowed
			// (Gateway API v1 HTTPRouteFilter.RequestMirror).
		default:
			return fmt.Sprintf("filter[%d] type %q is not supported", i, rule.Filters[i].Type)
		}
	}
	if redirectCount > 1 || rewriteCount > 1 || reqHeaderCount > 1 || respHeaderCount > 1 {
		return "a filter of the same type may only be specified once per rule"
	}
	if redirectCount > 0 && rewriteCount > 0 {
		return "RequestRedirect and URLRewrite must not be combined in one rule"
	}
	active, _ := activeBackends(rule)
	anyBackendFilter := false
	for i, ref := range rule.BackendRefs {
		headerMods := 0
		for j := range ref.Filters {
			if ref.Filters[j].Type != gatewayv1.HTTPRouteFilterRequestHeaderModifier {
				return fmt.Sprintf("backendRef[%d] filter[%d] type %q is not supported (only RequestHeaderModifier)",
					i, j, ref.Filters[j].Type)
			}
			headerMods++
		}
		if headerMods > 1 {
			return fmt.Sprintf("backendRef[%d]: RequestHeaderModifier filter may only be specified once", i)
		}
		if headerMods > 0 {
			anyBackendFilter = true
		}
	}
	// A multi-backend rule proxies through ONE rule-private weighted
	// upstream, which cannot carry per-backend headers. Rather than apply a
	// single backend's modifiers to every backend (spec violation:
	// backendRef filters apply "if and only if the request is being
	// forwarded to the backend defined here"), such rules are dropped
	// (documented deviation, DESIGN.md §5.1.1).
	if len(active) > 1 && anyBackendFilter {
		return "backendRef-level RequestHeaderModifier is supported only on single-backend rules"
	}
	if redirectCount > 0 && len(rule.BackendRefs) > 0 {
		return "RequestRedirect must not be combined with backendRefs"
	}
	if rewriteCount > 0 && len(rule.BackendRefs) == 0 {
		return "URLRewrite requires backendRefs to proxy to"
	}
	for mi, m := range rule.Matches {
		if m.Path != nil && m.Path.Type != nil && *m.Path.Type == gatewayv1.PathMatchRegularExpression {
			return fmt.Sprintf("match[%d]: RegularExpression path match is not supported", mi)
		}
	}
	return ""
}

// resolveBackend resolves one backendRef to an upstream name. Failures
// return a ResolvedRefs reason together with the static-500 marker upstream
// (GEP-1364: Accepted stays true, ResolvedRefs=False, rule answers 500).
//
// EndpointSlices provide the endpoint addresses (S3); the Service object —
// now watched (§7) — maps backendRef.port (the Service port) to the port
// name that disambiguates multi-port Services in the slices.
func resolveBackend(ref gatewayv1.BackendObjectReference, routeNS string,
	slices map[string][]*discoveryv1.EndpointSlice, services map[string]*corev1.Service,
	grants map[string][]*gatewayv1.ReferenceGrant) (upstream string, staticCode int, endpoints []contract.Endpoint, failReason, failMsg string) {
	static := func(reason, msg string) (string, int, []contract.Endpoint, string, string) {
		return StaticUpstreamName(500), 500, nil, reason, msg
	}

	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}
	kind := "Service"
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	if kind != "Service" || (group != "" && group != "core") {
		return static(string(gatewayv1.RouteReasonInvalidKind),
			fmt.Sprintf("backendRef kind %s/%s is not supported (only Service)", group, kind))
	}
	ns := routeNS
	if ref.Namespace != nil && *ref.Namespace != "" {
		ns = string(*ref.Namespace)
	}
	if ns != routeNS {
		// §3.5 / ReferenceGrant spec: cross-namespace backendRefs are
		// permitted only by a ReferenceGrant in the TARGET namespace whose
		// from matches the referencing HTTPRoute and whose to matches the
		// core/Service reference (to.name constrains when set).
		if !referenceGrantPermits(grants[ns],
			"gateway.networking.k8s.io", "HTTPRoute", routeNS,
			"", "Service", string(ref.Name)) {
			return static(string(gatewayv1.RouteReasonRefNotPermitted),
				fmt.Sprintf("cross-namespace backendRef to Service %s/%s is not permitted (no matching ReferenceGrant in %s)", ns, ref.Name, ns))
		}
	}
	if ref.Port == nil {
		return static(string(gatewayv1.RouteReasonUnsupportedValue),
			"backendRef.port is required for Service references")
	}

	svcSlices := slices[ns+"/"+string(ref.Name)]
	if len(svcSlices) == 0 {
		return static(string(gatewayv1.RouteReasonBackendNotFound),
			fmt.Sprintf("Service %s/%s not found (no EndpointSlices)", ns, ref.Name))
	}
	return upstreamFor(svcSlices, services[ns+"/"+string(ref.Name)], ns, string(ref.Name), *ref.Port)
}

// upstreamFor derives the upstream name and endpoints from a service's
// slices, mapping the requested Service port to the endpoint port:
//
//   - Service present: the spec.ports entry matching backendRef.port gives
//     the port NAME; the slice port with that name carries the resolved
//     endpoint port (targetPort). Unnamed single-port services resolve
//     directly from the slices.
//   - Service absent from the snapshot: fall back to the unambiguous
//     single-distinct-port inference (cache lag tolerance).
func upstreamFor(svcSlices []*discoveryv1.EndpointSlice, svc *corev1.Service, ns, name string, servicePort int32) (string, int, []contract.Endpoint, string, string) {
	unsupported := func(msg string) (string, int, []contract.Endpoint, string, string) {
		return StaticUpstreamName(500), 500, nil,
			string(gatewayv1.RouteReasonUnsupportedValue), msg
	}

	endpointPort := int32(-1)
	if svc != nil {
		portName := ""
		found := false
		for _, p := range svc.Spec.Ports {
			if p.Port == servicePort {
				portName = p.Name
				found = true
				break
			}
		}
		if !found {
			return unsupported(fmt.Sprintf("Service %s/%s has no port %d", ns, name, servicePort))
		}
		if portName != "" {
			for _, s := range svcSlices {
				for _, p := range s.Ports {
					if p.Port != nil && p.Name != nil && *p.Name == portName {
						endpointPort = *p.Port
					}
				}
			}
			if endpointPort < 0 {
				return unsupported(fmt.Sprintf(
					"Service %s/%s port %d (%q): no EndpointSlice port with that name", ns, name, servicePort, portName))
			}
		}
	}
	if endpointPort < 0 {
		// Unnamed port (or Service object not in the snapshot): infer from
		// the slices — unambiguous only when they expose exactly one
		// distinct port.
		ports := map[int32]struct{}{}
		for _, s := range svcSlices {
			for _, p := range s.Ports {
				if p.Port != nil {
					ports[*p.Port] = struct{}{}
				}
			}
		}
		if len(ports) != 1 {
			return unsupported(fmt.Sprintf(
				"Service %s/%s: cannot determine target port for service port %d (ambiguous EndpointSlice ports and no Service object)",
				ns, name, servicePort))
		}
		for p := range ports {
			endpointPort = p
		}
	}

	var endpoints []contract.Endpoint
	for _, s := range svcSlices {
		if s.AddressType == discoveryv1.AddressTypeFQDN {
			continue // only IPv4/IPv6 literals (DESIGN.md §3.3)
		}
		endpoints = append(endpoints, collectEndpoints(s, endpointPort)...)
	}
	if len(endpoints) == 0 {
		// Service exists but has no addresses: serve 500 rather than
		// emitting an empty upstream block (nginx rejects upstreams with no
		// servers). ResolvedRefs stays healthy — the ref itself resolved.
		return StaticUpstreamName(500), 500, nil, "", ""
	}
	return fmt.Sprintf("%s_%s_%d", ns, name, endpointPort), 0, endpoints, "", ""
}

// collectEndpoints turns one slice's endpoints into contract Endpoints.
// Conditions.Ready nil is interpreted as ready (k8s API convention);
// not-ready endpoints are kept with Ready=false (DESIGN.md §4 "down").
func collectEndpoints(s *discoveryv1.EndpointSlice, port int32) []contract.Endpoint {
	var out []contract.Endpoint
	for _, ep := range s.Endpoints {
		// Conditions is a value struct; Ready nil is interpreted as ready
		// (k8s API convention). Not-ready endpoints stay with Ready=false.
		ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
		for _, addr := range ep.Addresses {
			out = append(out, contract.Endpoint{IP: addr, Port: int(port), Ready: ready})
		}
	}
	return out
}

// pathSpecs converts rule matches into nginx-ready location matchers.
// Gateway API segment-prefix semantics are emulated with an exact entry plus
// a trailing-slash prefix entry ("/foo" → "= /foo" and "/foo/" — so "/foo"
// matches exactly "/foo" and "/foo/…" but never "/foobar").
func pathSpecs(rule gatewayv1.HTTPRouteRule) []string {
	if len(rule.Matches) == 0 {
		return []string{"/"}
	}
	seen := map[string]struct{}{}
	var out []string
	for i := range rule.Matches {
		for _, p := range pathSpecsOfMatch(&rule.Matches[i]) {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				out = append(out, p)
			}
		}
	}
	return out
}

// pathSpecsOfMatch converts ONE match into nginx-ready location matchers
// (per-match expansion: matches are OR'd within a rule, so each match's
// cases must carry only its own path).
func pathSpecsOfMatch(m *gatewayv1.HTTPRouteMatch) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if m.Path == nil {
		add("/")
		return out
	}
	value := "/"
	if m.Path.Value != nil && *m.Path.Value != "" {
		value = *m.Path.Value
	}
	t := gatewayv1.PathMatchPathPrefix
	if m.Path.Type != nil {
		t = *m.Path.Type
	}
	switch t {
	case gatewayv1.PathMatchExact:
		add("= " + value)
	default: // PathPrefix (RegularExpression was rejected earlier)
		if value == "/" {
			add("/")
			return out
		}
		v := strings.TrimSuffix(value, "/")
		add("= " + v)
		add(v + "/")
	}
	return out
}

// gep1742Duration matches the GEP-1742 duration grammar (1..5 digits with
// h|m|s|ms units, 1..4 components, total >= 1s). The literal is passed
// through to nginx unchanged: nginx accepts the same unit syntax.
var gep1742Duration = regexp.MustCompile(`^([0-9]{1,5}(h|m|s|ms)){1,4}$`)

// ruleTimeouts converts HTTPRoute rule timeouts (GEP-1742 duration strings)
// into contract.Timeouts nginx duration literals. Mapping: Request → read
// timeout (the whole request against the backend); BackendRequest → connect
// and send timeouts. Malformed values fall back to nginx defaults (nil).
func ruleTimeouts(rule gatewayv1.HTTPRouteRule) *contract.Timeouts {
	if rule.Timeouts == nil {
		return nil
	}
	t := &contract.Timeouts{}
	set := func(dst *string, src *gatewayv1.Duration) bool {
		if src == nil {
			return true
		}
		s := string(*src)
		if !gep1742Duration.MatchString(s) || !atLeastOneSecond(s) {
			return false
		}
		*dst = s
		return true
	}
	if !set(&t.Read, rule.Timeouts.Request) ||
		!set(&t.Connect, rule.Timeouts.BackendRequest) ||
		!set(&t.Send, rule.Timeouts.BackendRequest) {
		return nil
	}
	if t.Connect == "" && t.Send == "" && t.Read == "" {
		return nil
	}
	return t
}

// atLeastOneSecond verifies the GEP-1742 minimum total of one second.
func atLeastOneSecond(s string) bool {
	totalMs := 0
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		num := 0
		for _, c := range s[i:j] {
			num = num*10 + int(c-'0')
		}
		unit := s[j:]
		switch {
		case strings.HasPrefix(unit, "ms"):
			totalMs += num
			j += 2
		case strings.HasPrefix(unit, "s"):
			totalMs += num * 1000
			j++
		case strings.HasPrefix(unit, "m"):
			totalMs += num * 60_000
			j++
		case strings.HasPrefix(unit, "h"):
			totalMs += num * 3_600_000
			j++
		default:
			return false
		}
		i = j
	}
	return totalMs >= 1000
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
