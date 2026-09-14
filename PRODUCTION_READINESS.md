# jbalance — Production-Readiness Gap Analysis

**Date:** 2026-09-12
**Reviewed against:** `origin/main` @ `aa09dcace037` (after PRs #1–#10 merged)
**Scope:** Read-only survey. No functional code changes made in this step.

This document inventories what is already in place versus what is missing to
consider jbalance production-ready, across observability, lifecycle, config
safety, admin security, resource limits, resilience, container/K8s
readiness, test coverage, and CI hardening.

> **Important context for the plan that follows this survey.** Several items
> the original feature plan proposed to *add from scratch already exist* in
> the current codebase. The plan was drafted before this survey. The findings
> below correct that: where a capability already exists, the remaining work is
> a smaller *hardening/completion* task, not a greenfield feature. Each
> section flags this explicitly.

---

## Summary scorecard

| Area | Status | Remaining work |
|---|---|---|
| Prometheus `/metrics` + instrumentation | ✅ **Already exists** | Add retry counter + per-backend ejection metric (minor) |
| Structured logging (slog, levels, JSON) | ❌ **Missing** | Full item — currently ad-hoc `log.Printf` everywhere |
| Liveness / readiness probes | ⚠️ **Partial** | `/healthz` exists (L7 only); no `/readyz`, no probe on TCP/control-plane |
| Graceful shutdown / signal handling | ✅ **Mostly done** | Ordering + "mark-not-ready-then-drain" refinement |
| Startup config validation / fail-fast | ⚠️ **Partial** | Some checks exist; envflag swallows parse errors; no cross-field validation |
| Admin API auth | ⚠️ **Web UI only** | Web UI authenticated; **gRPC control API has NO app-layer auth** |
| Resource limits (conns/body/header) | ❌ **Missing** | No max-conns, no `MaxBytesReader`, no `MaxHeaderBytes`, no read/write/idle timeouts |
| Panic-recovery middleware | ❌ **Missing** | No `recover()` anywhere; a handler panic serves nothing / kills the conn |
| Container non-root user | ❌ **Missing** | Both Dockerfiles run as root |
| Container/compose healthcheck | ❌ **Missing** | No `HEALTHCHECK`; no compose healthchecks |
| Test coverage | ⚠️ **Good but gaps** | `cmd/dataplane` main wiring untested; no panic/limit tests (features don't exist yet) |
| CI hardening (govulncheck, pinning) | ⚠️ **Partial** | Deps pinned + `-race`; **no govulncheck**; no SBOM/image scan |

Legend: ✅ done · ⚠️ partial · ❌ missing

---

## 1. Observability

### 1a. Prometheus metrics — ✅ ALREADY EXISTS
`internal/dataplane/metrics.go` + `cmd/dataplane/main.go` already stand up a
dedicated Prometheus registry served on its own listener (`-metrics-addr`,
default `:9100`, env `LB_METRICS_ADDR`, disable with `LB_METRICS_DISABLE`) via
`promhttp`. It is deliberately on a separate port from the traffic listener so
scrapes never compete with proxied traffic, and it works even in L4/TCP mode.

Metrics currently exported:
- `jbalance_http_requests_total{group,status}` — counter (status is the `Nxx` class)
- `jbalance_http_request_duration_seconds{group}` — histogram (default buckets)
- `jbalance_active_connections{group}` — gauge (in-flight L7 requests)
- `jbalance_tcp_bytes_total{group,direction}` — counter
- `jbalance_tcp_connections_total{group}` — counter
- `jbalance_tcp_active_connections{group}` — gauge
- `jbalance_backends_healthy{group}` / `jbalance_backends_total{group}` — gauges via a live pull-on-scrape `backendsCollector`

**Gaps (small):**
- ❌ **No retry counter.** The proxy retries connection-level failures
  (`serveWithRetries`) but does not increment a metric — a retry storm is
  invisible to Prometheus.
- ❌ **No per-backend outlier/ejection metric.** Passive outlier detection
  ejects backends but there is no `jbalance_backends_ejected` gauge or ejection
  counter; only healthy/total are exposed.
- ❌ **No `go_*`/`process_*` collectors.** A custom `prometheus.NewRegistry()`
  is used without registering `collectors.NewGoCollector()` /
  `NewProcessCollector()`, so GC/goroutine/FD/memory metrics are absent.
- ⚠️ The control plane exposes **no** `/metrics` of its own (reconcile
  latency, provider errors, connected data-plane count). It only *receives*
  pushed summaries from data planes for the admin UI charts.

### 1b. Structured logging — ❌ MISSING (full item)
Every log call in both binaries and all `internal/*` packages uses the stdlib
`log` package (`log.Printf`/`log.Println`/`log.Fatalf`). There is:
- ❌ No `log/slog` structured logger.
- ❌ No log levels (`LB_LOG_LEVEL`) — everything is unconditional.
- ❌ No JSON/text switch (`LB_LOG_FORMAT`) — output is not machine-parseable.
- ❌ No request-id correlation in the general logs (the access log *does* emit
  a request id, but ad-hoc error logs like `proxy error forwarding to %s` do
  not carry it).

This is a genuine from-scratch item.

---

## 2. Liveness / readiness probes — ⚠️ PARTIAL

- ✅ The **L7 data plane** serves `GET /healthz` returning `200 ok`, and a
  `GET /debug/backends` diagnostics page (backend/healthy counts, version,
  routes, tracked groups) on the traffic listener.
- ❌ **No `/readyz`.** `/healthz` is a static "process is up" liveness check —
  it returns 200 even when zero backends are healthy. There is no readiness
  endpoint reflecting whether the data plane can actually serve traffic (≥1
  healthy backend). The building block exists: `BackendList` exposes
  `HealthyLen()` and `Len()`, so readiness is derivable but not wired.
- ❌ **L4/TCP mode has no HTTP endpoints at all** — no liveness, no readiness.
  A TCP-mode instance is unprobeable over HTTP today.
- ❌ **Control plane has no probe endpoints** (no admin/ops liveness/readiness;
  the admin UI listener is a full login-gated app, not a probe surface).
- ⚠️ Probes live on the **traffic listener**, not a dedicated ops/admin port.
  Best practice (and what later steps propose) is a separate ops listener so
  `/healthz`/`/readyz`/`/metrics` are not exposed on the public data path.

---

## 3. Graceful shutdown & signal handling — ✅ MOSTLY DONE

- ✅ Both binaries use `signal.NotifyContext(ctx, SIGINT, SIGTERM)`.
- ✅ Data plane L7: `server.Shutdown(ctx)` with `LB_SHUTDOWN_GRACE` budget.
- ✅ Data plane L4: closes the listener (stops new Accepts) then
  `proxy.Drain(grace)` waits for in-flight connections using a mutex-guarded
  inflight counter (the WaitGroup Add/Wait race was previously fixed).
- ✅ Control plane: `grpcServer.GracefulStop()`; admin UI `Shutdown` with a
  fresh-context timeout.
- ✅ SIGHUP triggers TLS cert hot-reload (keep-last-good on failure).

**Gaps (refinement, not greenfield):**
- ⚠️ **No "mark not-ready, then drain" ordering.** On SIGTERM the server should
  flip readiness to false *first* (so a K8s load balancer stops sending new
  requests) and *then* drain. Today shutdown and readiness are unrelated
  because `/readyz` doesn't exist.
- ⚠️ The metrics/admin listeners use a hardcoded 5s shutdown timeout rather
  than the configurable `LB_SHUTDOWN_GRACE`.

---

## 4. Startup config validation & fail-fast — ⚠️ PARTIAL

**What already validates & fails fast (`log.Fatalf`) at startup:**
- ✅ `-protocol` must be `http`|`tcp`; `-health-check-mode` must be `tcp`|`http`;
  `-backend-protocol` must be `http1`|`h2c`.
- ✅ TLS cert/key load errors are fatal; CA parse errors are fatal.
- ✅ Provider specs are parsed and rejected (azure-vmss and k8s require
  subscription/RG/groups; unknown provider is fatal).

**Gaps:**
- ❌ **`envflag` silently swallows parse errors.** `Bool`/`Int`/`Duration` all
  fall back to the *default* when the env var is malformed
  (`LB_PROXY_MAX_RETRIES=abc` → silently uses 1; `LB_HEALTH_CHECK_INTERVAL=5`
  with no unit → silently uses 5s default). A typo in an env var produces a
  silently-wrong config rather than a fail-fast error.
- ❌ **No numeric range validation.** Negative/zero timeouts, negative
  thresholds, `outlier-max-eject-percent` outside 0–100, `expect-status`
  outside 100–599 are not checked (some are clamped inside `ProxyConfig`, but
  silently, not reported).
- ❌ **No listen-address sanity check** beyond `net.Listen` failing later.
- ❌ **No cross-field / mutual-exclusion validation** (e.g. `http-tls-client-ca`
  set without any server cert; `health-check-scheme=https` with no way to trust
  the probe cert; TLS client-cert set without client-key is only caught by the
  loader, not surfaced as a config error).
- ❌ No single "validate all config, print every problem, exit 1" pass — errors
  are discovered lazily, one `log.Fatalf` at a time.

---

## 5. Admin API authentication & TLS — ⚠️ WEB UI ONLY (control API is a real gap)

### 5a. Admin web UI — ✅ AUTHENTICATED
- ✅ Password stored as a hash in a local store (`admin.Open`), random password
  generated on first run and printed once; force-reset via
  `-admin-force-reset-password`.
- ✅ Signed HMAC-SHA256 session cookie (`lb_admin_session`), `HttpOnly`,
  `SameSite=Strict`, `Secure` when TLS is on, 12h expiry; secret rotation
  invalidates sessions server-side.
- ✅ Per-IP login rate limiting / lockout (`loginLimiter`).
- ✅ Optional TLS on the admin listener (`-admin-tls-cert/-key`).
- ✅ Audit log of admin mutations.

### 5b. Control-plane gRPC API — ❌ NO APPLICATION-LAYER AUTH (real gap)
This is the most significant security gap. The control plane's gRPC server
(`internal/controlplane/server.go`) exposes the streaming subscription RPCs
(backend list, routes, health/metrics reporting) and mutation methods
(`SetOverride`, `ClearOverride`, and the config the admin UI drives). There is:
- ❌ **No `UnaryInterceptor`/`StreamInterceptor`** performing token or identity
  auth. Nothing checks *who* is calling.
- ⚠️ The only protection is **optional transport mTLS** (`-tls-client-ca`) — and
  only if the operator configures it. With TLS off (the default, with a loud
  WARNING) **anything that can reach `:9090` can subscribe to config and push
  overrides.**
- ❌ **No `LB_ADMIN_TOKEN`** / bearer-token mechanism exists anywhere in the
  codebase (confirmed by grep — zero matches).

**Remaining work:** add bearer-token auth (`LB_ADMIN_TOKEN`) and/or mandate
mTLS on the management surface, enforced via a gRPC interceptor, so reaching
the port is not equivalent to controlling the fleet.

---

## 6. Resource limits — ❌ MISSING (full item)

Confirmed by grep (zero matches for `MaxBytesReader`, `MaxHeaderBytes`,
`LimitListener`, `netutil`, `Semaphore`):
- ❌ **No max-concurrent-connections limit.** Neither the L7 `http.Server` nor
  the L4 accept loop bounds concurrency — a connection flood is unbounded.
- ❌ **No max request body size** (`http.MaxBytesReader`) — a large upload is
  proxied without limit.
- ❌ **No `Server.MaxHeaderBytes`** — header size uses the stdlib 1MB default,
  not an explicit, tunable value.
- ⚠️ **Timeouts: only `ReadHeaderTimeout` (10s) is set.** `ReadTimeout`,
  `WriteTimeout`, and `IdleTimeout` are **not** set on any `http.Server`
  (traffic, metrics, or admin). Slow-loris on the body and idle-connection
  exhaustion are not bounded at the server layer. (Per-*backend* timeouts via
  `ProxyConfig` do exist; per-*client* server timeouts do not.)

---

## 7. Panic-recovery middleware — ❌ MISSING (full item)

Confirmed by grep: **zero `recover()` calls** in the entire codebase.
- ❌ The L7 handler chain (`AccessLogMiddleware` → `proxy.Handler`) has no
  panic-recovery wrapper. A panic in a handler goroutine is caught only by
  `net/http`'s built-in per-request recover (which closes the connection and
  logs a bare stack) — there is no structured 500 response, no request-id
  correlation, and no metric.
- ❌ The admin UI handlers have no recovery middleware either.
- ❌ No panic counter metric.

---

## 8. Container / Kubernetes readiness — ❌ MOSTLY MISSING

Both `cmd/*/Dockerfile` are multi-stage (`golang:1.26-alpine` → `alpine:3.20`)
with `CGO_ENABLED=0` and `ca-certificates` — that part is good. But:
- ❌ **Runs as root.** No `USER` directive in either Dockerfile (grep: zero
  `USER ` matches). The container process is UID 0.
- ❌ **No `HEALTHCHECK`** in either Dockerfile.
- ❌ **Base image is full Alpine**, not `scratch`/`distroless` — larger attack
  surface than a static Go binary needs. (ca-certificates is the only real
  runtime dependency and can be `COPY`d into scratch.)
- ❌ **docker-compose has no healthchecks** on either service (only
  `restart: unless-stopped` and a `depends_on` with no condition).
- ❌ No read-only root filesystem / dropped-capabilities guidance; the admin
  store needs `/var/lib/go-loadbalancer` writable, which a non-root user must
  own.

---

## 9. Test coverage — ⚠️ GOOD, WITH GAPS

Existing coverage is strong: nearly every `internal/*` file has a matching
`_test.go` (proxy, tcpproxy, backends, healthcheck, healthreporter, groups,
router, ratelimit, outlier, canary, accesslog, metrics, tlsutil/certreloader,
pool/{fake,azurevmss,kubernetes}, all controlplane stores, admin
handlers/session/store/audit/ratelimit). CI runs `go test ./... -race
-shuffle=on` with atomic coverage.

**Gaps:**
- ❌ **`cmd/dataplane` has no test** (`cmd/dataplane/main.go` is 22KB of wiring:
  metrics server startup, TLS reloader assembly, shutdown goroutines, TCP
  branch — untested). `cmd/controlplane` *does* have `main_test.go`.
- ⚠️ **No tests for the features that don't exist yet** — panic recovery,
  resource limits, `/readyz`, config-validation failures, gRPC auth. These
  become required test targets as each feature lands.
- `proto/`, `scripts/` have no tests (generated/utility code — acceptable).

---

## 10. CI hardening — ⚠️ PARTIAL

Existing CI (`.github/workflows/ci.yml`) is solid: build, vet, `test -race
-shuffle=on` with coverage upload, `golangci-lint` (v2 config incl. gosec)
pinned to `v2.13.1`, `gofmt -l` gate, and Docker builds for both images.
Concurrency cancellation is configured.

**Gaps:**
- ❌ **No `govulncheck`** step (grep: zero matches). Known-vulnerable
  dependencies would not be caught.
- ⚠️ **Dependency pinning:** `go.mod` pins exact versions (good) and `go.sum`
  is committed, but there is no `go mod verify` step and no Dependabot/renovate
  config to keep pins current.
- ❌ No container image vulnerability scan (Trivy/Grype) and no SBOM generation
  in the Docker-build job.
- ⚠️ `golangci-lint` is pinned to a patch release (good), but the workflow does
  not run `go mod tidy` verification to catch drift.

---

## Recommended ordered work (feeds step 2)

Ordered by production risk, adjusted for what already exists:

1. **Admin/control-plane API auth** (§5b) — highest security risk; the gRPC
   management surface is currently open by default.
2. **Resource limits** (§6) — max conns, body size, header size, server
   timeouts. Directly bounds DoS blast radius.
3. **Panic-recovery middleware** (§7) — availability; a single panicking
   request path shouldn't degrade the instance.
4. **Structured logging** (§1b) — the only fully-greenfield observability item.
5. **Liveness/readiness completion** (§2) — add `/readyz`, a dedicated ops
   listener, and wire readiness into shutdown ordering (§3).
6. **Startup config validation / fail-fast** (§4) — make `envflag` strict and
   add a single validation pass.
7. **Container/K8s hardening** (§8) — non-root user, `HEALTHCHECK`,
   distroless/scratch, compose healthchecks.
8. **CI hardening** (§10) — `govulncheck`, image scan, `go mod verify`.
9. **Metrics completion** (§1a) — retry counter, ejection gauge, Go/process
   collectors, control-plane `/metrics`.
10. **Test coverage** (§9) — `cmd/dataplane` wiring + tests accompanying each
    feature above.

> Note vs. the pre-survey plan: proposed steps for "add a Prometheus
> `/metrics` endpoint" and "add liveness/`/healthz`" are **largely already
> done** — those steps should be re-scoped to the *completion* gaps in §1a and
> §2 rather than built from scratch.
