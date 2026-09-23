package errs

import (
	"errors"
	"strings"
)

// ConditionType is the metav1.Condition.Type string used by the Gateway API
// resources the controller writes to. We define only the types the status
// builder (internal/status) needs from this package; the controller lane
// owns the actual metav1.Condition construction.
type ConditionType string

const (
	// ConditionAccepted is the Gateway/HTTPRoute Accepted condition.
	ConditionAccepted ConditionType = "Accepted"
	// ConditionProgrammed is the Gateway Programmed condition.
	ConditionProgrammed ConditionType = "Programmed"
	// ConditionResolvedRefs is the HTTPRoute ResolvedRefs condition.
	ConditionResolvedRefs ConditionType = "ResolvedRefs"
	// ConditionConflicted is the per-listener Conflicted condition.
	ConditionConflicted ConditionType = "Conflicted"
)

// Status is the metav1.Condition.Status value.
type Status string

const (
	StatusTrue  Status = "True"
	StatusFalse Status = "False"
)

// Reason is the metav1.Condition.Reason value. Values mirror the standard
// Gateway API reasons enumerated in §3.4.
type Reason string

const (
	// ReasonInvalid is used when the controller finds the configuration
	// invalid (e.g. include missing, reload failure, validation failure).
	ReasonInvalid Reason = "Invalid"

	// ReasonPending is used when work cannot complete yet but is expected to
	// (e.g. nginx master is not running — DESIGN.md §6).
	ReasonPending Reason = "Pending"

	// ReasonProgrammed is the positive terminal reason on Programmed when
	// reload succeeds.
	ReasonProgrammed Reason = "Programmed"

	// ReasonAccepted is the positive reason on Accepted / ResolvedRefs when
	// the resource is accepted.
	ReasonAccepted Reason = "Accepted"

	// ReasonBackendNotFound is the ResolvedRefs reason when a backendRef
	// points at no Service (GEP-1364).
	ReasonBackendNotFound Reason = "BackendNotFound"

	// ReasonRefNotPermitted is the ResolvedRefs reason for cross-namespace
	// references (DESIGN.md §3.5).
	ReasonRefNotPermitted Reason = "RefNotPermitted"

	// ReasonNoMatchingListener is the HTTPRoute Accepted reason when no
	// listener intersects the route's hostnames.
	ReasonNoMatchingListener Reason = "NoMatchingListener"

	// ReasonNotAllowedByListeners is the HTTPRoute Accepted reason when
	// hostnames don't intersect.
	ReasonNotAllowedByListeners Reason = "NotAllowedByListeners"

	// ReasonHostnameConflict is the per-listener Conflicted reason when
	// port+hostname overlaps within or across Gateways (DESIGN.md §3.2).
	ReasonHostnameConflict Reason = "HostnameConflict"

	// ReasonProtocolConflict is the per-listener Conflicted reason for
	// protocol-level overlaps.
	ReasonProtocolConflict Reason = "ProtocolConflict"

	// ReasonPortUnavailable is the per-listener Conflicted reason when a
	// port is already in use elsewhere.
	ReasonPortUnavailable Reason = "PortUnavailable"
)

// Mapping is the (type, status, reason, message) tuple the status builder
// uses to populate one metav1.Condition.
type Mapping struct {
	Type    ConditionType
	Status  Status
	Reason  Reason
	Message string
}

// truncate caps a status message at a conservative size so that long nginx
// stderr lines cannot bloat the status patch. 1 KiB is enough room for the
// truncate-tail + ANSI-relevant noise while staying well within the
// status-subresource limits.
const maxMessageLen = 1024

// StatusReason translates an arbitrary error (typically one returned
// through the typed-error helpers in this package) into the (condition
// type, status, reason, message) tuple required by DESIGN.md §3.4.
//
// The function is purely a translator: it does not log, observe, or call
// out to Kubernetes. It only inspects errors.Is / errors.As against the
// sentinels declared above and walks the wrap chain with errors.Unwrap.
//
// When err is nil, StatusReason returns the Programmed=True / Programmed
// terminal mapping. When err does not match any known sentinel, the
// mapping falls back to Programmed=False / Invalid / "internal error" so
// that a swallowed panic never silently sets Programmed=True.
func StatusReason(err error) Mapping {
	if err == nil {
		return Mapping{
			Type:    ConditionProgrammed,
			Status:  StatusTrue,
			Reason:  ReasonProgrammed,
			Message: "configuration applied successfully",
		}
	}

	msg := err.Error()

	switch {
	case errors.Is(err, ErrIncludeMissing):
		return Mapping{
			Type:    ConditionAccepted,
			Status:  StatusFalse,
			Reason:  ReasonInvalid,
			Message: truncate(msg),
		}

	case errors.Is(err, ErrReload):
		return Mapping{
			Type:    ConditionProgrammed,
			Status:  StatusFalse,
			Reason:  ReasonInvalid,
			Message: truncate(msg),
		}

	case errors.Is(err, ErrNginxNotRunning):
		return Mapping{
			Type:    ConditionProgrammed,
			Status:  StatusFalse,
			Reason:  ReasonPending,
			Message: truncate(msg),
		}

	case errors.Is(err, ErrValidation):
		return Mapping{
			Type:    ConditionAccepted,
			Status:  StatusFalse,
			Reason:  ReasonInvalid,
			Message: truncate(msg),
		}
	}

	// Unknown error class — never silently Programmed=True.
	return Mapping{
		Type:    ConditionProgrammed,
		Status:  StatusFalse,
		Reason:  ReasonInvalid,
		Message: truncate("internal error: " + msg),
	}
}

// truncate shortens a message and appends an ellipsis so the consumer can
// see the truncation happened. Strips trailing newlines first so the
// rendered patch is tidy.
func truncate(s string) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen-1] + "…"
}
