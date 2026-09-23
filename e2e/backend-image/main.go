// Minimal HTTP backend for HostNginxGateway E2E: echoes a fixed body plus
// the request path so tests can assert routing through nginx →
// EndpointSlice endpoints. Build instructions: e2e/README.md.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	body := os.Getenv("BODY")
	if body == "" {
		body = "hng-e2e-backend"
	}
	addr := ":8080"
	if v := os.Getenv("LISTEN"); v != "" {
		addr = v
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s\n", body, r.URL.Path)
	})
	log.Printf("e2e backend listening on %s (body %q)", addr, body)
	log.Fatal(http.ListenAndServe(addr, nil))
}
