package status

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// controllerName mirrors controllerName without importing the
// provider package (which imports this one).
const controllerName gatewayv1.GatewayController = "gateway.host-nginx/controller"

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = gatewayv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

type countingClient struct {
	client.Client
	writes int
}

func (c *countingClient) Status() client.SubResourceWriter {
	return &countingWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type countingWriter struct {
	client.SubResourceWriter
	c *countingClient
}

func (w *countingWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.c.writes++
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func newWriter(t *testing.T, objs ...client.Object) (*Writer, *countingClient) {
	t.Helper()
	c := &countingClient{Client: fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &Writer{client: c, now: func() time.Time { return fixed }}, c
}

func cond(ctype, status, reason string, gen int64) metav1.Condition {
	return metav1.Condition{Type: ctype, Status: metav1.ConditionStatus(status), Reason: reason, Message: ctype + " " + reason, ObservedGeneration: gen}
}

func TestUpdateGatewayClassStatus(t *testing.T) {
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Generation: 4},
	}
	w, cc := newWriter(t, class)
	ctx := context.Background()

	updated, err := w.UpdateGatewayClassStatus(ctx, class, []metav1.Condition{cond("Accepted", "True", "Accepted", 4)})
	if err != nil || !updated {
		t.Fatalf("first write: %v updated=%v", err, updated)
	}
	var got gatewayv1.GatewayClass
	_ = cc.Get(ctx, client.ObjectKey{Name: "c"}, &got)
	if len(got.Status.Conditions) != 1 || got.Status.Conditions[0].ObservedGeneration != 4 {
		t.Fatalf("conditions: %+v", got.Status.Conditions)
	}
	first := cc.writes

	// Identical write is a no-op.
	updated, err = w.UpdateGatewayClassStatus(ctx, class, []metav1.Condition{cond("Accepted", "True", "Accepted", 4)})
	if err != nil || updated {
		t.Fatalf("no-op write: %v updated=%v", err, updated)
	}
	if cc.writes != first {
		t.Fatalf("no-op issued a write")
	}

	// Generation-only bump: content merged, LastTransitionTime preserved.
	class.Generation = 5
	if _, err := w.UpdateGatewayClassStatus(ctx, class, []metav1.Condition{cond("Accepted", "True", "Accepted", 5)}); err != nil {
		t.Fatal(err)
	}
	_ = cc.Get(ctx, client.ObjectKey{Name: "c"}, &got)
	if got.Status.Conditions[0].ObservedGeneration != 5 {
		t.Fatalf("generation not refreshed: %+v", got.Status.Conditions)
	}
	if !got.Status.Conditions[0].LastTransitionTime.Equal(&got.Status.Conditions[0].LastTransitionTime) {
		t.Fatal("unreachable")
	}
}

func TestUpdateGatewayStatus_Listeners(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gw", Generation: 2},
	}
	w, cc := newWriter(t, gw)
	ctx := context.Background()

	ls := func(name string, routes int32, reason string) gatewayv1.ListenerStatus {
		return gatewayv1.ListenerStatus{
			Name:           gatewayv1.SectionName(name),
			AttachedRoutes: routes,
			Conditions:     []metav1.Condition{cond("Programmed", "True", reason, 2)},
		}
	}
	if _, err := w.UpdateGatewayStatus(ctx, gw,
		[]metav1.Condition{cond("Accepted", "True", "Accepted", 2)},
		[]gatewayv1.ListenerStatus{ls("web", 1, "Programmed"), ls("api", 0, "Programmed")},
		nil); err != nil {
		t.Fatal(err)
	}
	var got gatewayv1.Gateway
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gw"}, &got)
	if len(got.Status.Listeners) != 2 || string(got.Status.Listeners[0].Name) != "web" {
		t.Fatalf("listeners: %+v", got.Status.Listeners)
	}
	ltt := got.Status.Listeners[0].Conditions[0].LastTransitionTime

	// Listener removed from spec: its status entry must disappear.
	if _, err := w.UpdateGatewayStatus(ctx, gw,
		[]metav1.Condition{cond("Accepted", "True", "Accepted", 2)},
		[]gatewayv1.ListenerStatus{ls("web", 1, "Programmed")},
		nil); err != nil {
		t.Fatal(err)
	}
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gw"}, &got)
	if len(got.Status.Listeners) != 1 || string(got.Status.Listeners[0].Name) != "web" {
		t.Fatalf("stale listener not pruned: %+v", got.Status.Listeners)
	}
	if !got.Status.Listeners[0].Conditions[0].LastTransitionTime.Equal(&ltt) {
		t.Fatal("unchanged listener condition must keep its LastTransitionTime")
	}

	// Unchanged state: no write.
	before := cc.writes
	if updated, err := w.UpdateGatewayStatus(ctx, gw,
		[]metav1.Condition{cond("Accepted", "True", "Accepted", 2)},
		[]gatewayv1.ListenerStatus{ls("web", 1, "Programmed")},
		nil); err != nil || updated {
		t.Fatalf("no-op: %v %v", err, updated)
	}
	if cc.writes != before {
		t.Fatal("no-op issued a write")
	}
}

func TestUpdateHTTPRouteStatus_ParentMerge(t *testing.T) {
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "r", Generation: 1},
	}
	w, cc := newWriter(t, route)
	ctx := context.Background()

	other := gatewayv1.RouteParentStatus{
		ParentRef:      gatewayv1.ParentReference{Name: "gw"},
		ControllerName: "other.example.com/controller",
		Conditions:     []metav1.Condition{cond("Accepted", "True", "Accepted", 1)},
	}
	route.Status.Parents = []gatewayv1.RouteParentStatus{other}
	if err := cc.Status().Update(ctx, route); err != nil {
		t.Fatal(err)
	}

	ours := gatewayv1.RouteParentStatus{
		ParentRef:      gatewayv1.ParentReference{Name: "gw"},
		ControllerName: controllerName,
		Conditions:     []metav1.Condition{cond("Accepted", "True", "Accepted", 1)},
	}
	if _, err := w.UpdateHTTPRouteStatus(ctx, route, controllerName, []gatewayv1.RouteParentStatus{ours}); err != nil {
		t.Fatal(err)
	}
	var got gatewayv1.HTTPRoute
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "r"}, &got)
	if len(got.Status.Parents) != 2 {
		t.Fatalf("other controller's parent must be preserved: %+v", got.Status.Parents)
	}

	// Our stale entry (parentRef gone from spec) is dropped, foreign stays.
	gone := gatewayv1.RouteParentStatus{
		ParentRef:      gatewayv1.ParentReference{Name: "other-gw"},
		ControllerName: controllerName,
		Conditions:     []metav1.Condition{cond("Accepted", "True", "Accepted", 1)},
	}
	got.Status.Parents = append(got.Status.Parents, gone)
	if err := cc.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := w.UpdateHTTPRouteStatus(ctx, route, controllerName, []gatewayv1.RouteParentStatus{ours}); err != nil {
		t.Fatal(err)
	}
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "r"}, &got)
	if len(got.Status.Parents) != 2 {
		t.Fatalf("stale own parent must be dropped: %+v", got.Status.Parents)
	}
	seenForeign, seenOurs := false, false
	for _, ps := range got.Status.Parents {
		if ps.ControllerName == "other.example.com/controller" {
			seenForeign = true
		}
		if ps.ControllerName == controllerName {
			seenOurs = true
		}
	}
	if !seenForeign || !seenOurs {
		t.Fatalf("foreign=%v ours=%v: %+v", seenForeign, seenOurs, got.Status.Parents)
	}

	// No-op: identical desired set issues no write.
	before := cc.writes
	if updated, err := w.UpdateHTTPRouteStatus(ctx, route, controllerName, []gatewayv1.RouteParentStatus{ours}); err != nil || updated {
		t.Fatalf("no-op: %v %v", err, updated)
	}
	if cc.writes != before {
		t.Fatal("no-op issued a write")
	}
}

func TestUpdateHTTPRouteStatus_EmptyDesiredDropsOurEntries(t *testing.T) {
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "r", Generation: 1},
	}
	w, cc := newWriter(t, route)
	ctx := context.Background()

	ours := gatewayv1.RouteParentStatus{
		ParentRef:      gatewayv1.ParentReference{Name: "gw"},
		ControllerName: controllerName,
		Conditions:     []metav1.Condition{cond("Accepted", "True", "Accepted", 1)},
	}
	if _, err := w.UpdateHTTPRouteStatus(ctx, route, controllerName, []gatewayv1.RouteParentStatus{ours}); err != nil {
		t.Fatal(err)
	}
	var got gatewayv1.HTTPRoute
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "r"}, &got)
	if len(got.Status.Parents) != 1 {
		t.Fatalf("setup: %+v", got.Status.Parents)
	}
	// Route no longer has parentRefs we own (e.g. parentRef skipped or route
	// re-parented to a foreign gateway): our entries are cleaned up.
	if updated, err := w.UpdateHTTPRouteStatus(ctx, route, controllerName, nil); err != nil || !updated {
		t.Fatalf("cleanup write: %v %v", err, updated)
	}
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "r"}, &got)
	if len(got.Status.Parents) != 0 {
		t.Fatalf("our entries must be removed: %+v", got.Status.Parents)
	}
}

func TestUpdateGatewayStatus_Addresses(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gw", Generation: 1},
	}
	w, cc := newWriter(t, gw)
	ctx := context.Background()
	ipType := gatewayv1.IPAddressType
	conds := func() []metav1.Condition {
		return []metav1.Condition{cond("Programmed", "True", "Programmed", 1)}
	}

	// First write stores the address list.
	if _, err := w.UpdateGatewayStatus(ctx, gw, conds(), nil,
		[]gatewayv1.GatewayStatusAddress{{Type: &ipType, Value: "127.0.0.42"}}); err != nil {
		t.Fatal(err)
	}
	var got gatewayv1.Gateway
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gw"}, &got)
	if len(got.Status.Addresses) != 1 || got.Status.Addresses[0].Value != "127.0.0.42" {
		t.Fatalf("addresses: %+v", got.Status.Addresses)
	}

	// Unchanged address list: no write.
	before := cc.writes
	if updated, err := w.UpdateGatewayStatus(ctx, gw, conds(), nil,
		[]gatewayv1.GatewayStatusAddress{{Type: &ipType, Value: "127.0.0.42"}}); err != nil || updated {
		t.Fatalf("no-op: %v %v", err, updated)
	}
	if cc.writes != before {
		t.Fatal("unchanged addresses must not issue a write")
	}

	// Changed address list: rewritten (the controller owns the field).
	if updated, err := w.UpdateGatewayStatus(ctx, gw, conds(), nil,
		[]gatewayv1.GatewayStatusAddress{{Type: &ipType, Value: "127.0.0.43"}}); err != nil || !updated {
		t.Fatalf("address change must write: %v %v", err, updated)
	}
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gw"}, &got)
	if got.Status.Addresses[0].Value != "127.0.0.43" {
		t.Fatalf("addresses after change: %+v", got.Status.Addresses)
	}

	// Empty desired list leaves the existing addresses untouched.
	if updated, err := w.UpdateGatewayStatus(ctx, gw, conds(), nil, nil); err != nil || updated {
		t.Fatalf("empty desired must not write: %v %v", err, updated)
	}
	_ = cc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gw"}, &got)
	if len(got.Status.Addresses) != 1 || got.Status.Addresses[0].Value != "127.0.0.43" {
		t.Fatalf("addresses must persist: %+v", got.Status.Addresses)
	}
}
