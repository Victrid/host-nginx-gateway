// Lane-local test helpers (internal/dataplane).
//
// Lane A is building internal/contract in parallel (DESIGN.md §10, step 1).
// To let Lane B's logic be unit-tested BEFORE Lane A's contract package is
// fully landed, this file declares local minimum-mirror copies of the
// contract types.
//
// In normal mode (production `go test ./internal/dataplane/...`), these
// stubs are NOT consulted because every *_test.go file imports the real
// `internal/contract` package directly. The stubs only become visible when
// this file is compiled with the build tag `lane_local_stubs`:
//
//   go test -tags lane_local_stubs ./internal/dataplane/...
//
// which is only useful when Lane A's contract is broken / unavailable and
// you want to test the rest of Lane B in isolation.
//
// IMPORTANT: this file's build tag *excludes* it from the default build,
// so the real `internal/contract` package is the canonical source of
// truth and Lane B's production code is unaffected.

//go:build lane_local_stubs

package dataplane

// Mirror of DESIGN.md §5.4 / S8 locked IR. Kept in sync manually with
// internal/contract/contract.go until Lane A's package is final.

type Listen struct {
	Port    int
	Address string
	SSL     bool
	HTTP2   bool
}

type Timeouts struct {
	Connect string
	Send    string
	Read    string
}

type Location struct {
	Path     string
	Upstream string
	Rewrite  string
	Timeouts *Timeouts
}

type Endpoint struct {
	IP    string
	Port  int
	Ready bool
}

type Upstream struct {
	Name      string
	Endpoints []Endpoint
}

type Server struct {
	Hostname  string
	Listens   []Listen
	TLSCert   string
	Locations []*Location
}

type Configuration struct {
	Servers   []*Server
	Upstreams []*Upstream
}
