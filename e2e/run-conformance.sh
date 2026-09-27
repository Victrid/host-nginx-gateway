#!/usr/bin/env bash
# Gateway API conformance baseline runner (e2e/conformance/BASELINE.md).
#
# Self-contained: deploys the controller as a DaemonSet via the Helm chart
# (DESIGN.md §8.1, same pattern as e2e/run-daemonset.sh — whose default path
# is untouched), imports the official suite's backend images, runs the
# OFFICIAL conformance suite (sigs.k8s.io/gateway-api/conformance, version
# pinned in e2e/conformance/go.mod) from the host against the HOST nginx
# data plane, and always tears down afterwards.
#
# Environment adaptations made by the harness (e2e/conformance/
# conformance_test.go, fully documented in BASELINE.md): none inside the
# suite flow — the round-5 product features replaced all three former
# shims (bind annotations, address injection, HTTPS base Gateway
# deletion). Round 9 added one pre/post-suite environment step: the
# script-managed default-server fixture
# (e2e/install-default-server-fixture.sh) is installed before the
# controller deploys and refreshed onto the suite certificate after the
# base Gateways are programmed — the controller itself never injects
# default servers (DESIGN.md §3.3).
#
# Prerequisites: same cluster/env as e2e/run.sh (k3s node hng-e2e, host
# nginx with the k8s-gw include), docker, helm (auto-installed like
# run-daemonset.sh), go.
#
# Usage: sudo -E bash e2e/run-conformance.sh
#        CONF_TEST_ARGS='-run-test HTTPRouteExactPathMatching' for one test.
set -u -o pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CONF_DIR="$REPO_ROOT/e2e/conformance"
KUBECONFIG_PATH=${E2E_KUBECONFIG:-/tmp/opencode/k3s-kubeconfig}
EXPECT_NODE=${EXPECT_NODE:-hng-e2e}
HELM=${HELM:-/tmp/opencode/bin/helm}
RELEASE=hng-conf
CHART_NS=kube-system
IMG=localhost/hng-e2e/controller
IMG_TAG=v1
NS_PREFIX=gateway-conformance
GWCLASS=host-nginx
CONTROLLER=gateway.host-nginx/controller
REPORT_OUT="$CONF_DIR/report.yaml"
TEST_LOG=/tmp/opencode/conformance-test.log
SUITE_IMAGES=(
  "registry.k8s.io/gateway-api/echo-basic:v1.5.1"
  "registry.k8s.io/gateway-api/echo-basic:v1.6.0-dev.2"
  "registry.k8s.io/gateway-api/echo-basic:v1.6.0-dev.3"
  "registry.k8s.io/coredns/coredns:v1.12.2"
)

# Deviations from implemented scope (DESIGN.md §3.3 / §9) → still exempted.
# Each entry: exact suite feature constant → DESIGN.md section.
# Round 5 removed the exemptions for backend weights, HTTPRouteMatches
# (method/header/query), RequestRedirect, URLRewrite, RequestHeaderModifier
# and ResponseHeaderModifier — all implemented now.
# Round 7 removed the FINAL four exemptions: RequestMirror (incl. multiple
# mirrors and percentage mirroring) and backendRef-level
# RequestHeaderModifier are implemented (DESIGN.md §5.1.1).
# NOTE: ReferenceGrant is a CORE feature of the GATEWAY-HTTP profile, and
# NewConformanceTestSuite unions profile CoreFeatures AFTER subtracting
# exemptions — so exempting it is a no-op (it is supported since round 3
# anyway and nothing needs to reference it here).
EXEMPT_FEATURES=$(paste -sd, <<'EOF'
EOF
)

# Features declared as supported (suite flags). With an EMPTY exempt list
# the suite switches to inferring features from GatewayClass
# status.supportedFeatures — which this controller does not write yet — and
# aborts ("no supported features were determined") when that status is
# empty. Declaring the implemented CORE set explicitly keeps the suite on
# the manual path with byte-for-byte the same test selection the 4-exemption
# rounds had (manual ∪ profile-core = the 3 core features). Extended
# features are deliberately NOT declared: the GATEWAY-HTTP core report
# makes no extended claims (mirrors & co. are implemented + unit-tested;
# declaring them would grow the run — a later round's step together with
# writing GatewayClass status.supportedFeatures).
SUPPORTED_FEATURES=$(paste -sd, <<'EOF'
Gateway
ReferenceGrant
HTTPRoute
EOF
)

# Tests that are NOT feature-gated but exercise documented deviations. Full
# reason table lives in e2e/conformance/BASELINE.md. Round 6 removed the last
# entry (HTTPRouteCrossNamespace): cross-namespace route attachment is now
# implemented per Gateway API v1 (target-listener allowedRoutes governs;
# no ReferenceGrant for parentRefs). An empty value behaves exactly like an
# unpassed -skip-tests flag (the suite's flag default is the empty string).
SKIP_TESTS=$(paste -sd, <<'EOF'
EOF
)

export KUBECONFIG="$KUBECONFIG_PATH"

log()  { printf '\n\033[1;34m[ conformance ] %s\033[0m\n' "$*"; }
ok()   { printf '\033[0;32mPASS\033[0m %s\n' "$*"; }
bad()  { printf '\033[0;31mFAIL\033[0m %s\n' "$*"; }
die()  { printf '\033[0;31mFATAL\033[0m %s\n' "$*"; exit 1; }
K() { kubectl "$@"; }
KN() { kubectl -n "$CHART_NS" "$@"; }

wait_until() { # DESC TIMEOUT 'snippet'
  local desc=$1 timeout=$2 snippet=$3
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if eval "$snippet" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  echo "    timeout waiting for: $desc" >&2
  return 1
}

# The chart's fullname helper always renders "host-nginx-gateway" (release
# name is not part of it), so resolve the DaemonSet via the instance label
# instead of hard-coding "<release>-host-nginx-gateway".
DS_NAME() { kubectl -n "$CHART_NS" get ds -l app.kubernetes.io/instance="$RELEASE" -o name 2>/dev/null | head -1 | cut -d/ -f2; }

ctr_image_present() { # REF — k3s ctr list can lag well over 30s right after import
  wait_until "image $1 in containerd" 120 \
    "k3s ctr images list 2>/dev/null | grep -q \"^\\$1 \""
}

teardown() {
  log "teardown"
  "$HELM" uninstall "$RELEASE" --namespace "$CHART_NS" >/dev/null 2>&1
  # CleanupBaseResources=true normally removes these; delete defensively so
  # a failed run does not leave the controller reconciling stale Gateways.
  kubectl delete ns gateway-conformance-infra gateway-conformance-app-backend \
    gateway-conformance-web-backend --ignore-not-found --timeout=60s >/dev/null 2>&1
  kubectl delete gatewayclass "$GWCLASS" --ignore-not-found >/dev/null 2>&1
  # no controller is running after the uninstall: drop generated files so
  # either deployment form can run next (same courtesy as run-daemonset.sh)
  rm -f /etc/nginx/conf.d/k8s-gw/00-global.conf /etc/nginx/conf.d/k8s-gw/00-global.conf.prev \
    /etc/nginx/conf.d/k8s-gw/error.log
  # The default-server fixture STAYS: it is part of this machine's
  # pre-configured master nginx.conf (like an administrator's default
  # server), not controller output. e2e/install-default-server-fixture.sh
  # re-applies it idempotently on the next run.
}

# ---------------------------------------------------------------------------
log "preflight"
[ "$(id -u)" = 0 ] || die "run as root (k3s ctr import, host file access)"
[ -f "$KUBECONFIG_PATH" ] || die "kubeconfig $KUBECONFIG_PATH not found"
kubectl get --raw /readyz >/dev/null 2>&1 || die "k3s API not reachable"
kubectl get nodes -o name 2>/dev/null | grep -qxF "node/$EXPECT_NODE" \
  || die "refusing: cluster has no node '$EXPECT_NODE' (cluster identity gate)"
kubectl get node "$EXPECT_NODE" -o jsonpath='{.status.conditions[?(@.type=="DiskPressure")].status}' 2>/dev/null \
  | grep -qx 'True' \
  && die "node $EXPECT_NODE is in DiskPressure — free disk space first"
systemctl is-active nginx >/dev/null 2>&1 || die "host nginx not running"
grep -qF 'include /etc/nginx/conf.d/k8s-gw/*.conf;' /etc/nginx/nginx.conf \
  || die "host nginx.conf lacks the k8s-gw include (run e2e/run.sh scenario 1b first)"
command -v docker >/dev/null || die "docker not found"
command -v go >/dev/null || die "go not found"
if [ ! -x "$HELM" ]; then
  log "installing helm to /tmp/opencode/bin (get.helm.sh — same as run-daemonset.sh)"
  mkdir -p "$(dirname "$HELM")" /tmp/opencode/helm-extract
  curl -fsSL -o /tmp/opencode/helm.tgz https://get.helm.sh/helm-v3.19.0-linux-amd64.tar.gz \
    || die "helm download failed"
  tar -xzf /tmp/opencode/helm.tgz -C /tmp/opencode/helm-extract linux-amd64/helm
  install -m 755 /tmp/opencode/helm-extract/linux-amd64/helm "$HELM"
fi
ok "preflight (node gate, nginx+include, docker $(docker --version | cut -d, -f1), helm, go $(go version | cut -d' ' -f3))"

# double-writer guard (same policy as run-daemonset.sh)
for pidfile in /tmp/opencode/e2e-scratch/controller.pid /tmp/opencode/controller.pid; do
  if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
    kill "$(cat "$pidfile")"; sleep 2
    log "stopped host-mode controller (pid $(cat "$pidfile")) — double-writer guard"
  fi
done
pgrep -f '/tmp/opencode/host-nginx-gateway(-e2e)? ' >/dev/null 2>&1 \
  && die "a host-mode controller is still running; stop it first"
# a previous conformance/helm run must not double-own the GatewayClass
if KN get ds host-nginx-gateway >/dev/null 2>&1; then
  log "removing stale helm release $RELEASE"
  "$HELM" uninstall "$RELEASE" --namespace "$CHART_NS" >/dev/null 2>&1
  wait_until "stale daemonset gone" 60 "! KN get ds host-nginx-gateway >/dev/null 2>&1"
fi
ok "no double writers"

# ---------------------------------------------------------------------------
# Default-server fixture (v9 policy: the controller never injects default
# servers — DESIGN.md §3.3). The suite asserts that requests with a Host/
# SNI no route claims answer 404; in production that is the host
# administrator's nginx.conf doing its job, so this controlled test
# environment pre-configures its own default servers. The shared installer
# (e2e/install-default-server-fixture.sh) drops a script-managed fixture
# into /etc/nginx/conf.d/ (never into the controller-owned k8s-gw/ dir),
# includes it from the master nginx.conf and reloads. The 443 blocks start
# on a self-signed fixture certificate; conformance_test.go refreshes them
# onto the suite's materialized certificate once the base Gateways are
# programmed (HTTPS requests to unmatched SNI must present a cert the
# suite client trusts).
log "default-server fixture (the script acts as the host administrator)"
bash "$REPO_ROOT/e2e/install-default-server-fixture.sh" \
  || die "default-server fixture installation failed"
ok "default-server fixture installed (80/443 wildcard + loopback pool)"

# ---------------------------------------------------------------------------
log "suite backend images (docker pull + k3s ctr import)"
for image in "${SUITE_IMAGES[@]}"; do
  if k3s ctr images list 2>/dev/null | grep -q "^$image "; then
    echo "    $image already in containerd"
    continue
  fi
  timeout 300 docker pull "$image" >/dev/null \
    || die "docker pull $image failed (network?)"
  # podman-backed docker cannot modify an existing archive — drop it first
  rm -f /tmp/opencode/hng-suite-img.tar
  docker save -o /tmp/opencode/hng-suite-img.tar "$image" \
    || die "docker save $image failed"
  k3s ctr images remove "$image" >/dev/null 2>&1 || true
  k3s ctr images import --local /tmp/opencode/hng-suite-img.tar >/dev/null 2>&1 \
    || sudo k3s ctr images import --local /tmp/opencode/hng-suite-img.tar >/dev/null \
    || die "ctr import failed for $image"
  ctr_image_present "$image" || die "ctr import verification failed ($image)"
  echo "    $image imported"
done
rm -f /tmp/opencode/hng-suite-img.tar
ok "suite images present"

# ---------------------------------------------------------------------------
log "build + import controller image (host build, prebuilt variant)"
CGO_ENABLED=0 go build -trimpath -o /tmp/opencode/hng-controller-bin \
  "$REPO_ROOT/cmd/host-nginx-gateway" || die "controller build failed"
cp /tmp/opencode/hng-controller-bin "$REPO_ROOT/docker/host-nginx-gateway"
APK_MIRROR=${APK_MIRROR:-https://mirrors.tuna.tsinghua.edu.cn/alpine}
timeout 420 docker build -q --network=host -f "$REPO_ROOT/docker/Dockerfile.controller.prebuilt" \
  --build-arg APK_MIRROR="$APK_MIRROR" \
  -t "$IMG:$IMG_TAG" "$REPO_ROOT/docker" >/dev/null || die "image build failed"
rm -f "$REPO_ROOT/docker/host-nginx-gateway"
rm -f /tmp/opencode/hng-controller.tar
docker save -o /tmp/opencode/hng-controller.tar "$IMG:$IMG_TAG" \
  || die "docker save failed"
# Import + verify with retries: when the tar carries NEW content (fresh
# binary), containerd's image-metadata view can lag the import for minutes
# (observed round 4: ctr import returns done, yet `ctr images list` keeps
# the image invisible past a single 120s poll), while already-present
# content verifies instantly. Retry the whole remove/import/verify cycle.
import_ok=false
for attempt in 1 2 3; do
  k3s ctr images remove "$IMG:$IMG_TAG" >/dev/null 2>&1 || true
  k3s ctr images import --local /tmp/opencode/hng-controller.tar >/dev/null 2>&1 \
    || sudo k3s ctr images import --local /tmp/opencode/hng-controller.tar >/dev/null \
    || { sleep 5; continue; }
  if ctr_image_present "$IMG:$IMG_TAG"; then import_ok=true; break; fi
  sleep 5
done
[ "$import_ok" = true ] || die "ctr import verification failed (3 attempts)"
ok "image $IMG:$IMG_TAG built and imported"

# ---------------------------------------------------------------------------
log "helm install (DaemonSet form)"
kubectl delete gatewayclass "$GWCLASS" --ignore-not-found >/dev/null 2>&1
"$HELM" upgrade --install "$RELEASE" "$REPO_ROOT/charts/host-nginx-gateway" \
  --namespace "$CHART_NS" \
  --set image.repository="$IMG" --set image.tag="$IMG_TAG" --set image.pullPolicy=Never \
  >/dev/null || { teardown; die "helm install failed"; }
wait_until "DaemonSet rollout" 180 "KN rollout status daemonset/\$(DS_NAME) --timeout=5s" \
  || { KN get pods -l app.kubernetes.io/instance=$RELEASE; teardown; die "DaemonSet not Ready"; }
POD=$(KN get pods -l app.kubernetes.io/instance=$RELEASE -o name | head -1 | cut -d/ -f2)
KN logs "$POD" 2>/dev/null | grep -q '"mode":"nsenter"' \
  && ok "DaemonSet pod Ready, exec mode nsenter ($POD)" \
  || bad "DaemonSet pod Ready but exec mode not nsenter ($POD)"
wait_until "GatewayClass $GWCLASS Accepted" 90 \
  "K get gatewayclass $GWCLASS -o jsonpath='{.status.conditions[?(@.type==\"Accepted\")].status}' | grep -q True" \
  || { K get gatewayclass "$GWCLASS" -o yaml; teardown; die "GatewayClass not Accepted"; }
ok "GatewayClass $GWCLASS Accepted (controller $CONTROLLER)"

# stale resources from an aborted run confuse Setup's "already exists" paths
kubectl delete ns ${NS_PREFIX}-infra ${NS_PREFIX}-app-backend ${NS_PREFIX}-web-backend \
  --ignore-not-found --timeout=60s >/dev/null 2>&1

trap teardown EXIT

# ---------------------------------------------------------------------------
log "run official conformance suite (GATEWAY-HTTP profile)"
# -allow-crds-mismatch: k3s ships gateway-api CRDs v1.6.1 (standard channel),
# the suite is v1.6.2 — minor skew, documented in BASELINE.md.
# Timeouts: Setup waits for all base pods; the suite's own per-test timeouts
# default to generous values (see utils/config).
rm -f "$REPORT_OUT"
set +e
( cd "$CONF_DIR" && timeout 3000 env KUBECONFIG="$KUBECONFIG_PATH" \
    go test -v -count=1 -timeout 50m . \
    -args \
      -gateway-class="$GWCLASS" \
      -conformance-profiles=GATEWAY-HTTP \
      -allow-crds-mismatch \
      -supported-features="$SUPPORTED_FEATURES" \
      -exempt-features="$EXEMPT_FEATURES" \
      -skip-tests="$SKIP_TESTS" \
      -report-output="$REPORT_OUT" \
      -organization=Victrid \
      -project=HostNginxGateway \
      -version="$(git -C "$REPO_ROOT" describe --tags --always 2>/dev/null || echo dev)" \
      -url=https://github.com/Victrid/HostNginxGateway \
      -contact=https://github.com/Victrid/HostNginxGateway \
      ${CONF_TEST_ARGS:-} ) 2>&1 | tee "$TEST_LOG"
RC=${PIPESTATUS[0]}
set -e

echo
log "results"
if [ -f "$REPORT_OUT" ]; then
  ok "conformance report written: $REPORT_OUT (full log: $TEST_LOG)"
else
  bad "no report written — suite died before cleanup (log: $TEST_LOG)"
fi
PASSN=$(grep -cE '^[[:space:]]*--- PASS:' "$TEST_LOG" || true)
FAILN=$(grep -cE '^[[:space:]]*--- FAIL:' "$TEST_LOG" || true)
SKIPN=$(grep -cE '^[[:space:]]*--- SKIP:' "$TEST_LOG" || true)
printf 'subtests: %d passed, %d failed, %d skipped\n' "$PASSN" "$FAILN" "$SKIPN"

if [ "$RC" = 0 ] && [ "${FAILN:-0}" = 0 ]; then
  ok "conformance run green"
  exit 0
fi
bad "conformance run has failures (go test rc=$RC) — see $TEST_LOG and e2e/conformance/BASELINE.md"
exit "${RC:-1}"
