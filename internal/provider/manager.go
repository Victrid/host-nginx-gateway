// Controller-runtime wiring (DESIGN.md §7): one manager, one full
// reconciler. The reconciler ignores its request entirely and rebuilds
// everything from the cache on every sync (Envoy Gateway style). Watches
// cover GatewayClass, Gateway, HTTPRoute, Secret and EndpointSlice with no
// predicates or indexes (S7). Rate limiting: a minimum 1s spacing between
// full syncs plus the workqueue's per-item exponential backoff.
package provider

import (
	"context"
	"fmt"
	"sync"
	"time"

	logr "github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/status"
)

// DefaultMinSyncInterval is the minimum spacing between full syncs
// (DESIGN.md §7). The applied-hash no-op (S6) makes redundant syncs cheap —
// a full rebuild plus a render/hash compare — so the floor mainly protects
// the dataplane from reload storms.
const DefaultMinSyncInterval = 250 * time.Millisecond

// Applier applies one rendered configuration plus the desired certificate
// set to the dataplane. Implemented by the cmd wiring on top of
// internal/dataplane (Publisher + CertsManager); errors must use the
// internal/errs taxonomy so they classify into §3.4 reasons.
type Applier interface {
	Apply(ctx context.Context, cfg *contract.Configuration, certs []Certificate) error
}

// Options configures Run.
type Options struct {
	// Applier is the dataplane seam. Nil keeps the controller running but
	// every Gateway is reported Programmed=False (Pending).
	Applier Applier

	// PublishAddresses is the operator's explicit status.addresses list
	// (--publish-addresses, DESIGN.md §3.4). Empty per-Gateway resolution:
	// the publish-addresses annotation wins, then this list, then the
	// auto-assigned loopback, then FallbackAddresses. Empty everywhere
	// leaves status.addresses untouched.
	PublishAddresses []string

	// FallbackAddresses is the last-resort status.addresses value (the
	// detected node primary IP). See PublishAddresses.
	FallbackAddresses []string

	// MinSyncInterval is the minimum spacing between full syncs; defaults
	// to 1s (DESIGN.md §7).
	MinSyncInterval time.Duration

	// MetricsBindAddress serves /metrics ("0" disables).
	MetricsBindAddress string
	// HealthProbeBindAddress serves /healthz ("0" disables).
	HealthProbeBindAddress string

	// Log receives lifecycle and sync logging.
	Log logr.Logger
}

// NewScheme builds the scheme used by the manager and fake clients in tests.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = gatewayv1.AddToScheme(s)
	return s
}

// LoadKubeConfig resolves the REST config: in-cluster ServiceAccount only
// (DaemonSet form is the only deployment form, DESIGN.md §8).
func LoadKubeConfig() (*rest.Config, error) {
	return rest.InClusterConfig()
}

// Run starts the manager and blocks until ctx is done (DESIGN.md §7: no
// leader election — single host, single instance).
func Run(ctx context.Context, opts Options) error {
	if opts.MinSyncInterval <= 0 {
		opts.MinSyncInterval = DefaultMinSyncInterval
	}
	if opts.Log.GetSink() == nil {
		opts.Log = ctrl.Log
	}

	cfg, err := LoadKubeConfig()
	if err != nil {
		return fmt.Errorf("provider: load kubeconfig: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 NewScheme(),
		Metrics:                metricsserver.Options{BindAddress: opts.MetricsBindAddress},
		HealthProbeBindAddress: opts.HealthProbeBindAddress,
	})
	if err != nil {
		return fmt.Errorf("provider: new manager: %w", err)
	}
	if opts.HealthProbeBindAddress != "" && opts.HealthProbeBindAddress != "0" {
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("provider: healthz check: %w", err)
		}
	}

	rec := NewReconciler(mgr.GetClient(), opts.Applier, opts.MinSyncInterval, opts.Log)
	rec.PublishAddresses = opts.PublishAddresses
	rec.FallbackAddresses = opts.FallbackAddresses

	// Full-reconcile controller: every event on any watched type enqueues a
	// full sync; the request itself is ignored. MaxConcurrentReconciles=1
	// keeps full syncs serialized (the graph is rebuilt from scratch).
	err = ctrl.NewControllerManagedBy(mgr).
		Named("host-nginx-gateway-full-sync").
		For(&gatewayv1.GatewayClass{}).
		Watches(&gatewayv1.Gateway{}, &handler.EnqueueRequestForObject{}).
		Watches(&gatewayv1.HTTPRoute{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Secret{}, &handler.EnqueueRequestForObject{}).
		Watches(&discoveryv1.EndpointSlice{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Service{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Namespace{}, &handler.EnqueueRequestForObject{}).
		Watches(&gatewayv1.ReferenceGrant{}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			MaxConcurrentReconciles: 1,
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				5*time.Millisecond, time.Minute),
		}).
		Complete(rec)
	if err != nil {
		return fmt.Errorf("provider: setup controller: %w", err)
	}

	opts.Log.Info("starting host-nginx-gateway controller",
		"controllerName", string(ControllerName), "minSyncInterval", opts.MinSyncInterval.String())
	return mgr.Start(ctx)
}

// ---------------------------------------------------------------------------
// Reconciler
// ---------------------------------------------------------------------------

// Reconciler is the single full-sync reconciler.
type Reconciler struct {
	Client  client.Client
	Applier Applier
	Writer  *status.Writer
	Log     logr.Logger

	// PublishAddresses / FallbackAddresses feed Gateway status.addresses
	// (§3.4; see Options).
	PublishAddresses  []string
	FallbackAddresses []string

	gate *syncGate
}

// NewReconciler builds a Reconciler (exported for cmd wiring and tests).
func NewReconciler(c client.Client, applier Applier, minSyncInterval time.Duration, log logr.Logger) *Reconciler {
	if log.GetSink() == nil {
		log = ctrl.Log
	}
	return &Reconciler{
		Client:  c,
		Applier: applier,
		Writer:  status.NewWriter(c),
		Log:     log.WithName("provider"),
		gate:    newSyncGate(minSyncInterval),
	}
}

// Reconcile performs a full sync; the request is deliberately ignored
// (DESIGN.md §7). Errors are returned so the workqueue applies exponential
// backoff; too-frequent syncs are requeued after the remaining interval.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	if wait := r.gate.reserve(); wait > 0 {
		return reconcile.Result{RequeueAfter: wait}, nil
	}
	if err := r.FullSync(ctx); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// FullSync runs one complete pipeline pass:
// list → graph → translate → apply → classify → status write-back.
func (r *Reconciler) FullSync(ctx context.Context) error {
	res, err := ListResources(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("provider: list resources: %w", err)
	}
	graph := BuildGraph(res)
	cfg := graph.Configuration()
	certs := graph.Certificates()

	var apply ApplyResult
	switch {
	case r.Applier != nil:
		applyErr := r.Applier.Apply(ctx, cfg, certs)
		apply = ApplyResultFromError(applyErr)
		if applyErr != nil {
			r.Log.Error(applyErr, "dataplane apply failed", "reason", apply.Reason)
		}
	default:
		apply = ApplyResult{
			Reason:  string(gatewayv1.GatewayReasonPending),
			Message: "dataplane applier is not configured (Programmed stays Pending)",
		}
		r.Log.Info("no dataplane applier configured; reporting Programmed=False (Pending)")
	}

	// Layer 1: GatewayClass Accepted.
	var firstErr error
	for _, ci := range graph.Classes {
		if _, err := r.Writer.UpdateGatewayClassStatus(ctx, ci.Resource, []metav1.Condition{ci.AcceptedCondition()}); err != nil {
			r.Log.Error(err, "gatewayclass status update failed", "name", ci.Resource.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	// Layer 2: Gateway + Listener conditions + status.addresses (§3.4).
	for _, gw := range graph.Gateways {
		addrs := gw.StatusAddresses(r.PublishAddresses, r.FallbackAddresses)
		_, err := r.Writer.UpdateGatewayStatus(ctx, gw.Resource,
			gw.Conditions(apply), gw.ListenerStatuses(apply), addrs)
		if err != nil {
			r.Log.Error(err, "gateway status update failed",
				"namespace", gw.Resource.Namespace, "name", gw.Resource.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	// Layer 3: HTTPRoute parent statuses.
	for _, ri := range graph.Routes {
		if _, err := r.Writer.UpdateHTTPRouteStatus(ctx, ri.Resource, ControllerName, ri.ParentStatuses()); err != nil {
			r.Log.Error(err, "httproute status update failed",
				"namespace", ri.Resource.Namespace, "name", ri.Resource.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ListResources snapshots every watched type from the (cached) client.
func ListResources(ctx context.Context, c client.Client) (*Resources, error) {
	res := &Resources{}

	var classes gatewayv1.GatewayClassList
	if err := c.List(ctx, &classes); err != nil {
		return nil, fmt.Errorf("gatewayclasses: %w", err)
	}
	for i := range classes.Items {
		res.GatewayClasses = append(res.GatewayClasses, &classes.Items[i])
	}

	var gateways gatewayv1.GatewayList
	if err := c.List(ctx, &gateways); err != nil {
		return nil, fmt.Errorf("gateways: %w", err)
	}
	for i := range gateways.Items {
		res.Gateways = append(res.Gateways, &gateways.Items[i])
	}

	var routes gatewayv1.HTTPRouteList
	if err := c.List(ctx, &routes); err != nil {
		return nil, fmt.Errorf("httproutes: %w", err)
	}
	for i := range routes.Items {
		res.HTTPRoutes = append(res.HTTPRoutes, &routes.Items[i])
	}

	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets); err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	for i := range secrets.Items {
		res.Secrets = append(res.Secrets, &secrets.Items[i])
	}

	var slices discoveryv1.EndpointSliceList
	if err := c.List(ctx, &slices); err != nil {
		return nil, fmt.Errorf("endpointslices: %w", err)
	}
	for i := range slices.Items {
		res.EndpointSlices = append(res.EndpointSlices, &slices.Items[i])
	}

	var services corev1.ServiceList
	if err := c.List(ctx, &services); err != nil {
		return nil, fmt.Errorf("services: %w", err)
	}
	for i := range services.Items {
		res.Services = append(res.Services, &services.Items[i])
	}

	var namespaces corev1.NamespaceList
	if err := c.List(ctx, &namespaces); err != nil {
		return nil, fmt.Errorf("namespaces: %w", err)
	}
	for i := range namespaces.Items {
		res.Namespaces = append(res.Namespaces, &namespaces.Items[i])
	}

	// ReferenceGrants gate cross-namespace backendRefs and listener
	// certificateRefs (§3.5). Watching them means a grant add/delete/modify
	// triggers a full sync, which re-evaluates every reference (the spec
	// requires revocation on grant deletion).
	var grants gatewayv1.ReferenceGrantList
	if err := c.List(ctx, &grants); err != nil {
		return nil, fmt.Errorf("referencegrants: %w", err)
	}
	for i := range grants.Items {
		res.ReferenceGrants = append(res.ReferenceGrants, &grants.Items[i])
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// syncGate: minimum spacing between full syncs (DESIGN.md §7)
// ---------------------------------------------------------------------------

type syncGate struct {
	mu      sync.Mutex
	min     time.Duration
	last    time.Time
	lastSet bool
}

func newSyncGate(min time.Duration) *syncGate {
	if min <= 0 {
		min = DefaultMinSyncInterval
	}
	return &syncGate{min: min}
}

// reserve claims a sync slot. A zero return means "go now"; a positive
// return is the duration the caller should requeue after.
func (g *syncGate) reserve() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if !g.lastSet || now.Sub(g.last) >= g.min {
		g.last, g.lastSet = now, true
		return 0
	}
	return g.min - now.Sub(g.last)
}
