# Kubernetes manifests

Plain manifests (no Helm) for the two runtime services, per plan Phase A
and the [HTTP API contract topology](../../specs/001-epiloyes-alpha/contracts/http-api.md):
the Astro **web** server is the only public HTTP surface; the Go **api**
stays cluster-internal.

## Topology

| Component | Exposure | Why |
|---|---|---|
| `api` (Go binary) | `ClusterIP` Service only — **no Ingress, no NodePort/LoadBalancer** | Contract topology: the API is not publicly routable in any deployment |
| `web` (Astro, Node adapter) | `ClusterIP` Service + the single `Ingress` | The only public surface, sitting behind the single crawler gate |
| `blnk` / `blnk-worker` / `redis` (`cashback/`, opt-in) | `ClusterIP` Services only, and the worker has none at all | The ledger (ADR-0002). An internet-reachable ledger API is an internet-reachable way to move members' money |

Replica load-balancing is native to Kubernetes Services; no external LB
product is involved.

## Ingress controller: Traefik

Constraint (founder direction on #9): open-source, free-to-use only —
ingress-nginx or Traefik. **Traefik** is the pick because:

- The upstream Kubernetes project retired ingress-nginx (announced
  November 2025; best-effort maintenance ended March 2026). Adopting it
  for a new deployment today means adopting an unmaintained controller
  with no future security releases.
- Traefik is actively maintained, MIT-licensed, and its open-source
  distribution fully supports the standard `networking.k8s.io/v1`
  Ingress resource used here — no commercial tier required for anything
  these manifests do.

The Ingress uses `ingressClassName: traefik` and the
`traefik.ingress.kubernetes.io/router.entrypoints` annotation. Switching
controllers later means changing only `web-ingress.yaml`.

## Files

| File | Contents |
|---|---|
| `configmap.yaml` | Non-secret env for the api (`HTTP_ADDR`, `APP_ENV`, `LOG_LEVEL`, `JWKS_URL`, `JWT_AUDIENCE`) |
| `examples/secret.example.yaml` | **Structure-only stub** for `DATABASE_URL` — placeholder value, never a real credential, outside the applied set |
| `api-deployment.yaml` / `api-service.yaml` | Go API, ClusterIP only |
| `web-deployment.yaml` / `web-service.yaml` | Astro server, ClusterIP |
| `web-ingress.yaml` | The single public entry point (web only) |
| `api-hpa.yaml` / `web-hpa.yaml` | CPU-based autoscaling for both Deployments |
| `api-pdb.yaml` / `web-pdb.yaml` | Disruption budgets keeping one replica through drains |
| `validate.sh` | The manifest set's **topology**, proved without a cluster — see [Validation](#validation) |
| `cashback/` | The Blnk ledger and its Redis (ADR-0002). **Opt-in — see below** |

## The cashback set is opt-in

`kubectl apply -f deploy/k8s/` does not recurse into subdirectories. That is
already what keeps `examples/` from overwriting a real credential, and
`cashback/` uses the same property for the same kind of reason: a cluster gets
a ledger because somebody applied it, never because they applied everything
else.

| File | Contents |
|---|---|
| `cashback/cashback-configmap.yaml` | `apivo-cashback-config` — the api's cashback env (`CASHBACK_ENABLED`, `LEDGER_DRIVER`, `BLNK_URL`, `REDIS_URL`, `NETWORK_DRIVER`, `HOUSE_ACCOUNT_ROUNDING`, `HOUSE_ACCOUNT_CLAWBACK`, `HOUSE_ACCOUNT_NETWORK_RECEIVABLE`, `PAYOUT_THRESHOLD_MINOR`, `PAYOUT_THRESHOLD_CURRENCY`) |
| `cashback/blnk-configmap.yaml` | `blnk-config` — non-secret Blnk configuration (`BLNK_REDIS_DNS`, `TZ`) |
| `cashback/blnk-deployment.yaml` / `cashback/blnk-service.yaml` | The ledger server: one replica, `Recreate`, migrates on boot, ClusterIP |
| `cashback/blnk-worker-deployment.yaml` | Blnk's queue worker. No Service — nothing calls a worker |
| `cashback/redis-deployment.yaml` / `cashback/redis-service.yaml` | Blnk's queue and cache: no persistence, `noeviction`, ClusterIP |

### What applying this set does today

**It gives you ledger infrastructure, not a working cashback product.** The Go
binary on `main` reads none of `CASHBACK_ENABLED`, `LEDGER_DRIVER`, `BLNK_URL`,
`REDIS_URL` or `NETWORK_DRIVER`, and mounts no cashback routes at all. Config
parsing arrives with **T001 (#291)** and route mounting with **T040 (#187)**.

So an operator who applies `deploy/k8s/cashback/` today gets a running Blnk, a
running Redis, and an api that ignores the ConfigMap entirely. That is a
useful and deliberate state — the ledger has to exist before anything can be
wired to it — but it is not cashback, and nothing here should be read as
claiming otherwise until both of those issues have landed.

### The switch, once the API honours it

The switch is the api Deployment's second `envFrom`, which references
`apivo-cashback-config` with `optional: true`. On a cluster that never applied
`cashback/` the ConfigMap does not exist, none of those keys reach the binary,
and the api pod starts normally. This is the same mechanism the Hetzner
deployment uses, in the vocabulary Kubernetes has: there, listing
`docker-compose.cashback.yml` in `COMPOSE_FILE` is the switch; here, applying
the subdirectory is.

First, create the ledger's runtime role. A one-off, run by an operator against
the cluster's database with the recipe in
[`scripts/spikes/ledger_schema/bootstrap.sql`](../../scripts/spikes/ledger_schema/bootstrap.sql)
— the production recipe, not a fixture:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
  -v blnk_password="$(openssl rand -base64 32)" \
  -f scripts/spikes/ledger_schema/bootstrap.sql
```

Then the Secret and the manifests:

```sh
# TWO ledger DSNs, one per role — see "The ledger runs as two roles" below.
# One shared value for the two secret-key halves: the ledger accepts it, the
# api presents it, and they must be equal. What makes the ledger CHECK is
# BLNK_SERVER_SECURE in cashback/blnk-configmap.yaml, not either key.
kubectl -n apivo create secret generic apivo-secrets \
  --from-literal=DATABASE_URL='<real connection string>' \
  --from-literal=BLNK_MIGRATE_DSN='<the database OWNER>' \
  --from-literal=BLNK_DATA_SOURCE_DNS='<the blnk_app role>' \
  --from-literal=BLNK_SERVER_SECRET_KEY="$LEDGER_KEY" \
  --from-literal=BLNK_SECRET_KEY="$LEDGER_KEY"

kubectl -n apivo apply -f deploy/k8s/
kubectl -n apivo apply -f deploy/k8s/cashback/
kubectl -n apivo rollout restart deployment/api   # picks up the ConfigMap
```

**A secret key is not an authentication switch.** Blnk only checks
credentials when `BLNK_SERVER_SECURE` is true; with it false the middleware
returns before it looks at a key (`api/middleware/auth.go` in v0.15.2), the
field is never defaulted, and Blnk logs *"SECURITY: server.secure is false —
API authentication is DISABLED"* at every start. It is set in
`cashback/blnk-configmap.yaml`, and `validate.sh` asserts it. `/health` is
skipped *before* that check, so the probes work either way — which is exactly
why the difference is invisible without an assertion.

The cashback Deployments map the ledger keys as **required** `secretKeyRef`s:
applying the ledger without adding them gives pods that refuse to start and
say why, which is the correct outcome for a ledger that cannot see its own
data. The api's `BLNK_SECRET_KEY` is `optional: true` instead, so a cluster
that never applied `cashback/` is not blocked by it — but the cluster runs
`APP_ENV=prod`, where the api refuses to start with cashback enabled and the
key unset, because a ledger reachable without a credential is a ledger anybody
on that network can post to.

### The ledger runs as two roles

The founder's split posture (2026-08-24), and the same one
`docker-compose.yml`, the CI job and `deploy/hetzner/` run — one answer in the
repository, not one per environment:

| | Role | Runs | Reads |
|---|---|---|---|
| **Migration** | the database **owner** | the `migrate` initContainer in `blnk-deployment.yaml` | Secret key `BLNK_MIGRATE_DSN` |
| **Runtime** | **`blnk_app`**, which owns nothing | the ledger server and the worker | Secret key `BLNK_DATA_SOURCE_DNS` |

Spike S1 established why the split is **required** rather than tidy: Blnk's
first migration issues `CREATE SCHEMA IF NOT EXISTS blnk`, and PostgreSQL
checks the database-level `CREATE` privilege *before* it takes the
`IF NOT EXISTS` shortcut — so pre-creating the schema does not avoid the
grant, and a single role would need `CREATE` on the database permanently for
one statement on one deploy. What `blnk_app` therefore cannot do is the point:
no `CREATE SCHEMA`, no DDL inside `blnk`, nothing in `public`.

Both DSNs arrive in their container under Blnk's own `BLNK_DATA_SOURCE_DNS`.
The difference is **which Secret key each container maps**, and that is the
whole mechanism — it makes the owner's credential *absent* from the long-lived
processes rather than merely unused by them. `validate.sh` asserts it,
including that the server and the worker never map the migration key.

An `initContainer` rather than a `Job`, deliberately: a Job is immutable, so
`kubectl apply` over an existing one fails and every rollout would need a
generated name or a Helm hook — and these are plain manifests by design. An
initContainer runs to completion before the app container starts, which is
compose's `service_completed_successfully` expressed in what Kubernetes has.
`replicas: 1` with `strategy: Recreate` is what keeps two from racing.

The worker sits in a different pod, so it cannot share that initContainer. It
gets a `wait-for-ledger` initContainer instead, which polls the ledger's
`/health` — the thing that only answers *after* the migration has run.

Two shapes here differ deliberately from the api and web Deployments, and
neither is an oversight:

- **One replica, `strategy: Recreate`, no HPA and no PDB.** The Blnk pod runs
  `blnk migrate up` in an initContainer before `blnk start`, and two replicas
  rolling out together race that migration. Redis
  is single-replica for a different reason: two pods behind one Service would
  split the queue between two unrelated instances. A `PodDisruptionBudget` with
  `minAvailable: 1` on a single-replica Deployment blocks node drains rather
  than protecting anything, so there is none. The cost is a short ledger outage
  on every rollout, which credits (created by a poller on a cycle) and
  withdrawals (idempotent, C-5) both survive.
- **Both the ledger and its worker are probed on `GET /health`.** Blnk has
  served an unauthenticated `/health` since v0.10.3 and this image is 0.15.2.
  Nothing needs installing into the image for it: an `httpGet` probe is
  performed by the kubelet from outside the container, unlike a compose
  healthcheck, which runs inside one. The **worker has its own** `/health`, on
  port 5004 — in `cmd/workers.go`, `runWorkers` calls
  `startMonitoringServer(conf)` unconditionally and that route answers
  `{"status": "UP", "service": "worker"}`, with only `/monitoring/` and
  `/metrics` behind `MetricsAuthHandler`. Worth probing rather than trusting:
  a worker dead on its first Redis call would otherwise sit `Running` with
  transactions piling up behind it. What proves the ledger's *numbers* are
  right is neither probe — that is the continuous C-1 zero-sum check, a query
  over real rows.

## Config/env parity with compose and `.env.example`

| Key | Local dev (`.env.example` / compose) | Kubernetes |
|---|---|---|
| `HTTP_ADDR` | `:8080` | `configmap.yaml` → api envFrom |
| `APP_ENV` | `dev` | `configmap.yaml` (`prod` — the binary accepts exactly `dev` or `prod`) |
| `LOG_LEVEL` | `info` | `configmap.yaml` |
| `BRAND_DIR` | unset everywhere | **Not shipped in any manifest, deliberately.** It names a directory holding a `brand.json` (ADR-0004): the legal entity, the domains, the support mailboxes and the revision of the terms members accept. No value this repository could ship would be anything but a lie about a real company, so the image carries none. A deployment without it starts and serves everything except the opt-in: `/api/v1/cashback/participation` answers **503** naming this key. Set it, and mount a real `brand.json` there, to let members join cashback. A path that holds no readable, complete brand file fails startup — a deployment that named one meant it |
| `JWKS_URL` | empty in `.env.example`; from the host env in compose | `configmap.yaml`, shipped **empty** — set a real endpoint to enable the editorial endpoints. Not a credential: it names where the provider publishes its **public** keys. **Empty means every `/api/v1/editorial/` route is unmounted and answers 404**; the api still starts and serves readers, logging an ERROR line naming the consequence — an editorial misconfiguration must not take the public site down. A placeholder hostname is deliberately *not* shipped: it would be a real value to the binary, selecting the fail-fast verifier path and crash-looping a fresh deploy |
| `JWT_AUDIENCE` | empty in `.env.example` | `configmap.yaml`, shipped **empty** (Supabase access tokens carry `authenticated`). Optional; only meaningful alongside a non-empty `JWKS_URL`, and setting it alone fails startup by design |
| `DATABASE_URL` | local Postgres from `docker-compose.yml` | api `secretKeyRef` → the `apivo-secrets` Secret, created out of band; structure documented in `examples/secret.example.yaml`, which a plain `kubectl apply -f deploy/k8s/` never touches (apply does not recurse into subdirectories) |
| `HOST` / `PORT` (web) | Astro defaults | Set inline in `web-deployment.yaml` (`0.0.0.0:4321` so the Node adapter binds the pod interface) |
| `CASHBACK_ENABLED`, `LEDGER_DRIVER`, `BLNK_URL`, `REDIS_URL`, `HOUSE_ACCOUNT_ROUNDING`, `HOUSE_ACCOUNT_CLAWBACK`, `HOUSE_ACCOUNT_NETWORK_RECEIVABLE`, `PAYOUT_THRESHOLD_MINOR`, `PAYOUT_THRESHOLD_CURRENCY` | `.env.example`; on Hetzner they come from `docker-compose.cashback.yml` | `cashback/cashback-configmap.yaml` → the api's **optional** second `envFrom`. Absent unless `cashback/` was applied, which is the whole switch |
| `NETWORK_DRIVER` | `.env.example`, set to `fixture` | `cashback/cashback-configmap.yaml`, shipped as `fixture`. The fixture adapter needs no credentials — the safe default while founder question Q1 is open (ADR-0003). Named rather than left empty: the api refuses to start with it unset |
| `BLNK_DATA_SOURCE_DNS` | Hetzner: `blnk.env`, read by the Docker daemon | Blnk `secretKeyRef` → `apivo-secrets`, **required**. A different Postgres role from `DATABASE_URL`: it owns the `blnk` schema and has no rights in `public` |
| `BLNK_REDIS_DNS` | Hetzner: set in the compose overlay from the container name | `cashback/blnk-configmap.yaml`, naming the `redis` Service |

## Deploying

Prerequisites: an ingress controller (Traefik — see above) and
**metrics-server** (the CPU-based HPAs consume `metrics.k8s.io`; without
it they never scale and report `<unknown>` targets).

```sh
kubectl create namespace apivo   # once

# Real secret is created out of band — never from a file in this repo.
# The structure-only stub lives in examples/ precisely so this apply can
# never overwrite a real credential with a placeholder:
kubectl -n apivo create secret generic apivo-secrets \
  --from-literal=DATABASE_URL='<real Supabase connection string>'

kubectl -n apivo apply -f deploy/k8s/
```

Before applying, replace the placeholder image references and the
placeholder Ingress host with real values (the container publish
pipeline pins image digests).

Health probes: the api exposes `/healthz` (liveness) and `/readyz`
(readiness, DB ping) — see the contract. The web has no dedicated health
route yet, so its probes hit `/`; move them to a dedicated route when
one lands.

## Validation

Two checks, answering two different questions.

**Shape** — CI runs kubeconform in strict mode over this directory. It walks
subdirectories, so `cashback/` and `examples/` are validated with no path list
to keep in step. Locally:

```sh
docker run --rm -v "$PWD/deploy/k8s:/manifests" \
  ghcr.io/yannh/kubeconform:v0.8.0@sha256:faffaf43f95aa6425306e1ab8d6fcad72acb9049158f38e574c085ea1ec0f64e \
  -strict -summary -kubernetes-version 1.32.0 /manifests
```

Or without Docker, which matters while Docker Desktop is unavailable:

```sh
go run github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0 \
  -strict -summary -kubernetes-version 1.32.0 deploy/k8s
```

**Meaning** — `validate.sh`, run by the `k8s-topology` workflow. kubeconform's
own documentation is explicit that anything beyond schema shape is out of
scope, and "is this Service publicly routable" is perfectly valid YAML either
way. This is to `deploy/k8s` what `deploy/hetzner/validate.sh` is to the
compose stacks: nothing but the frontend is publicly routable, the cashback
directory is a genuine opt-in rather than a dependency, and the addresses the
api is handed resolve to Services that exist in this repository.

```sh
sh deploy/k8s/validate.sh
```

## Connecting a publisher account

Applying the cashback set and setting `NETWORK_DRIVER` is not enough to ingest
anything. The adapter polls **on behalf of a publisher account**, and that
account is two rows in the database — `cashback.network`, which carries the
network's documented limits and the query parameter its click references
travel in, and `cashback.network_account`, which the two durable cursors hang
off. A deployment configured for a network it has no account row at starts
perfectly happily and logs, on every boot:

```
no publisher account "…" is connected at "…"
```

Ingestion is then off, and nothing else is wrong.

`apivo connect-network` writes both rows. It is a subcommand of the same
binary, so it runs anywhere the api runs and reads the same configuration the
api does:

```sh
kubectl exec deploy/apivo-api -- apivo connect-network -backfill-from 2026-06-01
```

**The network and the account come from the environment, not from flags.**
They are `NETWORK_DRIVER` and `NETWORK_ACCOUNT_ID` — the same two values the
running process resolves its adapter from — so it is not possible to connect
an account this deployment would not then poll.

**`-backfill-from` is where this account's history starts**, and it is
required the first time. Nothing in the system can work it out: too recent
silently skips history nobody notices is missing, too old asks the network for
years of it. It is **ignored on every later run**, and the command says so
when you pass one, because the trailing re-read walks from that instant for
about a hundred days and moving it forward would leave the span between the
old start and the new one never re-read — where every transaction would sit
pending forever, with nothing logged.

**`-inactive` connects without turning the network on**, and is also how a
live network is paused: activation is written on every run, on both rows
together. `cashback.offer`'s read joins on `network.active`, so an active
account at an inactive network polls happily while every offer on it is
unclickable.

Running it again is safe and expected — after a typo, from a fresh container,
or out of an init job. A re-run does **not** overwrite the network's limits,
its display name or its click-reference parameter: those are on the row so
they can be corrected without a release (if a network raises your rate, edit
the row), and a command that reset them to the documented defaults would put
the deployment back on numbers you had already found to be wrong. Neither
cursor is touched at any point.

**No credential is written.** `cashback.network_account.credential_ref`
records the *name* of the environment key the credential is read from —
`NETWORK_API_KEY` — and never its value (ADR-0003).
