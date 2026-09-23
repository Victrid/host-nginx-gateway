// Unit tests for HTTPRouteMatches dispatch (Gateway API v1, §3.3): method,
// header and query-param matching are compiled into nginx map chains. AND
// semantics within a match, OR across matches, and first-rule-wins
// precedence — requests that match no rule of the route answer 404.
package provider

import (
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func methodRule(method gatewayv1.HTTPMethod, path, svc string) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path:   &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr(path)},
			Method: ptr(method),
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(svc), Port: ptr(gatewayv1.PortNumber(8080))}},
		}},
	}
}

func matchLocations(t *testing.T, g *graphT) *contract.Location {
	t.Helper()
	var loc *contract.Location
	for _, s := range g.Configuration().Servers {
		for _, l := range s.Locations {
			if l.Path == "/m/" {
				loc = l
			}
		}
	}
	if loc == nil {
		t.Fatalf("/m/ location missing: %+v", g.Configuration())
	}
	return loc
}

// graphT exists purely to keep the helper signatures readable.
type graphT = Graph

func TestMethodDispatch(t *testing.T) {
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "m", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			methodRule(gatewayv1.HTTPMethodGet, "/m", "getsvc"),
			methodRule(gatewayv1.HTTPMethodPost, "/m", "postsvc"),
		),
		testSlice("default", "getsvc", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "postsvc", 8080, ptr(true), "10.0.0.2"),
	)
	loc := matchLocations(t, g)
	if loc.Upstream == "" || loc.Upstream[0] != '$' {
		t.Fatalf("method-matched location must dispatch via a map variable: %+v", loc)
	}
	maps := g.Configuration().Maps
	if len(maps) == 0 || maps[0].Source != "$request_method" {
		t.Fatalf("method map expected: %+v", maps)
	}
	entries := map[string]string{}
	for _, m := range maps {
		for _, e := range m.Entries {
			entries[m.Source+" "+e.Key] = e.Value
		}
	}
	if _, ok := entries["$request_method GET"]; !ok {
		t.Fatalf("GET entry missing: %+v", maps)
	}
}

func TestMethodDispatch_ExclusiveMatchAnswers(t *testing.T) {
	// A single method-constrained rule: GET → svc; every other method must
	// NOT route (dispatch resolves to "" → the renderer answers 404).
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "m", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			methodRule(gatewayv1.HTTPMethodGet, "/m", "getsvc"),
		),
		testSlice("default", "getsvc", 8080, ptr(true), "10.0.0.1"),
	)
	loc := matchLocations(t, g)
	if loc.Upstream == "" || loc.Upstream[0] != '$' {
		t.Fatalf("expected dispatch variable: %+v", loc)
	}
	m := g.Configuration().Maps[0]
	if m.Source != "$request_method" || len(m.Entries) != 2 {
		t.Fatalf("GET + default entries expected: %+v", m)
	}
	if m.Entries[0].Key != "GET" || m.Entries[0].Value != "default_getsvc_8080" {
		t.Fatalf("GET → upstream: %+v", m.Entries)
	}
	if m.Entries[1].Key != "" || m.Entries[1].Value != "" {
		t.Fatalf("non-matching methods must resolve to \"\" (→404): %+v", m.Entries)
	}
}

func TestHeaderDispatch_ExactAndRegex(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/m")},
			Headers: []gatewayv1.HTTPHeaderMatch{
				{Name: "X-Version", Value: "v2"},
				{Name: "X-Tenant", Type: ptr(gatewayv1.HeaderMatchRegularExpression), Value: "^org-[0-9]+$"},
			},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "hsvc", Port: ptr(gatewayv1.PortNumber(8080))}},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "h", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, rule),
		testSlice("default", "hsvc", 8080, ptr(true), "10.0.0.1"),
	)
	loc := matchLocations(t, g)
	if loc.Upstream == "" || loc.Upstream[0] != '$' {
		t.Fatalf("expected dispatch variable: %+v", loc)
	}
	// Follow the chain from the location's variable: root map keys on one
	// header; the satisfied branch descends to a second map for the other
	// header; every map's default is "" (no rule matched → 404).
	byName := map[string]*contract.MapBlock{}
	for _, m := range g.Configuration().Maps {
		byName["$"+m.Name] = m
	}
	root, ok := byName[loc.Upstream]
	if !ok || root.Source != "$http_x_tenant" {
		t.Fatalf("root map keys on the first header: %+v", g.Configuration().Maps)
	}
	if root.Entries[0].Key != "~^org-[0-9]+$" {
		t.Fatalf("regex key for RegularExpression header match: %+v", root.Entries)
	}
	if root.Entries[1].Key != "" || root.Entries[1].Value != "" {
		t.Fatalf("missing header → 404: %+v", root.Entries)
	}
	leaf, ok := byName[root.Entries[0].Value]
	if !ok || leaf.Source != "$http_x_version" {
		t.Fatalf("AND semantics: satisfied tenant descends to the version map: %+v", root.Entries)
	}
	if leaf.Entries[0].Key != "v2" || leaf.Entries[0].Value != "default_hsvc_8080" {
		t.Fatalf("v2 under matched tenant → upstream: %+v", leaf.Entries)
	}
	if leaf.Entries[1].Key != "" || leaf.Entries[1].Value != "" {
		t.Fatalf("version mismatch under matched tenant → 404: %+v", leaf.Entries)
	}
}

func TestQueryDispatch(t *testing.T) {
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/m")},
			QueryParams: []gatewayv1.HTTPQueryParamMatch{{
				Name: "animal", Type: ptr(gatewayv1.QueryParamMatchExact), Value: "whale",
			}},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "qsvc", Port: ptr(gatewayv1.PortNumber(8080))}},
		}},
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "q", 1, nil, []gatewayv1.ParentReference{gwParent("gw")}, rule),
		testSlice("default", "qsvc", 8080, ptr(true), "10.0.0.1"),
	)
	loc := matchLocations(t, g)
	if loc.Upstream == "" || loc.Upstream[0] != '$' {
		t.Fatalf("expected dispatch variable: %+v", loc)
	}
	m := g.Configuration().Maps[0]
	if m.Source != "$arg_animal" {
		t.Fatalf("query params map on $arg_<name>: %+v", m)
	}
	if m.Entries[0].Key != "whale" || m.Entries[0].Value != "default_qsvc_8080" {
		t.Fatalf("query exact match: %+v", m.Entries)
	}
}

func TestMatchPrecedence_EarlierConstrainedRuleWins(t *testing.T) {
	// rule1 (first): POST /m; rule2: /m unconstrained. POST → rule1's
	// backend; other methods fall to rule2 (GEP-722 rule precedence).
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "m", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			methodRule(gatewayv1.HTTPMethodPost, "/m", "postsvc"),
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/m", "anysvc", 8080),
		),
		testSlice("default", "postsvc", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "anysvc", 8080, ptr(true), "10.0.0.2"),
	)
	loc := matchLocations(t, g)
	m := g.Configuration().Maps[0]
	if m.Source != "$request_method" {
		t.Fatalf("method map: %+v", m)
	}
	if len(m.Entries) != 2 || m.Entries[0].Key != "POST" || m.Entries[0].Value != "default_postsvc_8080" {
		t.Fatalf("POST → rule1's backend: %+v", m.Entries)
	}
	if m.Entries[1].Key != "" || m.Entries[1].Value != "default_anysvc_8080" {
		t.Fatalf("other methods → rule2's backend: %+v", m.Entries)
	}
	_ = loc
}

func TestMatchPrecedence_SpecificMatchBeatsPlain(t *testing.T) {
	// rule1: plain /m (matches everything); rule2: POST /m. GEP-993
	// specificity: the match with MORE constraints (POST) outranks the
	// plain match, so POST → rule2 and other methods → rule1.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "m", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/m", "anysvc", 8080),
			methodRule(gatewayv1.HTTPMethodPost, "/m", "postsvc"),
		),
		testSlice("default", "postsvc", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "anysvc", 8080, ptr(true), "10.0.0.2"),
	)
	loc := matchLocations(t, g)
	m := g.Configuration().Maps[0]
	if m.Source != "$request_method" {
		t.Fatalf("method map: %+v", m)
	}
	if len(m.Entries) != 2 || m.Entries[0].Key != "POST" || m.Entries[0].Value != "default_postsvc_8080" {
		t.Fatalf("POST → the more specific rule: %+v", m.Entries)
	}
	if m.Entries[1].Key != "" || m.Entries[1].Value != "default_anysvc_8080" {
		t.Fatalf("other methods → the plain rule: %+v", m.Entries)
	}
	_ = loc
}

func TestExactSpecInsidePrefixLocationFallsThrough(t *testing.T) {
	// rule1: exact POST /m/special (its own location "= /m/special");
	// rule2: prefix /m unconstrained.
	// A POST to /m/special must hit rule1; a GET to /m/special falls
	// through to rule2 — the prefix rule covers the exact location's whole
	// URI set, so the exact location dispatches on the method.
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "m", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			methodRule(gatewayv1.HTTPMethodPost, "/m/special", "postsvc"),
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/m", "anysvc", 8080),
		),
		testSlice("default", "postsvc", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "anysvc", 8080, ptr(true), "10.0.0.2"),
	)
	var exact *contract.Location
	for _, s := range g.Configuration().Servers {
		for _, l := range s.Locations {
			if l.Path == "= /m/special" {
				exact = l
			}
		}
	}
	if exact == nil {
		t.Fatalf("= /m/special location missing: %+v", g.Configuration().Servers)
	}
	if exact.Upstream == "" || exact.Upstream[0] != '$' {
		t.Fatalf("exact location must dispatch (POST→rule1, else rule2): %+v", exact)
	}
	byName := map[string]*contract.MapBlock{}
	for _, m := range g.Configuration().Maps {
		byName["$"+m.Name] = m
	}
	root, ok := byName[exact.Upstream]
	if !ok || root.Source != "$request_method" {
		t.Fatalf("method map expected: %+v", g.Configuration().Maps)
	}
	if len(root.Entries) != 2 || root.Entries[0].Key != "POST" || root.Entries[0].Value != "default_postsvc_8080" {
		t.Fatalf("POST → rule1: %+v", root.Entries)
	}
	if root.Entries[1].Key != "" || root.Entries[1].Value != "default_anysvc_8080" {
		t.Fatalf("other methods fall through to rule2: %+v", root.Entries)
	}
}

func TestMatches_ORAcrossMatchesInOneRule(t *testing.T) {
	// Gateway API v1 (GEP-993): matches within a rule are OR'd. The live
	// conformance shape: rule1 = (path /) OR (path / + version=one) -> v1;
	// rule2 = (path /v2) OR (path / + version=two) -> v2. A plain request
	// "/" hits rule1's plain match -> v1; "/" with version=two hits rule2's
	// header match (more specific than rule1's plain match) -> v2.
	hdrMatch := func(path, val string) gatewayv1.HTTPRouteMatch {
		return gatewayv1.HTTPRouteMatch{
			Path:    &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr(path)},
			Headers: []gatewayv1.HTTPHeaderMatch{{Name: "version", Type: ptr(gatewayv1.HeaderMatchExact), Value: val}},
		}
	}
	plainMatch := func(path string) gatewayv1.HTTPRouteMatch {
		return gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr(path)}}
	}
	g := build(t,
		testClass("c", ControllerName, 1),
		testGateway("default", "gw", "c", 1, plainListener("web", 80, nil)),
		testRoute("default", "m", 1, nil, []gatewayv1.ParentReference{gwParent("gw")},
			gatewayv1.HTTPRouteRule{
				Matches:     []gatewayv1.HTTPRouteMatch{plainMatch("/"), hdrMatch("/", "one")},
				BackendRefs: []gatewayv1.HTTPBackendRef{backendOf("v1", 8080)},
			},
			gatewayv1.HTTPRouteRule{
				Matches:     []gatewayv1.HTTPRouteMatch{plainMatch("/v2"), hdrMatch("/", "two")},
				BackendRefs: []gatewayv1.HTTPBackendRef{backendOf("v2", 8080)},
			},
		),
		testSlice("default", "v1", 8080, ptr(true), "10.0.0.1"),
		testSlice("default", "v2", 8080, ptr(true), "10.0.0.2"),
	)
	byPath := map[string]*contract.Location{}
	for _, s := range g.Configuration().Servers {
		for _, l := range s.Locations {
			byPath[l.Path] = l
		}
	}
	maps := map[string]*contract.MapBlock{}
	for _, m := range g.Configuration().Maps {
		maps["$"+m.Name] = m
	}
	resolve := func(loc *contract.Location, version string) string {
		if loc.Upstream == "" {
			return "<404>"
		}
		if loc.Upstream[0] != '$' {
			return loc.Upstream // static upstream
		}
		m := maps[loc.Upstream]
		for m != nil {
			nxt := ""
			for _, e := range m.Entries {
				if e.Key == version || e.Key == "" {
					if strings.HasPrefix(e.Value, "$") {
						nxt = e.Value
						break
					}
					return e.Value
				}
			}
			m = maps[nxt]
		}
		return "<404>"
	}
	root := byPath["/"]
	if root.Upstream == "" || root.Upstream[0] != '$' {
		t.Fatalf("dispatch expected at /: %+v", root)
	}
	if got := resolve(root, ""); got != "default_v1_8080" {
		t.Fatalf("plain / must reach v1 (plain match of rule1), got %s", got)
	}
	if got := resolve(root, "one"); got != "default_v1_8080" {
		t.Fatalf("/ with version=one must reach v1, got %s", got)
	}
	if got := resolve(root, "two"); got != "default_v2_8080" {
		t.Fatalf("/ with version=two must reach the more specific match (v2), got %s", got)
	}
	v2 := byPath["/v2/"]
	if v2 == nil {
		t.Fatalf("/v2/ location missing")
	}
	if got := resolve(v2, ""); got != "default_v2_8080" {
		t.Fatalf("plain /v2 must reach v2, got %s", got)
	}
}
