// Package status provides pure helpers for assembling the metav1.Condition
// slices the controller writes back to Gateway / HTTPRoute objects.
//
// Lane A scope (DESIGN.md §10): this file owns the pure helpers only —
// the read-modify-write controller wiring (status subresource client,
// observedGeneration tracking, patch construction) belongs to the
// controller lane and will live in sibling files in this package.
//
// All exported helpers in this file are safe for concurrent use and have
// no Kubernetes client dependencies.
package status

import (
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Builder accumulates conditions in insertion order. The zero value is
// ready to use. Builder is safe for concurrent calls to Add.
type Builder struct {
	mu          sync.Mutex
	conditions  []metav1.Condition
	clock       func() time.Time
	nowOverride time.Time // when non-zero, used instead of clock
}

// NewBuilder returns a Builder with the supplied clock function. The
// controller lane passes time.Now; tests pass a fixed clock.
func NewBuilder(now func() time.Time) *Builder {
	if now == nil {
		now = time.Now
	}
	return &Builder{clock: now}
}

// FreezeNow causes the Builder to use t for every subsequent timestamp.
// Useful for tests that want deterministic LastTransitionTime values.
func (b *Builder) FreezeNow(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nowOverride = t
}

func (b *Builder) now() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timestamp()
}

// timestamp returns the current clock reading. The caller is responsible
// for holding b.mu so that FreezeNow and the clock function cannot race.
func (b *Builder) timestamp() time.Time {
	if !b.nowOverride.IsZero() {
		return b.nowOverride.UTC()
	}
	return b.clock().UTC()
}

// Add appends a condition built from the supplied fields. The ObservedGeneration
// field is set from observedGeneration. The LastTransitionTime is set to
// the Builder's current time.
//
// If a condition with the same Type already exists and the (Status, Reason,
// Message) tuple is unchanged, the existing condition is preserved verbatim
// (including its LastTransitionTime) — this is the standard behaviour that
// prevents spurious re-transition events when a controller resyncs without
// a state change (DESIGN.md §3.4, "read-modify-write").
func (b *Builder) Add(condType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) metav1.Condition {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.timestamp()
	c := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: metav1.NewTime(now),
	}

	for i, existing := range b.conditions {
		if existing.Type != condType {
			continue
		}
		if existing.Status == status &&
			existing.Reason == reason &&
			existing.Message == message {
			// No-op: update ObservedGeneration in place but preserve
			// LastTransitionTime so we don't flap.
			b.conditions[i].ObservedGeneration = observedGeneration
			return b.conditions[i]
		}
		b.conditions[i] = c
		return c
	}
	b.conditions = append(b.conditions, c)
	return c
}

// Conditions returns a copy of the accumulated conditions, sorted by Type
// for stable output (the dataplane-side applied-hash uses the rendered
// bytes, but stable ordering keeps logs and tests deterministic).
func (b *Builder) Conditions() []metav1.Condition {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]metav1.Condition, len(b.conditions))
	copy(out, b.conditions)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Type < out[j].Type
	})
	return out
}

// Reset clears the accumulated conditions. Useful for tests.
func (b *Builder) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.conditions = nil
}

// MergeByType merges incoming into existing with read-modify-write semantics:
//
//   - existing conditions win by Type when both sides agree on (Status,
//     Reason, Message); only ObservedGeneration is refreshed. This is the
//     "no-op" case described in DESIGN.md §3.4 and prevents unnecessary
//     LastTransitionTime churn on resync.
//   - If existing has a Type not present in incoming, it is dropped — the
//     caller is the authoritative writer and stale conditions must not
//     survive across generations.
//   - If incoming has a Type not present in existing, it is appended.
//
// The result is sorted by Type so it can be diffed byte-for-byte.
func MergeByType(existing, incoming []metav1.Condition) []metav1.Condition {
	if len(incoming) == 0 {
		return nil
	}
	byType := make(map[string]metav1.Condition, len(existing))
	for _, c := range existing {
		byType[c.Type] = c
	}

	out := make([]metav1.Condition, 0, len(incoming))
	for _, inc := range incoming {
		if ext, ok := byType[inc.Type]; ok {
			if ext.Status == inc.Status && ext.Reason == inc.Reason && ext.Message == inc.Message {
				// Preserve LastTransitionTime from existing; pull other
				// fields from incoming so ObservedGeneration is fresh.
				inc.LastTransitionTime = ext.LastTransitionTime
			}
		}
		out = append(out, inc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Type < out[j].Type
	})
	return out
}

// EqualConditions reports whether two condition slices describe the same
// state. LastTransitionTime is ignored (it can legitimately differ across
// processes); the comparison is on (Type, Status, Reason, Message,
// ObservedGeneration).
func EqualConditions(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	am := make(map[string]metav1.Condition, len(a))
	for _, c := range a {
		am[c.Type] = c
	}
	for _, c := range b {
		other, ok := am[c.Type]
		if !ok {
			return false
		}
		if other.Status != c.Status || other.Reason != c.Reason ||
			other.Message != c.Message || other.ObservedGeneration != c.ObservedGeneration {
			return false
		}
	}
	return true
}
