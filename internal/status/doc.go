// Package status — see builder.go for the pure helpers implemented in Lane A.
//
// Other lanes will add sibling files here:
//
//   - controller wiring: status subresource client, observedGeneration
//     tracking, patch construction, conflict retries (DESIGN.md §3.4).
//   - reason helpers that translate typed errors from internal/errs into
//     the standard reasons listed in DESIGN.md §3.4.
//
// Until those files land the package is intentionally minimal and
// dependency-light.
package status
