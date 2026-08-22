# Environments

Three environments, two hosts, one mechanism. This file is the single source
of truth for what runs where, what moves it, and what an agent may do without
asking anyone.

| | QA | Staging | Production |
|---|---|---|---|
| **Deploys on** | every push to `main` | a `-rc` tag | a final semver tag |
| **Gate** | none — automatic | none — automatic | one human approval |
| **Host** | pre-production VPS | pre-production VPS | its own VPS |
| **Database** | Postgres container on the host | its own Supabase EU project | its own Supabase EU project |
| **Data** | throwaway, resettable | production-shaped | real |
| **`APP_ENV`** | `prod` | `prod` | `prod` |
| **Channel** | `:qa` | `:staging` | `:prod` |
| **Exists today** | yes | yes | **no** |

**There is no production environment.** No VPS, no Supabase project, no DNS
record, and `APIVO_PROD_URL` in [environments.env](../deploy/hetzner/environments.env)
is empty. A final semver tag is refused by the release pipeline for exactly
that reason, before anything is published. Everything production needs is
written and rehearsed — the Caddy site, the compose stack, the systemd units,
the approval gate — so provisioning it later is running one script, not
designing a deployment on the day it is first needed.

## Why these three, and not two or four

QA and Staging earn their keep only if they are different things. They are:

**QA is where agents live.** Every merge to `main` is running there within a
minute or two, unreviewed by anyone after the merge. Its database is a
container with fixtures in it, so it can be reset without a conversation, and
a destructive migration costs nothing. Nothing here is precious.

**Staging is the dress rehearsal.** It deploys release candidates only, from
the identical pipeline that will later deploy production, against its own
Supabase project, with production's shape. Nothing is manual here that is not
manual in production. Its job is to make the first production release boring —
the first time that pipeline runs to a *new host*, not the first time it runs
at all.

If those two descriptions ever collapse into one, collapse the environments
too and save the money.

**Production is alone on its host**, and that is not about load. QA runs
whatever was merged minutes ago; a host is a shared blast radius. One
compromised dependency with a shell in a QA container is a `docker exec` away
from production's `DATABASE_URL` if they share a Docker daemon. Two boxes, and
that sentence stops being true.

## All three run `APP_ENV=prod`

Including QA. In this codebase `prod` means *hardened* — JSON logs, `Secure`
on every cookie, a `DATABASE_URL` that [must be encrypted](../internal/platform/config/config.go) —
not "the production instance".

That is why QA's Postgres container serves TLS and QA connects with
`sslmode=require`. The lazy alternative, `APP_ENV=dev`, would make QA the one
environment where a cookie bug cannot reproduce, because
[secure-request.ts](../web/src/lib/secure-request.ts) treats `APP_ENV=prod` as
authoritative for the `Secure` attribute. An environment that runs with the
guards off cannot find the bugs the guards would have caught.

`require` rather than `verify-full` for QA specifically: the server is a
container one hop away on a network marked `internal: true`, and a
self-signed certificate cannot prove an identity. Encrypting without claiming
to verify is honest. Staging and production reach Supabase across the public
internet and use `verify-full`.

## How a deploy actually happens

**Nothing pushes to a host.** No CI job holds an SSH key, no agent has a
shell on a VPS, and there is no inbound webhook to forge or to leave exposed.

```
merge / tag  ->  publish.yml builds, proves the stamp, MOVES A CHANNEL TAG
                                                              |
                                                       (the deploy)
                                                              |
             host asks the registry every minute: has my channel moved?
                                                              |
                                    yes -> pull by digest, roll out, prove it
```

The host side is [`apivo-reconcile`](../deploy/hetzner/bin/apivo-reconcile),
run by a systemd timer once a minute per environment. On the overwhelmingly
common tick nothing has changed and it costs two registry queries. When the
channel has moved it pins the new digest, rolls out, and then *proves the
roll-forward* rather than assuming it:

1. `docker compose up -d --wait` — every container reports healthy by its own
   healthcheck.
2. Each container is asked which image it is running; it must be the digest
   just pinned. (`--wait` alone is not enough: the *previous* container
   reports healthy just as happily if the new one never replaced it.)
3. The frontend fetches the api's `/healthz` across the compose network, and
   the version it serves must equal the version stamped into the image.

Any of those failing rolls the environment back to the digest that was
serving. Its decisions have [a test suite](../deploy/hetzner/bin/apivo-reconcile_test.sh)
that CI runs on every pull request.

**A tag is what an environment tracks; a digest is what it runs.** Every
container in every environment is pinned by digest, so "what is running in
staging" has one answer that cannot drift under a tag somebody moved.

### Why pull and not push

A push-based deploy needs an inbound endpoint on the host, a shared secret CI
can present, and a credential in CI that can reach a production box. This
needs none of the three. It also converges after a network partition with
nobody re-triggering anything, and it self-heals: a container that died
between ticks comes back on the next one, because the reconciler runs
`up -d` whether or not the channel moved.

The cost is latency — up to a minute before the host looks. That is the whole
trade, and for an alpha it is not close.

## What an agent may do

**Freely, with no approval:**

- Open a pull request. CI proves it.
- Merge to `main`. That deploys to QA automatically.
- Ask what is running anywhere: `sh scripts/env_status.sh` (add `--json`).
  One HTTPS request per environment, no credentials, no SSH, no registry
  token. It reports what each environment *serves*, which is the only account
  of a deployment that cannot be wrong.
- Cut a release candidate (`git tag -a v0.2.0-rc.1`). That deploys to
  staging.

**Never, and there is no mechanism to do it anyway:**

- Reach a VPS. There is no key to hold.
- Move the `prod` channel. It requires an approval on the `production` GitHub
  Environment, and the reviewers are configured there rather than in any file
  this repository can change.
- Change what an environment runs without going through the registry. The
  reconciler overwrites hand-edited pins on the next tick.

The loop an agent should use: merge, then poll `scripts/env_status.sh` until
the version it reports matches. If it does not converge within a few minutes,
the reconciler refused something and said why — ask a human to read
`journalctl -u apivo-reconcile@qa`.

## Provisioning a host

```sh
APIVO_HOST_ROLE=preprod \
APIVO_QA_HOST=qa.example.com APIVO_STAGING_HOST=staging.example.com \
APIVO_ORIGIN_CERT=/root/origin.pem APIVO_ORIGIN_KEY=/root/origin.key \
GHCR_USER=<github-user> GHCR_TOKEN=<PAT with read:packages> \
sh deploy/hetzner/provision.sh
```

Idempotent, and it never overwrites a file that holds a secret. It installs
the programs, the compose files, the Caddy config and the systemd timers, and
generates QA's Postgres certificate with the uid read out of the Postgres
image rather than assumed.

What it deliberately does **not** do:

- Fill in `DATABASE_URL`, the Supabase keys, or the translation credential. It
  writes `/etc/apivo/<env>/api.env` and `web.env` as templates with every key
  present and empty — a safe documented state for all of them — and then never
  touches them again. A script that could fill them in would be a script that
  had to be given them.
- Touch the firewall, unless `APIVO_CONFIGURE_FIREWALL=yes`. Getting that
  wrong locks you out of the box, so it is a deliberate act. **Do run it**: it
  admits 443 from Cloudflare's published ranges only, and without it anyone
  who learns the VPS address talks straight to Caddy and every protection
  configured at the edge is one DNS lookup away from irrelevant.

Finally, record the public URL in
[deploy/hetzner/environments.env](../deploy/hetzner/environments.env), in a
pull request. That file is what the release pipeline probes and what
`env_status.sh` reports on — an environment nobody wrote down is an
environment no release can verify. It is committed rather than kept in a
settings screen for the same reason `wrangler.jsonc` was: a value that lives
only in a dashboard has no history, no review, and no diff when it changes.

### TLS

Cloudflare terminates the browser's TLS. Caddy presents a **Cloudflare Origin
Certificate** — free, valid for years, trusted by Cloudflare and by nothing
else, which is exactly right for a certificate only ever shown to Cloudflare.
Set the zone to **Full (strict)** so it is actually verified.

There is deliberately no ACME on the box: HTTP-01 through an orange-clouded
record is unreliable, and DNS-01 would put a Cloudflare API token with DNS
edit rights on the VPS — a much larger key than the one problem it solves.
Wildcard per-PR preview hostnames would change that calculation; nothing else
does.

## Operating a host

`apivoctl` is on the `PATH` after provisioning.

| | |
|---|---|
| `apivoctl status` | what every environment is running, and whether it is healthy |
| `apivoctl deploy qa` | reconcile now instead of waiting for the tick |
| `apivoctl logs qa api` | follow an environment's logs |
| `apivoctl psql qa` | a psql shell on QA's own Postgres |
| `apivoctl pause qa` | **stop reconciling — do this before investigating** |
| `apivoctl resume qa` | reconcile again |
| `apivoctl rollback qa` | break glass; see below |
| `apivoctl edge reload` | apply an edited Caddy config |

**Pause before you investigate.** The reconciler converges every minute, so a
container you stopped by hand to look at comes back within the minute, taking
the evidence with it.

**Rolling back is moving the channel**, not touching a host: re-run the
release workflow from the previous tag ([RELEASING.md](RELEASING.md)). Every
host tracking that channel converges on its own, and the path is exactly as
tested as the release path because it *is* the release path.
`apivoctl rollback` exists for when CI itself is unavailable. It pins a digest
by hand and pauses reconciliation, because otherwise the next tick would roll
straight back to the build you are backing out of — which leaves the
environment running something no tag names, and that is a state to leave
quickly.

## Proving it without a host

`make hetzner-validate` runs what CI runs on every pull request:

- the reconciler's test suite — a stub registry and daemon, so the rollback,
  the digest mismatch and the version mismatch are all exercised;
- `shellcheck` over every script that runs on a host, two of them as root;
- every environment's compose configuration, rendered and asserted: names
  namespaced per environment, the api unpublished, the data network internal;
- both Caddyfiles through `caddy validate`.

This is to the Hetzner deployment what `wrangler deploy --dry-run` was to the
Cloudflare one: everything knowable without credentials, known on every pull
request instead of on the next deploy.

## Cloudflare's remaining job

Cloudflare stays — as DNS, edge TLS, CDN, WAF and rate limiting in front of
Hetzner origins. What is retired is **Cloudflare Containers**: the application
no longer runs there.

Two things got simpler in the move, and both are worth naming because they
were real costs:

- **The frontend reaches the API privately.** On Cloudflare a container was
  reachable only through its Durable Object, so the deployment's own public
  origin was the only address the web container had for the api — a hop out
  to the internet and back. Here it is `http://apivo-<env>-api:8080` across a
  compose network.
- **Ingestion no longer needs a cron trigger to exist.** Cloudflare stopped an
  idle container, which stopped the feed poll loop with it — a paper that
  quietly stopped updating on a quiet night — so a `*/15` cron existed purely
  to wake it. A container on a VPS is not stopped for being idle;
  `POLL_INTERVAL` simply works.

### The one thing that has not moved yet

[`deploy/cloudflare/`](../deploy/cloudflare/) and `wrangler.jsonc` are still
in the tree, and the `wrangler` CI job still proves them. They are **retired
but not deleted**, for one specific reason: the Worker carries the
**per-caller rate limit on the editorial endpoints**, expressed as an
inversion — every `/api/…` path that is not a public reader endpoint is
limited, so a route added later is limited from its first request.

Caddy does not carry that. It only chooses an upstream, and it says so in
[snippets.caddy](../deploy/hetzner/caddy/snippets.caddy). Nothing about
authorisation is weaker for it — a valid JWT and the database's second check
of the editor role are enforced in Go, by the same code, whatever proxy is in
front — but the *rate limit* would be lost if `worker.js` were deleted today.

So it stays until the limit has a new home. The right home is Go middleware
in the api itself: in-repo, testable, applying in every environment including
local development, and independent of which proxy is in front. Cloudflare WAF
rules would also work and would be configured in a dashboard, which this
project has already decided against. **That port is the blocking follow-up to
this change**, and deleting the Cloudflare deployment is what it unblocks.

## Deferred, deliberately

- **The rate limiter port**, above. The one thing blocking deletion of the
  Cloudflare path.
- **Per-PR preview environments** (`pr-137.qa.<domain>`), so an agent can see
  its own change running before it merges. The biggest remaining unlock, and
  the one thing that would justify wildcard certificates and therefore DNS-01.
- **The production host.** Provision it when there is something to protect.
- **Backups.** Supabase covers staging and production; QA is fixtures and
  deliberately disposable. Nothing on a VPS holds state that matters yet —
  and the moment production exists, that sentence needs re-checking.
- **`linux/arm64` images.** Built for `amd64` only. Hetzner's ARM line (CAX)
  needs the Dockerfile made cross-aware first — two lines, `TARGETARCH` and
  `--platform=$BUILDPLATFORM` — and is worth it if the box is ARM.
