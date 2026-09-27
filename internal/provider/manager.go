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
	"os"
	"sync"
	"time"

	logr "github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Victrid/HostNginxGateway/internal/contract"
	"github.com/Victrid/HostNginxGateway/internal/nodeaddrs"
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
	// (--publish-addresses, DESIGN.md §3.4) — the external override in
	// the status.addresses derivation. Below it, addresses derive from
	// the listens this node actually renders (spec.addresses
	// intersection / auto-assigned loopback), and wildcard binds fall
	// back to FallbackAddresses (the node's primary IP).
	PublishAddresses []string

	// FallbackAddresses is the wildcard-bind status.addresses value (the
	// detected node primary IP). See PublishAddresses.
	FallbackAddresses []string

	// AllowNginxSnippets enables the hng.victrid.dev/server-snippet and
	// hng.victrid.dev/location-snippet annotations
	// (--dangerously-allow-nginx-snippets, DESIGN-multinode-addresses.md
	// §5). Default false: the annotations are ignored with a warning.
	AllowNginxSnippets bool

	// AllowExtraFiles enables the hng.victrid.dev/extra-files annotation
	// (--dangerously-allow-extra-files, DESIGN-multinode-addresses.md §5).
	// Default false: the annotation is ignored with a warning.
	AllowExtraFiles bool

	// NodeAddrs supplies this node's address fingerprint for the
	// multinode ownership model (DESIGN-multinode-addresses.md §2). The
	// reconciler reads Current() at every full sync; fingerprint changes
	// fire OnChange, which Run wires to a full-sync trigger. Nil disables
	// filtering entirely (single-node semantics).
	NodeAddrs NodeAddressSource

	// NodeName identifies this node in ListenerSkippedOnNode events and
	// logs (empty falls back to the pod hostname).
	NodeName string

	// OnListenersSkipped, when set, receives the number of listeners this
	// node skipped per full sync because their spec.addresses are not
	// present on the node (the cmd wires this into the /metrics counter).
	OnListenersSkipped func(n int)

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

// NodeAddressSource is the node address fingerprint seam: the current Set
// (nil = unknown → own everything), a change callback (wired to a
// full-sync trigger) and the periodic refresh loop (run as a manager
// runnable). Implemented by *nodeaddrs.Prober.
type NodeAddressSource interface {
	Current() *nodeaddrs.Set
	OnChange(func())
	Start(ctx context.Context) error
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
	rec.AllowNginxSnippets = opts.AllowNginxSnippets
	rec.AllowExtraFiles = opts.AllowExtraFiles
	rec.NodeAddrs = opts.NodeAddrs
	rec.NodeName = opts.NodeName
	rec.Events = mgr.GetEventRecorderFor("host-nginx-gateway")
	rec.OnListenersSkipped = opts.OnListenersSkipped

	// Node address fingerprint → full-sync trigger: a fingerprint change
	// enqueues a (content-free) request through a channel source; the
	// reconciler ignores request content and rebuilds from the cache
	// (DESIGN-multinode-addresses.md §2 "变化时触发全量 reconcile").
	var nodeAddrSource source.Source
	if opts.NodeAddrs != nil {
		ch := make(chan event.TypedGenericEvent[reconcile.Request], 1)
		opts.NodeAddrs.OnChange(func() {
			select {
			case ch <- event.TypedGenericEvent[reconcile.Request]{}:
			default: // a pending trigger already covers this change
			}
		})
		nodeAddrSource = source.Channel(ch, handler.TypedFuncs[reconcile.Request, reconcile.Request]{
			GenericFunc: func(_ context.Context, _ event.TypedGenericEvent[reconcile.Request],
				q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				q.Add(reconcile.Request{})
			},
		})
		if err := mgr.Add(manager.RunnableFunc(opts.NodeAddrs.Start)); err != nil {
			return fmt.Errorf("provider: node address prober: %w", err)
		}
	}

	// Full-reconcile controller: every event on any watched type enqueues a
	// full sync; the request itself is ignored. MaxConcurrentReconciles=1
	// keeps full syncs serialized (the graph is rebuilt from scratch).
	blder := ctrl.NewControllerManagedBy(mgr).
		Named("host-nginx-gateway-full-sync").
		For(&gatewayv1.GatewayClass{}).
		Watches(&gatewayv1.Gateway{}, &handler.EnqueueRequestForObject{}).
		Watches(&gatewayv1.HTTPRoute{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Secret{}, &handler.EnqueueRequestForObject{}).
		Watches(&discoveryv1.EndpointSlice{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Service{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Namespace{}, &handler.EnqueueRequestForObject{}).
		Watches(&gatewayv1.ReferenceGrant{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.ConfigMap{}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			MaxConcurrentReconciles: 1,
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				5*time.Millisecond, time.Minute),
		})
	if nodeAddrSource != nil {
		blder = blder.WatchesRawSource(nodeAddrSource)
	}
	err = blder.Complete(rec)
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

	// AllowNginxSnippets / AllowExtraFiles gate the escape-hatch
	// annotations (see Options; DESIGN-multinode-addresses.md §5).
	AllowNginxSnippets bool
	AllowExtraFiles    bool

	// NodeAddrs is this node's address fingerprint seam (nil = no
	// fingerprint, single-node semantics). NodeName names this node in
	// ListenerSkippedOnNode events. Events is the Gateway event recorder
	// (nil-safe). OnListenersSkipped feeds the /metrics counter (nil-safe).
	NodeAddrs          NodeAddressSource
	NodeName           string
	Events             record.EventRecorder
	OnListenersSkipped func(n int)

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
	var nodeAddrs *nodeaddrs.Set
	if r.NodeAddrs != nil {
		nodeAddrs = r.NodeAddrs.Current()
	}
	res, err := ListResources(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("provider: list resources: %w", err)
	}
	graph := BuildGraph(res, GraphOptions{
		AllowNginxSnippets: r.AllowNginxSnippets,
		AllowExtraFiles:    r.AllowExtraFiles,
		NodeAddresses:      nodeAddrs,
	})
	// BuildGraph is pure: advisories (legacy annotation deprecations,
	// flag-gated annotations ignored, skipped extra-file refs) surface as
	// controller log lines here — deliberately NOT as status conditions.
	for _, w := range graph.Warnings {
		r.Log.Info(w)
	}
	// Silent-skip observability (DESIGN-multinode-addresses.md §0): every
	// listener this node does not own produces a Gateway Event naming the
	// node and the addresses it could not bind — the ownership model
	// itself never writes status for another node's listeners.
	r.reportSkippedListeners(graph)
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
	// Multinode ownership MVP: a Gateway this node does not own (zero
	// owned listeners) is left COMPLETELY untouched — the node holding
	// its addresses reports it (DESIGN-multinode-addresses.md §3).
	for _, gw := range graph.Gateways {
		if !gw.OwnedHere() {
			continue
		}
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

// reportSkippedListeners emits one ListenerSkippedOnNode Event per Gateway
// with listeners this node skipped (spec.addresses not present on this
// node — they belong to another node's nginx), and feeds the /metrics
// counter (DESIGN-multinode-addresses.md §0: "静默跳过的缓解：Kubernetes
// Event（ListenerSkippedOnNode，含节点名/地址/指纹）"). Events are
// observations, not status: they are emitted even for Gateways this node
// does not own at all.
func (r *Reconciler) reportSkippedListeners(graph *Graph) {
	skipped := 0
	for _, gw := range graph.Gateways {
		for _, li := range gw.Listeners {
			if li.Owned || !li.Valid {
				continue // owned here, or a spec error already in status
			}
			skipped++
			if r.Events != nil {
				r.Events.Eventf(gw.Resource, corev1.EventTypeNormal, "ListenerSkippedOnNode",
					"node %s does not own listener %q: addresses %v are not present on this node; another node serves it",
					r.nodeName(), li.Spec.Name, li.SkippedAddresses)
			}
		}
	}
	if skipped > 0 && r.OnListenersSkipped != nil {
		r.OnListenersSkipped(skipped)
	}
}

// nodeName resolves this node's identity for events (empty → hostname).
func (r *Reconciler) nodeName() string {
	if r.NodeName != "" {
		return r.NodeName
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "<unknown>"
	}
	return h
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

	// ConfigMaps feed the extra-files escape hatch (§5a): data keys are
	// materialised under <conf-dir>/files/.
	var configmaps corev1.ConfigMapList
	if err := c.List(ctx, &configmaps); err != nil {
		return nil, fmt.Errorf("configmaps: %w", err)
	}
	for i := range configmaps.Items {
		res.ConfigMaps = append(res.ConfigMaps, &configmaps.Items[i])
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
