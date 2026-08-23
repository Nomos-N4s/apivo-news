# Environments

Three environments, two hosts, one mechanism. This file is the single source
of truth for what runs where, what moves it, and what an agent may do without
asking anyone.

| | QA | Staging | Production |
|---|---|---|---|
| **Deploys on** | every push to `main` | a `-rc` tag | a final semver tag |
| **Gate** | none — automatic | none — automatic | one human approval |
| **Host** | pre-production VPS | pre-production VPS | its own VPS |
| **Database** | Postgres container on the host | nonprod Supabase EU project | its own Supabase EU project |
| **Auth** | nonprod Supabase project | nonprod Supabase project | its own Supabase project |
| **Data** | throwaway, resettable | production-shaped | real |
| **`APP_ENV`** | `prod` | `prod` | `prod` |
| **Channel** | `:qa` | `:staging` | `:prod` |
| **Provisioned** | yes — serving | host yes, no release yet | not yet |

**The pre-production host exists and QA is serving.** Every merge to `main`
publishes the `:qa` channel, the host converges within the minute, and
`https://ra1ze.com` answers. Nothing is pushed to it: the box asks the
registry whether its channel moved and rolls itself forward.

**Staging's host is ready and has never had a release.** Its Caddy site
answers on `https://reapie.com` — with a 502, correctly, because no release
candidate has moved the `:staging` channel and there is nothing to proxy to.
`APIVO_STAGING_URL` in
[environments.env](../deploy/hetzner/environments.env) is still empty, and
that emptiness is the guard: `release.yml` refuses a release to any channel
whose URL is unset, so an `-rc` tag is refused today. Filling it in and
cutting the first candidate are the two remaining steps, in that order.

**Production is not provisioned.** It needs a second VPS, its own Supabase
project and a DNS record, none of which exist. Everything it needs is written
and rehearsed — the Caddy site, the compose stack, the systemd units, the
approval gate — so provisioning it later is running one script, not designing
a deployment on the day it is first needed.

## Why these three, and not two or four

QA and Staging earn their keep only if they are different things. They are:

**QA is where agents live.** Every merge to `main` is running there within a
minute or two, unreviewed by anyone after the merge. Its database is a
container with fixtures in it, so it can be reset without a conversation, and
a destructive migration costs nothing. Nothing here is precious.

**Staging is the dress rehearsal.** It deploys release candidates only, from
the identical pipeline that will later deploy production, with production's
shape. Nothing is manual here that is not manual in production. Its job is to
make the first production release boring — the first time that pipeline runs
to a *new host*, not the first time it runs at all.

**Staging runs on a real Supabase project, and that is the point.** It uses
the *nonprod* project — not production's — so release-candidate migrations
never touch production data, while everything Supabase enforces is in the
path: connection limits, pooler behaviour, extension availability, and the
managed-service failure modes a container cannot reproduce. A release that
would fail on first contact with production's database now has somewhere to
fail first.

This was previously written here as an accepted gap, on the belief that the
free tier allowed a single project which had to be production. **That was
wrong: the free tier allows two active projects per organisation.** One is
production's; the other is nonprod, and staging uses it. The gap was never
necessary — only unexamined.

**QA deliberately keeps its container.** That is not a lesser version of
staging, it is a different job: reset without a conversation, no egress on
every query, no 500 MB ceiling, and no dependency on a free project that
[pauses after about a week idle](#the-nonprod-project-pauses).

Sharing one database between QA and staging is the tempting shortcut and the
one thing to avoid. **The api migrates on boot**; QA deploys on every merge to
`main` and staging deploys on `-rc` tags. A shared schema would therefore have
QA migrating the database staging is running against, leaving staging's older
binary talking to a schema from the future — the exact failure this split
exists to prevent.

Mechanically staging is now production's shape: `docker-compose.local-db.yml`
is absent from its `COMPOSE_FILE` and its `DATABASE_URL` points at Supabase.
Those are the same two facts that will be true of production, and neither is
a code change.

**Use the session-mode connection string, not transaction mode.**
`golang-migrate` takes a Postgres advisory lock around the migration run, and
transaction pooling does not hold session state, so that lock is unreliable
there — migrations can fail or strand it. The pooler on `5432` is session mode
and is correct; `6543` is not.

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

`require` rather than `verify-full` for QA and Staging: the server is a
container one hop away on a network marked `internal: true`, and a
self-signed certificate cannot prove an identity. Encrypting without claiming
to verify is honest. Production reaches Supabase across the public internet
and uses `verify-full`.

## Auth: nonprod and production, never one issuer

Every environment verifies bearer tokens against a **JWKS endpoint**, named
per environment by `JWKS_URL`. QA and Staging point at the *nonprod* Supabase
project; production points at its own. Previews point at nothing at all.

**The boundary that matters is nonprod ↔ production, and it is a real one.** A
token is valid wherever its issuer is trusted, so a single shared project would
make a token minted for a QA test editor cryptographically valid against
production's API. What stands in its way today is only that production's
`account` table has no matching row — `ErrUnknownAccount` from
[role.go](../internal/identity/role.go). That is a genuine backstop, but it is
one row away from being the entire authorisation boundary, and seeding rows is
a routine thing to do in a test environment. Two issuers, and the question
never arises.

**QA and Staging sharing the nonprod issuer is deliberate, and a much weaker
concern.** Both are throwaway, and their `account` tables live in different
databases, so an editor seeded into QA has no account in staging and gets
`ErrUnknownAccount` there. Splitting them further would buy an isolation
nobody is relying on, at the cost of a third project.

`JWKS_URL` empty is a supported, documented state rather than a broken one:
the composition root logs one line and leaves **every** `/api/v1/editorial/`
route unmounted, so an environment without auth configured exposes nothing
that would have needed it. Reader endpoints are unaffected.
`/api/v1/editorial/queue` answering **404 rather than 401** is the signal that
a host has no `JWKS_URL`.

`JWT_AUDIENCE` additionally requires the `aud` claim to carry a given value
(Supabase issues `authenticated`). It is optional, and
[config.go](../internal/platform/config/config.go) treats setting it *without*
`JWKS_URL` as a configuration error rather than as a no-op. Add it after
sign-in is known to work, so a rejected token has one possible cause instead
of two.

### The nonprod project pauses

Free Supabase projects pause after about a week of inactivity, and a paused
project answers neither its database nor its JWKS endpoint. The failure is
asymmetric, and worth knowing before it happens rather than during:

- **A running api keeps working.** The JWKS is cached in-process.
- **The next rollout fails to boot.** `NewVerifier` fetches the JWKS at
  startup and *fails construction* when it cannot — deliberately, so a
  container never starts half-authenticated. The reconciler then rolls the
  environment back to the digest that was serving.
- **Staging additionally loses its database**, which is in the same project.
  QA does not, which is one more reason its container stays.

A deploy failing with a JWKS error on an environment that was fine yesterday
is this, until proven otherwise.

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
- Cut a release candidate (`git tag -a v0.2.0-rc.1`). That deploys to staging
  **once `APIVO_STAGING_URL` is recorded**; until then the pipeline refuses
  it, before publishing anything.

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

## Previews: one environment per open pull request

A pull request gets `pr-142.<preview domain>` a couple of minutes after it is
opened, and loses it within a minute of being closed — merged or abandoned,
the same path either way.

This exists because without it, **the only way to see a change running is to
merge it**, which makes `main` the place changes are first tried. At any real
rate of merging, QA then becomes a blur in which nobody can say whose change
broke what.

**Teardown is a deleted tag, not a message.** CI publishes `api:pr-142` and
`web:pr-142` when the pull request opens or is pushed to, and deletes both
when it closes. The host lists `pr-*` tags every minute and converges: a tag
with no stack gets one, a stack with no tag is destroyed. A webhook straight
to the host would need an inbound endpoint and a secret, and would leak an
environment forever the one time it was dropped. An absent tag converges every
minute regardless.

That design has one dangerous edge, and it is worth naming because the code is
written around it: **a registry that cannot be reached must be an error, never
an empty list.** An empty list means "every pull request is closed" and tears
everything down. `apivo-previews` therefore checks the status of every call
rather than ending its pipeline in a `grep || true`, and
[its test suite](../deploy/hetzner/bin/apivo-previews_test.sh) spends four
assertions on exactly that distinction.

| | |
|---|---|
| **Database** | one shared Postgres, a database per pull request, dropped on teardown. Its data is on a tmpfs — a preview's contents are worth nothing once the pull request closes. |
| **Auth** | none. `PUBLIC_SUPABASE_URL` is empty, so the editorial screens serve their fixture preview and nobody signs in. A throwaway environment built from an unreviewed branch does not get keys to a real auth project. |
| **Ingestion** | off. `POLL_INTERVAL=0` and `TRANSLATION_INTERVAL=0`: feeds cost bandwidth and translation costs real money per article, per open pull request, and neither tells a reviewer anything. |
| **Cap** | `APIVO_PREVIEW_MAX`, default 5, newest pull requests first. Over the cap the host logs which ones it is not starting rather than silently dropping them. |
| **Isolation** | previews share a network with each other and reach neither QA, Staging, nor either database. |

**Editorial form posts do not work in a preview**, and that is a stated
limitation rather than an oversight: the wildcard site cannot do the
same-origin rewrite the named environments do (the reasons, and what was
tested, are in [snippets.caddy](../deploy/hetzner/caddy/snippets.caddy)). It
costs nothing today because a preview has no auth to sign in with. If previews
ever need working editorial forms, the fix is the better one anyway — make the
same-origin check independent of the proxy, in `web/src/lib/`.

## Provisioning a host

**Step by step, with this project's real domains and everything that needs a
human: [RUNBOOK.md](RUNBOOK.md).** What follows is the reference for the
script itself.

```sh
APIVO_HOST_ROLE=preprod \
APIVO_QA_HOST=ra1ze.com APIVO_STAGING_HOST=reapie.com \
APIVO_PREVIEW_DOMAIN=ra1ze.com \
APIVO_ORIGIN_CERT=/root/origin.pem APIVO_ORIGIN_KEY=/root/origin.key \
APIVO_STAGING_ORIGIN_CERT=/root/origin-staging.pem \
APIVO_STAGING_ORIGIN_KEY=/root/origin-staging.key \
GHCR_USER=<github-user> GHCR_TOKEN=<PAT with read:packages> \
APIVO_CONFIGURE_FIREWALL=yes \
sh deploy/hetzner/provision.sh
```

Idempotent, and it never overwrites a file that holds a secret. It installs
the programs, the compose files, the Caddy config and the systemd timers, and
generates the Postgres certificates for QA and Staging with the uid read out
of the Postgres image rather than assumed.

What it deliberately does **not** do:

- Fill in `DATABASE_URL`, the Supabase keys, or the translation credential. It
  writes `/etc/apivo/<env>/api.env` and `web.env` as templates with every key
  present and empty — a safe documented state for all of them — and then never
  touches them again. A script that could fill them in would be a script that
  had to be given them.
- Decide for you whether to configure the firewall. `APIVO_CONFIGURE_FIREWALL`
  has **no default** and the script refuses to run until it is `yes` or `no`,
  because both answers are dangerous in different directions and a default
  would pick one of those hazards silently. `yes` admits 443 from
  Cloudflare's published ranges only, plus ssh — check `APIVO_SSH_PORT` first
  if you do not use 22, since a wrong value locks you out of the box. `no` is
  correct only when something else already restricts inbound 443; otherwise
  anyone who learns the VPS address talks straight to Caddy and every
  protection configured at the edge is one DNS lookup away from irrelevant.

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

### THE EDITORIAL RATE LIMIT IS GONE, AND NOTHING REPLACES IT YET

State this plainly, because an earlier draft of this document did not: **the
Hetzner deployment has no rate limit on the editorial endpoints.**

The Cloudflare Worker carried one — a per-caller limit expressed as an
inversion, so that every `/api/…` path which is not a public reader endpoint
was limited, and a route added later was limited from its first request.
Nothing deploys that Worker any more. Keeping
[`deploy/cloudflare/`](../deploy/cloudflare/) and `wrangler.jsonc` in the tree
keeps the *design and its tests*; it does not keep the *protection*.

Caddy does not carry it, and does not pretend to
([snippets.caddy](../deploy/hetzner/caddy/snippets.caddy) says so). What is
unaffected is authorisation: a valid JWT and the database's second check of
the editor role are enforced in Go, by the same code, whatever proxy is in
front. What is absent is the bound on invalid-token load — an attacker who
cannot get *in* can still make the api do JWT verification work all day.

Nothing is publicly reachable today, so nothing is currently exposed. That is
the only reason this is a follow-up rather than a defect in production.

**Porting the limit to Go middleware is required before the first public
deployment.** In the api itself it is in-repo, testable, applies in every
environment including local development, and is independent of which proxy is
in front — strictly better than where it was. Cloudflare WAF rules would also
work, and would live in a dashboard, which this project has already decided
against. Deleting `deploy/cloudflare/` is what that port unblocks.

## Required before anything is publicly reachable

- **The editorial rate limit**, ported to Go middleware. See above. This is
  not a nice-to-have and not a follow-up that can drift: today there is no
  limit at all, and the environments are only safe because nobody can reach
  them.
- **The firewall**, on every host — `provision.sh` will not run without an
  explicit answer, and `no` is only correct when something else restricts
  inbound 443 to Cloudflare's ranges.

## Deferred, deliberately

- **The production host.** Provision it when there is something to protect.
- **Backups.** Supabase covers production. QA and Staging are containers on
  the VPS with no backup at all — QA is fixtures and deliberately disposable,
  and staging's data is rebuilt from a release candidate rather than kept.
  Nothing on a VPS holds state that matters yet — and the moment production
  exists, that sentence needs re-checking.
- **`linux/arm64` images.** Built for `amd64` only. Hetzner's ARM line (CAX)
  needs the Dockerfile made cross-aware first — two lines, `TARGETARCH` and
  `--platform=$BUILDPLATFORM` — and is worth it if the box is ARM.
