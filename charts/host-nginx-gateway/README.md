# host-nginx-gateway (DaemonSet form)

Runs the HostNginxGateway controller as a **DaemonSet** on nodes that host
an nginx (DESIGN.md §8.1). The controller never touches your
`/etc/nginx/nginx.conf`; it owns only `<nginx.confDir>/conf.d/k8s-gw/` and
requires one manual include line (see NOTES.txt on install):

```nginx
http {
    include /etc/nginx/conf.d/k8s-gw/*.conf;
}
```

## How it reaches the host nginx

| Aspect | Mechanism |
|---|---|
| `nginx -t` / `nginx -s reload` | `nsenter -t 1 -n -m -- /usr/sbin/nginx …` (host network + mount namespaces — the only exec path) |
| Config / certs / temp main config | hostPath `/etc/nginx` (ReadWrite) — same files the host nginx includes |
| Liveness probe | `/host/run/nginx.pid` via hostPath `/run`→`/host/run` (ReadOnly) + `kill(pid, 0)`; `hostPID: true` |
| API access | in-cluster ServiceAccount + ClusterRole (least privilege, bundled with the chart) |

## Security

Default `securityContext.privileged: true` — the simplest correct set for
`nsenter -n -m` (setns into the host network and mount namespaces requires
SYS_ADMIN + SYS_PTRACE) plus signaling the root nginx master (root or
CAP_KILL). The narrower alternative:

```yaml
podSecurityContext:
  runAsUser: 0
securityContext:
  privileged: false
  capabilities:
    add: ["SYS_ADMIN", "SYS_PTRACE", "KILL"]
```

The pod must also run as uid 0 (or the owner of the host nginx files): it
rewrites files under the host's `/etc/nginx/conf.d/k8s-gw`.

## Coexistence & exclusivity

* The controller binds **no ports** — the HOST nginx (outside Kubernetes)
  does all listening; no conflicts with in-cluster service load balancers.
* Gateways should use ports not already bound by the host's own servers;
  pin a Gateway to specific node addresses with `spec.addresses`
  (`type: IPAddress`, v0.3.0+): the node whose fingerprint holds the
  address renders the `listen` directive, every other node skips the
  Gateway (see the multinode ownership notes in the main README).
* **WARNING — run at most ONE controller instance per host.** Two
  instances (a second release of this chart, or a manually started
  binary) are both full writers of
  `/etc/nginx/conf.d/k8s-gw/00-global.conf` and would reload the host
  nginx against each other's state (double-writer).

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` / `image.tag` / `image.pullPolicy` | `host-nginx-gateway` / `0.1.0` / `IfNotPresent` | Controller image |
| `nginx.confDir` | `/etc/nginx` | HOST nginx prefix (hostPath mount; owned dir is `<confDir>/conf.d/k8s-gw`) |
| `nginx.pidPath` | `/host/run/nginx.pid` | nginx master pid file — host `/run` is mounted read-only at `/host/run` |
| `nginx.binary` | `/usr/sbin/nginx` | HOST nginx binary path (executed via nsenter) |
| `gatewayClass.create` / `.name` / `.controllerName` | `true` / `host-nginx` / `gateway.host-nginx/controller` | GatewayClass to claim |
| `nodeSelector` / `tolerations` / `affinity` | `{}` | Pin to the node(s) running the host nginx |
| `resources` | `{}` | Container resources |
| `args` | `[]` | Extra controller args appended after the templated ones |
| `dangerouslyAllowNginxSnippets` | `false` | Pass `--dangerously-allow-nginx-snippets`: honor `hng.victrid.dev/server-snippet` (Gateway) / `hng.victrid.dev/location-snippet` (HTTPRoute) raw nginx snippets. Annotation writers must be trusted at cluster-admin level |
| `dangerouslyAllowExtraFiles` | `false` | Pass `--dangerously-allow-extra-files`: honor `hng.victrid.dev/extra-files` (Gateway) — same-namespace ConfigMap/Secret data keys materialised under `<nginx.confDir>/conf.d/k8s-gw/files/` |
| `healthzAddr` | `127.0.0.1:9125` | In-pod healthz/metrics listener (keep off data-plane ports) |
| `probes.enabled` | `true` | exec-based liveness probe against healthz |
| `podSecurityContext` / `securityContext` | uid 0 / privileged | See Security above |
| `serviceAccount.*`, `rbac.create` | create | ServiceAccount / RBAC objects |

## No-registry install (k3s)

```sh
docker build -f docker/Dockerfile.controller -t localhost/hng/controller:v1 .
docker save -o /tmp/controller.tar localhost/hng/controller:v1
sudo k3s ctr images import --local /tmp/controller.tar
helm upgrade --install hng charts/host-nginx-gateway \
  --set image.repository=localhost/hng/controller \
  --set image.tag=v1 --set image.pullPolicy=Never
```
