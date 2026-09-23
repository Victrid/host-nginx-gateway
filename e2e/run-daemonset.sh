#!/usr/bin/env bash
# host-nginx-gateway DaemonSet-mode E2E (DESIGN.md §8.1).
#
# Installs the Helm chart into the running k3s, then verifies the core loop
# with the controller running as a POD driving the HOST nginx via nsenter:
#   0. pod Ready, exec mode resolved to nsenter, in-cluster SA works
#   1. include present → route serves through host nginx
#   2. backend deleted → GEP-1364 500
#   3. listener port change → host nginx actually reloaded by the POD
#      (new port bound, old port gone, traffic follows)
#
# Prerequisites: k3s at $E2E_KUBECONFIG (node $EXPECT_NODE), host nginx
# running with the k8s-gw include, docker, helm (auto-installed from
# get.helm.sh if missing — the Tsinghua github-release mirror does not
# carry helm; choice documented in e2e/README.md).
#
# Usage: sudo -E bash e2e/run-daemonset.sh
set -u -o pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
MANIFESTS="$REPO_ROOT/e2e/manifests"
KUBECONFIG_PATH=${E2E_KUBECONFIG:-/tmp/opencode/k3s-kubeconfig}
EXPECT_NODE=${EXPECT_NODE:-hng-e2e}
HELM=${HELM:-/tmp/opencode/bin/helm}
RELEASE=hng-ds
CHART_NS=kube-system
IMG=localhost/hng-e2e/controller
IMG_TAG=v1
CONF_DIR=/etc/nginx/conf.d/k8s-gw
INCLUDE_LINE='include /etc/nginx/conf.d/k8s-gw/*.conf;'
NS=e2e

export KUBECONFIG="$KUBECONFIG_PATH"

PASS=0; FAIL=0
log()  { printf '\n\033[1;34m[ e2e-ds ] %s\033[0m\n' "$*"; }
ok()   { printf '\033[0;32mPASS\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '\033[0;31mFAIL\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
die()  { printf '\033[0;31mFATAL\033[0m %s\n' "$*"; exit 1; }
K() { kubectl -n "$NS" "$@"; }
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
DS_POD() { KN get pods -l app.kubernetes.io/instance=$RELEASE -o name | head -1 | cut -d/ -f2; }

gw_cond() {
  K get gateway "$1" -o json 2>/dev/null | python3 -c '
import json,sys
try: g=json.load(sys.stdin)
except Exception: sys.exit(1)
for c in g.get("status",{}).get("conditions",[]):
    if c["type"]==sys.argv[1]:
        print(c["status"], c["reason"], c.get("message","")); sys.exit(0)
sys.exit(1)' "$2"
}
rt_cond() {
  K get httproute "$1" -o json 2>/dev/null | python3 -c '
import json,sys
try: r=json.load(sys.stdin)
except Exception: sys.exit(1)
for p in r.get("status",{}).get("parents",[]):
    for c in p["conditions"]:
        if c["type"]==sys.argv[1]:
            print(c["status"], c["reason"], c.get("message","")); sys.exit(0)
sys.exit(1)' "$2"
}
wait_expect() { # NAME TIMEOUT 'snippet' STATUS REASON
  local name=$1 timeout=$2 snippet=$3 ws=$4 wr=$5
  wait_until "$name" "$timeout" \
    "[[ \"\$(eval \"$snippet\" 2>/dev/null)\" == \"$ws $wr\"* ]]" || true
  local line; line=$(eval "$snippet" 2>/dev/null) || true
  if read -r gs gr _ <<<"${line:-}" && [ "$gs" = "$ws" ] && [ "$gr" = "$wr" ]; then
    ok "$name ($gs/$gr)"
  else
    bad "$name: want $ws/$wr, got '${line:-<none>}'"
  fi
}
kick() { K annotate gateway "$1" hng-e2e/tick="$SECONDS.$RANDOM" --overwrite >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
log "preflight"
[ "$(id -u)" = 0 ] || die "run as root (k3s ctr import, host file asserts)"
[ -f "$KUBECONFIG_PATH" ] || die "kubeconfig $KUBECONFIG_PATH not found"
kubectl --kubeconfig "$KUBECONFIG_PATH" get --raw /readyz >/dev/null 2>&1 \
  || die "k3s API not reachable"
kubectl --kubeconfig "$KUBECONFIG_PATH" get nodes -o name 2>/dev/null | grep -qxF "node/$EXPECT_NODE" \
  || die "refusing: cluster has no node '$EXPECT_NODE' (cluster identity gate)"
# refuse to start under node disk pressure: the NoSchedule taint silently
# turns every rollout into Pending pods and hangs the waits below
kubectl --kubeconfig "$KUBECONFIG_PATH" get node "$EXPECT_NODE" \
  -o jsonpath='{.status.conditions[?(@.type=="DiskPressure")].status}' 2>/dev/null \
  | grep -qx 'True' \
  && die "node $EXPECT_NODE is in DiskPressure (NoSchedule taint active) — free disk space and let the taint clear first"
systemctl is-active nginx >/dev/null 2>&1 || die "host nginx not running"
command -v docker >/dev/null || die "docker not found"
command -v python3 >/dev/null || die "python3 not found"
if [ ! -x "$HELM" ]; then
  log "installing helm to /tmp/opencode/bin (get.helm.sh — Tsinghua github-release does not mirror helm)"
  mkdir -p "$(dirname "$HELM")" /tmp/opencode/helm-extract
  curl -fsSL -o /tmp/opencode/helm.tgz https://get.helm.sh/helm-v3.19.0-linux-amd64.tar.gz \
    || die "helm download failed"
  tar -xzf /tmp/opencode/helm.tgz -C /tmp/opencode/helm-extract linux-amd64/helm
  install -m 755 /tmp/opencode/helm-extract/linux-amd64/helm "$HELM"
fi
"$HELM" version --short >/dev/null || die "helm unusable"
ok "preflight (k3s node gate, nginx, docker, helm $($HELM version --short))"

# double-writer guard: the host-mode controller must NOT run alongside the DS
for pidfile in /tmp/opencode/e2e-scratch/controller.pid /tmp/opencode/controller.pid; do
  if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
    kill "$(cat "$pidfile")"; sleep 2
    log "stopped host-mode controller (pid $(cat "$pidfile")) — double-writer guard"
  fi
done
if pgrep -f '/tmp/opencode/host-nginx-gateway(-e2e)? ' >/dev/null 2>&1; then
  die "a host-mode controller is still running; stop it before the DaemonSet test"
fi
ok "no host-mode controller running (double-writer guard)"

# baseline: include present, backend up, no leftover gateways/routes
grep -qF "$INCLUDE_LINE" /etc/nginx/nginx.conf \
  || die "host nginx.conf lacks the k8s-gw include (run e2e/run.sh scenario 1b first)"
K delete gateway --all --ignore-not-found >/dev/null 2>&1
K delete httproute --all --ignore-not-found >/dev/null 2>&1
# delete-before-create: a killed previous run can leave a backend Deployment
# whose pods are Error/Pending (e.g. after node pressure) and never converge
K delete deploy backend --ignore-not-found >/dev/null 2>&1
wait_until "stale backend pods gone" 60 "! K get pods -l app=backend -o name | grep -q ."
kubectl apply -f "$MANIFESTS/backend.yaml" >/dev/null
wait_until "backend rollout" 180 "kubectl -n $NS rollout status deploy/backend --timeout=5s" \
  && ok "backend ready" \
  || die "backend rollout failed — check: kubectl -n $NS get pods; kubectl describe node $EXPECT_NODE"

# ---------------------------------------------------------------------------
log "build + import controller image"
# Build the binary on the host (warm module cache — the in-container
# `go mod download` of the k8s dependency tree is impractically slow on
# this network) and assemble the runtime image from the prebuilt variant.
# The canonical multi-stage docker/Dockerfile.controller produces the same
# image contents for real releases (GOPROXY build-arg available).
CGO_ENABLED=0 go build -trimpath -o /tmp/opencode/hng-controller-bin \
  "$REPO_ROOT/cmd/host-nginx-gateway" || die "controller build failed"
cp /tmp/opencode/hng-controller-bin "$REPO_ROOT/docker/host-nginx-gateway"
# dl-cdn.alpinelinux.org connections hung indefinitely on this network
# (three builds stalled overnight inside `apk add`); default to the Tsinghua
# Alpine mirror. APK_MIRROR="" restores the canonical CDN.
APK_MIRROR=${APK_MIRROR:-https://mirrors.tuna.tsinghua.edu.cn/alpine}
# buildah's rootless netns (pasta) hangs indefinitely on RUN-step networking
# on this host (observed stalling builds for hours regardless of mirror);
# --network=host runs the apk step in the host netns — builds complete in ~9s.
# timeout: fail loudly instead of stalling the whole E2E on a hung build.
timeout 420 docker build -q --network=host -f "$REPO_ROOT/docker/Dockerfile.controller.prebuilt" \
  --build-arg APK_MIRROR="$APK_MIRROR" \
  -t "$IMG:$IMG_TAG" "$REPO_ROOT/docker" >/dev/null || die "image build failed (network? adjust APK_MIRROR)"
rm -f "$REPO_ROOT/docker/host-nginx-gateway"
rm -f /tmp/opencode/hng-controller.tar
docker save -o /tmp/opencode/hng-controller.tar "$IMG:$IMG_TAG" \
  || die "docker save failed (podman cannot modify an existing archive)"
# ctr refuses to re-import over an existing tag with different content
# ("docker-archive doesn't support modifying existing images") — drop it first
k3s ctr images remove "$IMG:$IMG_TAG" >/dev/null 2>&1 || true
k3s ctr images import --local /tmp/opencode/hng-controller.tar >/dev/null 2>&1 \
  || sudo k3s ctr images import --local /tmp/opencode/hng-controller.tar >/dev/null \
  || die "ctr import failed"
k3s ctr images list 2>/dev/null | grep -q "^$IMG:$IMG_TAG" \
  || die "ctr import verification failed ($IMG:$IMG_TAG not in containerd)"
ok "image $IMG:$IMG_TAG built and imported"

# ---------------------------------------------------------------------------
log "helm install"
kubectl delete gatewayclass host-nginx --ignore-not-found >/dev/null 2>&1
"$HELM" upgrade --install "$RELEASE" "$REPO_ROOT/charts/host-nginx-gateway" \
  --namespace "$CHART_NS" \
  --set image.repository="$IMG" --set image.tag="$IMG_TAG" --set image.pullPolicy=Never \
  >/dev/null || die "helm install failed"
wait_until "DaemonSet pod Ready" 180 \
  "KN rollout status daemonset/$RELEASE-host-nginx-gateway --timeout=5s"
POD=$(DS_POD)
# gate on the Ready condition itself: a CrashLooping pod still has a name,
# and the rollout wait above may have timed out just before it came up
if [ -n "$POD" ] && KN get pod "$POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; then
  ok "DaemonSet pod Ready: $POD"
else
  die "DaemonSet pod not Ready — kubectl -n $CHART_NS describe pod ${POD:-<none>}"
fi
wait_until "pod log shows nsenter exec mode" 60 \
  "KN logs "$POD" 2>/dev/null | grep -q '\"mode\":\"nsenter\"'"
KN logs "$POD" 2>/dev/null | grep -q '"mode":"nsenter"' \
  && ok "exec mode resolved to nsenter (in-cluster auto-detect)" \
  || bad "exec mode not nsenter — check 'KN logs $POD'"

# ---------------------------------------------------------------------------
log "scenario DS-1: route serves through host nginx (include present)"
cat > /tmp/opencode/ds-gw.yaml <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: main
  namespace: e2e
spec:
  gatewayClassName: host-nginx
  listeners:
    - name: web
      port: 8080
      protocol: HTTP
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: backend
  namespace: e2e
spec:
  parentRefs: [{ name: main }]
  hostnames: ["ds.example.com"]
  rules:
    - backendRefs: [{ name: backend, port: 8080 }]
EOF
kubectl apply -f /tmp/opencode/ds-gw.yaml >/dev/null
wait_expect "DS-1 gateway Programmed=True" 90 'gw_cond main Programmed' True Programmed
# bounded retry: right after Programmed flips, nginx graceful-reload drain
# can still answer on stale workers (old upstream IPs) for a few seconds
wait_until "DS-1 response body" 30 \
  "[ \"\$(curl -sf -m 5 -H 'Host: ds.example.com' http://127.0.0.1:8080/ds1 2>/dev/null)\" = 'hng-e2e-backend /ds1' ]" || true
BODY=$(curl -sf -m 5 -H 'Host: ds.example.com' http://127.0.0.1:8080/ds1 2>/dev/null || true)
[ "$BODY" = "hng-e2e-backend /ds1" ] && ok "DS-1 serving via host nginx" || bad "DS-1 curl: '$BODY'"

# ---------------------------------------------------------------------------
log "scenario DS-2: backend missing → GEP-1364 500"
K delete service backend >/dev/null
kick main
wait_expect "DS-2 route ResolvedRefs=False BackendNotFound" 90 'rt_cond backend ResolvedRefs' False BackendNotFound
CODE=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: ds.example.com' http://127.0.0.1:8080/x)
[ "$CODE" = "500" ] && ok "DS-2 static 500" || bad "DS-2 http code: $CODE"
kubectl apply -f "$MANIFESTS/backend.yaml" >/dev/null
K rollout status deploy/backend --timeout=180s >/dev/null

# ---------------------------------------------------------------------------
log "scenario DS-3: port change proves the POD reloads the HOST nginx"
BEFORE=$(sudo stat -c %Y "$CONF_DIR/00-global.conf" 2>/dev/null || echo 0)
K patch gateway main --type=json -p='[{"op":"replace","path":"/spec/listeners/0/port","value":8082}]' >/dev/null
kick main
wait_until "port 8082 bound by host nginx" 90 "ss -tln | grep -q ':8082 '"
ss -tln | grep -q ':8082 ' && ok "DS-3 new port 8082 bound" || bad "DS-3 8082 not bound"
ss -tln | grep -q ':8080 ' && bad "DS-3 old port 8080 still bound" || ok "DS-3 old port 8080 gone"
BODY=$(curl -sf -m 5 -H 'Host: ds.example.com' http://127.0.0.1:8082/ds3 2>/dev/null || true)
[ "$BODY" = "hng-e2e-backend /ds3" ] && ok "DS-3 traffic follows the new port" || bad "DS-3 curl: '$BODY'"
AFTER=$(sudo stat -c %Y "$CONF_DIR/00-global.conf" 2>/dev/null || echo 0)
[ "$AFTER" != "$BEFORE" ] && ok "DS-3 config file rewritten by the pod" || bad "DS-3 config file untouched"

# ---------------------------------------------------------------------------
log "cleanup"
"$HELM" uninstall "$RELEASE" --namespace "$CHART_NS" >/dev/null 2>&1
wait_until "daemonset gone" 60 "! KN get ds $RELEASE-host-nginx-gateway >/dev/null 2>&1"
KN get ds "$RELEASE-host-nginx-gateway" >/dev/null 2>&1 \
  && bad "daemonset still present" || ok "helm release uninstalled"
K delete gateway main --ignore-not-found >/dev/null 2>&1
K delete httproute backend --ignore-not-found >/dev/null 2>&1
# no controller is running now: drop the last generated file so the host dir
# is clean for whichever form runs next
rm -f "$CONF_DIR"/00-global.conf "$CONF_DIR"/00-global.conf.prev

printf '\n========== DAEMONSET E2E RESULT: %d passed, %d failed ==========\n' "$PASS" "$FAIL"
[ "$FAIL" = 0 ]
