# Host Nginx Gateway

[![Go Report](https://img.shields.io/badge/go-1.26-blue)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](#license)
![Release](https://img.shields.io/badge/release-v0.1.0-orange)
![Conformance](https://img.shields.io/badge/GATEWAY--HTTP%20core-37%2F37-brightgreen)

Host-nginx-gateway is a Kubernetes Gateway API implementation that uses your host-side nginx to provide HTTP routing. Existing gateways spawn their own proxy instance and exclusively hold ports like 80/443. Host-nginx-gateway reuses your host nginx to serve additional Gateways. It is designed to co-exist with your existing host-side nginx configuration, which makes it convenient for homelab users and for progressively introducing Kubernetes into an existing bare-metal setup.

We are working on conformance with the [Gateway API](https://github.com/kubernetes-sigs/gateway-api): **GATEWAY-HTTP core 37/37, zero exemptions**.

This project is not affiliated with nginx or Kubernetes.

## Features

- **Co-existence by design** — the controller owns only `/etc/nginx/conf.d/k8s-gw/` and never touches your `nginx.conf`; your existing sites on 80/443 keep serving (missing include → clear status message, never auto-injected)
- **Standard Gateway API v1** — GatewayClass / Gateway / HTTPRoute with spec-compliant status conditions (`Accepted`, `Programmed`, `Conflicted`, `ResolvedRefs`)
- **Ports are declarative** — `spec.listeners[].port` maps 1:1 to nginx `listen` directives, added/removed dynamically on reload; an annotation adds specific-address and IPv6 binds
- **GEP-1364 semantics** — a missing backend keeps the route `Accepted=True` with `ResolvedRefs=False (BackendNotFound)` and returns 500, never silently drops traffic
- **Ingress-nginx-style template pipeline** — `text/template` rendering, `nginx -t` validation against a temp copy of your main config, atomic publish with rollback, hash-based no-op skipping
- **Full HTTPRoute feature set** — path/method/header/query matching, backend weights, request redirects, URL rewrites, request/response/backend header modifiers, request mirroring (percentage via `split_clients`, following the [nginx-gateway-fabric](https://github.com/nginx/nginx-gateway-fabric) mechanism)
- **ReferenceGrant support** — cross-namespace backend and TLS-certificate references are permitted exactly when a grant authorizes them
- **GEP-722 precedence** — exact hostnames win over wildcards, with fall-through between route specificity levels
- **Safe reloads** — post-reload bind verification detects silent failures and rolls back to the previous config
- **Single DaemonSet** — runs one pod per node with its in-cluster ServiceAccount; drives the host nginx through `nsenter` (no host agent, no privileged sidecar beyond the pod itself)

> ⚠️ **One controller per host.** The controller claims `/etc/nginx/conf.d/k8s-gw/` exclusively; do not run two instances against the same nginx.

## Quick Start

```bash
# 1. Clone and install (installs k3s without traefik/servicelb so the
#    host nginx keeps 80/443, then deploys the controller via Helm)
git clone https://github.com/Victrid/HostNginxGateway
cd HostNginxGateway
sudo bash scripts/install-k3s.sh --k3s-install

# 2. Make sure your nginx.conf includes the managed directory
#    (inside the http block):
#    include /etc/nginx/conf.d/k8s-gw/*.conf;
sudo nginx -s reload
```

Deploy a demo workload and expose it:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  selector: {matchLabels: {app: demo}}
  template:
    metadata: {labels: {app: demo}}
    spec:
      containers:
        - name: demo
          image: hashicorp/http-echo
          args: ["-text=hello from k3s"]
          ports: [{containerPort: 5678}]
---
apiVersion: v1
kind: Service
metadata:
  name: demo
spec:
  selector: {app: demo}
  ports: [{port: 80, targetPort: 5678}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: demo-gw
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
  name: demo
spec:
  parentRefs:
    - name: demo-gw
  hostnames: ["demo.example.com"]
  rules:
    - backendRefs:
        - name: demo
          port: 80
```

```bash
kubectl apply -f demo.yaml
curl -H 'Host: demo.example.com' http://<node>:8080/
# hello from k3s
```

## Prerequisites

- A Linux host with nginx installed (any layout; the controller only needs the config prefix and the pid file)
- k3s (or any cluster whose pods are reachable from the host — k3s CNI routes are used directly, no NodePort needed)
- The include line above in your `nginx.conf`
- To serve privileged ports (<1024): nginx master running as root or with `CAP_NET_BIND_SERVICE` (the controller pod itself does not need them)

## Installation

For a cluster that already exists, skip the `--k3s-install` flag. See `scripts/install-k3s.sh --help` for offline image import, node selectors, and custom nginx paths.

```bash
sudo bash scripts/install-k3s.sh \
  --controller-image ghcr.io/victrid/host-nginx-gateway:0.1.0
```

Full Helm values are documented in [`charts/host-nginx-gateway/README.md`](charts/host-nginx-gateway/README.md).

## Usage

### Supported Gateway fields

| Field | Support | Notes |
|---|---|---|
| `listeners[].port` | ✅ | Authoritative; rendered as nginx `listen` |
| `listeners[].protocol` | ✅ | HTTP, HTTPS (`ssl http2`), TLS (SNI passthrough-style routing) |
| `listeners[].hostname` | ✅ | `server_name`, exact and wildcard per spec |
| `listeners[].allowedRoutes` | ✅ | `Same` (default), `Selector`, `All`; `kinds` → `supportedKinds` status |
| `listeners[].tls.certificateRefs` | ✅ | Same-namespace, or cross-namespace with ReferenceGrant |
| `spec.addresses` | ❌ | The controller reports `status.addresses` itself (`--publish-addresses` / node IP) |

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

### How it works

```
k3s API ──watch──► Full Reconciler ──► graph (IR) ──► text/template
                                                          │
                               /etc/nginx/conf.d/k8s-gw/ ◄┘
                     temp main config + nginx -t → atomic publish → nginx -s reload
```

The controller runs as a DaemonSet pod, watches Gateway resources through its in-cluster ServiceAccount, and renders the desired nginx configuration from an internal IR. Publishing validates against a temporary copy of your real `nginx.conf` (your config is never modified), replaces files atomically, signals the host nginx through the host mount namespace, and verifies new listen sockets actually came up — rolling back automatically on failure.

## Limitations

- TLSRoute, GRPCRoute, TCPRoute, and UDPRoute are not implemented; GATEWAY-HTTP core only
- Multi-backend rules with backendRef-level header filters are rejected (stricter than required)
- Listener-level `TLS` with `BackendTLSPolicy` (upstream encryption) is out of scope
- nginx's graceful reload cannot atomically switch a port between wildcard and specific binds — change the port when changing the bind set

See [DESIGN.md](DESIGN.md) for the full design, including the co-existence model, status-condition matrix, and reload state machine, and [e2e/conformance/BASELINE.md](e2e/conformance/BASELINE.md) for the conformance run history.

## Documentation

| Document | Description |
|---|---|
| [DESIGN.md](DESIGN.md) | Architecture, Gateway API mapping, reload pipeline, security model |
| [charts/host-nginx-gateway](charts/host-nginx-gateway/README.md) | Helm chart values and deployment mechanics |
| [e2e/README.md](e2e/README.md) | End-to-end test environment and conformance harness |
| [e2e/conformance/BASELINE.md](e2e/conformance/BASELINE.md) | Official conformance run history (rounds 1–7) |

## Contributing

```bash
go build ./... && go test ./...
sudo -E bash e2e/run-daemonset.sh     # DaemonSet smoke (15 checks)
sudo -E bash e2e/run-conformance.sh   # official conformance suite
```

## License

[MIT](LICENSE)
