// Graph-level tests for the round-7 features: RequestMirror filters
// (percentage + multiple mirrors) and backendRef-level RequestHeaderModifier
// (BackendRequestHeaderModification). Spec-derived per Gateway API v1
// HTTPRouteFilter.RequestMirror: backendRef + optional percent/fraction
// (mutually exclusive, fraction denominator>0, 0≤numerator≤denominator),
// unset/100% mirrors everything, mirror filters never affect the normal
// request handling, an unresolvable mirror backendRef drops the mirror and
// reports ResolvedRefs=False while the route keeps serving.
package provider

import (
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func mirrorRule(path string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr(path)},
		}},
		Filters:     filters,
		BackendRefs: []gatewayv1.HTTPBackendRef{backendRefTo("svc", 8080)},
	}
}

func mirrorFilter(svc string, port int32, percent *int32, fraction *gatewayv1.Fraction) gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
			BackendRef: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(svc),
				Port: ptr(gatewayv1.PortNumber(port)),
			},
			Percent:  percent,
			Fraction: fraction,
		},
	}
}

func backendRefTo(svc string, port int32) gatewayv1.HTTPBackendRef {
	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(svc),
				Port: ptr(gatewayv1.PortNumber(port)),
			},
		},
	}
}

// graphOf builds the graph + configuration for a mirror test with the main
// backend "svc" plus any extra EndpointSlices.
func graphOf(t *testing.T, extraSlices []string, rules ...gatewayv1.HTTPRouteRule) (*Graph, *contract.Configuration) {
	t.Helper()
	objs := []any{
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		filterRoute("h", rules...),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	}
	for _, svc := range extraSlices {
		objs = append(objs, testSlice("default", svc, 8080, ptr(true), "10.9.0.1"))
	}
	g := build(t, objs...)
	return g, g.Configuration()
}

// findLocation returns the location with the given path spec.
func findLocation(t *testing.T, cfg *contract.Configuration, path string) *contract.Location {
	t.Helper()
	for _, s := range cfg.Servers {
		for _, loc := range s.Locations {
			if loc.Path == path {
				return loc
			}
		}
	}
	t.Fatalf("location %q not found in %d server(s)", path, len(cfg.Servers))
	return nil
}

func TestMirror_UnsetPercentMirrorsEverythingWithoutSplit(t *testing.T) {
	rule := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, nil, nil))
	_, cfg := graphOf(t, []string{"mirror-svc"}, rule)

	if len(cfg.SplitClients) != 0 {
		t.Fatalf("unset percent must not produce split_clients: %+v", cfg.SplitClients)
	}
	loc := findLocation(t, cfg, "= /m")
	if len(loc.Mirrors) != 1 {
		t.Fatalf("expected one mirror target, got %+v", loc.Mirrors)
	}
	ml := findLocation(t, cfg, "= "+loc.Mirrors[0].Path)
	if !strings.HasPrefix(ml.Path, "= /hng_mirror_") {
		t.Fatalf("mirror path must use the hng_ convention: %q", ml.Path)
	}
	if !ml.Internal {
		t.Fatalf("mirror location must be internal")
	}
	if ml.MirrorGate != "" {
		t.Fatalf("100%% mirror must have no gate, got %q", ml.MirrorGate)
	}
	if ml.Upstream != "default_mirror-svc_8080" {
		t.Fatalf("mirror upstream = %q", ml.Upstream)
	}
	if ml.ProxyPassURI != "$request_uri" {
		t.Fatalf("mirror location must restore $request_uri, got %q", ml.ProxyPassURI)
	}
	// Mirror upstreams reuse the existing upstream generation.
	found := false
	for _, u := range cfg.Upstreams {
		if u.Name == "default_mirror-svc_8080" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mirror upstream missing from Configuration.Upstreams")
	}
}

func TestMirror_100PercentBehavesLikeUnset(t *testing.T) {
	hundred := int32(100)
	rule := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, &hundred, nil))
	_, cfg := graphOf(t, []string{"mirror-svc"}, rule)

	if len(cfg.SplitClients) != 0 {
		t.Fatalf("100%% must not produce split_clients: %+v", cfg.SplitClients)
	}
	loc := findLocation(t, cfg, "= /m")
	ml := findLocation(t, cfg, "= "+loc.Mirrors[0].Path)
	if ml.MirrorGate != "" {
		t.Fatalf("100%% mirror must have no gate")
	}
}

func TestMirror_PercentageGatingProducesSplitClientsAndGate(t *testing.T) {
	twenty := int32(20)
	rule := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, &twenty, nil))
	_, cfg := graphOf(t, []string{"mirror-svc"}, rule)

	if len(cfg.SplitClients) != 1 {
		t.Fatalf("expected one split_clients block, got %+v", cfg.SplitClients)
	}
	sc := cfg.SplitClients[0]
	if sc.Source != "$request_id" {
		t.Fatalf("split_clients must key on $request_id, got %q", sc.Source)
	}
	loc := findLocation(t, cfg, "= /m")
	ml := findLocation(t, cfg, "= "+loc.Mirrors[0].Path)
	if ml.MirrorGate != sc.Name {
		t.Fatalf("gate %q must reference the split variable %q", ml.MirrorGate, sc.Name)
	}
	if len(sc.Entries) != 2 {
		t.Fatalf("expected 2 distributions, got %+v", sc.Entries)
	}
	if sc.Entries[0].Percent != "20.00" || sc.Entries[0].Value != loc.Mirrors[0].Path {
		t.Fatalf("mirror distribution wrong: %+v", sc.Entries[0])
	}
	if sc.Entries[1].Percent != "*" || sc.Entries[1].Value != "" {
		t.Fatalf("catch-all must resolve to \"\": %+v", sc.Entries[1])
	}
}

func TestMirror_SplitClientsSortedByName(t *testing.T) {
	// Two gated mirrors of one target: the location carries ONE directive at
	// the max percentage, so exactly one split block exists and the
	// producer sorts SplitClients by name (applied-hash determinism).
	twentyFive, fifty := int32(25), int32(50)
	rule := mirrorRule("/m",
		mirrorFilter("mirror-a", 8080, &fifty, nil),
		mirrorFilter("mirror-b", 8080, &twentyFive, nil),
	)
	_, cfg := graphOf(t, []string{"mirror-a", "mirror-b"}, rule)
	if len(cfg.SplitClients) != 2 {
		t.Fatalf("two gated targets → two split blocks, got %+v", cfg.SplitClients)
	}
	for i := 1; i < len(cfg.SplitClients); i++ {
		if cfg.SplitClients[i-1].Name >= cfg.SplitClients[i].Name {
			t.Fatalf("split_clients not sorted by name: %v vs %v",
				cfg.SplitClients[i-1].Name, cfg.SplitClients[i].Name)
		}
	}
}

func TestMirror_ZeroPercentDropsMirror(t *testing.T) {
	zero := int32(0)
	rule := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, &zero, nil))
	_, cfg := graphOf(t, []string{"mirror-svc"}, rule)

	if len(cfg.SplitClients) != 0 {
		t.Fatalf("0%% must not produce split_clients: %+v", cfg.SplitClients)
	}
	loc := findLocation(t, cfg, "= /m")
	if len(loc.Mirrors) != 0 {
		t.Fatalf("0%% mirror must be dropped, got %+v", loc.Mirrors)
	}
	if !ruleImplementsProxying(t, cfg) {
		t.Fatalf("the route must keep proxying normally")
	}
}

func ruleImplementsProxying(t *testing.T, cfg *contract.Configuration) bool {
	t.Helper()
	loc := findLocation(t, cfg, "= /m")
	return loc.Upstream == "default_svc_8080"
}

func TestMirror_FractionConversion(t *testing.T) {
	half := &gatewayv1.Fraction{Numerator: 1, Denominator: ptr(int32(2))}
	third := &gatewayv1.Fraction{Numerator: 1, Denominator: ptr(int32(3))}
	rule := mirrorRule("/m",
		mirrorFilter("mirror-a", 8080, nil, half),
		mirrorFilter("mirror-b", 8080, nil, third),
	)
	_, cfg := graphOf(t, []string{"mirror-a", "mirror-b"}, rule)

	byValue := map[string]string{}
	for _, sc := range cfg.SplitClients {
		if sc.Entries[0].Percent != "50.00" && sc.Entries[0].Percent != "33.33" {
			t.Fatalf("unexpected percentage %q (want 1/2 → 50.00, 1/3 → 33.33)", sc.Entries[0].Percent)
		}
		byValue[sc.Entries[0].Percent] = sc.Name
	}
	if _, ok := byValue["50.00"]; !ok {
		t.Fatalf("fraction 1/2 must convert to 50.00: %+v", cfg.SplitClients)
	}
	if _, ok := byValue["33.33"]; !ok {
		t.Fatalf("fraction 1/3 must floor-convert to 33.33: %+v", cfg.SplitClients)
	}
}

func TestMirror_FractionDefaultDenominator(t *testing.T) {
	// {numerator: 1} with omitted denominator defaults to /100 → 1%.
	rule := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, nil, &gatewayv1.Fraction{Numerator: 1}))
	_, cfg := graphOf(t, []string{"mirror-svc"}, rule)
	if len(cfg.SplitClients) != 1 || cfg.SplitClients[0].Entries[0].Percent != "1.00" {
		t.Fatalf("fraction {1} must default to denominator 100 → 1.00%%: %+v", cfg.SplitClients)
	}
}

func TestMirror_InvalidFractionDropsRule(t *testing.T) {
	cases := map[string]struct {
		percent  *int32
		fraction *gatewayv1.Fraction
	}{
		"percent and fraction together": {
			percent: ptr(int32(10)),
			fraction: &gatewayv1.Fraction{
				Numerator: 1, Denominator: ptr(int32(2)),
			},
		},
		"zero denominator": {fraction: &gatewayv1.Fraction{Numerator: 0, Denominator: ptr(int32(0))}},
		"negative denominator": {
			fraction: &gatewayv1.Fraction{Numerator: 1, Denominator: ptr(int32(-2))},
		},
		"numerator above denominator": {
			fraction: &gatewayv1.Fraction{Numerator: 3, Denominator: ptr(int32(2))},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rule := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, tc.percent, tc.fraction))
			g, cfg := graphOf(t, []string{"mirror-svc"}, rule)
			routeRule := g.Routes[0].Rules[0]
			if routeRule.Valid {
				t.Fatalf("rule with invalid mirror percent/fraction must be dropped")
			}
			if routeRule.InvalidMsg == "" {
				t.Fatalf("dropped rule must carry a reason")
			}
			// PartiallyInvalid semantics: dropped rule, no locations emitted.
			for _, s := range cfg.Servers {
				for _, loc := range s.Locations {
					if loc.Path == "= /m" {
						t.Fatalf("dropped rule must not emit a location")
					}
				}
			}
			// ResolvedRefs stays healthy: the failure is a filter value
			// error, not a reference error.
			if pi := g.Routes[0].Parents[0]; !pi.ResolvedRefs {
				t.Fatalf("invalid fraction must not fail ResolvedRefs: %s/%s", pi.RefsReason, pi.RefsMsg)
			}
		})
	}
}

func TestMirror_PercentOutOfRangeDropsRule(t *testing.T) {
	for _, p := range []int32{-1, 101} {
		rule := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, ptr(p), nil))
		g, _ := graphOf(t, []string{"mirror-svc"}, rule)
		if g.Routes[0].Rules[0].Valid {
			t.Fatalf("percent %d must drop the rule", p)
		}
	}
}

func TestMirror_MultipleTargetsProduceMultipleMirrorDirectives(t *testing.T) {
	rule := mirrorRule("/m",
		mirrorFilter("mirror-a", 8080, nil, nil),
		mirrorFilter("mirror-b", 8080, nil, nil),
	)
	_, cfg := graphOf(t, []string{"mirror-a", "mirror-b"}, rule)

	loc := findLocation(t, cfg, "= /m")
	if len(loc.Mirrors) != 2 {
		t.Fatalf("expected two mirror targets, got %+v", loc.Mirrors)
	}
	if loc.Mirrors[0].Path == loc.Mirrors[1].Path {
		t.Fatalf("distinct targets must have distinct internal paths")
	}
	seen := map[string]bool{}
	for _, m := range loc.Mirrors {
		ml := findLocation(t, cfg, "= "+m.Path)
		if !ml.Internal || ml.Upstream == "" {
			t.Fatalf("mirror location %q misconfigured: %+v", m.Path, ml)
		}
		seen[ml.Upstream] = true
	}
	if !seen["default_mirror-a_8080"] || !seen["default_mirror-b_8080"] {
		t.Fatalf("both mirror upstreams must be wired: %v", seen)
	}
}

func TestMirror_SameTargetAcrossRulesTakesMaxPercentage(t *testing.T) {
	// Two rules sharing no paths — each is its own location. Then the same
	// target mirrored from two FILTERS of one rule (one location).
	low, high := int32(25), int32(50)
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/m")},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{
			mirrorFilter("mirror-svc", 8080, &low, nil),
			mirrorFilter("mirror-svc", 8080, &high, nil),
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendRefTo("svc", 8080)},
	}
	_, cfg := graphOf(t, []string{"mirror-svc"}, rule)

	loc := findLocation(t, cfg, "= /m")
	if len(loc.Mirrors) != 1 {
		t.Fatalf("same target in one location must dedupe to ONE mirror directive, got %+v", loc.Mirrors)
	}
	if len(cfg.SplitClients) != 1 {
		t.Fatalf("expected exactly one split_clients, got %+v", cfg.SplitClients)
	}
	if got := cfg.SplitClients[0].Entries[0].Percent; got != "50.00" {
		t.Fatalf("max percentage must win: got %s, want 50.00", got)
	}
	ml := findLocation(t, cfg, "= "+loc.Mirrors[0].Path)
	if ml.MirrorGate != cfg.SplitClients[0].Name {
		t.Fatalf("the surviving mirror must be gated by the max-percentage split")
	}
}

func TestMirror_SameTargetAcrossRulesInOneLocationMaxWins(t *testing.T) {
	// Two REACHABLE rules sharing one location (dispatch by method): 25%
	// (any request) and 50% (POST) mirrors of the same target must merge to
	// one directive at 50%.
	low, high := int32(25), int32(50)
	r1 := mirrorRule("/m", mirrorFilter("mirror-svc", 8080, &low, nil))
	r2 := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path:   &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/m")},
			Method: ptr(gatewayv1.HTTPMethodPost),
		}},
		Filters:     []gatewayv1.HTTPRouteFilter{mirrorFilter("mirror-svc", 8080, &high, nil)},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendRefTo("svc", 8080)},
	}
	_, cfg := graphOf(t, []string{"mirror-svc"}, r1, r2)

	loc := findLocation(t, cfg, "= /m")
	if len(loc.Mirrors) != 1 {
		t.Fatalf("one target → one mirror directive, got %+v", loc.Mirrors)
	}
	if len(cfg.SplitClients) != 1 || cfg.SplitClients[0].Entries[0].Percent != "50.00" {
		t.Fatalf("max percentage must win across cases: %+v", cfg.SplitClients)
	}
}

func TestMirror_UnresolvedBackendKeepsRouteFunctional(t *testing.T) {
	rule := mirrorRule("/m", mirrorFilter("missing", 8080, nil, nil))
	g, cfg := graphOf(t, nil, rule)

	// Route stays functional: normal proxying to the main backend.
	loc := findLocation(t, cfg, "= /m")
	if loc.Upstream != "default_svc_8080" {
		t.Fatalf("route must keep serving its main backend, got %q", loc.Upstream)
	}
	if len(loc.Mirrors) != 0 {
		t.Fatalf("unresolved mirror must be dropped, got %+v", loc.Mirrors)
	}
	for _, s := range cfg.Servers {
		for _, l := range s.Locations {
			if strings.HasPrefix(l.Path, "= /hng_mirror_") {
				t.Fatalf("no mirror location may be emitted: %q", l.Path)
			}
		}
	}
	for _, u := range cfg.Upstreams {
		if u.Name == "default_missing_8080" {
			t.Fatalf("unresolved mirror backend must not be configured")
		}
	}
	// Spec: ResolvedRefs=False with BackendNotFound.
	pi := g.Routes[0].Parents[0]
	if pi.ResolvedRefs {
		t.Fatalf("unresolved mirror backendRef must set ResolvedRefs=False")
	}
	if pi.RefsReason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Fatalf("RefsReason = %q, want BackendNotFound", pi.RefsReason)
	}
	// GEP-1364 static-500 must NOT leak into the rule: only the mirror
	// failed, normal handling continues.
	for _, b := range g.Routes[0].Rules[0].Backends {
		if IsStaticUpstream(b.Upstream) {
			t.Fatalf("main backend must be unaffected by the mirror failure")
		}
	}
}

func TestMirror_BackendRequestHeaderModifier_AppliedToMainBackend(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/b")},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: backendRefTo("svc", 8080).BackendRef,
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set:    []gatewayv1.HTTPHeader{{Name: "Backend", Value: "svc"}},
					Remove: []string{"X-Drop"},
				},
			}},
		}},
	}
	_, cfg := graphOf(t, nil, rule)

	loc := findLocation(t, cfg, "= /b")
	var hasSet, hasRemove bool
	for _, h := range loc.RequestHeaders {
		if h.Name == "Backend" && h.Value == "svc" {
			hasSet = true
		}
		if h.Name == "X-Drop" && h.Value == "" {
			hasRemove = true
		}
	}
	if !hasSet || !hasRemove {
		t.Fatalf("backendRef-level modifier must reach the location: %+v", loc.RequestHeaders)
	}
}

func TestMirror_BackendRequestHeaderModifier_OverridesRuleLevelOnSameName(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/b")},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set: []gatewayv1.HTTPHeader{{Name: "X-Both", Value: "rule"}},
			},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: backendRefTo("svc", 8080).BackendRef,
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set: []gatewayv1.HTTPHeader{{Name: "x-both", Value: "backend"}},
				},
			}},
		}},
	}
	_, cfg := graphOf(t, nil, rule)

	loc := findLocation(t, cfg, "= /b")
	count := 0
	for _, h := range loc.RequestHeaders {
		if strings.EqualFold(h.Name, "X-Both") {
			count++
			if h.Value != "backend" {
				t.Fatalf("backend-level entry must win on the same header: %+v", loc.RequestHeaders)
			}
		}
	}
	if count != 1 {
		t.Fatalf("same-name entries must collapse to one, got %d: %+v", count, loc.RequestHeaders)
	}
}

func TestMirror_MultiBackendRuleWithBackendFilterDropped(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/b")},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr(gatewayv1.PortNumber(8080))},
					Weight:                 ptr(int32(1)),
				},
				Filters: []gatewayv1.HTTPRouteFilter{{
					Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
					RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "A", Value: "b"}}},
				}},
			},
			{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc2", Port: ptr(gatewayv1.PortNumber(8080))},
				Weight:                 ptr(int32(1)),
			}},
		},
	}
	g, _ := graphOf(t, []string{"svc2"}, rule)
	ri := g.Routes[0].Rules[0]
	if ri.Valid {
		t.Fatalf("multi-backend rule with backendRef filters must be dropped (documented deviation)")
	}
	if !strings.Contains(ri.InvalidMsg, "single-backend") {
		t.Fatalf("unexpected reason: %q", ri.InvalidMsg)
	}
}

func TestMirror_BackendRefFilterUnsupportedTypeDropped(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/b")},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: backendRefTo("svc", 8080).BackendRef,
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
				ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set: []gatewayv1.HTTPHeader{{Name: "A", Value: "b"}},
				},
			}},
		}},
	}
	g, _ := graphOf(t, nil, rule)
	if g.Routes[0].Rules[0].Valid {
		t.Fatalf("backendRef-level ResponseHeaderModifier must be rejected")
	}
}

func TestMirror_RuleLevelModifierAppliesToMirroredCopy(t *testing.T) {
	twenty := int32(20)
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/m")},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set: []gatewayv1.HTTPHeader{{Name: "X-Header-Set", Value: "set-overwrites-values"}},
				},
			},
			mirrorFilter("mirror-svc", 8080, &twenty, nil),
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendRefTo("svc", 8080)},
	}
	_, cfg := graphOf(t, []string{"mirror-svc"}, rule)

	loc := findLocation(t, cfg, "= /m")
	ml := findLocation(t, cfg, "= "+loc.Mirrors[0].Path)
	found := false
	for _, h := range ml.RequestHeaders {
		if h.Name == "X-Header-Set" && h.Value == "set-overwrites-values" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mirror location must carry the rule-level request modifier: %+v", ml.RequestHeaders)
	}
}
