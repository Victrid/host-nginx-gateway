package provider

import (
	"context"
	"testing"
	"time"

	logr "github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/errs"
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
