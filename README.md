# Go Load Balancer — xDS-style control plane + L7 data plane

A small, hybrid-cloud-aware load balancer split into a control plane and
data plane, the same architectural pattern real service meshes (Envoy's
xDS, Istio) use:

- **Control plane** (`cmd/controlplane`) watches a backend pool source and
  streams backend list updates to connected data planes over gRPC. It never
  touches traffic itself.
- **Data plane** (`cmd/dataplane`) is a "dumb" L7 HTTP reverse proxy. Every
  instance has a default backend group (`-group`), but can route different
  requests (by host, path prefix, and/or method) to other groups per the
  control plane's L7 route table — it has no knowledge of where backends
  come from either way, just proxying to whatever backend list the control
  plane most recently pushed for the resolved group, using whichever
  load-balancing algorithm (weighted round robin, least connections, or
  random) the control plane has selected for that group.

This split means the control plane's backend-discovery logic (today: a fake
provider; eventually: Azure VMSS, vCenter, etc.) is completely decoupled
from the proxying logic. Swapping in a real cloud provider means
implementing one small interface (`pool.Provider`) — nothing else changes.

## Why this design

- **Push, not poll.** Data planes subscribe once via a gRPC stream and
  receive updates as they happen. No data plane ever polls the control
  plane, and the control plane doesn't need to know how many data planes
  exist to fan updates out to them.
- **Data plane failure is independent of control plane failure.** If the
  control plane goes down, existing data planes keep proxying with the last
  backend list they received — they just stop getting updates until the
  control plane comes back, at which point they reconnect automatically
  with exponential backoff.
- **Versioned updates.** Every backend set has a monotonically increasing
  version. Data planes ignore any update that isn't newer than what they
  already have, so a reconnect that briefly replays state can't move a data
  plane backwards.

## Running locally

Requires Go 1.26+.

### 1. Start some fake backends

```bash
go run ./scripts/fake_backend.go -port 8081
go run ./scripts/fake_backend.go -port 8082
go run ./scripts/fake_backend.go -port 8083
```

Each just responds with "hello from backend on port <port>" — enough to
visibly confirm requests are being distributed across them.

### 2. Start the control plane

```bash
go run ./cmd/controlplane -simulate-scaling=false
```

By default it manages two fake groups: `web-tier` (3 backends) and
`api-tier` (2 backends), matching the ports the fake backends above are
listening on. Pass `-simulate-scaling=true` (the default when the flag is
omitted) to have it randomly add/remove backends on a timer, simulating
real scale-out/scale-in events.

### 3. Start a data plane instance

```bash
go run ./cmd/dataplane -listen-addr :8080 -group web-tier
```

### 4. Send traffic

```bash
curl http://localhost:8080/
curl http://localhost:8080/debug/backends   # current backend count + version
curl http://localhost:8080/healthz
```

Repeated requests will round-robin across the backends the control plane
currently reports for `web-tier`.

## Project layout

```
cmd/controlplane/   entrypoint for the control plane binary
cmd/dataplane/       entrypoint for the data plane (proxy) binary
internal/pool/       backend pool Provider interface + fake/testing impl
internal/controlplane/  gRPC server: reconciliation loop + subscriber fan-out
internal/dataplane/  gRPC client, weighted backend list, HTTP reverse proxy
proto/               gRPC service definition + generated Go stubs
scripts/             fake_backend.go — manual-testing helper, not part of the main build
```

## Admin web management UI

The control plane serves a small password-protected web dashboard (default
`:9091`) showing live backend group state — which groups exist, their
current version, backends, per-backend health status, and how many data
plane instances are subscribed. The dashboard auto-refreshes every 5
seconds. All admin state (password hash, session secret, audit log) lives
in local JSON files inside the container (default
`/var/lib/go-loadbalancer/`) — no external database.

**Per-backend health status.** The control plane has no direct visibility
into whether a backend is actually healthy — that's only known locally by
whichever data plane instance is proxying to it. Each data plane
periodically reports its own health checker's view back to the control
plane over gRPC (`LB_HEALTH_REPORT_INTERVAL`, default 10s), which the
dashboard then displays per backend as **Healthy**, **Unhealthy**, or
**Unknown** (no data plane has reported on that address recently — e.g. no
data plane is currently subscribed to that group). If multiple data planes
report on the same backend and disagree, it's shown as unhealthy — a
single instance seeing a backend as down is a real signal worth surfacing,
even if others can still reach it. Reports older than 30 seconds are
treated as stale and revert to Unknown, so a dead/disconnected data plane
doesn't leave misleadingly "healthy" state on the dashboard forever.

**Fleet.** Available at **Fleet** in the nav bar — every data plane
instance that has opened a `StreamBackends` gRPC connection to this
control plane, whether currently streaming or disconnected. Shows the
instance ID, which group it serves, connection state, how long it's been
connected (or how long since it disconnected), and its most recent
`ReportHealth` call (time and backend count). An instance's entry is kept
even after it disconnects, so "this one dropped 20 minutes ago and hasn't
come back" stays visible rather than silently vanishing — it's only
cleared by a control plane restart.

**Audit log.** Available at **Audit Log** in the nav bar — records login
successes/failures, rate-limit lockouts, password changes, password resets
(via `-admin-force-reset-password`), logouts, pool overrides, and algorithm
changes, each with a timestamp and client IP. Kept as a bounded (last 500
events) local JSON file — this is meant for "did someone lock themselves
out this morning" debugging, not a permanent compliance record.

**Pool control (weight overrides and draining).** Each backend row on the
dashboard has a weight field and a Drain/Undrain button. Draining a backend
excludes it from what's sent to data planes immediately (no waiting for
the next reconcile tick), without needing the underlying pool provider to
report it as gone — useful for taking one instance out of rotation for
maintenance. A drained backend stays visible on the dashboard (marked
"drained") so it can be un-drained later. Setting a weight override
replaces whatever weight the pool provider reports for that backend until
it's cleared. Both are stored in a local JSON file
(`-admin-overrides-path`, default
`/var/lib/go-loadbalancer/overrides.json`) keyed by group and address, so
they survive a control plane restart as long as that path is on a
persistent volume.

**Load-balancing algorithm selection.** Each group on the dashboard has an
algorithm dropdown — **round_robin** (weighted, the default),
**least_connections** (weighted by outstanding in-flight requests per
backend), or **random** (weighted). Changing it takes effect immediately
across every data plane subscribed to that group. The selection is stored
in a local JSON file (`-admin-algorithms-path`, default
`/var/lib/go-loadbalancer/algorithms.json`), per group, and defaults to
round_robin for any group with no explicit selection.

**First run:** a random password is generated and printed exactly once to
the process log, in a clearly marked block:

```bash
docker compose up
docker compose logs controlplane | grep -A6 "initial password"
```

or, running directly:

```bash
go run ./cmd/controlplane
```

```
=========================================================
 Go Load Balancer — admin web UI initial password

   Password: C9mtwMTp0xHrhplLstAIdSpapNZQGLMX

 This password is shown ONLY ONCE and is not stored in
 plaintext anywhere. Save it now. You can change it from
 the web UI after signing in, or recover a lost password
 by restarting with -admin-force-reset-password.
=========================================================
```

Sign in at `http://localhost:9091` with that password, then change it from
**Change Password** in the nav bar — this rotates the session secret,
signing out any other active sessions.

**Lost your password?** Restart the control plane once with
`-admin-force-reset-password` (or `LB_ADMIN_FORCE_RESET_PASSWORD=true`) to
generate and print a fresh one, then unset the flag again.

**Persistence:** if you don't mount a volume over the store path (the
Docker Compose setup in this repo does, via `controlplane-admin-data`), a
new random password is generated every time the container restarts — the
store file only exists inside that container's writable layer otherwise.

**Security notes:**
- Sessions are signed cookies (HMAC, `HttpOnly`, `SameSite=Strict`) with no
  server-side session store — changing the password or restarting with a
  fresh store invalidates all of them at once.
- Login attempts are rate-limited per IP (5 failures locks out for 15
  minutes) to slow down password brute-forcing.
- The admin UI serves plain HTTP by default, same as the gRPC/data-plane
  listeners — set `-admin-tls-cert`/`-admin-tls-key` for anything beyond
  local development. If it's placed behind a reverse proxy that terminates
  TLS and sets `X-Forwarded-For`, pass `-admin-trust-forwarded-for` so rate
  limiting keys on the real client IP rather than the proxy's.
- Disable it entirely with `-admin-disable` if you don't want the web UI
  running at all.

## L7 routing

By default, a data plane instance is pinned to a single backend group
(`-group`) for its entire lifetime — every request it proxies goes to that
one group. **Routes**, in the admin web UI, lets a global L7 route table
send different requests to different groups from that same instance and
listener, without running a separate data plane per group.

Each rule matches on:
- **Host** — exact match; blank or `*` matches any host.
- **Path prefix** — a literal prefix (e.g. `/api/`); blank or `/` matches
  every path. Not a pattern language — deliberately simple for a first cut.
- **Methods** — a comma-separated list (e.g. `GET, POST`); blank matches
  any method.

and, if matched, sends the request to the rule's **target group** instead
of the instance's default. Rules are evaluated top to bottom; the first
match wins, and the routes editor's row order is that evaluation order.
An empty table — or a request matching no rule — falls back to the
instance's own `-group`, so upgrading to a version with L7 routing changes
nothing for a deployment that never configures a route.

The route table is global (not per-group) and pushed to every connected
data plane instance the moment it's saved, over its own gRPC stream
(`StreamRoutes`, alongside each group's `StreamBackends` stream). A data
plane instance discovers additional groups referenced by a route lazily —
the first request that actually resolves to a group it isn't already
subscribed to triggers that group's subscription, health checking, and
health reporting to start, exactly as if `-group` had named it from the
start. Stored in a local JSON file (`-admin-routes-path`, default
`/var/lib/go-loadbalancer/routes.json`), same pattern as overrides and
algorithm selection.

**Current limitation:** the admin UI's Fleet view shows one group per
instance ID, taken from its most recently opened `StreamBackends` stream —
an instance now proxying to multiple groups via routing will show
whichever group it subscribed to last, not the full list. Fixing this
would mean tracking group per-stream rather than per-instance in the
Fleet view; noted as a follow-up rather than blocking this feature, since
it's a display-only gap and doesn't affect actual routing/proxying
behavior.

## Sticky sessions

By default, every load-balancing algorithm (round robin, least
connections, random) picks a backend independently per request — nothing
ties a client to the same backend across multiple requests. **Sticky
sessions**, configurable per group from the dashboard, changes that: once
enabled for a group, the data plane pins each client to whichever backend
it was first sent to, via a cookie, and keeps sending that client's
subsequent requests to the same backend as long as it stays healthy.

Per-group settings:
- **Enabled** — on/off. Disabled by default, and disabling it takes
  effect immediately for new requests (existing affinity cookies simply
  stop being honoured).
- **Cookie name** — defaults to `jb_affinity` if left blank.
- **TTL** — how long the affinity cookie lives, refreshed on every
  request that uses it. Defaults to 30 minutes if left blank or zero.

**Failover behavior:** if a client's pinned backend becomes unhealthy or
is removed from the group entirely, the very next request from that
client falls back to the group's normal load-balancing algorithm and the
client is re-pinned to whatever backend that selects — a client is never
stuck failing because its original backend went away.

**How pinning works:** the affinity cookie's value is the backend's own
address. This is deliberately simple, with a real (if minor) tradeoff:
the client can see the internal address of the backend it's pinned to.
Forging or tampering with the cookie value can't route a client to an
address outside the group, though — every incoming cookie value is
checked against the group's actual current healthy backend list before
being honoured, and anything else falls back to normal selection.

Like the load-balancing algorithm, sticky-session configuration is
pushed to every data plane instance as part of its `BackendSet` update —
no separate round trip, no data plane restart required. Stored in a
local JSON file (`-admin-sticky-path`, default
`/var/lib/go-loadbalancer/sticky.json`), same pattern as the other
per-group settings.

**Interaction with L7 routing:** sticky sessions are configured per
*backend group*, not per route. If a route table sends different
requests from the same client to different groups, each group's affinity
(if enabled) is tracked independently via its own cookie.

## Azure VMSS provider

A real backend pool provider is included: `pool.AzureVMSSProvider` reports
the running instances of one or more Azure VM Scale Sets as backends,
using their private IP addresses.

```bash
export LB_PROVIDER=azure-vmss
export LB_AZURE_SUBSCRIPTION_ID=<your-subscription-id>
export LB_AZURE_RESOURCE_GROUP=<resource-group-containing-the-scale-sets>
export LB_AZURE_VMSS_GROUPS=web-tier:vmss-web:8080,api-tier:vmss-api:8081

go run ./cmd/controlplane
```

`LB_AZURE_VMSS_GROUPS` maps each control-plane group name to the scale set
that backs it and the port the application on each instance listens on:
`group:scaleSetName:port[:weight]`, comma-separated for multiple groups.

**How it works:** on each reconcile tick, the provider makes two Azure API
calls per group — one to list the scale set's VM instances with their
instance view expanded (to check `ProvisioningState` and
`PowerState/running`), and one to list the scale set's network interfaces
(to get each running instance's private IP). This is a fixed two-call cost
regardless of instance count, so it scales to large scale sets without an
N+1 call pattern. Only instances that are both successfully provisioned
and currently running are reported as backends.

**Authentication** uses `azidentity.DefaultAzureCredential`, which tries
(in order) environment variables for a service principal
(`AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, `AZURE_TENANT_ID`), workload
identity, managed identity, and Azure CLI credentials. None of these are
specific to this tool — see [Azure SDK for Go authentication
docs](https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication)
for how to configure whichever applies to your environment. When running
as an Azure resource (a VM, AKS pod, Container App, etc.), a managed
identity with **Reader** access on the resource group is the simplest and
most secure option — no secrets to manage at all.

## Adding a different backend pool provider

To back the control plane with something other than the fake provider or
Azure VMSS (vCenter, a different cloud, a service registry, etc.),
implement `pool.Provider`:

```go
type Provider interface {
	Groups(ctx context.Context) ([]string, error)
	Snapshot(ctx context.Context, group string) (Snapshot, error)
}
```

`Groups` should return the pool/group names you want the control plane to
manage. `Snapshot` should return the current set of running instance
IP:port pairs for that group. Wire it in alongside the `fake`/`azure-vmss`
cases in `buildProvider` (`cmd/controlplane/main.go`) — nothing else in the
control plane or data plane needs to change.

## Testing

```bash
go test ./... -race
```

Covers weighted round-robin selection, stale-version rejection, the control
plane's change-detection (only publishes when a backend set actually
differs), and the fake provider's scaling bounds.

## Running via Docker / Docker Compose

```bash
cp .env.example .env
# edit .env as needed — see comments in the file for every available setting
docker compose up --build
```

This starts both the control plane and a single data plane instance, wired
together via Compose's internal DNS (`controlplane:9090`). All settings can
be provided via `.env` — see `.env.example` for the full list, or run
either binary with `-h` to see the equivalent CLI flags (flags always
override the environment).

**Important:** the built-in fake backend provider reports addresses like
`127.0.0.1:8081`, which only resolve correctly when the control plane and
data plane share a network namespace (e.g. both running directly on your
host, as in the "Running locally" section above). Inside separate Docker
containers, `127.0.0.1` refers to *that container*, not the host or other
containers — so the fake provider's backends will correctly show up as
unreachable/unhealthy under Compose. This is expected: the fake provider
exists to exercise the control-plane/data-plane wiring, not to simulate a
real deployment. For a real Docker Compose deployment, wire in a
`pool.Provider` that reports actually-routable addresses (a real cloud
provider, or other containers on the same Compose network by service
name).

## Known limitations (by design, for now)

- No sticky sessions — every algorithm (round robin, least connections,
  random) is stateless per-request; there's no session affinity to a
  specific backend.
- No connection draining delay when a backend is removed by the pool
  provider itself (as opposed to a manual drain via the admin UI, which
  does take effect immediately without waiting on in-flight requests to
  finish) — in-flight requests to a backend the provider stops reporting
  will fail once it's gone rather than being allowed to complete first.
