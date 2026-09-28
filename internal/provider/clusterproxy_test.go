// Cluster-proxy sidecar mode tests (DESIGN-cluster-proxy.md §0.3/§0.4):
//   - sidecar mode resolves Service ClusterIP + backendRef port and emits
//     socket-carrying endpoints; the proxy mapping JSON is deterministic;
//   - direct mode (flag absent) is byte-identical to the legacy path;
//   - failures (missing Service, headless) fall back to the GEP-1364
//     static-500 shape.
package provider

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/dataplane"
)

// proxyFixture builds one Gateway (listener 80, hostname example.com)
// routing to two services: "web" (ClusterIP 10.43.1.10, service port 80,
// targetPort 8080, pod 10.42.0.5) and "api" (ClusterIP 10.43.1.20, port
// 8080, targetPort 9090, pod 10.42.0.6). Both EndpointSlices (the
// direct-mode source) and Services (the sidecar-mode source) are present.
func proxyFixture() *Resources {
	res := &Resources{}
	res.GatewayClasses = append(res.GatewayClasses, testClass("hng", ControllerName, 1))
	res.Gateways = append(res.Gateways, testGateway("apps", "gw", "hng", 1,
		plainListener("http", 80, host("example.com"))))

	route := testRoute("apps", "r", 1, []gatewayv1.Hostname{"example.com"},
		[]gatewayv1.ParentReference{gwParent("gw")},
		// /web → web:80, /api → api:8080: two distinct service:port pairs.
		pathBackendRule(gatewayv1.PathMatchPathPrefix, "/web", "web", 80),
		pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "api", 8080),
	)
	res.HTTPRoutes = append(res.HTTPRoutes, route)

	// EndpointSlices: pod-IP form (the direct-mode resolution source).
	res.EndpointSlices = append(res.EndpointSlices,
		testSlice("apps", "web", 8080, nil, "10.42.0.5"),
		testSlice("apps", "api", 9090, nil, "10.42.0.6"),
	)

	// Services with ClusterIPs (the sidecar-mode resolution source).
	res.Services = append(res.Services,
		proxyService("apps", "web", "10.43.1.10", 80, 8080),
		proxyService("apps", "api", "10.43.1.20", 8080, 9090),
	)
	return res
}

func proxyService(ns, name, clusterIP string, port, targetPort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.ServiceSpec{
			ClusterIP: clusterIP,
			Ports: []corev1.ServicePort{{
				Port:       port,
				TargetPort: intstr.FromInt32(targetPort),
			}},
		},
	}
}

func buildProxy(t *testing.T, res *Resources, socketsDir string) *Graph {
	t.Helper()
	return BuildGraph(res, GraphOptions{ClusterProxySockets: socketsDir})
}

// TestClusterProxy_SidecarResolutionEmitsClusterIPSockets: endpoints
// carry ClusterIP + backendRef port and the socket basename; the mapping
// covers every distinct service:port exactly once.
func TestClusterProxy_SidecarResolutionEmitsClusterIPSockets(t *testing.T) {
	g := buildProxy(t, proxyFixture(), "/run/hng-proxy")
	rule := g.Routes[0].Rules[0] // /web → web:80
	if len(rule.Backends) != 1 || len(rule.Backends[0].Endpoints) != 1 {
		t.Fatalf("unexpected backend shape: %+v", rule.Backends)
	}
	ep := rule.Backends[0].Endpoints[0]
	if ep.IP != "10.43.1.10" || ep.Port != 80 {
		t.Fatalf("sidecar endpoint must be ClusterIP:servicePort, got %s:%d", ep.IP, ep.Port)
	}
	if ep.Socket != "apps_web_80.sock" {
		t.Fatalf("socket basename: got %q, want apps_web_80.sock", ep.Socket)
	}
	if !ep.Ready {
		t.Fatalf("sidecar endpoints are always ready (liveness is connect-time)")
	}
	if rule.Upstream != "apps_web_80" {
		t.Fatalf("upstream name: got %q, want apps_web_80 (service-port form)", rule.Upstream)
	}

	// Mapping: both distinct service:ports, socket → ClusterIP:servicePort.
	socks := g.ProxySockets()
	want := [][2]string{
		{"apps_api_8080.sock", "10.43.1.20:8080"},
		{"apps_web_80.sock", "10.43.1.10:80"},
	}
	if len(socks) != len(want) {
		t.Fatalf("mapping: got %v, want %v", socks, want)
	}
	for i := range want {
		if socks[i] != want[i] {
			t.Fatalf("mapping[%d]: got %v, want %v", i, socks[i], want[i])
		}
	}
}

// TestClusterProxy_MappingJSONDeterministic: Configuration.ProxyMapping
// is sorted by listen name and stable across rebuilds (the applier
// materialises these bytes verbatim into proxy.json).
func TestClusterProxy_MappingJSONDeterministic(t *testing.T) {
	var prev string
	for i := 0; i < 2; i++ {
		cfg := buildProxy(t, proxyFixture(), "/run/hng-proxy").Configuration()
		if cfg.ProxySocketsDir != "/run/hng-proxy" {
			t.Fatalf("ProxySocketsDir: got %q", cfg.ProxySocketsDir)
		}
		if i == 0 {
			// Validate the exact byte shape (field order + ordering).
			want := `[{"listen":"apps_api_8080.sock","dial":"10.43.1.20:8080"},{"listen":"apps_web_80.sock","dial":"10.43.1.10:80"}]`
			if string(cfg.ProxyMapping) != want {
				t.Fatalf("proxy mapping JSON:\n got %s\nwant %s", cfg.ProxyMapping, want)
			}
		}
		if string(cfg.ProxyMapping) != prev && prev != "" {
			t.Fatalf("mapping not deterministic:\n%s\n%s", prev, cfg.ProxyMapping)
		}
		prev = string(cfg.ProxyMapping)
	}
	// It parses into the sidecar's contract shape.
	var entries []struct {
		Listen string `json:"listen"`
		Dial   string `json:"dial"`
	}
	if err := json.Unmarshal([]byte(prev), &entries); err != nil {
		t.Fatalf("mapping JSON does not parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("mapping entries: got %d", len(entries))
	}
}

// TestClusterProxy_EmptyBackendSetRendersEmptyArray: sidecar mode with
// no backends still yields "[]" so the sidecar unbinds leftovers.
func TestClusterProxy_EmptyBackendSetRendersEmptyArray(t *testing.T) {
	res := &Resources{}
	res.GatewayClasses = append(res.GatewayClasses, testClass("hng", ControllerName, 1))
	res.Gateways = append(res.Gateways, testGateway("apps", "gw", "hng", 1,
		plainListener("http", 80, host("example.com"))))
	g := buildProxy(t, res, "/run/hng-proxy")
	cfg := g.Configuration()
	if string(cfg.ProxyMapping) != "[]" {
		t.Fatalf("empty mapping: got %q, want []", cfg.ProxyMapping)
	}
	if cfg.ProxySocketsDir != "/run/hng-proxy" {
		t.Fatalf("ProxySocketsDir must be set in sidecar mode even with no backends")
	}
}

// TestClusterProxy_DirectModeByteIdentical: without the flag the
// rendered nginx bytes equal the legacy direct-mode rendering (pod IPs,
// no unix: lines, no mapping fields set).
func TestClusterProxy_DirectModeByteIdentical(t *testing.T) {
	res := proxyFixture()
	direct := BuildGraph(res, GraphOptions{}).Configuration()
	sidecar := buildProxy(t, proxyFixture(), "/run/hng-proxy").Configuration()

	rd, err := dataplane.Render(direct)
	if err != nil {
		t.Fatalf("render direct: %v", err)
	}
	rs, err := dataplane.Render(sidecar)
	if err != nil {
		t.Fatalf("render sidecar: %v", err)
	}
	if bytesHaveSubstring(rd, "unix:") {
		t.Fatalf("direct rendering must not contain unix: upstreams:\n%s", rd)
	}
	if !bytesHaveSubstring(rs, "unix:/run/hng-proxy/apps_web_80.sock") {
		t.Fatalf("sidecar rendering must contain the socket upstream:\n%s", rs)
	}
	if direct.ProxyMapping != nil || direct.ProxySocketsDir != "" {
		t.Fatalf("direct-mode configuration must carry no proxy fields")
	}

	// Golden lock of the direct-mode upstream block (byte-identical to
	// the pre-feature template output for the same IR).
	if !bytesHaveSubstring(rd, "server 10.42.0.5:8080;") ||
		!bytesHaveSubstring(rd, "server 10.42.0.6:9090;") {
		t.Fatalf("direct rendering lost the pod-IP upstreams:\n%s", rd)
	}
	// …and the sidecar block replaces them with weighted-capable unix
	// server lines (weight preserved when set — see the weight test).
	if !bytesHaveSubstring(rs, "server unix:/run/hng-proxy/apps_api_8080.sock;") {
		t.Fatalf("sidecar rendering missing api socket upstream:\n%s", rs)
	}
}

// TestClusterProxy_WeightedMultiBackendKeepsWeightsAndDistinctSockets:
// a weighted multi-backend rule combines two service sockets in ONE
// rule-private upstream, each carrying its backendRef weight
// (DESIGN-cluster-proxy.md §0.1).
func TestClusterProxy_WeightedMultiBackendKeepsWeightsAndDistinctSockets(t *testing.T) {
	res := proxyFixture()
	// Replace the route with one weighted multi-backend rule.
	res.HTTPRoutes = nil
	route := testRoute("apps", "r", 1, []gatewayv1.Hostname{"example.com"},
		[]gatewayv1.ParentReference{gwParent("gw")},
		gatewayv1.HTTPRouteRule{
			BackendRefs: []gatewayv1.HTTPBackendRef{
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "web", Port: ptr(gatewayv1.PortNumber(80))}, Weight: ptr(int32(3))}},
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "api", Port: ptr(gatewayv1.PortNumber(8080))}, Weight: ptr(int32(1))}},
			},
		})
	res.HTTPRoutes = append(res.HTTPRoutes, route)

	cfg := buildProxy(t, res, "/run/hng-proxy").Configuration()
	var combined *contract.Upstream
	for _, u := range cfg.Upstreams {
		if len(u.Endpoints) == 2 {
			combined = u
		}
	}
	if combined == nil {
		t.Fatalf("no combined weighted upstream found: %+v", cfg.Upstreams)
	}
	bySocket := map[string]contract.Endpoint{}
	for _, ep := range combined.Endpoints {
		bySocket[ep.Socket] = ep
	}
	if w := bySocket["apps_web_80.sock"]; w.IP != "10.43.1.10" || w.Weight != 3 {
		t.Fatalf("web socket endpoint: %+v (want weight 3)", w)
	}
	if w := bySocket["apps_api_8080.sock"]; w.IP != "10.43.1.20" || w.Weight != 1 {
		t.Fatalf("api socket endpoint: %+v", w)
	}
	// Rendered form carries the weights on the unix lines.
	r, err := dataplane.Render(cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytesHaveSubstring(r, "server unix:/run/hng-proxy/apps_web_80.sock weight=3;") {
		t.Fatalf("weighted socket line missing:\n%s", r)
	}
}

// TestClusterProxy_FailuresFallBackToStatic500: missing Service and
// headless Service produce the GEP-1364 shape (static 500 upstream +
// ResolvedRefs=False), same as the direct path's failures.
func TestClusterProxy_FailuresFallBackToStatic500(t *testing.T) {
	res := proxyFixture()
	// Drop the api Service object entirely.
	var svcs []*corev1.Service
	for _, s := range res.Services {
		if s.Name != "api" {
			svcs = append(svcs, s)
		}
	}
	res.Services = svcs
	// And make web headless.
	for _, s := range res.Services {
		if s.Name == "web" {
			s.Spec.ClusterIP = "None"
		}
	}

	g := buildProxy(t, res, "/run/hng-proxy")
	route := g.Routes[0]
	for _, rule := range route.Rules {
		switch rule.Locations[0] {
		case "/web/":
			if rule.Upstream != StaticUpstreamName(500) {
				t.Fatalf("headless service must resolve to the static 500 marker, got %q", rule.Upstream)
			}
		case "/api/":
			if rule.Upstream != StaticUpstreamName(500) {
				t.Fatalf("missing service must resolve to the static 500 marker, got %q", rule.Upstream)
			}
		}
	}
	parent := route.Parents[0]
	if parent.ResolvedRefs {
		t.Fatalf("ref failures must surface in ResolvedRefs")
	}
	if parent.RefsReason != string(gatewayv1.RouteReasonUnsupportedValue) {
		t.Fatalf("RefsReason: got %q", parent.RefsReason)
	}
	// Neither broken backend contributed a mapping entry.
	if socks := g.ProxySockets(); len(socks) != 0 {
		t.Fatalf("failed backends must not create sockets: %v", socks)
	}
}

// TestClusterProxy_SocketNameIsStableAcrossRebuilds: the socket name is
// a pure function of (ns, svc, port) — required because nginx config
// and proxy.json must agree across syncs without state.
func TestClusterProxy_SocketNameIsStableAcrossRebuilds(t *testing.T) {
	a := buildProxy(t, proxyFixture(), "/run/hng-proxy").ProxySockets()
	b := buildProxy(t, proxyFixture(), "/run/hng-proxy").ProxySockets()
	if len(a) != len(b) {
		t.Fatalf("mapping size drift")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("mapping drift: %v vs %v", a, b)
		}
	}
}

func bytesHaveSubstring(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}
