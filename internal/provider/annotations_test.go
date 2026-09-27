// Annotation-namespace migration (v0.2.0) and danger-flag tests
// (DESIGN-multinode-addresses.md §0 / §5):
//   - own annotations moved to hng.victrid.dev/<name>; the legacy
//     gateway.host-nginx/<name> spelling is still read when the new-style
//     key is absent, recording a deprecation warning; new-style wins on
//     conflict;
//   - snippet / extra-file annotations are ignored with a recorded warning
//     when their danger flag is off, honored when on;
//   - extra-files resolution: same-namespace refs only, missing refs
//     skipped, entries keyed files/<ns>_<name>/<key>.
package provider

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

func warningsContaining(g *Graph, substr string) []string {
	var out []string
	for _, w := range g.Warnings {
		if strings.Contains(w, substr) {
			out = append(out, w)
		}
	}
	return out
}

// addObjects appends test objects to a Resources snapshot (the same type
// switch as the build() helper, but against a caller-owned snapshot so
// tests can pass GraphOptions to BuildGraph).
func addObjects(t *testing.T, res *Resources, objs ...any) {
	t.Helper()
	for _, o := range objs {
		switch v := o.(type) {
		case *gatewayv1.GatewayClass:
			res.GatewayClasses = append(res.GatewayClasses, v)
		case *gatewayv1.Gateway:
			res.Gateways = append(res.Gateways, v)
		case *gatewayv1.HTTPRoute:
			res.HTTPRoutes = append(res.HTTPRoutes, v)
		case *corev1.Secret:
			res.Secrets = append(res.Secrets, v)
		case *corev1.ConfigMap:
			res.ConfigMaps = append(res.ConfigMaps, v)
		case *corev1.Service:
			res.Services = append(res.Services, v)
		case *discoveryv1.EndpointSlice:
			res.EndpointSlices = append(res.EndpointSlices, v)
		default:
			t.Fatalf("unsupported test object %T", o)
		}
	}
}

func TestAnnotationMigration_ListenAddresses(t *testing.T) {
	mk := func(ann map[string]string) *gatewayv1.Gateway {
		gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, host("example.com")))
		gw.Annotations = ann
		return gw
	}
	cases := []struct {
		name    string
		ann     map[string]string
		want    []string
		wantDep bool
	}{
		{"new namespace", map[string]string{ListenAddressesAnnotation: "192.168.1.10"}, []string{"192.168.1.10"}, false},
		{"legacy fallback", map[string]string{"gateway.host-nginx/listen-addresses": "192.168.1.10"}, []string{"192.168.1.10"}, true},
		{"both: new wins", map[string]string{
			ListenAddressesAnnotation:             "10.0.0.1",
			"gateway.host-nginx/listen-addresses": "192.168.1.10",
		}, []string{"10.0.0.1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := build(t, testClass("c", ControllerName, 1), mk(tc.ann))
			li := listenerOf(t, gatewayOf(t, g, "default", "gw"), "web")
			if len(li.Addresses) != len(tc.want) {
				t.Fatalf("addresses = %v, want %v", li.Addresses, tc.want)
			}
			for i := range tc.want {
				if li.Addresses[i] != tc.want[i] {
					t.Fatalf("addresses = %v, want %v", li.Addresses, tc.want)
				}
			}
			deps := warningsContaining(g, "listen-addresses")
			if tc.wantDep && len(deps) != 1 {
				t.Fatalf("deprecation warning missing: %v", g.Warnings)
			}
			if !tc.wantDep && len(deps) != 0 {
				t.Fatalf("unexpected deprecation warning: %v", deps)
			}
		})
	}
}

func TestAnnotationMigration_PublishAddresses(t *testing.T) {
	gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, host("example.com")))
	gw.Annotations = map[string]string{"gateway.host-nginx/publish-addresses": "203.0.113.9"}
	g := build(t, testClass("c", ControllerName, 1), gw)
	info := gatewayOf(t, g, "default", "gw")
	if info.PublishAddresses != "203.0.113.9" {
		t.Fatalf("PublishAddresses = %q", info.PublishAddresses)
	}
	if len(warningsContaining(g, "publish-addresses is deprecated")) != 1 {
		t.Fatalf("deprecation warning missing: %v", g.Warnings)
	}
	addrs := info.StatusAddresses(nil, nil)
	if len(addrs) != 1 || addrs[0].Value != "203.0.113.9" {
		t.Fatalf("status addresses = %+v", addrs)
	}

	// New-style wins over legacy on conflict.
	gw2 := testGateway("default", "gw2", "c", 1, plainListener("web", 80, host("example.com")))
	gw2.Annotations = map[string]string{
		PublishAddressesAnnotation:             "198.51.100.1",
		"gateway.host-nginx/publish-addresses": "203.0.113.9",
	}
	g2 := build(t, testClass("c", ControllerName, 1), gw2)
	if got := gatewayOf(t, g2, "default", "gw2").PublishAddresses; got != "198.51.100.1" {
		t.Fatalf("new-style must win, got %q", got)
	}
	if len(warningsContaining(g2, "publish-addresses is deprecated")) != 0 {
		t.Fatalf("no deprecation warning expected when new-style present: %v", g2.Warnings)
	}
}

func TestAnnotationMigration_LegacySnippetsNotHonored(t *testing.T) {
	// The danger annotations exist ONLY in the new namespace: legacy
	// spellings are unknown keys and have no effect regardless of flags.
	gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, host("example.com")))
	gw.Annotations = map[string]string{"gateway.host-nginx/server-snippet": "return 410;"}
	route := testRoute("default", "r", 1, []gatewayv1.Hostname{"example.com"},
		[]gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080))
	route.Annotations = map[string]string{"gateway.host-nginx/location-snippet": "return 410;"}
	route.Annotations["gateway.host-nginx/extra-files"] = "configmap:default/cm"

	res := &Resources{}
	addObjects(t, res, testClass("c", ControllerName, 1), gw, route)
	g := BuildGraph(res, GraphOptions{AllowNginxSnippets: true, AllowExtraFiles: true})
	cfg := g.Configuration()
	for _, s := range cfg.Servers {
		if s.RawServerSnippet != "" {
			t.Fatalf("legacy server-snippet must not be honored, got %q", s.RawServerSnippet)
		}
		for _, l := range s.Locations {
			if l.RawSnippet != "" {
				t.Fatalf("legacy location-snippet must not be honored, got %q", l.RawSnippet)
			}
		}
	}
	if len(cfg.ExtraFiles) != 0 {
		t.Fatalf("legacy extra-files must not be honored, got %+v", cfg.ExtraFiles)
	}
}

func TestDangerFlags_OffIgnoresAnnotationsWithWarning(t *testing.T) {
	gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, host("example.com")))
	gw.Annotations = map[string]string{
		ServerSnippetAnnotation: "    sub_filter_types text/css;",
		ExtraFilesAnnotation:    "configmap:default/cm",
	}
	route := testRoute("default", "r", 1, []gatewayv1.Hostname{"example.com"},
		[]gatewayv1.ParentReference{gwParent("gw")}, backendRule("svc", 8080))
	route.Annotations = map[string]string{LocationSnippetAnnotation: "        proxy_buffering off;"}

	objs := []any{
		testClass("c", ControllerName, 1),
		gw, route,
		testSlice("default", "svc", 8080, nil, "10.0.0.1"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cm"},
			Data: map[string]string{"app.lua": "-- lua"}},
	}
	res := &Resources{}
	for _, o := range objs {
		addObjects(t, res, o)
	}

	g := BuildGraph(res, GraphOptions{}) // both flags off
	cfg := g.Configuration()
	for _, s := range cfg.Servers {
		if s.RawServerSnippet != "" {
			t.Fatalf("server-snippet honored with flag off: %q", s.RawServerSnippet)
		}
		for _, l := range s.Locations {
			if l.RawSnippet != "" {
				t.Fatalf("location-snippet honored with flag off: %q", l.RawSnippet)
			}
		}
	}
	if len(cfg.ExtraFiles) != 0 {
		t.Fatalf("extra-files honored with flag off: %+v", cfg.ExtraFiles)
	}
	// Each ignored annotation produced exactly one warning naming the flag.
	for _, substr := range []string{
		"--dangerously-allow-nginx-snippets is off",
		"--dangerously-allow-extra-files is off",
	} {
		if n := len(warningsContaining(g, substr)); n != 2 && n != 1 {
			// snippet flag gate covers both server- and location-snippet
			// warnings; extra-files its own.
			t.Fatalf("warnings for %q = %d: %v", substr, n, g.Warnings)
		}
	}
}

func TestDangerFlags_OnPopulatesIR(t *testing.T) {
	gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, host("example.com")))
	gw.Annotations = map[string]string{
		ServerSnippetAnnotation: "    sub_filter_types text/css;",
		ExtraFilesAnnotation:    "configmap:default/cm,secret:default/chain",
	}
	route := testRoute("default", "r", 1, []gatewayv1.Hostname{"example.com"},
		[]gatewayv1.ParentReference{gwParent("gw")},
		pathBackendRule(gatewayv1.PathMatchPathPrefix, "/api", "svc", 8080),
		backendRule("svc", 8080),
	)
	route.Annotations = map[string]string{LocationSnippetAnnotation: "        proxy_buffering off;"}

	res := &Resources{}
	for _, o := range []any{
		testClass("c", ControllerName, 1),
		gw, route,
		testSlice("default", "svc", 8080, nil, "10.0.0.1"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cm"},
			Data:       map[string]string{"app.lua": "-- lua"},
			BinaryData: map[string][]byte{"blob.bin": []byte{0x00, 0x01}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "chain"},
			Data: map[string][]byte{"ca.crt": []byte("CA")}},
	} {
		addObjects(t, res, o)
	}

	g := BuildGraph(res, GraphOptions{AllowNginxSnippets: true, AllowExtraFiles: true})
	cfg := g.Configuration()

	// Server snippet present on every server block rendered from the Gateway.
	if len(cfg.Servers) == 0 {
		t.Fatal("no servers")
	}
	for _, s := range cfg.Servers {
		if s.RawServerSnippet != "    sub_filter_types text/css;" {
			t.Fatalf("server snippet = %q", s.RawServerSnippet)
		}
	}
	// Both rules' locations carry the route-level snippet.
	snippetLocs := 0
	for _, s := range cfg.Servers {
		for _, l := range s.Locations {
			if l.RawSnippet == "        proxy_buffering off;" {
				snippetLocs++
			}
		}
	}
	if snippetLocs == 0 {
		t.Fatal("no location carries the route snippet")
	}
	// Extra files: one entry per data key, deterministic paths.
	var paths []string
	for _, ef := range cfg.ExtraFiles {
		paths = append(paths, ef.Path)
	}
	want := []string{
		"files/default_chain/ca.crt",
		"files/default_cm/app.lua",
		"files/default_cm/blob.bin",
	}
	if len(paths) != len(want) {
		t.Fatalf("extra files = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("extra files = %v, want %v (sorted)", paths, want)
		}
	}
	byPath := map[string]*contract.ExtraFile{}
	for _, ef := range cfg.ExtraFiles {
		byPath[ef.Path] = ef
	}
	if string(byPath["files/default_cm/app.lua"].Content) != "-- lua" {
		t.Fatalf("configmap data content = %q", byPath["files/default_cm/app.lua"].Content)
	}
	if string(byPath["files/default_cm/blob.bin"].Content) != "\x00\x01" {
		t.Fatalf("configmap binaryData content = %q", byPath["files/default_cm/blob.bin"].Content)
	}
	if string(byPath["files/default_chain/ca.crt"].Content) != "CA" {
		t.Fatalf("secret content = %q", byPath["files/default_chain/ca.crt"].Content)
	}
	// No warnings recorded for the flag gates.
	if len(g.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", g.Warnings)
	}
}

func TestExtraFiles_EnforcementRules(t *testing.T) {
	mk := func(ann string, cm, secret bool) (*Graph, *GatewayInfo) {
		gw := testGateway("default", "gw", "c", 1, plainListener("web", 80, nil))
		gw.Annotations = map[string]string{ExtraFilesAnnotation: ann}
		res := &Resources{}
		for _, o := range []any{
			testClass("c", ControllerName, 1),
			gw,
		} {
			addObjects(t, res, o)
		}
		if cm {
			addObjects(t, res, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cm"},
				Data: map[string]string{"k": "v"}})
		}
		if secret {
			addObjects(t, res, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s"},
				Data: map[string][]byte{"k": []byte("v")}})
		}
		g := BuildGraph(res, GraphOptions{AllowExtraFiles: true})
		return g, gatewayOf(t, g, "default", "gw")
	}

	// Cross-namespace ref: denied with a warning, ref skipped.
	g, gwInfo := mk("configmap:other/cm", true, false)
	if len(gwInfo.ExtraFiles) != 0 {
		t.Fatalf("cross-namespace ref resolved: %+v", gwInfo.ExtraFiles)
	}
	if len(warningsContaining(g, "cross-namespace")) != 1 {
		t.Fatalf("cross-namespace warning missing: %v", g.Warnings)
	}

	// Missing object: skipped with a warning.
	g, gwInfo = mk("configmap:default/nope", false, false)
	if len(gwInfo.ExtraFiles) != 0 || len(warningsContaining(g, "not found")) != 1 {
		t.Fatalf("missing ref must be skipped with a warning: %+v %v", gwInfo.ExtraFiles, g.Warnings)
	}

	// Malformed ref and unknown kind: skipped with warnings.
	g, gwInfo = mk("garbage,service:default/cm", true, false)
	if len(gwInfo.ExtraFiles) != 0 {
		t.Fatalf("malformed refs resolved: %+v", gwInfo.ExtraFiles)
	}
	if len(warningsContaining(g, "malformed")) != 1 || len(warningsContaining(g, "unsupported kind")) != 1 {
		t.Fatalf("warnings = %v", g.Warnings)
	}

	// Namespace-less ref defaults to the Gateway's namespace.
	g, gwInfo = mk("configmap:cm,secret:s", true, true)
	if len(gwInfo.ExtraFiles) != 2 {
		t.Fatalf("same-ns shorthand refs = %+v", gwInfo.ExtraFiles)
	}

	// Duplicate refs collapse.
	g, gwInfo = mk("configmap:default/cm,configmap:cm", true, false)
	if len(gwInfo.ExtraFiles) != 1 || gwInfo.ExtraFiles[0].RelPath != "files/default_cm/k" {
		t.Fatalf("duplicate refs = %+v", gwInfo.ExtraFiles)
	}
}
