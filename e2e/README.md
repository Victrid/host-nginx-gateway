# HostNginxGateway E2E smoke (DESIGN.md §10.4)

`run-daemonset.sh` drives the DaemonSet deployment form (DESIGN.md §8.1,
the only deployment form) against a real k3s + the host nginx on this
machine. Latest runs: DaemonSet smoke **15 passed, 0 failed**;
conformance suite **37 passed, 0 failed, 0 skipped** (GATEWAY-HTTP core).

`run-conformance.sh` runs the **official Gateway API conformance suite**
against the DaemonSet deployment — see `e2e/conformance/BASELINE.md` for
the recorded baseline (suite v1.6.2, GATEWAY-HTTP profile: **23 passed /
3 failed / 11 skipped** core), the exemption/skip tables, and the harness
adaptations (loopback bind + status.address shims).

## Environment setup (as performed on this machine)

k3s is NOT preinstalled here; it was installed locally for the test:

```sh
# 1. k3s binary from the Tsinghua mirror (no GitHub access needed):
curl -fsSL -o /tmp/opencode/k3s \
  https://mirrors.tuna.tsinghua.edu.cn/github-release/k3s-io/k3s/LatestRelease/k3s
curl -fsSL -o /tmp/opencode/sha256sum-amd64.txt \
  https://mirrors.tuna.tsinghua.edu.cn/github-release/k3s-io/k3s/LatestRelease/sha256sum-amd64.txt
sha256sum -c <(grep ' k3s$' /tmp/opencode/sha256sum-amd64.txt)   # verify
sudo install -m 755 /tmp/opencode/k3s /usr/local/bin/k3s

# 2. airgap system images (optional but robust — avoids registry access):
curl -fsSL -o /tmp/opencode/k3s-airgap-images-amd64.tar.zst \
  https://mirrors.tuna.tsinghua.edu.cn/github-release/k3s-io/k3s/LatestRelease/k3s-airgap-images-amd64.tar.zst
zstd -d /tmp/opencode/k3s-airgap-images-amd64.tar.zst
sudo mkdir -p /tmp/opencode/k3s-data/agent/images
sudo mv /tmp/opencode/k3s-airgap-images-amd64.tar /tmp/opencode/k3s-data/agent/images/

# 3. server: traefik/servicelb disabled (they would grab :80/:443 — the
#    whole point of this project is that the HOST nginx owns them)
sudo nohup k3s server \
  --disable traefik --disable servicelb --disable metrics-server \
  --data-dir /tmp/opencode/k3s-data \
  --node-name hng-e2e \
  --write-kubeconfig /tmp/opencode/k3s-kubeconfig --write-kubeconfig-mode 644 \
  > /tmp/opencode/k3s-server.log 2>&1 &
# Gateway API CRDs ship with k3s v1.37+ (helm chart gateway-api-crd) and
# establish themselves within ~1 min.

# 4. backend image (offline): docker build + import into k3s containerd
cd e2e/backend-image && go mod init hng-e2e-backend 2>/dev/null; \
  CGO_ENABLED=0 go build -o e2e-backend . && docker build -q -t hng-e2e/backend:v1 . && \
  docker save -o /tmp/opencode/hng-e2e-backend-v1.tar hng-e2e/backend:v1
sudo k3s ctr images import --local /tmp/opencode/hng-e2e-backend-v1.tar

# 5. nginx
sudo systemctl start nginx    # /run/nginx.pid, default server on :80
```

## Running

```sh
sudo -E bash e2e/run-daemonset.sh     # DaemonSet smoke (15 checks)
sudo -E bash e2e/run-conformance.sh   # official conformance suite (GATEWAY-HTTP core)
RUN_CONFORMANCE=1 sudo -E bash e2e/run-daemonset.sh   # optional combined run
```

The smoke script builds the controller image from the repo, installs the
helm chart, and validates the DaemonSet form end-to-end. The conformance
script reuses the same deployment. Both **ignore any ambient `KUBECONFIG`**
(`E2E_KUBECONFIG` overrides) and refuse to run unless the target cluster has
node `hng-e2e` (`EXPECT_NODE` overrides) — an earlier revision once inherited
the operator's real-cluster kubeconfig and ran its reset steps against the
wrong cluster; the identity gate prevents any repeat.

## Historical note: the removed host-mode E2E

The original host-binary E2E (scenarios 1–7, 41 checks) validated the
removed systemd deployment form; it was deleted along with that form. Two
product limitations it documented have since been **fixed in product code**:

* **Reload signal blindness** — `internal/dataplane/publisher.go` now tails
  the owned error log for `bind()` failures after reloads that add listen
  sockets and rolls back (DESIGN §5.2). Detection is errno-classified:
  EADDRINUSE (98) lines for sockets the previously-applied config already
  listens on are tolerated (the master hands held sockets over on reload),
  while unassignable addresses (99) and other failures roll back.
* **Wildcard↔specific same-port listen transitions** — partially mitigated
  by the same verification; changing the port alongside the bind set remains
  the clean path.

The cert-orphan ordering, include-glob recursion, temp-main-config location,
`kill(pid,0)` EPERM, and healthz port-collision bugs it found are all fixed
(see git history and DESIGN.md §5).

## Manual leftovers & teardown

Currently left running on this machine: k3s (data in
`/tmp/opencode/k3s-data`, kubeconfig `/tmp/opencode/k3s-kubeconfig`),
nginx (now WITH the k8s-gw include in `/etc/nginx/nginx.conf`), the test
backend in ns `e2e`. The controller is stopped. Teardown:

```sh
sudo pkill -f 'k3s server'            # stop k3s
sudo rm -rf /tmp/opencode/k3s-data /usr/local/bin/k3s
sudo sed -i '\|include /etc/nginx/conf.d/k8s-gw/\*.conf;|d' /etc/nginx/nginx.conf
sudo rm -rf /etc/nginx/conf.d/k8s-gw && sudo systemctl reload nginx
```

## DaemonSet mode (`run-daemonset.sh`)

Covers the DaemonSet deployment form (DESIGN.md §8.1) on the same
environment. Preconditions beyond the `run.sh` setup: docker, helm, and
the k8s-gw include already present in nginx.conf (scenario 1b).

* **helm**: the Tsinghua github-release mirror does not carry helm
  (`/github-release/helm/helm/` → 404), so the script auto-installs
  **helm v3.19.0 from get.helm.sh** to `/tmp/opencode/bin/helm`. Override
  with `HELM=<path>`. (Decision documented per the lane brief.)
* **image**: built from `docker/Dockerfile.controller.prebuilt` — the
  binary is compiled on the host (warm module cache; the in-container
  `go mod download` of the k8s dependency tree is impractically slow on
  this network — proxy.golang.org is unreachable from build containers,
  and goproxy.cn took >20 min). The canonical multi-stage
  `docker/Dockerfile.controller` produces identical image contents
  (alpine + util-linux/nsenter + ca-certificates + static binary) and has
  a `GOPROXY` build-arg for restricted networks. The image is imported
  into k3s with `k3s ctr images import --local` (no registry) and the
  chart installs with `image.pullPolicy=Never`.
* **double-writer guard**: the script refuses to start if a host-mode
  controller is still running (pidfiles + process scan) — at most one
  controller may run per host.

Checks:

| # | Check |
|---|-------|
| DS-0 | helm install; DaemonSet pod Ready; pod log shows `mode=nsenter` (in-cluster auto-detect via `KUBERNETES_SERVICE_HOST`) |
| DS-1 | Gateway `Programmed=True` (in-cluster ServiceAccount, RBAC); route serves through the HOST nginx via Host header |
| DS-2 | backend Service deleted → route `ResolvedRefs=False/BackendNotFound`, endpoint answers 500 |
| DS-3 | listener port 8080→8082 → host nginx actually reloaded **by the pod via nsenter**: new port bound, old port gone, traffic follows, `00-global.conf` rewritten |

Cleanup: `helm uninstall`, gateways/routes removed, last generated config
dropped from the host dir so either deployment form can run next.
