# Host Nginx Gateway

Host-nginx-gateway is a Kubernetes Gateway API implementation that uses your host-side nginx to provide HTTP routing. Existing gateways spawn their own proxy instance and exclusively hold ports like 80/443. Host-nginx-gateway reuses your host nginx to serve additional Gateways. It is designed to co-exist with your existing host-side nginx configuration, which makes it convenient for homelab users and for progressively introducing Kubernetes into an existing bare-metal setup.

> [!WARNING]
>  Do Not Use In Production: Current version is preliminary and untested.

This is a preliminary version. We are working on conformance with the [Gateway API](https://github.com/kubernetes-sigs/gateway-api).

This project is not affiliated with nginx or Kubernetes.

## Features

![intro](assets/intro.svg)

> [!WARNING] 
> **One Controller per Node**:  The controller claims `/etc/nginx/conf.d/k8s-gw/` exclusively; do not run two instances against the same nginx. Using DaemonSet can guarantee this. 

## Quick Start

Values you need to take care of:

1. Host nginx's configuration
```yaml
    nginx:
    confDir: /etc/nginx
    pidPath: /host/run/nginx.pid
    binary: /usr/sbin/nginx # Sometimes /usr/bin/nginx
```
2. `nodeSelector`: if you deploy it over multiple devices, pin the node running host Nginx.

Full Helm values are documented in [charts/host-nginx-gateway/README.md](charts/host-nginx-gateway/README.md).

```bash
# Install Gateway API CRDs
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.2/standard-install.yaml

helm repo add host-nginx-gateway https://victrid.github.io/host-nginx-gateway/
helm upgrade --install my-host-nginx-gateway host-nginx-gateway/host-nginx-gateway -n host-nginx-gateway --create-namespace -f <Your Values YAML>
```

Nginx configuration:

Add following line to Nginx server's main conf and run `nginx -s reload` in your host:
```
http {
    include <nginx.confDir>/conf.d/k8s-gw/*.conf;
    ...
}
```

It's suggested to add a catch-all server, to meet Gateway API's expectation; You should configure this on your own.
```
server {
    listen 443 ssl http2;
    server_name _;
    ...
}
```


## Prerequisites

- A Linux host with nginx installed (any layout; the controller only needs the config prefix and the pid file)
- k3s (or any cluster whose pods are reachable from the host — k3s CNI routes are used directly, no NodePort needed)
- The include line above in your `nginx.conf`
- To serve privileged ports (<1024): nginx master running as root or with `CAP_NET_BIND_SERVICE` (the controller pod itself does not need them)

## Usage

### Supported Gateway fields

| Field | Support | Notes |
|---|---|---|
| `listeners[].port` | ✅ | Authoritative; rendered as nginx `listen` |
| `listeners[].protocol` | ✅ | HTTP, HTTPS (`ssl http2`), TLS (SNI passthrough-style routing) |
| `listeners[].hostname` | ✅ | `server_name`, exact and wildcard per spec |
| `listeners[].allowedRoutes` | ✅ | `Same` (default), `Selector`, `All`; `kinds` → `supportedKinds` status |
| `listeners[].tls.certificateRefs` | ✅ | Same-namespace, or cross-namespace with ReferenceGrant |
| `spec.addresses` | ✅ | **Binding intent** (v0.3.0+): `type: IPAddress` entries become the `listen` addresses; each node of the DaemonSet renders only the addresses its fingerprint holds, so addresses partition Gateways across nodes. Only `IPAddress` is bindable — a `Hostname`-type entry makes the listener `Accepted=False` |

### Supported HTTPRoute fields

| Field | Support | Notes |
|---|---|---|
| `hostnames` | ✅ | Intersection with listener hostname drives `server_name` dispatch |
| `rules[].matches` (path/method/headers/queryParams) | ✅ | Map chains, AND within a match, OR across matches |
| `rules[].filters` RequestRedirect / URLRewrite | ✅ | `return 30x` / `rewrite` |
| `rules[].filters` RequestHeaderModifier / ResponseHeaderModifier | ✅ | set/add/remove |
| `rules[].filters` RequestMirror | ✅ | Percentage via `split_clients`; multiple mirrors per rule |
| `rules[].backendRefs` weights | ✅ | nginx upstream `weight=` |
| `rules[].backendRefs` filters (BackendRequestHeaderModifier) | ✅ | Single-backend rules |
| `rules[].timeouts` | ✅ | GEP-1742 subset |
| Cross-namespace `parentRefs` | ✅ | Governed by `allowedRoutes` (no ReferenceGrant needed, per spec) |
| Cross-namespace `backendRefs` / TLS Secrets | ✅ | Requires ReferenceGrant |

### Annotations

The controller's own annotations live in the `hng.victrid.dev/` namespace. Since v0.3.0 there are only the flag-gated escape-hatch annotations below — `listen-addresses` and `publish-addresses` were **removed** (see the migration note).

> [!IMPORTANT]
> **Breaking changes in v0.3.0** (multinode ownership model):
> * `spec.addresses` is now **binding intent**: set `type: IPAddress` entries to pin a Gateway to specific node addresses. Each node renders only the intersection with its own address fingerprint (probed at startup and every 60 s, debounced); a Gateway whose addresses a node does not hold is skipped there entirely (no server block, no status write) — another node serves it. Gateways without `spec.addresses` keep the wildcard bind on every node, and the per-Gateway loopback auto-assignment for indistinct listeners is unchanged.
> * The `hng.victrid.dev/listen-addresses` and `hng.victrid.dev/publish-addresses` annotations (and their legacy `gateway.host-nginx/` spellings) are **removed with no fallback**. Migrate binds to `spec.addresses` (`type: IPAddress`); `status.addresses` is now derived automatically from the listens each node actually renders (`--publish-addresses` remains as an external override, e.g. for a load balancer in front of the nodes).
> * `Hostname`-type `spec.addresses` entries are not bindable: the listener reports `Accepted=False` per the Gateway API spec.

### Escape hatch: raw nginx snippets and extra files (danger flags)

> [!WARNING]
> **Threat model**: annotation writers are trusted at cluster-admin level. Snippets are injected into your host nginx **verbatim** — no sanitization. The safety net is the existing `nginx -t` validation + automatic rollback.

Two opt-in flags unlock escape-hatch annotations (both default to **off**; ignored annotations produce a controller warning log, never a status condition):

* `--dangerously-allow-nginx-snippets` (Helm: `dangerouslyAllowNginxSnippets: true`)
  * `hng.victrid.dev/server-snippet` on a **Gateway**: raw nginx config injected inside every server block rendered from that Gateway (after the `server_name`/`ssl_*` directives, before the `location` blocks).
  * `hng.victrid.dev/location-snippet` on an **HTTPRoute**: raw nginx config appended inside every location block generated from that route's rules.
* `--dangerously-allow-extra-files` (Helm: `dangerouslyAllowExtraFiles: true`)
  * `hng.victrid.dev/extra-files` on a **Gateway**: comma-separated same-namespace refs (`configmap:ns/name`, `secret:ns/name`; `configmap:name` defaults to the Gateway's namespace). Every data key is materialised under `<nginx-conf-dir>/files/<ns>_<name>/<key>` (atomic write + orphan cleanup). Cross-namespace refs and missing objects are skipped with a warning.

Inside snippet text, `@<key>@` is replaced with the absolute materialised path of that extra-file entry (e.g. `content_by_lua_file @app.lua@;`). A placeholder matching no extra file fails the whole sync — `Programmed=False`, nothing written. **Snippets containing other literal `@` characters are your problem**: any `@…@` pair is treated as a placeholder.

These annotations exist only in the new namespace — there is no `gateway.host-nginx/` fallback for them.

### How it works

```
k3s API ──watch──► Full Reconciler ──► graph (IR) ──► text/template
                                                          │
                               /etc/nginx/conf.d/k8s-gw/ ◄┘
                     temp main config + nginx -t → atomic publish → nginx -s reload
```

The controller runs as a DaemonSet pod, watches Gateway resources through its in-cluster ServiceAccount, and renders the desired nginx configuration from an internal IR. Publishing validates against a temporary copy of your real `nginx.conf` (your config is never modified), replaces files atomically, signals the host nginx through the host mount namespace, and verifies new listen sockets actually came up — rolling back automatically on failure.

### Multinode deployments (`spec.addresses`)

In a DaemonSet over several nodes running host nginx, `spec.addresses` (`type: IPAddress`) partitions Gateways across nodes by address:

* Each node's controller probes the node's addresses (startup + every 60 s, with a 2-probe debounce) and renders `listen` directives only for the addresses it actually holds — the rest of the cluster's Gateways are skipped on that node (no server block, no status write; a `ListenerSkippedOnNode` event and the `hng_listeners_skipped_total` metric make the skip observable).
* A Gateway with no `spec.addresses` binds the wildcard on **every** node (all nodes serve it). The whole `127/8` loopback range is considered held by every node, which is what backs the automatic per-Gateway loopback assignment for indistinct listeners.
* `status.addresses` is derived from the listens each node actually renders (plus `--publish-addresses` as an external override). A Gateway whose listeners' effective addresses land on different nodes is reported by each owning node for its own slice — listener status entries are only written by nodes that own the listener.
* Helm/argocd-generated Gateways that need a fixed address should pin it via `spec.addresses` with the node's IP.

### ClusterIP reachability and the connection sidecar

Kubernetes does **not** guarantee that a *host* network namespace can reach Service ClusterIPs — k3s happens to allow it, but eBPF CNIs, certain NetworkPolicies or custom routing can break the path. Host-nginx-gateway dials backends from the host (that is the whole point of using the host nginx), so on such clusters every request would 502 even though all Gateway statuses look healthy.

The controller probes this once at startup: it dials the apiserver Service address (`KUBERNETES_SERVICE_HOST:KUBERNETES_SERVICE_PORT` — a ClusterIP-backed address every cluster has) *from the host network namespace* via `nsenter -t 1 -n`. Unreachable + sidecar mode off → a warning log pointing at this section. The probe never changes behavior by itself.

**When to use**: Gateways report `Programmed=True` but backend requests fail with 502/timeouts, and `curl <clusterIP>:<port>` from the host does not work.

**Chart flag**: `connectionSidecar: true` (default `false`).

**How it works** (3 lines):

1. Backend resolution emits Service **ClusterIP** + backendRef port instead of EndpointSlice pod IPs, rendered as `server unix:/run/hng-proxy/<ns>_<svc>_<port>.sock;` upstream lines (weight/down semantics unchanged).
2. The controller writes the socket→ClusterIP mapping to `/run/hng-proxy/proxy.json` (atomic write, deterministic order); nginx forwards semantics are 100% unchanged — all L7 behavior (Host rewrites, snippets, filters) stays in nginx.
3. An injected sidecar container (`ghcr.io/victrid/hng-sidecar`, same version tag as the controller, no ServiceAccount token at all) watches that file, binds one unix socket per entry and pumps bytes L4-transparently to the ClusterIP *from the pod network namespace*, where reachability is guaranteed.

The single Helm value keeps both containers in sync (sidecar + `--cluster-proxy-sockets=/run/hng-proxy` on the controller + a new RW `/run/hng-proxy` hostPath mounted into both). Turn it off and the next sync returns to direct connections.

**Caveats**:

* **Sidecar down = 502.** The controller does not track the sidecar's liveness; a dead sidecar is indistinguishable from dead backends (nginx retries/fails over per its normal upstream semantics). The sidecar's readiness probe (`/healthz`: mapping loaded and socket count == mapping count — deliberately *not* a dial check) keeps it out of endpoints until it serves.
* **`/run` tmpfs is cleared on reboot — self-healing by design.** Both sides assume the directory may be empty: the sidecar unlinks stale `*.sock` files at startup and rebinds from `proxy.json`; the controller rewrites `proxy.json` on its first sync. No state survives, none is needed.
* Sockets only exist per distinct `service:port` backend; idle connections are reaped after 15 min to prevent deadlocks; half-close (FIN) is propagated in both directions so keep-alive/websocket shutdown behaves like a direct connection.

### Default servers and unknown hosts (FAQ)

**The controller never injects a default server.** Your nginx.conf owns the default server for every port nginx listens on. Every server block the controller emits is backed by a real route claim (Gateway listeners/routes); when no route claims a catch-all (hostname-less) position, the controller emits no block for it at all.

Consequence: if you have not declared a default server yourself, nginx falls back to its own rule — the *first* server block listed for that socket becomes the default. Requests for unknown/incorrect `Host` headers may then be served by one of your Gateway routes. If you care about this (e.g. because the controller shares the host with other services), declare your own default server in `nginx.conf`, e.g.:

```nginx
server {
    listen 80 default_server;
    server_name _;
    return 404;
}
# For HTTPS listeners you also need one per TLS port, with a certificate:
# server {
#     listen 443 ssl default_server;
#     server_name _;
#     ssl_certificate /etc/nginx/snakeoil.pem;
#     ssl_certificate_key /etc/nginx/snakeoil.key;
#     return 404;
# }
```

This is intentional for co-existence: the controller treats default-server policy as the host administrator's business, not its own.

## Limitations

- TLSRoute, GRPCRoute, TCPRoute, and UDPRoute are not implemented (Generally they cannot be handled with Nginx mux)
- Multi-backend rules with backendRef-level header filters are rejected (stricter than required)
- Listener-level `TLS` with `BackendTLSPolicy` (upstream encryption) is out of scope
- nginx's graceful reload cannot atomically switch a port between wildcard and specific binds — change the port when changing the bind set

## License

Apache License, Version 2.0
