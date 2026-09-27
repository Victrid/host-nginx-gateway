# Gateway API Conformance Baseline

Recorded baseline of HostNginxGateway against the **official** Gateway API
conformance suite. Round 1 captured the honest starting point; round 2 fixed
small status-semantics gaps and a suite-harness environment poison; round 3
implemented per-route hostname dispatch + ReferenceGrant; round 4 compressed
suite timeouts, verified the Host-preservation + suffix-match fixes live, and
isolated the remaining failures to GEP-722 hostname-precedence fall-through.
Round 5 was the B-grade conformance push (precedence fall-through,
controller-written status.addresses, reload-effect verification, weights,
filters, matching — all harness shims removed). Round 6
implemented cross-namespace HTTPRoute attachment (allowedRoutes-governed) —
the last §3.5 deviation is gone and GATEWAY-HTTP core is 37/37 with nothing
skipped. Round 7 (this commit) implemented RequestMirror (single/multiple/
percentage) and backendRef-level RequestHeaderModifier — the exemption list
is now EMPTY.

## Suite & environment

| What | Value |
|---|---|
| Suite | `sigs.k8s.io/gateway-api/conformance` **v1.6.2** (official, unmodified) |
| Suite module | `e2e/conformance/go.mod` (separate Go module; root `go.mod` pins `sigs.k8s.io/gateway-api v1.6.2` — same release) |
| Module-layout note | Since gateway-api ~v1.4 the conformance suite ships as its own Go module (`sigs.k8s.io/gateway-api/conformance`); it is NOT part of the main module zip. Pinned to v1.6.2 to match the repo's gateway-api version. |
| Cluster CRDs | gateway-api **v1.6.1**, channel `standard` (k3s v1.37.0+k3s1 `gateway-api-crd` helm chart). Run with `--allow-crds-mismatch` (v1.6.1 ↔ v1.6.2 skew; the report then records `gatewayAPIVersion: UNDEFINED`). |
| Profile | **GATEWAY-HTTP** (core; extended features not declared this round) |
| Controller | DaemonSet form via `charts/host-nginx-gateway` (DESIGN.md §8.1), pod drives the HOST nginx through `nsenter` |
| GatewayClass / controllerName | `host-nginx` / `gateway.host-nginx/controller` (wired via `--gateway-class`) |
| Report | `e2e/conformance/report.yaml` (suite-generated ConformanceReport; `ConformanceProfiles` is set so the report is CR-ready) |
| Entry point | `sudo -E bash e2e/run-conformance.sh` (self-contained: deploy → run → teardown) |

### Why not GATEWAY-HTTPS

The base manifest `same-namespace-with-https-listener` defines four 443
listeners distinguished by hostname; §3.2 conflicts `*.wildcard.org` with
`fourth-example.wildcard.org` (they genuinely overlap). More importantly its
listeners bind **wildcard 443** before the harness can adapt anything, and
nginx cannot transition a port from wildcard to specific binds via reload
(see "round-2 lessons" below). The harness therefore deletes this Gateway
when running the GATEWAY-HTTP profile; admitting GATEWAY-HTTPS needs the
§3.2 same-port/multi-hostname policy plus the bind-transition problem solved
first (round B/C).

## Results

| Round | Passed | Failed | Skipped (not-supported/skip-tests) | Notes |
|---|---|---|---|---|
| 1 (pre-fix baseline) | 12 | 19 | 122 | exemption of `ReferenceGrant` was a silent no-op (see below); several failures were collateral of the round-2 harness poison |
| 2 | 23 | 3 | 122 | GATEWAY-HTTP core; the 3 failures share ONE root cause (per-route hostname dispatch) |
| 3 | 23 | 3 | 122 | per-route hostname dispatch + ReferenceGrant implemented; counts unchanged — run-diagnosis exposed the *second* root cause (Host-header preservation) |
| 4 | 23 | 3 | 11 | timeout compression (60s/180s → ~5s; suite wall 306s→49s) + Host preservation + suffix-match fixes verified live: `HTTPRouteHostnameIntersection` passes 25/28 subtests; the 3 remaining failures are narrowed to GEP-722 hostname-precedence fall-through |
| 4 | 23 | 3 | 11 | timeout compression (60s/180s → ~5s; suite wall 306s→49s) + Host preservation + suffix-match fixes verified live: `HTTPRouteHostnameIntersection` passes 25/28 subtests; the 3 remaining failures are narrowed to GEP-722 hostname-precedence fall-through |
| 5 | 36 | 0 | 1 | **GATEWAY-HTTP core green minus one.** B-grade feature push (weights, filters, method/header/query matching), GEP-722 precedence fall-through, controller-written status.addresses, per-Gateway loopback address assignment, reload-effect verification — ALL THREE harness shims removed. The single skip was `HTTPRouteCrossNamespace` (§3.5 deviation). |
| 6 | 37 | 0 | 0 | **GATEWAY-HTTP core FULLY GREEN, nothing skipped.** Cross-namespace HTTPRoute attachment implemented per Gateway API v1: the TARGET Gateway's per-listener `allowedRoutes.namespaces` governs (Same/All/Selector/None), `allowedRoutes` IS the authorization (no ReferenceGrant for parentRefs), backendRefs keep defaulting to the route's namespace. |
| **7 (this commit)** | **37** | **0** | **0** | **37/37 with a ZEROED exemption list.** RequestMirror (single + multiple + percentage via `split_clients $request_id` + internal mirror locations) and backendRef-level RequestHeaderModifier implemented per Gateway API v1 / the NGF mechanism (DESIGN.md §5.1.1). The four exemptions were no-ops for core test selection (all four are EXTENDED features), but they documented non-support; with the features implemented they are removed. Zeroing them flips the suite to GWC-status inference (unsupported), so the harness now declares the core feature set via `-supported-features` instead — identical selection. |
| 9 (default-server policy) | 37 | 0 | 0 | **The controller no longer emits a synthetic default-server block** (DESIGN.md §3.3 v9: default servers belong to the host administrator's nginx.conf). Every emitted block is backed by a route claim; unmatched hosts follow nginx's own default-server rules. The test environment takes the administrator role via the script-managed fixture (see "Harness adaptations"). 37/37 unchanged. |

Suite-reported core counts (round 7, `e2e/conformance/report.yaml`):
**37 passed, 0 failed, 0 skipped** (GATEWAY-HTTP core, `result: success`).
Round 7 keeps round 6's counts exactly: the four removed exemptions
(`HTTPRouteRequestMirror`, `HTTPRouteRequestMultipleMirrors`,
`HTTPRouteRequestPercentageMirror`,
`HTTPRouteBackendRequestHeaderModification`) are EXTENDED features and the
GATEWAY-HTTP core profile never ran their tests — exempting them was a
no-op for test selection, but it documented non-support. With mirrors and
backendRef-level header modifiers implemented (unit-tested at the graph,
translation and render layers; the same `split_clients` + internal
mirror-location mechanism NGF uses), the run script's exemption list is
empty and the claim is real.

One harness adjustment was REQUIRED by zeroing the exemptions: with an
empty exempt list (and no explicit supported list), the v1.6.2 suite
switches to inferring supported features from GatewayClass
`status.supportedFeatures` — which this controller does not write yet —
and aborts with "no supported features were determined". The harness now
declares the implemented CORE set explicitly
(`-supported-features=Gateway,ReferenceGrant,HTTPRoute`), which keeps the
manual path with test selection byte-for-byte identical to the
4-exemption rounds (manual ∪ profile-core = the 3 core features).
Extended features are deliberately NOT declared: the GATEWAY-HTTP core
report makes no extended claims. Declaring the implemented extended set
(mirrors included) — or, better, the controller writing GatewayClass
`status.supportedFeatures` and letting the suite infer — is the natural
next round: the run then grows to cover the implemented extended tests
live.

### Round-7 changes (RequestMirror + BackendRequestHeaderModification — exemptions zeroed)

Product feature (spec-first, Gateway API v1, NGF mechanism — not NGF code):

1. **IR extensions** (`internal/contract`): `Location.Mirrors` (one entry per
   mirror target; the renderer emits `mirror <path>;`), `Location.Internal`,
   `Location.MirrorGate` (the split_clients variable a gated mirror location
   consults) and `Location.ProxyPassURI` (`"$request_uri"` — the mirror
   subrequest URI is the internal path, so the original client request
   URI must be restored), plus http-context `SplitClients` blocks.
2. **Graph layer** (`internal/provider/graph.go`): RequestMirror filters are
   parsed and resolved like backendRefs (same Service/port/EndpointSlice
   resolution, ReferenceGrant honored). Percent/fraction normalized to a
   0–100 float (fraction `num*100/den`, denominator defaults to 100;
   percent=0 drops the mirror; 100/unset mirrors unconditionally).
   Validation per spec — percent+fraction mutually exclusive, bounds —
   violations drop the RULE (PartiallyInvalid). An unresolvable mirror
   backendRef drops only the MIRROR, reports `ResolvedRefs=False`
   (BackendNotFound/RefNotPermitted) and leaves the rule serving (spec:
   "dropped from the Gateway … not configure this backend"). Deterministic
   mirror paths `hng_mirror_<hash>` per (route, rule, target). Multiple
   mirror filters per rule are allowed (spec). backendRef-level
   RequestHeaderModifier is accepted on single-backend rules; multi-backend
   rules carrying one are dropped (the weighted combined upstream cannot
   carry per-backend headers — documented deviation; stricter than NGF,
   which rejects ALL backendRef filters).
3. **Translation** (`translate.go` / `dispatch.go`): mirrors merge per
   location — nginx `mirror` directives are location-wide — with the max
   percentage winning per target (NGF behavior); `<pct>%` strictly between
   0 and 100 emits `split_clients $request_id $hng_sc_<hash>` (two-decimal
   distributions, `*` → "") and a `if ($gate = "") { return 204; }` gate
   inside the internal mirror location. Mirror locations carry the
   rule-level RequestHeaderModifier (mirrored copies, NGF behavior) and
   `proxy_pass http://<upstream>$request_uri`. Mirror upstreams reuse the
   existing `ns_svc_port` generation. The backendRef-level modifier lands
   on the business location when the location unambiguously serves a
   single-backend rule, applied after (and overriding same-name entries of)
   the rule-level modifier (GEP-1310).
4. **Dataplane** (`nginx.tmpl`, `template.go`): `split_clients` rendering,
   `internal;` / gate / `mirror` / suffixed `proxy_pass` directives.
5. **Exemptions**: `e2e/run-conformance.sh` EXEMPT_FEATURES list is now
   empty (rounds 5–6 removed everything else). Zeroing it flips the suite
   into GatewayClass-status feature inference, which this controller does
   not support yet — so the harness now also passes
   `-supported-features=Gateway,ReferenceGrant,HTTPRoute` (explicit core
   declaration; identical test selection to the previous manual
   empty-set-∪-profile-core path).

Unit tests: `internal/provider/mirror_test.go` (spec-derived: 100%/unset →
no split_clients, percentage gating config, 0% dropped, fraction conversion
incl. default denominator, invalid fraction/percent → rule dropped,
unresolved mirror backend → route functional + ResolvedRefs=False,
multiple targets, max-percentage dedup within one rule and across
reachable cases of one location, backendRef modifier on the main backend,
rule-vs-backend same-name precedence, multi-backend + backend-filter
rejection) and `internal/dataplane/render_mirror_test.go` (split_clients
block shape, gated/ungated internal mirror locations, `mirror` directives,
Host preservation + modifiers on the mirrored copy).

Round 6 ran exactly one more test than round 5: `HTTPRouteCrossNamespace`
left the `--skip-tests` list, and the previously passing negative case
`HTTPRouteInvalidCrossNamespaceParentRef` (expects
`Accepted=False/NotAllowedByListeners`) keeps passing — the allowedRoutes
evaluation is the authorization path for BOTH tests.

**Timing comparison round 3 → round 4:** `go test` wall time **305.7s →
48.8s** (6.3×). Failing tests now fail in ≤ 8s instead of 60–153s — the
compressed timeouts convert "stuck implementation" stalls into fast
signal. Passing tests are unaffected (most finish in ≤ 2s either way).

### Round-6 changes (cross-namespace HTTPRoute attachment — last skip cleared)

Product feature (spec-first, Gateway API v1):

1. **Cross-namespace parentRef attachment** (`internal/provider/graph.go`
   `resolveParentRef`/`routeAllowed`): the §3.5-v3 unconditional refusal of
   cross-namespace attachment (`Accepted=False/RefNotPermitted`) is REMOVED.
   Attachment is now governed ONLY by the TARGET Gateway's per-listener
   `allowedRoutes.namespaces`, evaluated per matched listener:
   `from=Same` (also the CRD default `{namespaces:{from:Same}}` when
   allowedRoutes/namespaces/from is unset) keeps admitting same-namespace
   routes and reports `NotAllowedByListeners` for cross-namespace ones;
   `from=All` admits any namespace; `from=Selector` matches the ROUTE
   namespace's labels (Namespace objects are watched since round 2);
   `from=None` refuses everything. `sectionName`/`port` matching and the
   §3.3 hostname-intersection rules are namespace-independent and unchanged.
   **Trust model (spec):** `allowedRoutes` IS the authorization for
   Gateway-route attachment — ReferenceGrant is neither required nor
   consulted for parentRefs. Grants continue to gate cross-namespace
   backendRefs (route ns → Service ns) and listener certificateRefs
   (Gateway → Secret) exactly as in round 3. Status side: status.parents[]
   carries the spec parentRef verbatim (including the Gateway's namespace)
   with this controller's controllerName; the writer needs no change (its
   read-modify-write is keyed on the ROUTE object, not the Gateway's
   namespace). backendRefs keep defaulting to the ROUTE's namespace
   (`resolveBackend`) regardless of where the attached Gateway lives.
   Unit tests: `internal/provider/crossns_test.go` (from=All attach +
   upstream/status/data-plane assertions, from=Selector match/no-match/
   unknown-ns, from=Same reject + same-ns control, per-listener policy
   precedence on one Gateway, hostname-intersection combination,
   route-ns backend default + grant-scoped cross-ns backendRef), and the
   two round-2 tests that encoded the refusal
   (`TestRouteAttachment_AllowedRoutes`, `TestSameNamespaceRestriction`)
   now assert the spec behaviour.

Exemption/skip list changes (`e2e/run-conformance.sh`):
- `--skip-tests` is now EMPTY: `HTTPRouteCrossNamespace` is implemented
  (the flag itself is still passed with an empty value — byte-identical to
  the suite's flag default).
- `--exempt-features` unchanged (the four §9 non-goals).

### Round-5 changes (B-grade conformance push — all shims removed)

Product features implemented (spec-first, Gateway API v1):

1. **GEP-722 hostname-precedence fall-through** (`internal/provider/translate.go`,
   `internal/provider/dispatch.go`): location evaluation is now SOCKET-scoped.
   All listeners whose (post-annotation) listen sets coincide share one nginx
   socket, and their location entries are grouped per effective hostname.
   Within each listener's claim, exact-hostname blocks absorb the entries of
   covering wildcard groups and of the catch-all group (most specific first),
   so a request whose Host matches a more specific route without a matching
   PATH falls through to a broader route. When a listener's own claim and a
   route hostname share no host (apex listener vs wildcard route), the
   attachment serves nothing (`refineHostname`). An attachment-less claim set
   still renders an empty block. A synthetic empty default block answers 404
   for hosts no route claims. (SUPERSEDED in round 9: the synthetic default
   block was removed — default servers belong to the host administrator's
   nginx.conf; the e2e environment provides its own via
   `e2e/install-default-server-fixture.sh`.)
2. **Listener distinctness fixed to spec** (`internal/provider/graph.go`):
   same-Gateway indistinctness is now EQUAL hostnames (or both empty) only —
   an exact listener and its covering wildcard are distinguishable
   (GEP-722: requests match most specific first) and both program. This
   un-broke `HTTPRouteListenerHostnameMatching` and lets the HTTPS base
   Gateway's four listeners program without §3.2 conflicts.
3. **Per-Gateway loopback address assignment** (`graph.go`
   `assignCrossGatewayAddresses`): cross-Gateway indistinct listener pairs
   (the controller merges ALL Gateways onto one data plane; the spec's
   distinctness rules apply to the merged set) are separated by assigning
   each Gateway its own 127.0.0.N bind (`127.0.0.(8 + hash(ns/name) mod 232)`,
   linear probing; hash-stable so unrelated Gateways never shift another's
   address). Explicit `listen-addresses` annotations (v0.2.0:
   `hng.victrid.dev/listen-addresses`, legacy spelling still read with a
   deprecation warning) win; unresolvable
   collisions keep the §3.2 both-sides-Conflicted behaviour. This replaces
   the round-2 BIND SHIM with a product feature.
4. **Gateway status.addresses** (`translate.go StatusAddresses`,
   `internal/status/writer.go`, `cmd/host-nginx-gateway/addresses.go`):
   controller-written, type IPAddress. Source precedence:
   `hng.victrid.dev/publish-addresses` annotation (v0.2.0 renamed from
   `gateway.host-nginx/publish-addresses`; the legacy spelling is still
   read with a deprecation warning) → `--publish-addresses`
   flag → auto-assigned loopback → node primary IP (`HNG_NODE_IP` downward
   API in the DaemonSet form, else interface-route detection). Replaces the
   round-2 ADDRESS SHIM.
5. **Reload-effect verification** (`internal/dataplane/publisher.go`): a
   reload that ADDS listen sockets is verified by tailing the nginx error
   log (emitted into the OWNED directory via a new http-context `error_log`
   directive) for `bind() … failed` within a bounded window (default 200 ms);
   detection triggers the §5.2 rollback and Programmed=False (Invalid).
   This retires the round-2 "reload signal blindness" limitation that
   motivated deleting the HTTPS base Gateway → **the HTTPS base Gateway is
   back**, restored by the relaxed ListenerHostnameMatching semantics plus
   per-Gateway binds (no wildcard→specific bind transitions ever occur).
6. **backendRef weights**: multi-backend rules render one rule-private
   weighted upstream (`hng_wr_*`) with per-server `weight=N`; weight-0 refs
   receive no servers; a rule whose refs exist but are ALL weight 0 matches
   no traffic (falls through, 404 — GEP-993); any UNRESOLVABLE backendRef
   keeps the GEP-1364 static-500 for the whole rule.
7. **Filters** (`dispatch.go`): RequestRedirect (`return <code> …` with
   `$scheme`/`$host` preservation, default-port elision, replaceFullPath,
   and replacePrefixMatch via guarded `if ($uri ~ …)` captures for
   301/302/303/307/308), URLRewrite (hostname → `proxy_set_header Host`,
   replaceFullPath/replacePrefixMatch → `rewrite … break` with capture),
   RequestHeaderModifier (set/add → `proxy_set_header`, remove → empty
   value; add uses a generated append-map so the inbound value is
   preserved: `"in,v"`), ResponseHeaderModifier (add → `add_header …
   always`, set → hide+add, remove → `proxy_hide_header`).
8. **method/header/query matching**: matches compile into chained nginx
   `map`s — one dispatch CASE per MATCH (OR across matches, AND within a
   match), tree levels ordered method → headers → query params, exact keys
   case-insensitive (matches header-Exact semantics), `~regex` keys in
   declaration order; leaf = upstream name, "" → the location's 404 guard.
   GEP-993 precedence: path specificity first (locations), then more
   constraints beat fewer, then rule/match order.
9. **Status.latency tuning**: min full-sync spacing 1s → 250 ms and the
   verify window bounded at 200 ms keep Gateway statuses inside the
   suite's compressed 5 s condition windows even when several tests create
   Gateways simultaneously.

Harness adaptations REMOVED (conformance_test.go): `annotateBaseGateways`,
`startAddressInjector`/`injectAddresses`, and `deleteHTTPSBaseGateway`.
`waitBaseGatewaysProgrammed` remains as the readiness gate and now
validates real controller behaviour (Programmed + non-empty
status.addresses).

Exemption/skip list changes (`e2e/run-conformance.sh`):
- `--exempt-features` reduced to: `HTTPRouteBackendRequestHeaderModification`,
  `HTTPRouteRequestMirror`, `HTTPRouteRequestMultipleMirrors`,
  `HTTPRouteRequestPercentageMirror` (still §9 non-goals).
- `--skip-tests` reduced to: `HTTPRouteCrossNamespace` (cross-namespace
  route attachment remains refused per §3.5).
- Newly passing this round: `HTTPRouteWeight`, `HTTPRouteHeaderMatching`,
  `HTTPRouteMatching`, `HTTPRouteQueryParamMatching` (via exemption removal),
  `HTTPRouteMethodMatching`, `HTTPRouteRedirectHostAndStatus`,
  `HTTPRouteRedirectPath`, `HTTPRouteRedirectPort`, `HTTPRouteRedirectPortAndScheme`,
  `HTTPRouteRedirectScheme`, `HTTPRouteRewritePath` (where offered by the
  suite), `HTTPRouteRequestHeaderModifier`,
  `HTTPRouteResponseHeaderModifier`, `HTTPRouteHTTPSListener`,
  `HTTPRouteReferenceGrant`,
  `HTTPRoutePartiallyInvalidViaInvalidReferenceGrant`,
  `GatewaySecretReferenceGrantAllInNamespace`,
  `GatewaySecretReferenceGrantSpecific`.

Diagnosis notes for future rounds (all resolved, kept for history):
- Long generated map variable names exceeded nginx's default
  `variables_hash_bucket_size` (64) → `nginx -t` emerg → whole config
  rejected. Map variables are now `hng_s<N>` counters.
- Per-rule constraint union broke OR-across-matches (GEP-993): dispatch
  cases are now per MATCH.
- Static-500 markers (GEP-1364) must keep head precedence inside a
  location; viable-case collection initially let covering rules shadow
  them.
- Zero-weight-only rules must NOT answer 500 (GEP-1364 covers absent
  backends only); they now fall through (404).
- Status write latency (sync gate + verify window) raced the compressed
  5 s condition polls when several tests created Gateways at once.

### Round-3 changes (features) and round-3 diagnosis (verified in round 4)

Implemented in round 3:

1. **Per-route hostname dispatch** (`internal/provider/translate.go`):
   each listener's locations are grouped by *effective hostname* (the
   per-attachment `spec.hostnames` ∩ listener-hostname intersection) and one
   nginx `server` block is emitted per group — `server_name` = the route's
   effective hostname (wildcards rendered verbatim; nginx matches them as
   suffix patterns). Routes without hostnames inherit the listener
   hostname; duplicate paths dedupe **within** a hostname group only.
   Attachment-time hostname intersection follows the v1 spec exactly
   (`internal/provider/graph.go` `hostnameIntersection`): wildcard = suffix
   match (`*.example.com` covers `foo.test.example.com`), an exact listener
   also matches the covering route wildcard (`test.example.com` ∩
   `*.example.com`), and the apex `example.com` is *not* covered by its own
   `*.example.com`. Unit-tested in `internal/provider/hostnamedispatch_test.go`.
2. **ReferenceGrant** (`internal/provider/graph.go`, `manager.go`, RBAC in
   `deploy/rbac.yaml` + chart): cross-namespace backendRefs (HTTPRoute →
   Service) and listener certificateRefs (Gateway → Secret) are permitted
   only by a ReferenceGrant **in the target namespace** whose matching
   `from` and `to` entries belong to the *same* grant (`to.name` constrains
   when set); permitted refs resolve normally, unpermitted keep the exact
   round-2 behavior (GEP-1364 static-500 + `RefNotPermitted`; listener
   `RefNotPermitted` + no server block). ReferenceGrant is watched, so a
   grant add/delete re-evaluates on the next full sync (spec: revocation on
   deletion). Also fixed on the way: the certificate Secret is now looked
   up in the *reference's* namespace (was always the Gateway's — only
   reachable once cross-ns is permitted), and `Certificates()` uses the
   Secret's own namespace for the deterministic pem filename. Unit-tested
   in `internal/provider/referencegrant_test.go`.

Round-3 failures — re-diagnosed from the run log: requests *did* dispatch
between routes, but the echo backend reported the upstream group name as
the request Host (`expected host to be first.com, got
gateway-conformance-infra_infra-backend-v2_3000`). nginx's `proxy_pass`
sends the upstream NAME as Host by default. Gateway API v1 (HTTPRoute
`spec.hostnames` doc) requires the Host header to be forwarded
**unmodified** to the backend. Fixed post-round-3 (spec-text-driven,
unit-tested, **verified live in round 4**): `proxy_set_header Host
$http_host;` in every proxying location (`internal/dataplane/template.go`),
plus wildcard suffix-match intersection semantics (multi-label subdomains
and route-wildcard-under-exact-listener now attach — the round-3 log showed
wildcard-hostname routes rejected with `NoMatchingListenerHostname` that
the spec accepts).

### Round-4 changes and round-4 failures (narrowed to one feature gap)

1. **Timeout compression** (`e2e/conformance/conformance_test.go` — suite
   config only, no test/skip changes): condition/status-poll timeouts
   (`HTTPRouteMustHaveCondition`, `GatewayMustHaveCondition`,
   `GWCMustBeAccepted`, `RouteMustHaveParents`,
   `GatewayStatusMustHaveListeners`, `GatewayListenersMustHaveConditions`,
   `LatestObservedGenerationSet`, xRoute variants) **180s/60s → 5s**;
   `MaxTimeToConsistency` **30s → 5s**; `GatewayMustHaveAddress`
   **180s → 10s** (the address shim's 3s controller-first-write race guard
   needs margin); `NamespacesMustBeReady` **300s → 30s** (includes image
   pulls). NOT compressed (IO/API bounds, not implementation convergence):
   Create/Delete/Get/ManifestFetch/RequestTimeout, DefaultTestTimeout,
   DefaultPollInterval, RequiredConsecutiveSuccesses.
2. **Intersection completion** (`internal/provider/graph.go`): a route
   wildcard `*.P` also intersects listeners whose hostname lies within P's
   coverage (exact subdomain listener, narrower wildcard listener) — the
   spec's "Listener test.example.com matches routes specifying
   *.example.com" generalized. Round-4 live evidence: `foo.bar.wildcard.io`
   (multi-label), `bar.anotherwildcard.io`, and port-stripped
   `very.specific.com:1234` all route correctly; the `AttachedRoutes`
   counting subtest passes (was failing in round 3).
3. Harness infra: image import+verify retry in `e2e/run-conformance.sh`
   (containerd metadata lag after fresh-content imports made a single
   120s poll flaky — three setup aborts preceded the fix).

The 3 remaining failures (`HTTPRouteHostnameIntersection` — only subtests
13/14/15 of 28 — `HTTPRouteListenerHostnameMatching`,
`HTTPRouteMatchingAcrossRoutes`) share ONE newly-isolated root cause:
**GEP-722 hostname-precedence fall-through**. Evidence (live generated
config + curl probes captured during the round-4 run): with routes
`very.specific.com`→v1(/s1) and `*.specific.com`→v3(/s3) both attached, a
request `very.specific.com/s3` must fall through from the exact-hostname
route (which has no /s3) to the broader-wildcard route; nginx selects the
server block purely by Host and cannot fall through by path (the exact
block 404s /s3). Related: unmatched hosts must not leak into the socket's
default-server block (first-listed `server { }` inherits them). Expressing
this needs a route-evaluation layer (merge broader-hostname routes'
locations into narrower-host blocks in precedence order, or a
`map`/njs-based dispatch) — the round-5 work item, now the largest single
feature left.

Everything else that ran passed (rounds 2 and 3 alike): all path-matching
traffic (`HTTPRouteExactPathMatching`, `HTTPRoutePathMatchOrder`,
`HTTPRouteSimpleSameNamespace`), GEP-1364 500-on-unresolvable-backend
(nonexistent / cross-ns / invalid-kind / zero-backendRefs), status semantics
(observedGeneration bumps, invalid route kinds, unsupported protocol,
invalid TLS configuration incl. malformed PEM, invalid parametersRef,
attached-routes counting, listener add/remove), and the negative
ReferenceGrant cases (`HTTPRouteInvalidReferenceGrant`,
`HTTPRouteInvalidCrossNamespaceBackendRef`,
`GatewaySecretInvalidReferenceGrant`,
`GatewaySecretMissingReferenceGrant` — refusing ungranted cross-namespace
refs remains conformant alongside positive-grant support).

## Round-1 → round-2 changes

### Product fixes (small spec-compliance gaps; no new features)

1. **Multi-port Service resolution** (`internal/provider/graph.go`): watch
   Services (RBAC + chart updated) and map `backendRef.port` → service port
   name → EndpointSlice port. Round 1 answered `UnsupportedValue`/500 for
   the suite's 3-port `infra-backend-v1`; this was the single biggest
   unlock (most traffic tests).
2. **allowedRoutes `from=Selector`** (`graph.go` + Namespace watch):
   round 1 rejected Selector listeners wholesale → `GatewayWithAttachedRoutes`
   failed with `AttachedRoutes=0`.
3. **§3.2 wildcard refinement** (`hostnamesOverlap`): a wildcard listener
   and a hostname'd listener on the same port no longer conflict (nginx
   dispatches to the more specific `server_name`; identical/overlapping
   hostnames still conflict on both sides — B2 preserved).
4. **`allowedRoutes.kinds` status** (`graph.go`, `translate.go`):
   `status.listeners[].supportedKinds` is the intersection with the
   supported set; any unsupported kind sets listener
   `ResolvedRefs=False (InvalidRouteKinds)` → `GatewayInvalidRouteKind`.
5. **GatewayClass `parametersRef` rejection**: any parametersRef → class
   `Accepted=False (InvalidParameters)`, its Gateways rejected and
   unconfigured.
6. **Gateway `spec.infrastructure.parametersRef` rejection** → Gateway
   `Accepted=False (InvalidParameters)` (`GatewayInvalidParametersRef`).
7. **CertificateRef group validation** (non-core group →
   `InvalidCertificateRef`) and **malformed-PEM detection** (garbage
   `tls.crt`/`tls.key` → `ResolvedRefs=False (InvalidCertificateRef)` while
   still emitting the cert so `nginx -t` fails loudly — keeps the §5.2
   rollback behavior of e2e scenario 6).
8. **Unresolvable certificateRefs** (missing Secret / cross-ns / wrong
   kind): no server block is generated and the listener reports
   `Programmed=False (Pending)` instead of a bogus `Programmed=True`
   (`GatewayWithAttachedRoutes` "unresolved refs" subtest).
9. **Zero/empty `backendRefs` rules** answer **500** (GEP-1364 static
   marker) instead of being dropped to a 404 (`HTTPRouteNoBackendRefs`).
10. **Cross-namespace parentRef reason**: allowedRoutes is evaluated first
    — `from=Same` refusals report `NotAllowedByListeners` (spec-precise);
    only when attachment would otherwise be permitted does §3.5's
    `RefNotPermitted` appear (`HTTPRouteInvalidCrossNamespaceParentRef`).
11. **Gateway Accepted with partially-invalid listeners**:
    `Accepted=True (ListenersNotValid)` when ≥1 listener is valid
    (`GatewayListenerUnsupportedProtocol`).

DESIGN.md was updated for all of the above (§3.2, §3.3 table, §3.5, §7, §8).

### Round-2 lessons (harness/environment)

- **Profile-core features cannot be exempted**: `NewConformanceTestSuite`
  unions the selected profile's `CoreFeatures` **after** subtracting
  `--exempt-features`, so exempting `ReferenceGrant` (core in GATEWAY-HTTP)
  was a silent no-op — round 1 "ran" 6 ReferenceGrant-positive tests that
  should have been skips. Fix: those tests moved to `--skip-tests`.
- **Wildcard→specific bind transitions freeze nginx**: the base HTTPS
  Gateway first bound wildcard 443; annotating it afterwards
  (`listen 127.0.0.11:443`) made every subsequent reload fail with
  `bind() ... Address already in use` — and because `nginx -s reload`
  exits 0 anyway (the documented "reload signal blindness" limitation) the
  controller reported `Programmed=True` while the master silently kept the
  last good config. **Every traffic test then 404'd** through the host's
  default :80 server. Harness fix: delete the HTTPS base Gateway (443 never
  enters the generated config). This failure mode is worth a product-level
  fix in round B/C (detect rejected reloads, e.g. re-check binds or tail the
  error log).
- **Suite client throttling**: client-go's default QPS/burst starved the
  parallel tests into `context deadline exceeded` fetch errors; the harness
  now raises QPS=20/Burst=60 on the suite's RestConfig.
- **Address-injector race**: injecting `status.addresses` into a
  brand-new Gateway could race the controller's first status write (stale
  resourceVersion from the cached client → lost conditions). The injector
  now waits until a Gateway is 3 s old.

## Harness adaptations

The suite flow carries NO shims: rounds 2–4 carried three documented
workarounds (BIND annotations, an address injector, HTTPS base Gateway
deletion); round 5 replaced each with a product feature (per-Gateway
loopback assignment, controller-written status.addresses, and the
listener-distinctness + reload-verification fixes). See "Round-5 changes"
above. `waitBaseGatewaysProgrammed` remains in `conformance_test.go`
purely as a readiness gate.

Round 9 added ONE environment step, outside the suite flow: the
**script-managed default-server fixture**
(`e2e/install-default-server-fixture.sh`, installed by the runner scripts
before the controller deploys and refreshed by `refreshDefaultServerFixture`
in `conformance_test.go` once the base Gateways are programmed). The
controller never injects default servers (DESIGN.md §3.3); the fixture IS
the host administrator's default server for this test machine — placed in
`/etc/nginx/conf.d/` (never in the controller-owned `k8s-gw/` directory)
and included from the master nginx.conf. It answers 404 for unmatched
Host/SNI on ports 80/443, both on the wildcard bind and across the
controller's whole loopback auto-assignment pool (127.0.0.8–127.0.0.239).
On 443 it presents the suite's own materialized certificate so the
`HTTPRouteHTTPSListener` unmatched-SNI case (`unknown-example.org` → 404)
passes client verification.

## Exempt features (documented deviations → DESIGN.md)

| Feature constant (`--exempt-features`) | Deviation | DESIGN.md |
|---|---|---|
| `HTTPRouteBackendRequestHeaderModification` | no backendRef-level HeaderModifier (rule-level is implemented) | §3.3 |
| `HTTPRouteRequestMirror` | no traffic mirroring | §9 |
| `HTTPRouteRequestMultipleMirrors` | no multi-target mirroring | §9 |
| `HTTPRouteRequestPercentageMirror` | no percentage mirroring | §9 |

(Round 5 removed eight exemptions: method/query matching, port/scheme/path
redirect, host/path rewrite, and response-header modification are all
implemented. `ReferenceGrant` is core in GATEWAY-HTTP and cannot be
exempted; it is supported since round 3.)

## Skipped tests (not feature-gated)

**None.** Round 6 removed the last entry (`HTTPRouteCrossNamespace` —
cross-namespace route attachment is implemented, see "Round-6 changes").
`--skip-tests` is passed empty, which is byte-identical to the suite's flag
default.

(The negative ReferenceGrant tests and all remaining GATEWAY-HTTP core
tests run and pass. `GatewayStaticAddresses` and friends skip as
"not supported" suite features — Gateway IP provisioning beyond the
controller-assigned loopbacks.)

## Remaining product gaps (post round 5 — GATEWAY-HTTP core is green)

1. ~~Per-route hostname dispatch~~ — done (rounds 3–5, including GEP-722
   precedence fall-through and listener-claim scoping).
2. ~~GEP-722 hostname-precedence fall-through~~ — done (round 5;
   `HTTPRouteHostnameIntersection` passes 28/28 subtests).
3. ~~Header/method/query matching~~ — done (round 5, map-chain dispatch).
4. ~~Backend weights~~ — done (round 5, rule-private weighted upstreams).
5. ~~RequestRedirect / URLRewrite / HeaderModifier filters~~ — done
   (round 5; backendRef-level HeaderModifier and RequestMirror remain
   exempted §9 non-goals).
6. ~~ReferenceGrant~~ — done (round 3; positive tests now measured and
   passing).
7. ~~Cross-namespace route attachment~~ — done (round 6;
   allowedRoutes-governed, `HTTPRouteCrossNamespace` passes).
8. **GATEWAY-HTTPS profile** — unblocked by round 5 (the HTTPS base
   Gateway programs cleanly; 443 binds coexist with the host nginx, which
   owns no 443 here). Candidate for the next round.
9. ~~Reload "signal blindness"~~ — mitigated (round 5, §5.2
   reload-effect verification with rollback).

## Files

- `e2e/conformance/go.mod`, `go.sum` — pinned harness module (suite v1.6.2)
- `e2e/conformance/conformance_test.go` — suite runner; shims removed in
  round 5, `waitBaseGatewaysProgrammed` kept as the readiness gate
- `e2e/conformance/report.yaml` — suite-generated round-6 report
- `e2e/conformance/summarize.sh` — log/report summarizer
- `e2e/run-conformance.sh` — self-contained entry point (`--skip-tests`
  empty since round 6)
- Product (round 6): `internal/provider/graph.go` (allowedRoutes-governed
  cross-namespace attachment; spec-default from=Same), `internal/provider/
  crossns_test.go` (spec-derived unit tests), updated round-2 tests in
  `internal/provider/graph_test.go`; DESIGN.md §3.5.
- Product (round 5): `internal/contract/contract.go` (dispatch/redirect/
  header/map types), `internal/provider/{graph,translate,dispatch}.go`,
  `internal/status/writer.go` (status.addresses), `internal/dataplane/
  {template.go,nginx.tmpl,publisher.go}` (render + reload verification),
  `cmd/host-nginx-gateway/{main.go,addresses.go}` (--publish-addresses,
  --nginx-error-log), `internal/provider/*_test.go` (GEP-722, filters,
  matches, addresses, weights), `charts/.../daemonset.yaml` (HNG_NODE_IP),
  `DESIGN.md`
