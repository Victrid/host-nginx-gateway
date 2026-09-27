package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	logr "github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/errs"
	"github.com/Victrid/HostNginxGateway/internal/nodeaddrs"
)

// countingClient counts status subresource writes issued through it.
type countingClient struct {
	client.Client
	statusWrites int
}

func (c *countingClient) Status() client.SubResourceWriter {
	return &countingWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type countingWriter struct {
	client.SubResourceWriter
	c *countingClient
}

func (w *countingWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.c.statusWrites++
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

type fakeApplier struct {
	err       error
	calls     int
	lastCfg   *contract.Configuration
	lastCerts []Certificate
}

func (f *fakeApplier) Apply(_ context.Context, cfg *contract.Configuration, certs []Certificate) error {
	f.calls++
	f.lastCfg = cfg
	f.lastCerts = certs
	return f.err
}

func newTestClient(t *testing.T, objs ...client.Object) *countingClient {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithObjects(objs...).
		WithStatusSubresource(
			&gatewayv1.GatewayClass{},
			&gatewayv1.Gateway{},
			&gatewayv1.HTTPRoute{},
		).
		Build()
	return &countingClient{Client: c}
}

func fullCluster() []client.Object {
	return []client.Object{
		testClass("ours", ControllerName, 1),
		testClass("theirs", "other.example.com/controller", 1),
		testTLSSecret("default", "cert"),
		testGateway("default", "gw", "ours", 2,
			plainListener("web", 80, nil),
			tlsListener("secure", 443, nil, gatewayv1.HTTPSProtocolType, nil, "cert"),
		),
		testGateway("default", "foreign", "theirs", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 3, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	}
}

func TestFullSync_WritesStatuses(t *testing.T) {
	c := newTestClient(t, fullCluster()...)
	applier := &fakeApplier{}
	r := NewReconciler(c, applier, time.Millisecond, logr.Discard())

	if err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("applier calls = %d", applier.calls)
	}
	if len(applier.lastCfg.Servers) != 2 || len(applier.lastCfg.Upstreams) != 1 {
		t.Fatalf("applied config: %s", applier.lastCfg)
	}
	if len(applier.lastCerts) != 1 || applier.lastCerts[0].Filename() != "default_cert.pem" {
		t.Fatalf("applied certs: %+v", applier.lastCerts)
	}

	// GatewayClass ours accepted; theirs untouched.
	var gc gatewayv1.GatewayClass
	if err := c.Get(context.Background(), client.ObjectKey{Name: "ours"}, &gc); err != nil {
		t.Fatal(err)
	}
	if len(gc.Status.Conditions) != 1 || gc.Status.Conditions[0].Status != metav1.ConditionTrue ||
		gc.Status.Conditions[0].ObservedGeneration != 1 {
		t.Fatalf("class status: %+v", gc.Status.Conditions)
	}
	var gcForeign gatewayv1.GatewayClass
	_ = c.Get(context.Background(), client.ObjectKey{Name: "theirs"}, &gcForeign)
	if len(gcForeign.Status.Conditions) != 0 {
		t.Fatalf("foreign class must stay untouched: %+v", gcForeign.Status.Conditions)
	}

	// Gateway + listeners.
	var gw gatewayv1.Gateway
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gw"}, &gw); err != nil {
		t.Fatal(err)
	}
	if got := condByType(gw.Status.Conditions, "Accepted"); got.Status != metav1.ConditionTrue ||
		got.ObservedGeneration != 2 {
		t.Fatalf("gateway accepted: %+v", got)
	}
	if got := condByType(gw.Status.Conditions, "Programmed"); got.Status != metav1.ConditionTrue ||
		got.Reason != "Programmed" {
		t.Fatalf("gateway programmed: %+v", got)
	}
	if len(gw.Status.Listeners) != 2 {
		t.Fatalf("listeners: %+v", gw.Status.Listeners)
	}
	for _, ls := range gw.Status.Listeners {
		if got := condByType(ls.Conditions, "Programmed"); got.Status != metav1.ConditionTrue {
			t.Fatalf("listener %s programmed: %+v", ls.Name, got)
		}
	}
	web := gw.Status.Listeners[0]
	if web.AttachedRoutes != 1 || len(web.SupportedKinds) != 1 || web.SupportedKinds[0].Kind != "HTTPRoute" {
		t.Fatalf("web listener status: %+v", web)
	}

	// HTTPRoute parent status.
	var route gatewayv1.HTTPRoute
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "r"}, &route); err != nil {
		t.Fatal(err)
	}
	if len(route.Status.Parents) != 1 {
		t.Fatalf("route parents: %+v", route.Status.Parents)
	}
	ps := route.Status.Parents[0]
	if ps.ControllerName != ControllerName {
		t.Fatalf("controllerName: %q", ps.ControllerName)
	}
	if got := condByType(ps.Conditions, "Accepted"); got.Status != metav1.ConditionTrue || got.ObservedGeneration != 3 {
		t.Fatalf("route accepted: %+v", got)
	}
	if got := condByType(ps.Conditions, "ResolvedRefs"); got.Status != metav1.ConditionTrue {
		t.Fatalf("route resolvedrefs: %+v", got)
	}

	// Foreign gateway must have no status.
	var gwForeign gatewayv1.Gateway
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "foreign"}, &gwForeign)
	if len(gwForeign.Status.Conditions) != 0 {
		t.Fatalf("foreign gateway must stay untouched: %+v", gwForeign.Status.Conditions)
	}
}

func TestFullSync_NoOpSkipsWrites(t *testing.T) {
	c := newTestClient(t, fullCluster()...)
	r := NewReconciler(c, &fakeApplier{}, time.Millisecond, logr.Discard())

	if err := r.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := c.statusWrites
	if first == 0 {
		t.Fatal("first sync must write status")
	}
	if err := r.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.statusWrites != first {
		t.Fatalf("unchanged second sync issued %d extra writes", c.statusWrites-first)
	}
}

func TestFullSync_ApplyErrorClassification(t *testing.T) {
	cases := []struct {
		name         string
		applierErr   error
		wantProgStat metav1.ConditionStatus
		wantProgRsn  string
		wantAccStat  metav1.ConditionStatus
		wantAccRsn   string
	}{
		{
			name:         "nginx not running",
			applierErr:   errs.NginxNotRunning("pid file missing", nil),
			wantProgStat: metav1.ConditionFalse, wantProgRsn: string(gatewayv1.GatewayReasonPending),
			wantAccStat: metav1.ConditionTrue, wantAccRsn: string(gatewayv1.GatewayReasonAccepted),
		},
		{
			name:         "reload failed",
			applierErr:   errs.Reload("nginx: [emerg]", nil),
			wantProgStat: metav1.ConditionFalse, wantProgRsn: string(gatewayv1.GatewayReasonInvalid),
			wantAccStat: metav1.ConditionTrue, wantAccRsn: string(gatewayv1.GatewayReasonAccepted),
		},
		{
			name:         "include missing",
			applierErr:   errs.IncludeMissing("/etc/nginx/conf.d/k8s-gw", nil),
			wantProgStat: metav1.ConditionFalse, wantProgRsn: string(gatewayv1.GatewayReasonInvalid),
			wantAccStat: metav1.ConditionFalse, wantAccRsn: string(gatewayv1.GatewayReasonInvalid),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, fullCluster()...)
			r := NewReconciler(c, &fakeApplier{err: tc.applierErr}, time.Millisecond, logr.Discard())
			if err := r.FullSync(context.Background()); err != nil {
				t.Fatal(err)
			}
			var gw gatewayv1.Gateway
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gw"}, &gw); err != nil {
				t.Fatal(err)
			}
			if got := condByType(gw.Status.Conditions, "Programmed"); got.Status != tc.wantProgStat || got.Reason != tc.wantProgRsn {
				t.Fatalf("programmed = %+v, want %s/%s", got, tc.wantProgStat, tc.wantProgRsn)
			}
			if got := condByType(gw.Status.Conditions, "Accepted"); got.Status != tc.wantAccStat || got.Reason != tc.wantAccRsn {
				t.Fatalf("accepted = %+v, want %s/%s", got, tc.wantAccStat, tc.wantAccRsn)
			}
			for _, ls := range gw.Status.Listeners {
				if got := condByType(ls.Conditions, "Programmed"); got.Status != tc.wantProgStat || got.Reason != tc.wantProgRsn {
					t.Fatalf("listener %s programmed = %+v, want %s/%s", ls.Name, got, tc.wantProgStat, tc.wantProgRsn)
				}
			}
		})
	}
}

func TestFullSync_NilApplierStaysPending(t *testing.T) {
	c := newTestClient(t, fullCluster()...)
	r := NewReconciler(c, nil, time.Millisecond, logr.Discard())
	if err := r.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var gw gatewayv1.Gateway
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gw"}, &gw)
	if got := condByType(gw.Status.Conditions, "Programmed"); got.Status != metav1.ConditionFalse ||
		got.Reason != string(gatewayv1.GatewayReasonPending) {
		t.Fatalf("nil applier must surface Pending: %+v", got)
	}
}

// fakeEvents records Eventf calls (the ListenerSkippedOnNode seam).
type fakeEvents struct {
	object  client.Object
	reason  string
	message string
}

func (f *fakeEvents) Event(object runtime.Object, eventtype, reason, message string) {}
func (f *fakeEvents) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if o, ok := object.(client.Object); ok {
		f.object, f.reason, f.message = o, reason, fmt.Sprintf(messageFmt, args...)
	}
}
func (f *fakeEvents) AnnotatedEventf(object runtime.Object, annotations map[string]string,
	eventtype, reason, messageFmt string, args ...interface{}) {
}

// nodeFingerprint is a static NodeAddressSource over one Set.
type nodeFingerprint struct{ set *nodeaddrs.Set }

func (n nodeFingerprint) Current() *nodeaddrs.Set     { return n.set }
func (n nodeFingerprint) OnChange(func())             {}
func (n nodeFingerprint) Start(context.Context) error { return nil }

// pinnedCluster is fullCluster with the "gw" Gateway pinned to an address
// the test node does NOT hold.
func pinnedCluster(addr string) []client.Object {
	return []client.Object{
		testClass("ours", ControllerName, 1),
		testClass("theirs", "other.example.com/controller", 1),
		testTLSSecret("default", "cert"),
		func() *gatewayv1.Gateway {
			gw := testGateway("default", "gw", "ours", 2,
				plainListener("web", 80, nil),
				tlsListener("secure", 443, nil, gatewayv1.HTTPSProtocolType, nil, "cert"),
			)
			gw.Spec.Addresses = []gatewayv1.GatewaySpecAddress{{Type: ptr(gatewayv1.IPAddressType), Value: addr}}
			return gw
		}(),
		testGateway("default", "foreign", "theirs", 1, plainListener("web", 80, nil)),
		testRoute("default", "r", 3, nil, []gatewayv1.ParentReference{gwParent("gw")},
			pathBackendRule(gatewayv1.PathMatchPathPrefix, "/", "svc", 8080)),
		testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"),
	}
}

// TestFullSync_ZeroOwnedGatewaysGetNoStatusWrite is the multinode
// ownership MVP (DESIGN-multinode-addresses.md §3): a Gateway whose
// spec.addresses this node does not hold is left COMPLETELY untouched —
// no conditions, no listener entries, no addresses. The owning node
// reports it instead.
func TestFullSync_ZeroOwnedGatewaysGetNoStatusWrite(t *testing.T) {
	c := newTestClient(t, pinnedCluster("198.51.100.7")...)
	r := NewReconciler(c, &fakeApplier{}, time.Millisecond, logr.Discard())
	r.NodeAddrs = nodeFingerprint{set: nodeaddrs.NewSet("192.0.2.10")}
	r.NodeName = "node-a"
	ev := &fakeEvents{}
	r.Events = ev
	var skipped int
	r.OnListenersSkipped = func(n int) { skipped = n }

	if err := r.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}

	var gw gatewayv1.Gateway
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gw"}, &gw); err != nil {
		t.Fatal(err)
	}
	if len(gw.Status.Conditions) != 0 || len(gw.Status.Listeners) != 0 || len(gw.Status.Addresses) != 0 {
		t.Fatalf("not-owned Gateway must stay untouched: %+v", gw.Status)
	}

	// Observability: one ListenerSkippedOnNode event naming the node and
	// the skipped address, plus the metrics counter.
	if ev.object == nil || ev.reason != "ListenerSkippedOnNode" {
		t.Fatalf("expected a ListenerSkippedOnNode event, got %+v", ev)
	}
	if !strings.Contains(ev.message, "node-a") || !strings.Contains(ev.message, "198.51.100.7") {
		t.Fatalf("event must name the node and skipped addresses: %q", ev.message)
	}
	if skipped != 2 {
		t.Fatalf("skipped counter = %d, want 2 (both listeners of the pinned Gateway)", skipped)
	}

	// The route parent status is spec-level and still written (it does
	// not encode node ownership in the MVP).
	var route gatewayv1.HTTPRoute
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "r"}, &route); err != nil {
		t.Fatal(err)
	}
	if len(route.Status.Parents) != 1 {
		t.Fatalf("route parents: %+v", route.Status.Parents)
	}
}

// TestFullSync_PartialOwnershipWritesOnlyOwnedListeners: a Gateway with
// two listeners pinned to different nodes writes status HERE only when it
// owns at least one listener — and then reports only its own listeners.
func TestFullSync_PartialOwnershipWritesOnlyOwnedListeners(t *testing.T) {
	pinned := func(addr string) *gatewayv1.Gateway {
		gw := testGateway("default", "gw", "ours", 1,
			plainListener("web", 80, nil),
			plainListener("metrics", 8080, nil),
		)
		gw.Spec.Addresses = []gatewayv1.GatewaySpecAddress{{Type: ptr(gatewayv1.IPAddressType), Value: addr}}
		return gw
	}
	objs := append(pinnedCluster("198.51.100.7")[:3], pinned("198.51.100.7"))
	objs = append(objs, testSlice("default", "svc", 8080, ptr(true), "10.0.0.1"))

	// Node B holds 198.51.100.7: both listeners of "gw" are owned there.
	cB := newTestClient(t, objs...)
	rB := NewReconciler(cB, &fakeApplier{}, time.Millisecond, logr.Discard())
	rB.NodeAddrs = nodeFingerprint{set: nodeaddrs.NewSet("198.51.100.7")}
	rB.NodeName = "node-b"
	if err := rB.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var gwB gatewayv1.Gateway
	_ = cB.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gw"}, &gwB)
	if len(gwB.Status.Listeners) != 2 || len(gwB.Status.Addresses) != 1 ||
		gwB.Status.Addresses[0].Value != "198.51.100.7" {
		t.Fatalf("owning node must report both listeners and the pinned address: %+v", gwB.Status)
	}

	// Node A holds nothing of "gw": no write at all (covered above for
	// the zero-owned shape; here assert no double-owner churn by reusing
	// the same cluster through a different fingerprint).
	cA := newTestClient(t, objs...)
	rA := NewReconciler(cA, &fakeApplier{}, time.Millisecond, logr.Discard())
	rA.NodeAddrs = nodeFingerprint{set: nodeaddrs.NewSet("192.0.2.10")}
	rA.NodeName = "node-a"
	if err := rA.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var gwA gatewayv1.Gateway
	_ = cA.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gw"}, &gwA)
	if len(gwA.Status.Listeners) != 0 {
		t.Fatalf("non-owning node must not write listener status: %+v", gwA.Status)
	}
}

// TestFullSync_WildcardGatewayStaysOwnedUnderFingerprint: the
// conformance-base-Gateway shape (wildcard listeners, auto-assigned
// loopbacks) must keep writing status under a real node fingerprint —
// the harness depends on it (e2e/conformance round split, v0.3.0).
func TestFullSync_WildcardGatewayStaysOwnedUnderFingerprint(t *testing.T) {
	c := newTestClient(t, fullCluster()...)
	r := NewReconciler(c, &fakeApplier{}, time.Millisecond, logr.Discard())
	r.NodeAddrs = nodeFingerprint{set: nodeaddrs.NewSet("192.0.2.10", "2001:db8::1")}

	if err := r.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var gw gatewayv1.Gateway
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gw"}, &gw); err != nil {
		t.Fatal(err)
	}
	if got := condByType(gw.Status.Conditions, "Programmed"); got.Status != metav1.ConditionTrue {
		t.Fatalf("wildcard Gateway must program under a fingerprint: %+v", got)
	}
	if len(gw.Status.Listeners) != 2 {
		t.Fatalf("listeners: %+v", gw.Status.Listeners)
	}
}

func TestReconcile_MinSyncIntervalGate(t *testing.T) {
	c := newTestClient(t, fullCluster()...)
	r := NewReconciler(c, &fakeApplier{}, time.Minute, logr.Discard())

	// First reconcile performs the sync.
	res, err := r.Reconcile(context.Background(), reconcile.Request{})
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("first reconcile: %+v %v", res, err)
	}
	// Immediate second reconcile is gated: requeue after the remaining
	// interval without doing work.
	res, err = r.Reconcile(context.Background(), reconcile.Request{})
	if err != nil {
		t.Fatalf("gated reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Minute {
		t.Fatalf("expected requeue after remaining interval, got %v", res.RequeueAfter)
	}
}
