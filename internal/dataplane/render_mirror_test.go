// Rendered-output tests for the round-7 contract extensions: split_clients
// blocks, internal mirror locations (gate + $request_uri proxying) and
// `mirror` directives in business locations.
package dataplane

import (
	"strings"
	"testing"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// mirrorRenderConfig is the IR one percentage-gated mirror produces:
// business location "= /m" mirroring to internal "= /hng_mirror_ab12cd34"
// at 20%, plus a second unconditional mirror.
func mirrorRenderConfig() *contract.Configuration {
	return &contract.Configuration{
		SplitClients: []*contract.SplitClients{{
			Name:   "hng_sc_20aabbcc",
			Source: "$request_id",
			Entries: []contract.SplitEntry{
				{Percent: "20.00", Value: "/hng_mirror_ab12cd34"},
				{Percent: "*", Value: ""},
			},
		}},
		Upstreams: []*contract.Upstream{{
			Name:      "default_svc_8080",
			Endpoints: []contract.Endpoint{{IP: "10.0.0.1", Port: 8080, Ready: true}},
		}},
		Servers: []*contract.Server{{
			Hostname: "example.com",
			Listens:  []contract.Listen{{Port: 80}},
			Locations: []*contract.Location{
				{
					Path:     "= /m",
					Upstream: "default_svc_8080",
					Mirrors: []contract.Mirror{
						{Path: "/hng_mirror_ab12cd34"},
						{Path: "/hng_mirror_ef567890"},
					},
				},
				{
					Path:         "= /hng_mirror_ab12cd34",
					Internal:     true,
					Upstream:     "default_svc_8080",
					ProxyPassURI: "$request_uri",
					MirrorGate:   "hng_sc_20aabbcc",
				},
				{
					Path:         "= /hng_mirror_ef567890",
					Internal:     true,
					Upstream:     "default_svc_8080",
					ProxyPassURI: "$request_uri",
				},
			},
		}},
	}
}

func TestRender_SplitClientsAndMirrorLocations(t *testing.T) {
	out := render(t, mirrorRenderConfig())

	for _, want := range []string{
		// split_clients block (http context, keyed on $request_id)
		`split_clients $request_id $hng_sc_20aabbcc {`,
		`20.00% "/hng_mirror_ab12cd34";`,
		`* "";`,
		// business location: one mirror directive per target
		`mirror /hng_mirror_ab12cd34;`,
		`mirror /hng_mirror_ef567890;`,
		// gated internal mirror location
		`location = /hng_mirror_ab12cd34 {`,
		`internal;`,
		`if ($hng_sc_20aabbcc = "") {`,
		`return 204;`,
		`proxy_pass http://default_svc_8080$request_uri;`,
		// unconditional mirror location: no gate
		`location = /hng_mirror_ef567890 {`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, out)
		}
	}
	// The ungated mirror location must NOT contain a gate.
	ungated := out[strings.Index(out, "location = /hng_mirror_ef567890"):]
	if strings.Contains(ungated, "return 204") {
		t.Fatalf("ungated mirror location must not return 204:\n%s", ungated)
	}
	// split_clients is http-context: emitted before server blocks.
	if strings.Index(out, "split_clients $request_id") > strings.Index(out, "server {") {
		t.Fatalf("split_clients must precede server blocks:\n%s", out)
	}
}

func TestRender_SplitClientsBlocksRender(t *testing.T) {
	cfg := mirrorRenderConfig()
	cfg.SplitClients = append(cfg.SplitClients, &contract.SplitClients{
		Name:   "hng_sc_00aabbcc",
		Source: "$request_id",
		Entries: []contract.SplitEntry{
			{Percent: "5.00", Value: "/hng_mirror_ef567890"},
			{Percent: "*", Value: ""},
		},
	})
	out := render(t, cfg)
	for _, want := range []string{
		`split_clients $request_id $hng_sc_00aabbcc {`,
		`5.00% "/hng_mirror_ef567890";`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, out)
		}
	}
}

func TestRender_MirrorLocationPreservesHostAndHeaders(t *testing.T) {
	cfg := mirrorRenderConfig()
	// A mirrored copy carries the rule-level request modifier.
	mirrorLoc := cfg.Servers[0].Locations[1]
	mirrorLoc.RequestHeaders = []contract.Header{{Name: "X-Header-Set", Value: "v"}}
	out := render(t, cfg)

	ml := out[strings.Index(out, "location = /hng_mirror_ab12cd34"):]
	if !strings.Contains(ml, `proxy_set_header Host $http_host;`) {
		t.Fatalf("mirror location must preserve the client Host:\n%s", ml)
	}
	if !strings.Contains(ml, `proxy_set_header "X-Header-Set" "v";`) {
		t.Fatalf("mirror location must carry request modifiers:\n%s", ml)
	}
}
