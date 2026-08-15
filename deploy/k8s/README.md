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

CI runs kubeconform in strict mode over this directory. Locally:

```sh
docker run --rm -v "$PWD/deploy/k8s:/manifests" \
  ghcr.io/yannh/kubeconform:v0.8.0@sha256:faffaf43f95aa6425306e1ab8d6fcad72acb9049158f38e574c085ea1ec0f64e \
  -strict -summary -kubernetes-version 1.32.0 /manifests
```
