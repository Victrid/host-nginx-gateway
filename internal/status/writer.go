// Status write-back wiring (DESIGN.md §3.4, three layers): GatewayClass
// Accepted; Gateway Accepted/Programmed + status.listeners[].conditions;
// HTTPRoute status.parents[]. All writes go through the status subresource
// with a read-modify-write cycle, merged by condition type,
// observedGeneration = metadata.generation, and skipped when nothing
// changed. The pure condition helpers live in builder.go (Lane A).
package status

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Writer performs status subresource updates against a controller-runtime
// client (the manager's client: cache-backed reads, direct writes).
type Writer struct {
	client client.Client
	now    func() time.Time
}

// NewWriter returns a Writer. A nil clock defaults to time.Now.
func NewWriter(c client.Client) *Writer {
	return &Writer{client: c, now: time.Now}
}

// UpdateGatewayClassStatus writes the GatewayClass Accepted condition
// (layer 1). Returns true when an update was issued.
func (w *Writer) UpdateGatewayClassStatus(ctx context.Context, class *gatewayv1.GatewayClass, conditions []metav1.Condition) (bool, error) {
	if len(conditions) == 0 {
		return false, nil
	}
	current := &gatewayv1.GatewayClass{}
	if err := w.client.Get(ctx, types.NamespacedName{Name: class.Name}, current); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	merged := MergeByType(current.Status.Conditions, w.stamp(conditions))
	if EqualConditions(current.Status.Conditions, merged) {
		return false, nil // no-op (DESIGN.md §3.4)
	}
	current.Status.Conditions = merged
	return true, w.client.Status().Update(ctx, current)
}

// UpdateGatewayStatus writes the Gateway conditions (layer 2a), the
// per-listener statuses (layer 2b) and status.addresses (Gateway API v1:
// the addresses the implementation assigned to the Gateway, DESIGN.md
// §3.4). Listener status entries are keyed by listener name: existing
// entries for listeners that disappeared from the spec are dropped; entries
// from other writers are left untouched (their condition merge still
// applies per type). Addresses are owned by this controller: a non-empty
// desired set replaces whatever is present; an empty set leaves existing
// addresses untouched (nothing authoritative known).
func (w *Writer) UpdateGatewayStatus(ctx context.Context, gw *gatewayv1.Gateway, conditions []metav1.Condition, listeners []gatewayv1.ListenerStatus, addresses []gatewayv1.GatewayStatusAddress) (bool, error) {
	current := &gatewayv1.Gateway{}
	if err := w.client.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}, current); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	changed := false
	merged := MergeByType(current.Status.Conditions, w.stamp(conditions))
	if !EqualConditions(current.Status.Conditions, merged) {
		changed = true
	}
	current.Status.Conditions = merged

	desired := make([]gatewayv1.ListenerStatus, 0, len(listeners))
	currentByName := map[string]gatewayv1.ListenerStatus{}
	for _, ls := range current.Status.Listeners {
		currentByName[string(ls.Name)] = ls
	}
	for _, ls := range listeners {
		if prev, ok := currentByName[string(ls.Name)]; ok {
			ls.Conditions = MergeByType(prev.Conditions, w.stamp(ls.Conditions))
		} else {
			ls.Conditions = w.stamp(ls.Conditions)
		}
		desired = append(desired, ls)
	}
	if !equalListenerStatuses(current.Status.Listeners, desired) {
		changed = true
	}
	current.Status.Listeners = desired

	if len(addresses) > 0 && !equalAddresses(current.Status.Addresses, addresses) {
		changed = true
		current.Status.Addresses = addresses
	}

	if !changed {
		return false, nil
	}
	return true, w.client.Status().Update(ctx, current)
}

// equalAddresses compares status.addresses by (type, value), order-significant.
func equalAddresses(a, b []gatewayv1.GatewayStatusAddress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		at, bt := "", ""
		if a[i].Type != nil {
			at = string(*a[i].Type)
		}
		if b[i].Type != nil {
			bt = string(*b[i].Type)
		}
		if at != bt || a[i].Value != b[i].Value {
			return false
		}
	}
	return true
}

// UpdateHTTPRouteStatus writes status.parents[] entries owned by
// controllerName (layer 3). Parents written by other controllers are
// preserved untouched; our entries are replaced key-matched by parentRef.
func (w *Writer) UpdateHTTPRouteStatus(ctx context.Context, route *gatewayv1.HTTPRoute, controllerName gatewayv1.GatewayController, parents []gatewayv1.RouteParentStatus) (bool, error) {
	current := &gatewayv1.HTTPRoute{}
	if err := w.client.Get(ctx, types.NamespacedName{Namespace: route.Namespace, Name: route.Name}, current); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	merged := make([]gatewayv1.RouteParentStatus, 0, len(current.Status.Parents)+len(parents))
	// Other controllers' entries are preserved verbatim; ALL of our entries
	// are replaced by the desired set (the graph is authoritative — dropped
	// parentRefs must not leave stale status behind).
	for _, ps := range current.Status.Parents {
		if ps.ControllerName == controllerName {
			continue
		}
		merged = append(merged, ps)
	}
	for _, ps := range parents {
		ps.Conditions = w.stamp(ps.Conditions)
		merged = append(merged, ps)
	}

	if equalParentStatuses(current.Status.Parents, merged) {
		return false, nil
	}
	current.Status.Parents = merged
	return true, w.client.Status().Update(ctx, current)
}

// stamp fills LastTransitionTime on every condition (MergeByType preserves
// the previous timestamp when status/reason/message are unchanged).
func (w *Writer) stamp(conds []metav1.Condition) []metav1.Condition {
	now := metav1.NewTime(w.now())
	for i := range conds {
		conds[i].LastTransitionTime = now
	}
	return conds
}

// parentRefKey identifies a parentRef within one route's status.
func parentRefKey(ref gatewayv1.ParentReference) string {
	group, kind, ns := "", "", ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	if ref.Namespace != nil {
		ns = string(*ref.Namespace)
	}
	section := ""
	if ref.SectionName != nil {
		section = string(*ref.SectionName)
	}
	return group + "/" + kind + "/" + ns + "/" + string(ref.Name) + "/" + section
}

func equalListenerStatuses(a, b []gatewayv1.ListenerStatus) bool {
	if len(a) != len(b) {
		return false
	}
	bm := map[string]gatewayv1.ListenerStatus{}
	for _, ls := range b {
		bm[string(ls.Name)] = ls
	}
	for _, ls := range a {
		other, ok := bm[string(ls.Name)]
		if !ok {
			return false
		}
		if ls.AttachedRoutes != other.AttachedRoutes {
			return false
		}
		if !EqualConditions(ls.Conditions, other.Conditions) {
			return false
		}
		if !equalKinds(ls.SupportedKinds, other.SupportedKinds) {
			return false
		}
	}
	return true
}

func equalKinds(a, b []gatewayv1.RouteGroupKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind {
			return false
		}
		if !groupEqual(a[i].Group, b[i].Group) {
			return false
		}
	}
	return true
}

func groupEqual(a, b *gatewayv1.Group) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalParentStatuses(a, b []gatewayv1.RouteParentStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ControllerName != b[i].ControllerName {
			return false
		}
		if parentRefKey(a[i].ParentRef) != parentRefKey(b[i].ParentRef) {
			return false
		}
		if !EqualConditions(a[i].Conditions, b[i].Conditions) {
			return false
		}
	}
	return true
}
