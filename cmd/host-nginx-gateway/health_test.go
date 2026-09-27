// Tests for the /healthz and /metrics endpoints.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	m := &Metrics{}
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthzRejectsNonGET(t *testing.T) {
	m := &Metrics{}
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/healthz", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("post /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz status = %d, want 405", resp.StatusCode)
	}
}

func TestMetricsCounters(t *testing.T) {
	m := &Metrics{}
	m.Reconciles.Add(7)
	m.ReloadSuccesses.Add(5)
	m.ReloadFailures.Add(2)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	for _, want := range []string{
		"hng_reconciles_total 7",
		`hng_dataplane_applies_total{result="success"} 5`,
		`hng_dataplane_applies_total{result="failure"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q\ngot:\n%s", want, body)
		}
	}
}
