// Package errs defines the typed-error taxonomy used across the controller
// and the mapping from typed errors to the standard condition reasons
// documented in DESIGN.md §3.4.
//
// Two audiences consume this package:
//
//  1. The K8s provider lane wraps every failure path it produces in one of
//     the typed errors declared here (ErrValidation, ErrReload,
//     ErrNginxNotRunning, ErrIncludeMissing).
//  2. The controller wiring uses StatusReason(err) to translate the typed
//     error (and any wrapped cause) into the (conditionType, reason,
//     message) tuple the status builder stores on the Gateway / Listener /
//     HTTPRoute object.
//
// All four typed errors support errors.Is via the Unwrap method, so
// callers can do `errors.Is(err, errs.ErrReload)` even when the error was
// returned through several layers of fmt.Errorf("...: %w", errs.ErrReload).
package errs

import (
	"errors"
	"fmt"
)

// Typed sentinel errors. Wrap with fmt.Errorf("...: %w", errs.ErrXxx) to
// attach context; StatusReason will unwrap and inspect the chain.
var (
	// ErrValidation covers user-input mistakes that no retry will fix:
	// unsupported protocol, missing spec.port, parse failures, ReferenceGrant
	// violations (cross-namespace refs per DESIGN.md §3.5), etc.
	ErrValidation = errors.New("validation failed")

	// ErrReload covers failures of `nginx -t` and `nginx -s reload` after
	// the rollback state machine has run (DESIGN.md §5.2). These map to
	// Gateway Programmed=False / Invalid with the truncated stderr in
	// message (N6).
	ErrReload = errors.New("nginx reload failed")

	// ErrNginxNotRunning is reported when the liveness probe (pid file +
	// kill -0) determines the nginx master is not up. The provider skips
	// reload entirely and surfaces Gateway Programmed=False / Pending
	// (DESIGN.md §6).
	ErrNginxNotRunning = errors.New("nginx master not running")

	// ErrIncludeMissing is reported when the temporary-main-config
	// validation (DESIGN.md §5.1) shows the user has no effective include
	// of the owned conf.d directory. Surfaces Gateway Accepted=False /
	// Invalid and instructs the operator to add the include directive
	// (we never auto-inject — DESIGN.md §2, S2).
	ErrIncludeMissing = errors.New("nginx main config does not include owned conf.d")
)

// typed wraps a sentinel and an optional cause. The Cause field is what gets
// returned by Unwrap so errors.Is matches the sentinel. Callers usually
// wrap via the helpers below.
type typed struct {
	sentinel error
	cause    error
	msg      string
}

func (e *typed) Error() string {
	if e.msg == "" {
		if e.cause != nil {
			return e.sentinel.Error() + ": " + e.cause.Error()
		}
		return e.sentinel.Error()
	}
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *typed) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return e.sentinel
}

// Is supports the standard sentinel chain. errors.Is walks Unwrap(), which
// returns cause (an arbitrary error) — to still match the sentinel we
// forward Is on the sentinel itself.

func (e *typed) Is(target error) bool {
	return errors.Is(e.sentinel, target)
}

// Validation wraps ErrValidation with a free-form detail message and an
// optional cause. Returned from the provider lane for user-input failures.
func Validation(detail string, cause error) error {
	return &typed{
		sentinel: ErrValidation,
		cause:    cause,
		msg:      detail,
	}
}

// Reload wraps ErrReload with the truncated stderr/stdout from
// `nginx -t` or `nginx -s reload` so it can flow into the Programmed
// condition message (N6).
func Reload(detail string, cause error) error {
	return &typed{
		sentinel: ErrReload,
		cause:    cause,
		msg:      detail,
	}
}

// NginxNotRunning wraps ErrNginxNotRunning with the probe detail
// (e.g. "pid file /run/nginx.pid missing", "kill -0 ESRCH").
func NginxNotRunning(detail string, cause error) error {
	return &typed{
		sentinel: ErrNginxNotRunning,
		cause:    cause,
		msg:      detail,
	}
}

// IncludeMissing wraps ErrIncludeMissing with the path the include was
// expected to cover.
func IncludeMissing(path string, cause error) error {
	return &typed{
		sentinel: ErrIncludeMissing,
		cause:    cause,
		msg:      fmt.Sprintf("expected effective include of %q", path),
	}
}
