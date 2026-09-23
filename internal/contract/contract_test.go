package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStringDoesNotPanicOnNil(t *testing.T) {
	// A defensive guard: the IR contract is consumed by the renderer in
	// later lanes, and String() is used in logs.
	cases := []struct {
		name string
		got  string
	}{
		{"Configuration", (*Configuration)(nil).String()},
		{"Server", (*Server)(nil).String()},
		{"Upstream", (*Upstream)(nil).String()},
		{"Location", (*Location)(nil).String()},
		{"ListenEmpty", Listen{}.String()},
	}
	for _, tc := range cases {
		if tc.got == "" {
			t.Errorf("%s: empty String()", tc.name)
		}
	}
}

func TestListenStringCarriesModifiers(t *testing.T) {
	cases := []struct {
		in   Listen
		want string
	}{
		{Listen{Port: 80}, "listen=*:80"},
		{Listen{Port: 443, SSL: true}, "listen=*:443 ssl"},
		{Listen{Port: 443, SSL: true, HTTP2: true}, "listen=*:443 ssl http2"},
		{Listen{Port: 8080, Address: "192.168.1.10"}, "listen=192.168.1.10:8080"},
		{Listen{Port: 8080, Address: "[::]"}, "listen=[::]:8080"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Listen%+v.String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEndpointMarksReadyVsDown(t *testing.T) {
	eUp := Endpoint{IP: "10.0.0.1", Port: 80, Ready: true}
	if !strings.Contains(eUp.String(), "(up)") {
		t.Errorf("ready endpoint should mark up: %s", eUp.String())
	}
	eDown := Endpoint{IP: "10.0.0.1", Port: 80, Ready: false}
	if !strings.Contains(eDown.String(), "(down)") {
		t.Errorf("not-ready endpoint should mark down: %s", eDown.String())
	}
}

func TestJSONTagsRoundTrip(t *testing.T) {
	// Ensures the JSON tag set is stable for applied-hash consumers.
	cfg := &Configuration{
		Servers: []*Server{{
			Hostname: "example.com",
			Listens:  []Listen{{Port: 443, SSL: true, HTTP2: true}},
			TLSCert:  "certs/ns_example.pem",
			Locations: []*Location{{
				Path:     "/",
				Upstream: "ns_svc_80",
				Timeouts: &Timeouts{Connect: "5s", Read: "30s"},
			}},
		}},
		Upstreams: []*Upstream{{
			Name: "ns_svc_80",
			Endpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080, Ready: true},
				{IP: "10.0.0.2", Port: 8080, Ready: false},
			},
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Configuration
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Servers) != 1 || got.Servers[0].Hostname != "example.com" {
		t.Errorf("server round-trip mismatch: %+v", got.Servers)
	}
	if len(got.Upstreams) != 1 || len(got.Upstreams[0].Endpoints) != 2 {
		t.Errorf("upstream round-trip mismatch: %+v", got.Upstreams)
	}
	if got.Servers[0].Listens[0].HTTP2 != true {
		t.Errorf("HTTP2 flag lost in round-trip")
	}
	if got.Servers[0].Locations[0].Timeouts == nil ||
		got.Servers[0].Locations[0].Timeouts.Connect != "5s" {
		t.Errorf("Timeouts lost in round-trip: %+v", got.Servers[0].Locations[0].Timeouts)
	}
	if got.Upstreams[0].Endpoints[1].Ready != false {
		t.Errorf("Ready=false lost in round-trip")
	}
}
