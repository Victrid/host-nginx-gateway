#!/usr/bin/env bash
# Default-server fixture for the HostNginxGateway e2e/conformance environment.
#
# The controller NEVER injects default servers (DESIGN.md §3.3, v9 policy):
# every server block it emits is backed by a route, and default servers
# belong to the host administrator's nginx.conf. The conformance suite,
# however, contains subtests that REQUIRE an environment default server:
#
#   - unmatched-host requests must answer 404
#     (HTTPRouteHostnameIntersection: non.matching.com/third.com/…,
#     HTTPRouteListenerHostnameMatching: foo.com/no.matching.host);
#   - unmatched-SNI HTTPS requests must answer 404 with a certificate the
#     suite client trusts (HTTPRouteHTTPSListener: unknown-example.org).
#
# This script plays the host-administrator role for the controlled test
# machine: it installs/refreshes a script-managed fixture file under
# /etc/nginx/conf.d/ (NEVER inside the controller-owned k8s-gw/ directory)
# and includes it from the master /etc/nginx/nginx.conf, exactly like a
# hand-written administrator default server.
#
# Sockets covered (the suite listens on ports 80/443):
#   - wildcard 0.0.0.0:80 / :443. Safe unconditionally: catch-all (""-group)
#     blocks only arise on hostname-less listeners, and every hostname-less
#     Gateway in the suite is cross-Gateway indistinct → loopback-assigned;
#     wildcard-bound Gateways only ever carry named-host blocks, which
#     server_name matching always prefers over the default server.
#   - the controller's loopback auto-assignment pool 127.0.0.8–127.0.0.239
#     (internal/provider/graph.go autoAssignBase/autoAssignSpan:
#     127.0.0.(8 + fnv32a(ns/name) mod 232), linear probing) on both ports —
#     e.g. httproute-hostname-intersection-all's unmatched-host 404s land on
#     its assigned socket.
#
# EXCLUDED from the pool: the suite's BASE Gateways (same-namespace,
# all-namespaces, backend-namespaces). Their sockets carry the suite's
# catch-all (hostname-less) routes, and the suite reaches those routes by
# sending requests with Host = the Gateway's loopback IP — nginx routes a
# non-matching Host to the socket's DEFAULT server, which must therefore be
# the route-backed catch-all block (the controller lists it first), not the
# fixture. A default_server there would break every base-gateway traffic
# test. The base Gateways are suite constants (utils/suite), so their
# addresses are computable with the controller's own hash.
#
# TLS: the 443 blocks present the suite's own certificate once it has been
# materialized by the controller
# (/etc/nginx/conf.d/k8s-gw/certs/gateway-conformance-infra_tls-validity-
# checks-certificate.pem — a combined cert+key PEM, so both directives
# point at it). Before that file exists (first run, before the cluster is
# up) a script-generated self-signed fixture certificate is used;
# e2e/conformance/conformance_test.go re-invokes this script with
# --wait-suite-cert after the base Gateways are programmed.
#
# Idempotent: re-applied on every invocation; nginx is reloaded only when
# the fixture file or the include line actually changed.
#
# Usage: sudo bash e2e/install-default-server-fixture.sh [--wait-suite-cert]
set -u -o pipefail

FIXTURE_CONF=/etc/nginx/conf.d/k8s-gw-default-fixture.conf
FIXTURE_CERT=/etc/nginx/conf.d/k8s-gw-default-fixture.pem
FIXTURE_KEY=/etc/nginx/conf.d/k8s-gw-default-fixture.key
FIXTURE_INCLUDE='include /etc/nginx/conf.d/k8s-gw-default-fixture.conf;'
MAIN_CONF=/etc/nginx/nginx.conf
SUITE_CERT=/etc/nginx/conf.d/k8s-gw/certs/gateway-conformance-infra_tls-validity-checks-certificate.pem
WAIT_SUITE_CERT=0
# The controller's loopback auto-assignment pool (graph.go).
ASSIGN_BASE=8
ASSIGN_SPAN=232
# Suite Gateways whose sockets carry catch-all routes reached via
# Host=<loopback IP> — their sockets must keep the route-backed catch-all
# block as default server (see header comment).
EXCLUDE_GATEWAYS=(
  gateway-conformance-infra/same-namespace
  gateway-conformance-infra/all-namespaces
  gateway-conformance-infra/backend-namespaces
)

for arg in "$@"; do
  case "$arg" in
    --wait-suite-cert) WAIT_SUITE_CERT=1 ;;
    *) echo "install-default-server-fixture: unknown flag: $arg" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" = 0 ] || { echo "install-default-server-fixture: run as root" >&2; exit 1; }
[ -f "$MAIN_CONF" ] || { echo "install-default-server-fixture: $MAIN_CONF not found" >&2; exit 1; }

# fnv32a replicates the controller's hash (internal/provider graph.go) so
# the fixture can compute the same per-Gateway loopback addresses.
fnv32a() {
  local h=2166136261 i b
  for ((i = 0; i < ${#1}; i++)); do
    printf -v b "%d" "'${1:i:1}"
    h=$(( (h ^ b) ))
    h=$(( (h * 16777619) & 0xFFFFFFFF ))
  done
  echo "$h"
}

declare -A EXCLUDE_ADDR
for gw in "${EXCLUDE_GATEWAYS[@]}"; do
  n=$(( ASSIGN_BASE + $(fnv32a "$gw") % ASSIGN_SPAN ))
  EXCLUDE_ADDR[$n]=1
done

TLS_CERT=$FIXTURE_CERT
TLS_KEY=$FIXTURE_KEY
if [ "$WAIT_SUITE_CERT" = 1 ]; then
  deadline=$((SECONDS + 90))
  while [ ! -f "$SUITE_CERT" ] && [ "$SECONDS" -lt "$deadline" ]; do
    sleep 2
  done
fi
# The suite cert is the controller-materialized combined PEM (cert+key in
# ONE file — the template renders ssl_certificate and ssl_certificate_key
# with the same path), so both directives point at it.
if [ -f "$SUITE_CERT" ]; then
  TLS_CERT=$SUITE_CERT
  TLS_KEY=$SUITE_CERT
fi

if [ ! -f "$FIXTURE_CERT" ] || [ ! -f "$FIXTURE_KEY" ]; then
  openssl req -x509 -newkey rsa:2048 -keyout "$FIXTURE_KEY" -out "$FIXTURE_CERT" \
    -days 3650 -nodes -subj "/CN=hng-e2e-default-fixture" >/dev/null 2>&1 \
    || { echo "install-default-server-fixture: certificate generation failed (openssl?)" >&2; exit 1; }
  chmod 600 "$FIXTURE_KEY"
fi

tmp=$(mktemp) || exit 1
{
  echo "# Default-server fixture for the HostNginxGateway e2e/conformance"
  echo "# environment — installed by e2e/install-default-server-fixture.sh,"
  echo "# NOT by the controller. The controller never injects default"
  echo "# servers (DESIGN.md §3.3): unmatched hosts follow nginx's own"
  echo "# default-server rules, owned by the host administrator. This file"
  echo "# IS that administrator-provided default server for the test"
  echo "# machine; it answers 404 for every Host/SNI no route claims."
  echo "#"
  echo "# wildcard sockets + the controller's loopback auto-assignment pool"
  echo "# (127.0.0.8–127.0.0.239, graph.go autoAssignBase/Span), ports 80/443,"
  echo "# EXCEPT the base Gateways' sockets (their catch-all routes are"
  echo "# reached via Host=<loopback IP> and must stay the default server):"
  for n in "${!EXCLUDE_ADDR[@]}"; do
    echo "#   excluded: 127.0.0.$n (catch-all-route Gateway)"
  done
  echo "server {"
  echo "    listen 80 default_server;"
  echo "    server_name _;"
  echo "    return 404;"
  echo "}"
  echo "server {"
  echo "    listen 443 ssl default_server;"
  echo "    server_name _;"
  echo "    ssl_certificate $TLS_CERT;"
  echo "    ssl_certificate_key $TLS_KEY;"
  echo "    return 404;"
  echo "}"
  n=$ASSIGN_BASE
  end=$((ASSIGN_BASE + ASSIGN_SPAN))
  while [ "$n" -lt "$end" ]; do
    if [ -z "${EXCLUDE_ADDR[$n]:-}" ]; then
      echo "server { listen 127.0.0.$n:80 default_server; server_name _; return 404; }"
      echo "server { listen 127.0.0.$n:443 ssl default_server; server_name _; ssl_certificate $TLS_CERT; ssl_certificate_key $TLS_KEY; return 404; }"
    fi
    n=$((n + 1))
  done
} > "$tmp"

changed=0
if ! cmp -s "$tmp" "$FIXTURE_CONF"; then
  mv "$tmp" "$FIXTURE_CONF" || exit 1
  changed=1
else
  rm -f "$tmp"
fi
if ! grep -qF "$FIXTURE_INCLUDE" "$MAIN_CONF"; then
  # Anchor on the k8s-gw include (asserted present by the runners' preflight):
  # the fixture include lands directly below it, inside the http block. It
  # deliberately comes AFTER the k8s-gw include: on sockets WITHOUT a
  # default_server the first-listed block wins, and route-backed catch-all
  # blocks (emitted first by the controller) must outrank the fixture.
  sed -i '\|include /etc/nginx/conf.d/k8s-gw/\*\.conf;|a '"$FIXTURE_INCLUDE" \
    "$MAIN_CONF" \
    || { echo "install-default-server-fixture: could not add the include to $MAIN_CONF" >&2; exit 1; }
  changed=1
fi
if ! nginx -t >/dev/null 2>&1; then
  nginx -t
  echo "install-default-server-fixture: nginx -t rejected the fixture" >&2
  exit 1
fi
if [ "$changed" = 1 ]; then
  nginx -s reload
  echo "install-default-server-fixture: installed and reloaded (TLS cert: $TLS_CERT)"
else
  echo "install-default-server-fixture: up to date (TLS cert: $TLS_CERT)"
fi
