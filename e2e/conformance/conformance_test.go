/*
Gateway API conformance harness for HostNginxGateway.

Runs the OFFICIAL suite (sigs.k8s.io/gateway-api/conformance, version pinned
in go.mod — must match the gateway-api version of the repo root go.mod) with
the minimal host-nginx environment adaptations documented in
e2e/conformance/BASELINE.md. The suite code itself is not modified.

Former adaptations, now PRODUCT features (round 5):

  - BIND SHIM → REMOVED: the controller auto-assigns each Gateway whose
    cross-Gateway listeners would be indistinct (same port, overlapping
    hostnames) its own 127.0.0.N bind (Gateway API v1 "Distinct Listeners"
    over merged data planes) and reports that address in status.addresses.
    Since v0.3.0 the multinode ownership model intersects spec.addresses
    with the node's address fingerprint — the base Gateways set no
    spec.addresses (wildcard intent) and the auto-assigned pool is inside
    127/8, which every node owns implicitly, so this harness path is
    unchanged (wildcard/loopback ownership preserved; see
    TestNodeFilter_LoopbackAutoAssignPreserved).

  - ADDRESS SHIM → REMOVED: the controller writes status.addresses itself
    (v0.3.0 derivation: --publish-addresses flag → the listens this node
    renders for the Gateway → wildcard binds reported as the node primary
    IP via HNG_NODE_IP), so the suite's address gate validates real
    controller behavior.

  - HTTPS BASE GATEWAY DELETION → REMOVED: with per-Gateway loopback binds
    (auto-assignment) and reload-effect verification (error-log bind
    failure detection + rollback), the 443 TLS base Gateway reconciles
    cleanly alongside the host nginx (which owns no 443 here).

Round 9: the controller no longer injects synthetic default servers
(DESIGN.md §3.3) — unmatched-host requests are answered by the
environment's own default servers, installed by
e2e/install-default-server-fixture.sh exactly like an administrator's
nginx.conf would. The harness only REFRESHES that script-managed fixture
onto the suite certificate after the base Gateways are programmed (see
refreshDefaultServerFixture); it never writes nginx configuration itself.

Everything else — GatewayClass/controllerName wiring, supported/exempt
features, skipped tests — is passed via the standard suite flags from
e2e/run-conformance.sh. See BASELINE.md for the recorded baseline.
*/
package conformance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/gateway-api/conformance"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/tests"
	confsuite "sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/yaml"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// baseGateways are the suite's base Gateways (utils/suite/suite.go
// constants) living in the infra namespace; the readiness gate waits for
// the controller to program them.
var baseGateways = []string{"same-namespace", "all-namespaces", "backend-namespaces"}

const infraNamespace = "gateway-conformance-infra"

func TestConformance(t *testing.T) {
	opts := conformance.DefaultOptions(t)

	// The suite's per-test clients inherit this RestConfig; client-go's
	// default QPS/burst (5/10) throttles the parallel conformance tests into
	// "context deadline exceeded" fetch errors (observed in round 1).
	if opts.RestConfig != nil {
		opts.RestConfig.QPS = 20
		opts.RestConfig.Burst = 60
	}

	// TIMEOUT COMPRESSION (round 4): the controller is event-driven with
	// ~1s full-sync spacing, so correct status/traffic behavior is visible
	// within seconds. The suite's 60s/180s condition-poll defaults only
	// measure how long a STUCK implementation stalls the run — compress
	// them to ~5s and fail fast. SetupTimeoutConfig only backfills zero
	// values, so explicit non-zero fields here are used verbatim.
	// Deliberately NOT compressed: IO/API bounds (Create/Delete/Get/
	// ManifestFetch/RequestTimeout), DefaultTestTimeout, DefaultPollInterval
	// and RequiredConsecutiveSuccesses (they bound Kubernetes/image/HTTP
	// operations, not implementation convergence).
	tc := opts.TimeoutConfig
	tc.GatewayMustHaveCondition = 5 * time.Second           // was 180s
	tc.GWCMustBeAccepted = 5 * time.Second                  // was 180s
	tc.HTTPRouteMustHaveCondition = 5 * time.Second         // was 60s
	tc.HTTPRouteMustNotHaveParents = 5 * time.Second        // was 60s
	tc.RouteMustHaveParents = 5 * time.Second               // was 60s
	tc.GatewayStatusMustHaveListeners = 5 * time.Second     // was 60s
	tc.GatewayListenersMustHaveConditions = 5 * time.Second // was 60s
	tc.LatestObservedGenerationSet = 5 * time.Second        // was 60s
	tc.MaxTimeToConsistency = 5 * time.Second               // was 30s (spec max for conformant impls)
	// status.addresses is written by the controller itself on the next
	// full sync (~1-2s); 5s leaves margin without racing a shim.
	tc.GatewayMustHaveAddress = 5 * time.Second // was 180s
	// NamespacesMustBeReady includes Deployment rollout + image pulls:
	// 30s keeps fail-fast pressure without flaking on cold pulls.
	tc.NamespacesMustBeReady = 30 * time.Second    // was 300s
	tc.TLSRouteMustHaveCondition = 5 * time.Second // was 60s (profiles not run; compressed for completeness)
	tc.TCPRouteMustHaveCondition = 5 * time.Second // was 60s
	tc.UDPRouteMustHaveCondition = 5 * time.Second // was 60s
	opts.TimeoutConfig = tc

	// Report metadata defaults (flags override — see run-conformance.sh).
	if opts.Implementation.Organization == "" {
		opts.Implementation.Organization = "Victrid"
	}
	if opts.Implementation.Project == "" {
		opts.Implementation.Project = "HostNginxGateway"
	}
	if opts.Implementation.URL == "" {
		opts.Implementation.URL = "https://github.com/Victrid/HostNginxGateway"
	}
	if len(opts.Implementation.Contact) == 0 {
		opts.Implementation.Contact = []string{"https://github.com/Victrid/HostNginxGateway"}
	}

	cSuite, err := confsuite.NewConformanceTestSuite(opts)
	require.NoError(t, err, "error initializing conformance suite")

	if opts.ReportOutputPath != "" {
		t.Cleanup(func() {
			report, rErr := cSuite.Report()
			require.NoError(t, rErr, "error generating conformance profile report")
			raw, yErr := yaml.Marshal(report)
			require.NoError(t, yErr, "error marshalling report")
			require.NoError(t, os.WriteFile(opts.ReportOutputPath, raw, 0o644), "error writing report")
			t.Logf("conformance report written to %s", opts.ReportOutputPath)
			summarizeReport(t, report)
		})
	}

	cSuite.Setup(t, tests.ConformanceTests)

	// Readiness gate: the controller programs the base Gateways itself
	// (auto-assigned per-Gateway loopback binds + status.addresses); no
	// harness annotations or injectors are needed anymore.
	waitBaseGatewaysProgrammed(t, cSuite)

	// Default-server fixture refresh: the controller never injects default
	// servers (DESIGN.md §3.3) — unmatched-host requests are answered by
	// the environment's own default servers, installed by
	// e2e/install-default-server-fixture.sh exactly like an administrator
	// would. The script-managed fixture starts on a self-signed
	// certificate; now that the base Gateways are programmed the suite's
	// TLS certificate is materialized under
	// /etc/nginx/conf.d/k8s-gw/certs/, so re-apply the fixture to present
	// it on the 443 default sockets (HTTPRouteHTTPSListener verifies the
	// server certificate even for its unmatched-SNI 404 case).
	refreshDefaultServerFixture(t)

	err = cSuite.Run(t, tests.ConformanceTests)
	require.NoError(t, err)
}

// refreshDefaultServerFixture re-applies the script-managed default-server
// fixture (e2e/install-default-server-fixture.sh). It is idempotent and
// reloads nginx only when the fixture content changed (first run: fixture
// certificate → suite certificate on the 443 default blocks).
func refreshDefaultServerFixture(t *testing.T) {
	t.Helper()
	script := filepath.Join("..", "install-default-server-fixture.sh")
	cmd := exec.Command("bash", script, "--wait-suite-cert")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("default-server fixture refresh failed: %v\n%s", err, out)
	}
	t.Logf("default-server fixture: %s", strings.TrimSpace(string(out)))
}

// summarizeReport prints the per-profile pass/fail/skip counts so the shell
// wrapper can surface them without a YAML parser.
func summarizeReport(t *testing.T, report *confv1.ConformanceReport) {
	for _, p := range report.ProfileReports {
		t.Logf("profile %s: core=%s (%d passed, %d failed, %d skipped) summary=%q",
			p.Name, p.Core.Result, p.Core.Statistics.Passed, p.Core.Statistics.Failed,
			p.Core.Statistics.Skipped, p.Summary)
		if p.Extended != nil {
			t.Logf("profile %s: extended=%s (%d passed, %d failed, %d skipped)",
				p.Name, p.Extended.Result, p.Extended.Statistics.Passed,
				p.Extended.Statistics.Failed, p.Extended.Statistics.Skipped)
		}
	}
}

// waitBaseGatewaysProgrammed blocks until the controller has reconciled the
// base Gateways (per-Gateway binds assigned, server blocks published,
// status.addresses written), or fails the run with the offending status for
// triage. It validates REAL controller behavior — the round-5 product
// features replaced the harness bind/address shims.
func waitBaseGatewaysProgrammed(t *testing.T, s *confsuite.ConformanceTestSuite) {
	mustProgrammed := map[string]bool{
		"same-namespace": true, "all-namespaces": true, "backend-namespaces": true,
	}
	ctx := context.Background()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		allProgrammed := true
		for name := range mustProgrammed {
			gw := &gatewayv1.Gateway{}
			if err := s.Client.Get(ctx, types.NamespacedName{Namespace: infraNamespace, Name: name}, gw); err != nil {
				allProgrammed = false
				break
			}
			if !conditionTrue(gw.Status.Conditions, "Programmed") || len(gw.Status.Addresses) == 0 {
				allProgrammed = false
				break
			}
		}
		if allProgrammed {
			t.Log("base Gateways Programmed=True with status.addresses (controller-native)")
			return
		}
		time.Sleep(2 * time.Second)
	}
	dumpGatewayStatuses(t, s)
	t.Fatal("base Gateways not Programmed within 90s — see statuses above and the controller pod logs")
}

func dumpGatewayStatuses(t *testing.T, s *confsuite.ConformanceTestSuite) {
	ctx := context.Background()
	for _, name := range baseGateways {
		gw := &gatewayv1.Gateway{}
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: infraNamespace, Name: name}, gw); err != nil {
			t.Logf("gateway %s: get error %v", name, err)
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "gateway %s conditions:", name)
		for _, c := range gw.Status.Conditions {
			fmt.Fprintf(&b, " [%s=%s reason=%s %s]", c.Type, c.Status, c.Reason, c.Message)
		}
		fmt.Fprintf(&b, " addresses:")
		for _, a := range gw.Status.Addresses {
			fmt.Fprintf(&b, " %s", a.Value)
		}
		for _, l := range gw.Status.Listeners {
			for _, c := range l.Conditions {
				fmt.Fprintf(&b, "\n  listener %s: [%s=%s reason=%s %s]", l.Name, c.Type, c.Status, c.Reason, c.Message)
			}
		}
		t.Log(b.String())
	}
}

func conditionTrue(conds []metav1.Condition, condType string) bool {
	for _, c := range conds {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
