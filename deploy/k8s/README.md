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
| `cashback/cashback-configmap.yaml` | `apivo-cashback-config` — the api's cashback env (`CASHBACK_ENABLED`, `LEDGER_DRIVER`, `BLNK_URL`, `REDIS_URL`, `NETWORK_DRIVER`) |
| `cashback/blnk-configmap.yaml` | `blnk-config` — non-secret Blnk configuration (`BLNK_REDIS_DNS`, `TZ`) |
| `cashback/blnk-deployment.yaml` / `cashback/blnk-service.yaml` | The ledger server: one replica, `Recreate`, migrates on boot, ClusterIP |
| `cashback/blnk-worker-deployment.yaml` | Blnk's queue worker. No Service — nothing calls a worker |
| `cashback/redis-deployment.yaml` / `cashback/redis-service.yaml` | Blnk's queue and cache: no persistence, `noeviction`, ClusterIP |

The switch is the api Deployment's second `envFrom`, which references
`apivo-cashback-config` with `optional: true`. On a cluster that never applied
`cashback/` the ConfigMap does not exist, none of those keys reach the binary,
the cashback routes are not mounted, and the api pod starts normally. This is
the same mechanism the Hetzner deployment uses, in the vocabulary Kubernetes
has: there, listing `docker-compose.cashback.yml` in `COMPOSE_FILE` is the
switch; here, applying the subdirectory is.

```sh
# The ledger's own Postgres role — NOT the api's. It owns the `blnk` schema
# and has no rights in `public`, which is what makes "Blnk's migrations never
# touch public" enforceable rather than hoped for (ADR-0002 spike S1).
kubectl -n apivo create secret generic apivo-secrets \
  --from-literal=DATABASE_URL='<real connection string>' \
  --from-literal=BLNK_DATA_SOURCE_DNS='<the blnk role, search_path=blnk>'

kubectl -n apivo apply -f deploy/k8s/
kubectl -n apivo apply -f deploy/k8s/cashback/
kubectl -n apivo rollout restart deployment/api   # picks up the ConfigMap
```

Both cashback Deployments map `BLNK_DATA_SOURCE_DNS` as a **required**
`secretKeyRef`: applying the ledger without adding the key gives pods that
refuse to start and say why, which is the correct outcome for a ledger that
cannot see its own data.

Two shapes here differ deliberately from the api and web Deployments, and
neither is an oversight:

- **One replica, `strategy: Recreate`, no HPA and no PDB.** The Blnk container
  runs `blnk migrate up` before `blnk start`, exactly as upstream's own compose
  file does, and two replicas rolling out together race that migration. Redis
  is single-replica for a different reason: two pods behind one Service would
  split the queue between two unrelated instances. A `PodDisruptionBudget` with
  `minAvailable: 1` on a single-replica Deployment blocks node drains rather
  than protecting anything, so there is none. The cost is a short ledger outage
  on every rollout, which credits (created by a poller on a cycle) and
  withdrawals (idempotent, C-5) both survive.
- **The ledger is probed on `GET /health`; the worker is not probed at all.**
  Blnk has served an unauthenticated `/health` since v0.10.3 and this image is
  0.15.2. Nothing needs installing into the image for it: an `httpGet` probe is
  performed by the kubelet from outside the container, unlike a compose
  healthcheck, which runs inside one. The worker gets no probe because
  `/health` belongs to the process `blnk start` runs, not to `blnk workers` —
  a probe against a port that may never be listened on would not detect a
  broken worker, it would crash-loop a working one and stop the queue it was
  draining. What proves the ledger's *numbers* are right is neither: that is
  the continuous C-1 zero-sum check, a query over real rows.

## Config/env parity with compose and `.env.example`

| Key | Local dev (`.env.example` / compose) | Kubernetes |
|---|---|---|
| `HTTP_ADDR` | `:8080` | `configmap.yaml` → api envFrom |
| `APP_ENV` | `dev` | `configmap.yaml` (`prod` — the binary accepts exactly `dev` or `prod`) |
| `LOG_LEVEL` | `info` | `configmap.yaml` |
| `JWKS_URL` | empty in `.env.example`; from the host env in compose | `configmap.yaml`, shipped **empty** — set a real endpoint to enable the editorial endpoints. Not a credential: it names where the provider publishes its **public** keys. **Empty means every `/api/v1/editorial/` route is unmounted and answers 404**; the api still starts and serves readers, logging an ERROR line naming the consequence — an editorial misconfiguration must not take the public site down. A placeholder hostname is deliberately *not* shipped: it would be a real value to the binary, selecting the fail-fast verifier path and crash-looping a fresh deploy |
| `JWT_AUDIENCE` | empty in `.env.example` | `configmap.yaml`, shipped **empty** (Supabase access tokens carry `authenticated`). Optional; only meaningful alongside a non-empty `JWKS_URL`, and setting it alone fails startup by design |
| `DATABASE_URL` | local Postgres from `docker-compose.yml` | api `secretKeyRef` → the `apivo-secrets` Secret, created out of band; structure documented in `examples/secret.example.yaml`, which a plain `kubectl apply -f deploy/k8s/` never touches (apply does not recurse into subdirectories) |
| `HOST` / `PORT` (web) | Astro defaults | Set inline in `web-deployment.yaml` (`0.0.0.0:4321` so the Node adapter binds the pod interface) |
| `CASHBACK_ENABLED`, `LEDGER_DRIVER`, `BLNK_URL`, `REDIS_URL` | `.env.example`; on Hetzner they come from `docker-compose.cashback.yml` | `cashback/cashback-configmap.yaml` → the api's **optional** second `envFrom`. Absent unless `cashback/` was applied, which is the whole switch |
| `NETWORK_DRIVER` | `.env.example`, empty = `fixture` | `cashback/cashback-configmap.yaml`, shipped **empty**. Empty means the fixture adapter, which needs no credentials — the safe default while founder question Q1 is open (ADR-0003) |
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
