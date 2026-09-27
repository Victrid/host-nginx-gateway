// Command host-nginx-gateway is a Kubernetes Gateway API controller that
// configures a host-local nginx instance. It runs as a DaemonSet pod on
// the node (DESIGN.md §8.1) and reconciles Gateway / HTTPRoute / Secret /
// EndpointSlice objects into /etc/nginx/conf.d/k8s-gw/*.conf files
// (DESIGN.md §1, §7).
//
// End-to-end wiring:
//
//	flags → healthz/metrics listener → dataplane (probe / certs / publish)
//	→ provider manager (watches + full reconcile + status write-back)
//
// SIGTERM/SIGINT cancel the root context; the controller-runtime manager
// stops, the HTTP server is shut down, and the process exits 0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	logr "github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Victrid/HostNginxGateway/internal/dataplane"
	"github.com/Victrid/HostNginxGateway/internal/provider"
)

// nginxPrefix/nginxMainConfig locate the user's main nginx config for the
// temporary-main-config validation (DESIGN.md §5.1). They are constants —
// not flags — because §2 fixes the co-existence layout: the user's config
// is /etc/nginx/nginx.conf and the owned dir is conf.d/k8s-gw beneath it.
const (
	nginxPrefix     = "/etc/nginx"
	nginxMainConfig = "/etc/nginx/nginx.conf"
)

// config carries the parsed flag values; a struct (instead of main-locals)
// keeps run() testable.
type config struct {
	nginxConfDir       string
	nginxBinary        string
	nginxPID           string
	healthzAddr        string
	publishAddresses   string
	nginxErrorLog      string
	allowNginxSnippets bool
	allowExtraFiles    bool
}

func main() {
	var cfg config
	// Private FlagSet (not flag.CommandLine): importing controller-runtime
	// (via internal/provider) pulls in pkg/client/config whose init()
	// registers its own global --kubeconfig flag on flag.CommandLine. A
	// private set keeps the CLI surface exactly as specified and unaffected
	// by that init.
	fs := flag.NewFlagSet("host-nginx-gateway", flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.nginxConfDir, "nginx-conf-dir", "/etc/nginx/conf.d/k8s-gw",
		"Directory the controller owns and writes generated *.conf files into.")
	fs.StringVar(&cfg.nginxBinary, "nginx-binary", "/usr/sbin/nginx",
		"Path to the nginx binary used for -t validation and -s reload.")
	fs.StringVar(&cfg.nginxPID, "nginx-pid", "/run/nginx.pid",
		"Path to the nginx master PID file used for liveness probing.")
	// 9125 keeps the health/metrics listener OFF the data-plane port space:
	// the historical ":8080" default collided with the most common Gateway
	// listener port and silently shadowed nginx binds (E2E finding).
	fs.StringVar(&cfg.healthzAddr, "healthz-addr", "127.0.0.1:9125",
		"Address the /healthz and /metrics HTTP servers listen on. "+
			"Must not overlap any Gateway listener port.")
	fs.StringVar(&cfg.publishAddresses, "publish-addresses", "",
		"Comma-separated IPs to report in Gateway status.addresses "+
			"(DESIGN.md §3.4). Default: detect the node's primary IP "+
			"(HNG_NODE_IP env in the DaemonSet form, else the interface "+
			"route default). A Gateway's hng.victrid.dev/publish-addresses "+
			"annotation overrides this per Gateway (the legacy "+
			"gateway.host-nginx/publish-addresses spelling is still read "+
			"with a deprecation warning and removed in v0.3.0).")
	fs.StringVar(&cfg.nginxErrorLog, "nginx-error-log", "",
		"Path to the error log used for reload-effect verification and "+
			"emitted as the http-context error_log directive. Defaults to "+
			"<nginx-conf-dir>/error.log (inside the owned directory — the "+
			"host nginx may otherwise log to stderr only). The literal "+
			"value \"off\" disables the verification.")
	// Danger flags (DESIGN-multinode-addresses.md §5): escape hatches for
	// raw nginx snippets and extra files. Off by default; annotation
	// writers are trusted at cluster-admin level (threat model in the
	// design doc). nginx -t + rollback remain the safety net.
	fs.BoolVar(&cfg.allowNginxSnippets, "dangerously-allow-nginx-snippets", false,
		"Escape hatch: honor the hng.victrid.dev/server-snippet (Gateway) "+
			"and hng.victrid.dev/location-snippet (HTTPRoute) annotations — "+
			"raw nginx config injected verbatim into server/location blocks. "+
			"Annotation writers must be trusted at cluster-admin level.")
	fs.BoolVar(&cfg.allowExtraFiles, "dangerously-allow-extra-files", false,
		"Escape hatch: honor the hng.victrid.dev/extra-files Gateway "+
			"annotation — same-namespace ConfigMap/Secret data keys "+
			"materialised under <nginx-conf-dir>/files/ and referenceable "+
			"from snippets via @<key>@ placeholders. Annotation writers "+
			"must be trusted at cluster-admin level.")
	_ = fs.Parse(os.Args[1:])

	switch {
	case cfg.nginxConfDir == "":
		fatal("--nginx-conf-dir must not be empty")
	case cfg.nginxBinary == "":
		fatal("--nginx-binary must not be empty")
	case cfg.nginxPID == "":
		fatal("--nginx-pid must not be empty")
	case cfg.healthzAddr == "":
		fatal("--healthz-addr must not be empty")
	}

	logger := zap.New() // klog/controller-runtime logs flow through here too
	log.SetLogger(logger)

	// Startup config summary.
	logger.Info("host-nginx-gateway starting",
		"nginx-conf-dir", cfg.nginxConfDir,
		"nginx-binary", cfg.nginxBinary,
		"nginx-pid", cfg.nginxPID,
		"healthz-addr", cfg.healthzAddr,
		"nginx-main-config", nginxMainConfig,
		"nginx-error-log", cfg.nginxErrorLog,
		"dangerously-allow-nginx-snippets", cfg.allowNginxSnippets,
		"dangerously-allow-extra-files", cfg.allowExtraFiles)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error(err, "host-nginx-gateway exited with error")
		os.Exit(1)
	}
	logger.Info("host-nginx-gateway stopped")
}

// run blocks until ctx is cancelled (SIGTERM/SIGINT) or the controller
// fails. Ordering: listeners first (fail fast on a busy port), then the
// provider manager which loads the in-cluster config and blocks.
func run(ctx context.Context, cfg config, logger logr.Logger) error {
	// 1. healthz/metrics (DESIGN.md §1). controller-runtime's own
	// metrics/health servers are disabled below; this is the single
	// listener. Bind before starting the manager so a port clash is a
	// startup error, not a background surprise.
	ln, err := net.Listen("tcp", cfg.healthzAddr)
	if err != nil {
		return fmt.Errorf("healthz listener on %s: %w", cfg.healthzAddr, err)
	}
	metrics := &Metrics{}
	httpSrv := &http.Server{
		Handler:           metrics.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	logger.Info("healthz/metrics server listening", "addr", cfg.healthzAddr)

	// 2. Dataplane: probe/reload client, validator (temporary main config,
	// §5.1), publisher (rollback state machine, §5.2) and certs manager
	// (§5.3). NOTE: the validator's include detection is baked to
	// /etc/nginx/conf.d/k8s-gw (DESIGN.md §2); overriding --nginx-conf-dir
	// is therefore a developer convenience only.
	//
	// Every nginx invocation (-t validation and -s reload) goes through
	// `nsenter -t 1 -n -m --` into the host's network + mount namespaces
	// (DESIGN.md §8.1 DaemonSet 部署形态) — -n so `nginx -t` sees the HOST's
	// addresses when probing `listen` directives. The Commander seam below
	// is also where tests inject fakes.
	if _, err := exec.LookPath("nsenter"); err != nil {
		return fmt.Errorf("the controller requires the nsenter binary (util-linux) in the image: %w", err)
	}
	commander := dataplane.NsenterCommander(1)
	logger.Info("nginx exec mode resolved",
		"mode", "nsenter", "binary", cfg.nginxBinary)
	nginx := dataplane.NewOSNginx(dataplane.NginxConfig{
		Binary:    cfg.nginxBinary,
		PIDFile:   cfg.nginxPID,
		Commander: commander,
	})
	validator := &dataplane.Validator{
		NginxBinary:    cfg.nginxBinary,
		MainConfigPath: nginxMainConfig,
		Prefix:         nginxPrefix,
		RequireInclude: true,
		Commander:      commander,
	}
	publisher, err := dataplane.NewPublisher(dataplane.PublisherOptions{
		OutputDir:         cfg.nginxConfDir,
		Validator:         validator,
		Nginx:             nginx,
		ErrorLogPath:      effectiveErrorLog(cfg),
		VerifyReloadDelay: 0, // default 1s
	})
	if err != nil {
		return fmt.Errorf("dataplane publisher: %w", err)
	}
	certs := dataplane.NewCertsManager(filepath.Join(cfg.nginxConfDir, "certs"))
	files := dataplane.NewFilesManager(cfg.nginxConfDir)

	applier := &DataplaneApplier{
		ConfDir:      cfg.nginxConfDir,
		Nginx:        nginx,
		Certs:        certs,
		Files:        files,
		Publisher:    publisher,
		Metrics:      metrics,
		Log:          logger.WithName("dataplane"),
		ErrorLogPath: effectiveErrorLog(cfg),
	}

	// 3. Provider manager (watches, full reconcile, status write-back).
	// Connects with the in-cluster ServiceAccount (no kubeconfig flag —
	// the DaemonSet pod's projected token is the only credential).
	// Blocks until ctx is done. Our own healthz/metrics listener replaces
	// controller-runtime's ("0" disables both).
	err = provider.Run(ctx, provider.Options{
		Applier:                applier,
		PublishAddresses:       parseAddressList(cfg.publishAddresses),
		FallbackAddresses:      detectPublishAddresses(),
		AllowNginxSnippets:     cfg.allowNginxSnippets,
		AllowExtraFiles:        cfg.allowExtraFiles,
		MetricsBindAddress:     "0",
		HealthProbeBindAddress: "0",
		Log:                    logger.WithName("provider"),
	})

	// Surface a concurrent health-listener failure (if any) over a clean
	// manager shutdown.
	select {
	case lerr := <-serveErr:
		if !errors.Is(lerr, http.ErrServerClosed) {
			return fmt.Errorf("healthz server: %w", lerr)
		}
	default:
	}
	return err
}

// effectiveErrorLog resolves the error log used by the reload-effect
// verification and the emitted http-context error_log directive: the
// explicit flag, else <nginx-conf-dir>/error.log (a file inside the OWNED
// directory — the host nginx may otherwise log to stderr only). The literal
// value "off" disables the whole mechanism.
func effectiveErrorLog(cfg config) string {
	if cfg.nginxErrorLog == "off" {
		return "off"
	}
	if cfg.nginxErrorLog != "" {
		return cfg.nginxErrorLog
	}
	return filepath.Join(cfg.nginxConfDir, "error.log")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "host-nginx-gateway: "+msg)
	os.Exit(2)
}
