# Bringing the hosts up

Everything in this file needs a human. Nothing in it can be done by an agent,
and that is deliberate: no CI job holds an SSH key, no agent has a shell on a
VPS, and the Cloudflare and Supabase credentials never pass through this
repository. What an agent *can* do is everything on the other side of these
steps — see [ENVIRONMENTS.md](ENVIRONMENTS.md).

The order matters in two places and is otherwise flexible. Both are called out
where they occur.

| Domain | Environment | Exists |
|---|---|---|
| `ra1ze.com` | QA, and every per-pull-request preview at `pr-<n>.ra1ze.com` | to build |
| `reapie.com` | Staging | to build |
| `apivo.com` | Production | **not yet, and not in this pass** |

QA and Staging share one Hetzner box. Production gets its own, later, and
nothing below provisions it.

---

## 0. What to have ready

- **A Hetzner VPS**, Ubuntu 24.04, in the EU. Size it for **two full stacks
  plus previews**: QA and Staging each run an api, a web and a Postgres
  container, and previews add two containers apiece.
  - **16 GB** (e.g. CPX41) is comfortable and leaves the preview cap at 5.
  - **8 GB** works if you drop `APIVO_PREVIEW_MAX` to 2 in
    `/etc/apivo/preview/stack.env` after provisioning.
  - 4 GB is not enough for this shape.
  - `amd64`, not ARM. The images are built `linux/amd64` only; Hetzner's CAX
    line needs the Dockerfile made cross-aware first.
- **The domains on Cloudflare**, in the same account. Each is its own zone
  and gets its own origin certificate — see step 2.
- **A GitHub personal access token** with `read:packages` only. This is the
  one credential the box needs and the only thing it uses it for: pulling
  images. It can read nothing else and write nothing.
- Your **SSH public key** on the Hetzner box, and the **port** sshd listens
  on. If that is not 22, you need it in step 4 or you will lock yourself out.

- **A Supabase organisation with two projects**: one *nonprod*, one for
  production later. The free tier allows two active projects per
  organisation, so this costs nothing. From the **nonprod** project you need
  its URL, its **anon** key, and — for seeding editors, not for any env file
  — its **service_role** key. QA and Staging both use it: staging for its
  database and both for auth. See [ENVIRONMENTS.md](ENVIRONMENTS.md).

---

## 1. Cloudflare — zones and TLS mode

For **`ra1ze.com`** and **`reapie.com`** (and `apivo.com` whenever production
happens):

1. Add the zone to Cloudflare and point the registrar's nameservers at it.
2. **SSL/TLS → Overview → Full (strict).**

Full (strict) is not optional here. The box presents a Cloudflare Origin
Certificate, which is trusted by Cloudflare and by nothing else. On *Flexible*
Cloudflare would talk to the origin in cleartext; on *Full* it would accept
any certificate at all, which makes the encryption decorative.

## 2. Cloudflare — two origin certificates, one per zone

**One per Cloudflare zone**, so two: one in `ra1ze.com`, one in `reapie.com`.

In **each** zone: **SSL/TLS → Origin Server → Create Certificate**, take every
default (Cloudflare generates the key, 15 years, and the hostname list is
already right), Create.

The default list is `example.com, *.example.com` — the apex and the
first-level wildcard — which is exactly what each zone needs:

| Zone | Covers | Serves |
|---|---|---|
| `ra1ze.com` | `ra1ze.com`, `*.ra1ze.com` | QA **and** every preview |
| `reapie.com` | `reapie.com`, `*.reapie.com` | Staging |

`*.ra1ze.com` is what makes previews possible — every pull request gets
`pr-<n>.ra1ze.com`. It is in the default list, so simply do not remove it.

Save both blocks for each. **Each private key is shown exactly once.** Name
them so the next step is unambiguous:

```
origin.pem          origin.key          <- from the ra1ze.com zone
origin-staging.pem  origin-staging.key  <- from the reapie.com zone
```

**Why two and not one covering everything.** A single certificate spanning
both zones *is* possible — but only through Cloudflare's API, with an Origin
CA Key and a hand-built CSR. The **dashboard cannot do it**: its hostname
list is confined to the zone you are in. Two certificates and two `scp`
arguments is the cheaper trade, and Caddy presents the right one per site.

## 3. Cloudflare — DNS

Create these records, all pointing at the VPS's IPv4 address:

| Type | Name | Content | Proxy |
|---|---|---|---|
| `A` | `ra1ze.com` (`@`) | VPS IP | **Proxied** |
| `A` | `*.ra1ze.com` (`*`) | VPS IP | **Proxied** |
| `A` | `reapie.com` (`@`) | VPS IP | **Proxied** |

> **Check the wildcard actually goes orange.** Proxying a wildcard record has
> historically been a paid-plan feature at Cloudflare, and if your plan will
> not proxy `*.ra1ze.com` the record saves as DNS-only (grey cloud) instead.
> That does not break QA or Staging — only previews — but it breaks them in a
> confusing way, because a grey-clouded record sends the browser straight to
> the VPS IP, and the firewall in step 5 admits 443 from Cloudflare's ranges
> only. The symptom is previews timing out while QA works.
>
> If the wildcard will not proxy, you have three options: leave previews
> grey-clouded and add the visitor's own network to the firewall; skip the
> firewall for 443 and rely on Cloudflare's ranges being the only ones that
> know the hostname (weaker, and stated as such); or leave
> `APIVO_PREVIEW_DOMAIN` empty and run without previews. Tell me which and I
> will wire it.

Nothing else needs a DNS record. Staging is `reapie.com` itself, QA is
`ra1ze.com` itself, and previews are the wildcard.

---

## 4. The Hetzner box — provision it

Copy the certificate and key onto the box first:

```sh
# from your machine
scp origin.pem origin.key \
    origin-staging.pem origin-staging.key \
    root@<vps-ip>:/root/
```

Then, on the box as root:

```sh
apt-get update && apt-get install -y git ufw iptables-persistent
git clone https://github.com/Nomos-N4s/apivo-news.git
cd apivo-news

APIVO_HOST_ROLE=preprod \
APIVO_QA_HOST=ra1ze.com \
APIVO_STAGING_HOST=reapie.com \
APIVO_PREVIEW_DOMAIN=ra1ze.com \
APIVO_ORIGIN_CERT=/root/origin.pem \
APIVO_ORIGIN_KEY=/root/origin.key \
APIVO_STAGING_ORIGIN_CERT=/root/origin-staging.pem \
APIVO_STAGING_ORIGIN_KEY=/root/origin-staging.key \
GHCR_USER=<your-github-username> \
GHCR_TOKEN=<the read:packages token> \
APIVO_CONFIGURE_FIREWALL=yes \
APIVO_SSH_PORT=22 \
sh deploy/hetzner/provision.sh
```

**`APIVO_SSH_PORT` must match reality.** `APIVO_CONFIGURE_FIREWALL=yes`
installs a deny-by-default firewall that admits 443 from Cloudflare's
published ranges and ssh on that port. A wrong value locks you out of the box
and Hetzner's console is the only way back in.

**Install `ufw` and `iptables-persistent` first**, which is why they are in
the `apt-get` line above:

- Without `ufw`, provisioning dies at the firewall step — after everything
  else has already been installed, so you get a half-provisioned box and a
  one-line error.
- Without `iptables-persistent`, the rules in the `DOCKER-USER` chain **do
  not survive a reboot**. That matters more than it sounds: `ufw` alone does
  not cover Docker's published ports at all — Docker writes its own DNAT and
  FORWARD rules that are consulted before ufw's INPUT chain ever sees the
  packet, so `ufw deny 443` leaves Caddy wide open while `ufw status` claims
  otherwise. `DOCKER-USER` is the chain that actually closes it, and it is
  the one that is lost on reboot if nothing persists it. The script warns and
  continues rather than failing, so this is easy to miss.

The script installs Docker if it is missing, installs the compose files, the
Caddy config and the systemd timers, generates a Postgres certificate for QA
and for Staging, and writes `/etc/apivo/<env>/api.env` as an **empty
template**. It is idempotent — re-run it as often as you like; it never
overwrites a file that holds a secret.

It deliberately does not fill in any credential. That is step 5.

## 5. The Hetzner box — databases and auth

This is the first of the two ordering constraints: the api will not start
without a `DATABASE_URL`.

**QA uses its container; Staging uses the nonprod Supabase project.** Why they
differ is in [ENVIRONMENTS.md](ENVIRONMENTS.md); what to type is here.

### QA's database — the generated container password

`provision.sh` generated it. This reads it out of `stack.env` and writes the
URL into `api.env` without printing the password or leaving it in your shell
history:

```sh
pw=$(sed -n 's/^APIVO_PG_PASSWORD=//p' /etc/apivo/qa/stack.env)
[ -n "$pw" ] || echo "no password found — stop"
sed -i '/^#\?DATABASE_URL=/d' /etc/apivo/qa/api.env
printf 'DATABASE_URL=postgres://apivo:%s@postgres:5432/apivo?sslmode=require\n' "$pw" \
  >> /etc/apivo/qa/api.env
chmod 600 /etc/apivo/qa/api.env
unset pw
```

The host is `postgres`, the compose service name, not `localhost`. And
`sslmode=require` is not decoration: every environment here runs
`APP_ENV=prod`, and the api **refuses to start** on a `DATABASE_URL` whose
`sslmode` permits an unencrypted session — `disable`, `allow`, `prefer`, or
absent.

### Staging's database — the nonprod Supabase project

Supabase → the **nonprod** project → *Connect*. Take the connection string on
port **`5432`** and append `?sslmode=require`:

```
DATABASE_URL=postgres://postgres.<ref>:<password>@<region>.pooler.supabase.com:5432/postgres?sslmode=require
```

**Port `5432`, not `6543`.** `golang-migrate` takes a Postgres advisory lock
around the migration run; `6543` is transaction pooling, which does not hold
session state, so the lock is unreliable there — migrations can fail or strand
it.

Staging must also stop composing a local database, or it starts a Postgres
container nothing connects to. In `/etc/apivo/staging/stack.env`, drop
`:/opt/apivo/compose/docker-compose.local-db.yml` from the end, leaving:

```
COMPOSE_FILE=/opt/apivo/compose/docker-compose.yml
```

`APIVO_PG_PASSWORD` and the generated certificate become dead weight in that
file. Leaving them costs nothing and removes a step that could be got wrong.

### Auth — both environments, the nonprod project

**Check the endpoint answers before wiring it.** `NewVerifier` fetches the
JWKS at boot and *fails construction* when it cannot, so a wrong URL means the
api never starts, the rollout fails, and the reconciler rolls back. Recoverable,
but avoidable:

```sh
curl -sS https://<ref>.supabase.co/auth/v1/.well-known/jwks.json | head -c 200
```

You want JSON containing `"keys"`. Then, for **each** of `qa` and `staging`:

```sh
printf 'JWKS_URL=https://<ref>.supabase.co/auth/v1/.well-known/jwks.json\n' \
  >> /etc/apivo/<env>/api.env
printf 'PUBLIC_SUPABASE_URL=https://<ref>.supabase.co\nPUBLIC_SUPABASE_ANON_KEY=<anon key>\n' \
  >> /etc/apivo/<env>/web.env
chmod 600 /etc/apivo/<env>/api.env /etc/apivo/<env>/web.env
```

Leave `JWT_AUDIENCE` unset for now. It is a real check and worth adding, but
if it is wrong every token is rejected — add it once sign-in demonstrably
works, so a failure has one cause rather than two.

Verify after the next reconcile:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://ra1ze.com/api/v1/editorial/queue
```

**404 → 401 is the result you want.** 401 means the module is mounted and
refusing you for having no token. Still 404 means `JWKS_URL` did not take.

Leave everything else in `api.env` and `web.env` empty. Empty is a documented,
safe state for all of them, and a placeholder is *worse* than empty — the
binary treats it as real and crash-loops.

Re-running `provision.sh` undoes none of this: it writes `stack.env` only when
absent, and never overwrites `api.env` or `web.env`.

Leave everything else in `api.env` and `web.env` empty for now. Empty is a
documented, safe state for all of them: no `JWKS_URL` unmounts the editorial
routes and keeps serving readers, and no translation keys leave the pipeline
off. A placeholder would be *worse* than empty — the binary treats it as real
and crash-loops.

## 6. Start it, and watch it converge

`provision.sh` has already **enabled and started** the reconcile timers and
the preview timer, so QA and Staging are reconciling every minute before you
get here. What it enabled but did not start is the edge, because Caddy
binding 443 is the moment the box becomes reachable and that should be your
decision, not a script's:

```sh
systemctl start apivo-edge
```

Then watch it:

```sh
apivoctl status qa
apivoctl status staging
journalctl -u apivo-reconcile@qa -f
```

Nothing is pushed to this box, ever again. Every minute it asks the registry
whether its channel tag moved and converges if it did. `main` is already
publishing `:qa`, so QA comes up on its own.

If you ran the timers before step 5, the api will have been crash-looping on
an empty `DATABASE_URL` — that is the expected symptom, and it clears on the
next tick once the URL is there. `apivoctl deploy qa` reconciles immediately
instead of waiting.

Then, from anywhere:

```sh
curl -sS https://ra1ze.com/healthz
```

---

## 7. Back in the repository — one line

Once the **edge** answers on `reapie.com`, open a pull request setting:

```
APIVO_STAGING_URL=https://reapie.com
```

in [`deploy/hetzner/environments.env`](../deploy/hetzner/environments.env).

**The edge answering is not the app answering, and here you want the first.**
Check with:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://reapie.com/healthz
```

A **502 is the expected, correct answer at this point** — it means DNS,
Cloudflare, the origin certificate and Caddy all work, and Caddy has nothing
to proxy to because staging has no images yet. Staging gets its first images
from the first release candidate, and that is the *next* step, not this one.
A timeout or a TLS error is the failure to chase; a 502 is the green light.

Waiting for a 200 here would deadlock: `:staging` cannot move until this URL
is set, and the URL is what this step sets.

This is the second ordering constraint, and it runs the other way: that value
is empty *on purpose* until the host exists. An empty URL is the guard in
`release.yml` that refuses to publish images and move the `:staging` tag
toward an environment that is not there. Filling it in before the box is
provisioned does not make staging work sooner — it only deletes the guard, so
an `-rc` tag would move the channel and then fail its probe with nothing to
roll back to.

`APIVO_QA_URL` and `APIVO_PREVIEW_DOMAIN` are already set, because neither
gates anything.

### Then cut the first release candidate

QA fills itself — every merge to `main` publishes `:qa` and the box converges
within the minute. **Staging does not.** Nothing moves `:staging` except a
release candidate, so until you cut one, `reapie.com` stays a 502 however
healthy the box is:

```sh
git tag -a v0.1.0-rc.1 -m "first staging release candidate"
git push origin v0.1.0-rc.1
```

That runs `release.yml`, which moves `:staging`, waits for the box to
converge and probes `https://reapie.com/healthz` for up to five minutes —
long enough for the timer to fire, the pull, the rollout and the schema
migration. A green run means staging is serving that exact version, proven
by the version stamped in the payload rather than by a 200 alone.

## 8. GitHub — three settings

None of these block the box coming up.

1. **Branch protection on `main`**, with `ci.yml`'s jobs as required status
   checks. CI runs on every pull request today but nothing makes it *block* a
   merge — that is a settings click, and the checks have to be named
   individually (`go`, `lint`, `frontend`, `docker`, `web-image`, `openapi`,
   `sqlc-drift`, `ts-types-drift`, `kubeconform`, `wrangler`,
   `commit-hygiene`, `hetzner`).
2. **Merge queue** on `main`. CI already accepts `merge_group`; enabling the
   queue is the click.
3. **A `production` Environment** (Settings → Environments) with yourself as a
   required reviewer. This is the approval gate for final releases; it does
   nothing until production exists, and it costs nothing to create now.
   **Leave its "Deployment branches and tags" rule as *All branches*, or add
   a tag rule matching `v*`** — every real release enters this Environment on
   a tag ref, and the default branch-only rule would block the approval job
   rather than prompt you.
4. **One secret, for preview teardown only.** `GITHUB_TOKEN` publishes
   package versions but generally cannot delete them — `packages: write`
   covers the push, not the delete — so a closed pull request's preview
   images stay in the registry and its environment would keep running until
   the cap evicts it. Either:
   - add a repository secret `PREVIEW_CLEANUP_TOKEN`, a classic PAT with
     **read:packages** and **delete:packages**; or
   - on each package's page (`api` and `web`) under *Package settings →
     Manage Actions access*, give this repository the **Admin** role.

   Either one is enough. Nothing else about GHCR needs configuring.

One consequence of this repository being **public**: pull requests from forks
get a read-only `GITHUB_TOKEN` regardless of what a workflow asks for, so
they cannot publish preview images. `preview.yml` skips both its jobs for
fork pull requests rather than failing them. A maintainer who wants a preview
of an outside contribution pushes the branch to this repository.

---

## Updating a box that already exists

The reconciler updates **images** on its own, every sixty seconds. It does
not update anything else. The host scripts, the compose files, the Caddy
configuration and the systemd units all come from the checkout made in step
4, and they change only when you go and get them.

```sh
cd ~/apivo-news && git pull
```

That alone changes nothing on the host — the files under `/opt/apivo`,
`/etc/apivo` and `/etc/systemd/system` are copies. To install what you just
pulled, either re-run `provision.sh` with the same environment block as step
4, or, when one script is all that changed, install that one:

```sh
install -m 0755 deploy/hetzner/bin/apivo-seed-editors /opt/apivo/bin/apivo-seed-editors
ln -sf /opt/apivo/bin/apivo-seed-editors /usr/local/bin/apivo-seed-editors
```

Prefer the single install when you know what changed. `provision.sh` is
re-runnable and never overwrites an existing `stack.env`, but it wants every
variable step 4 gave it — the certificate paths, the GHCR credentials, the
firewall flag — and running it with some of them missing produces a partial
host rather than an error.

`apivo-seed-editors` also needs `jq`, which a box provisioned before it
existed will not have:

```sh
apt-get install -y jq
```

### Seeding editors into QA

```sh
read -rs SUPABASE_SERVICE_ROLE_KEY && export SUPABASE_SERVICE_ROLE_KEY
apivo-seed-editors qa 3
unset SUPABASE_SERVICE_ROLE_KEY
```

`read -rs` does not echo, so the key never appears on a command line, in
`ps`, or in the shell history. **Do not** put it in the command itself, and
do not write it into any file on the host: the service-role key bypasses
row-level security entirely and is the one credential in this system that
can read and write every table as the owner. The anon key in `web.env` is a
different thing and is safe there.

The command prints three `editor<n>@example.com` addresses and their
passwords **once**. Re-running it issues new passwords for the same
accounts rather than recovering the old ones.

It refuses `prod`, and refuses any environment whose `web.env` has no
`PUBLIC_SUPABASE_URL` — an editor with nothing to sign in to is not worth
creating.

### Demo pacing on QA

The poll and translation loops default to fifteen minutes, which is right
in production and fatal in front of an audience: a source added on camera
sits untouched until the next cycle. `docs/SHOWCASE.md` sets both to `1m`
for the local stack; on QA the same two keys go where every per-environment
value goes — appended to `/etc/apivo/qa/api.env`, last occurrence winning:

```sh
printf 'POLL_INTERVAL=1m\nTRANSLATION_INTERVAL=1m\n' >> /etc/apivo/qa/api.env
chmod 600 /etc/apivo/qa/api.env
apivoctl deploy qa
```

The deploy recreates the api container with the new environment; waiting a
minute for the next tick does the same, since every tick runs `up -d`.
`TRANSLATION_INTERVAL` matters only where the translation pipeline is
configured at all — unset provider keys keep it off whatever the interval
says. Neither key costs anything on QA: nothing it fetches is published,
and between changes its feeds answer the conditional GETs with 304s.

Afterwards, put the defaults back the same way — a key appended back to
empty returns to the binary's documented default, exactly as Docker reads
the file:

```sh
printf 'POLL_INTERVAL=\nTRANSLATION_INTERVAL=\n' >> /etc/apivo/qa/api.env
apivoctl deploy qa
```

Not on staging. It is production-shaped by design, and a cadence that
differs from production's is exactly the kind of drift it exists to catch.

---

## Switching cashback on — QA first

Everything cashback needs on a host is already there in outline: the
overlay, the two ledger env files `provision.sh` wrote as templates, the
`blnk_app` recipe, and an empty brand directory. What follows is the order
that turns them into a running wallet on QA, and why each step is where it
is. The gate [ENVIRONMENTS.md](ENVIRONMENTS.md#cashback-the-ledger-and-the-docker-free-path)
names — the three ADR-0002 spikes — is recorded as passed in
`specs/002-apivo-cashback-alpha/research.md` (2026-08-29).

**QA, and on the fixture network first.** QA reconciles on every merge to
`main`, which makes it the right place for the first real ledger and the
wrong place for a publisher's credentials: a poll is a real call against a
real publisher account, counted against a real rate limit, and QA would
spend that budget continuously (`api.env.example` says so at `NETWORKS`).
The fixture adapter needs no credential and carries a small catalogue of
invented retailers, embedded in the binary — enough to prove the whole
path from brand to click-out before a real network is named.

### 1. Refresh the compose files

The overlay gained the brand mount, and the compose files on the host are
copies made at provisioning time — the reconciler updates images, never
these. Bring the checkout up to date and install just the compose files:

```sh
cd ~/apivo-news && git pull
cp deploy/hetzner/compose/*.yml /opt/apivo/compose/
cp deploy/hetzner/caddy/* /opt/apivo/caddy/
apivoctl edge reload
mkdir -p /etc/apivo/qa/brand
```

Nothing but the edge restarts. A copied compose file is read at the next
`up`, and QA's `COMPOSE_FILE` does not list the overlay yet. The Caddy
snippets are copied because the edge now sends only `/api/v1/*` to the
api: the frontend serves the click-out and the guided tours under `/api/`
itself, and an edge provisioned before that change hands both to the api,
which answers 404 (#546).

### 2. Write the brand definition

The api ships no brand (ADR-0004): no legal entity, domain or terms
revision this repository could carry would be true of a real company.
Without one it refuses every opt-in and schedules no catalogue import, so
step 7 would have nothing to publish on. The file is
`/etc/apivo/qa/brand/brand.json`, its full shape is
`internal/platform/brand/testdata/fixture/brand.json`, and the rules the
api holds it to are each a startup failure when broken:

- `id` is a slug; `legal.jurisdiction` is an ISO 3166-1 alpha-2 code; the
  `terms` and `privacy` documents each carry an `id` and a `version`. The
  terms `version` is what a member's participation records — change it when
  the terms change, and not otherwise.
- `domains.primary` and every alias are bare lower-case host names, and all
  three `support` addresses sit on one of them: `ra1ze.com` for QA.
- `theme.colours` has `bg`, `surface`, `text` and `accent` as lower-case
  six-digit hex; the three `assets` are rooted paths under the web root.
- `defaults.language` is a BCP-47 primary subtag (`el`), `defaults.place` a
  slug, and `defaults.currency` is `EUR` on this host: it is the currency a
  member's wallet is kept in, and the overlay's `PAYOUT_THRESHOLD_CURRENCY`
  is EUR — a threshold in one currency cannot be compared against a balance
  in another.
- `payout.descriptor` is at most 22 printable ASCII characters: it is what
  a member reads on their bank statement.

```sh
cp ~/apivo-news/internal/platform/brand/testdata/fixture/brand.json /etc/apivo/qa/brand/brand.json
chmod 644 /etc/apivo/qa/brand/brand.json
```

Then replace every value in it. World-readable is required rather than
careless: the api runs as an unprivileged user inside its container, and a
brand it cannot read is a brand that fails startup. Nothing in the file is
a secret.

### 3. The ledger's database role

The ledger serves as `blnk_app`, which does not exist until the recipe in
`scripts/spikes/ledger_schema/bootstrap.sql` creates it. It runs as the
database owner — on QA the container's own `apivo` user over its local
socket, so nothing asks for a password — and it must run **before** the
first `blnk migrate`, because its default-privilege grants apply to tables
created after it. Generate the role's password into a variable first, since
step 4 writes it into `blnk.env`; hex, so it is safe inside a URL:

```sh
pw=$(openssl rand -hex 32)
docker exec -i apivo-qa-postgres psql -U apivo -d apivo -v ON_ERROR_STOP=1 \
  -v blnk_password="$pw" -f - < ~/apivo-news/scripts/spikes/ledger_schema/bootstrap.sql
```

`ON_ERROR_STOP=1` is load-bearing: several statements in the recipe are
guards that work by raising. The recipe is re-runnable — a second run
re-passwords the role rather than failing on it.

### 4. The two ledger env files, and the shared secret

`provision.sh` wrote `/etc/apivo/qa/blnk.env` and
`/etc/apivo/qa/blnk-migrate.env` with their DSNs empty. One takes the
runtime role, the other the owner — two files because Blnk reads the DSN
from one variable whichever job it does, and the owner's credential must be
absent from the long-lived server's environment rather than merely unused
by it. The owner's password is the one `provision.sh` generated into
`stack.env`:

```sh
owner=$(sed -n 's/^APIVO_PG_PASSWORD=//p' /etc/apivo/qa/stack.env)
[ -n "$owner" ] || echo "no password found — stop"
sed -i '/^BLNK_DATA_SOURCE_DNS=/d' /etc/apivo/qa/blnk.env /etc/apivo/qa/blnk-migrate.env
printf 'BLNK_DATA_SOURCE_DNS=postgres://blnk_app:%s@postgres:5432/apivo?sslmode=require\n' "$pw" \
  >> /etc/apivo/qa/blnk.env
printf 'BLNK_DATA_SOURCE_DNS=postgres://apivo:%s@postgres:5432/apivo?sslmode=require\n' "$owner" \
  >> /etc/apivo/qa/blnk-migrate.env
unset pw owner
```

Then the credential the api presents to the ledger and the ledger accepts.
One value in two files — `BLNK_SERVER_SECRET_KEY` in `blnk.env`,
`BLNK_SECRET_KEY` in `api.env` — and with cashback enabled the api refuses
to start without its half:

```sh
secret=$(openssl rand -hex 32)
sed -i '/^BLNK_SERVER_SECRET_KEY=/d' /etc/apivo/qa/blnk.env
sed -i '/^BLNK_SECRET_KEY=/d' /etc/apivo/qa/api.env
printf 'BLNK_SERVER_SECRET_KEY=%s\n' "$secret" >> /etc/apivo/qa/blnk.env
printf 'BLNK_SECRET_KEY=%s\n' "$secret" >> /etc/apivo/qa/api.env
unset secret
chmod 600 /etc/apivo/qa/blnk.env /etc/apivo/qa/blnk-migrate.env /etc/apivo/qa/api.env
```

### 5. What the api needs to know

Four keys appended to `/etc/apivo/qa/api.env`, last occurrence winning: the
brand's path inside the container, the network to poll, the account it
polls as, and the language that network's catalogue arrives in.

```sh
printf 'BRAND_DIR=/etc/apivo/brand\nNETWORKS=fixture\nNETWORK_FIXTURE_ACCOUNT_ID=fixture-publisher\nNETWORK_FIXTURE_SOURCE_LANGUAGE=en\n' \
  >> /etc/apivo/qa/api.env
```

And two things already in that file to check rather than add. `JWKS_URL`
must be set: every cashback route is a member's or an operator's and mounts
behind the same verifier as the editorial routes, so without it the whole
product answers 404 — step 5 put it there. And `DATABASE_URL` must carry
`pool_max_conns=14`: the scheduler takes a connection per job, the six jobs
a fully configured cashback deployment registers need fourteen where pgx
defaults to four, and the api refuses to start rather than deadlock under
load.

```sh
grep '^JWKS_URL=' /etc/apivo/qa/api.env
grep -q 'pool_max_conns' /etc/apivo/qa/api.env || \
  sed -i 's#^\(DATABASE_URL=.*sslmode=require\)$#\1\&pool_max_conns=14#' /etc/apivo/qa/api.env
grep '^DATABASE_URL=' /etc/apivo/qa/api.env | sed 's#://.*@#://…@#'
```

### 6. The switch

Listing the overlay in `COMPOSE_FILE` **is** the decision; nothing in
`api.env` is a second switch, on purpose. Append it and reconcile:

```sh
grep -q 'docker-compose.cashback.yml' /etc/apivo/qa/stack.env || \
  sed -i 's#^COMPOSE_FILE=.*#&:/opt/apivo/compose/docker-compose.cashback.yml#' /etc/apivo/qa/stack.env
grep '^COMPOSE_FILE=' /etc/apivo/qa/stack.env
apivoctl deploy qa
```

The deploy runs the ledger's migration as a one-shot container, then waits
on every healthcheck — the api's, the ledger's, the worker's. Three checks
say it worked:

```sh
apivoctl ps qa
curl -sS -o /dev/null -w '%{http_code}\n' https://ra1ze.com/api/v1/cashback/wallet
docker logs apivo-qa-api 2>&1 | grep -E 'brand definition|BRAND_DIR|NOT MOUNTED|cannot poll|scheduler started'
```

The six long-running containers healthy (the migration is a one-shot and
has already exited). **401 from the wallet**, for the reason 401 was the
answer for the editorial queue in step 5: mounted, and refusing you for
having no token. 404 means cashback did not mount, and the log names why —
a network that cannot poll, or `JWKS_URL` missing. The grep should show
`brand definition loaded` and neither of the two `BRAND_DIR` ERROR lines.

### 7. Connect the network, restart the api, then publish a rate

The account has to be told where its history starts — nothing invents a
`backfill_from` — and the catalogue has to arrive before there is a route
to publish a band on. Both are subcommands of the deployed binary, run
inside its container so they read its environment:

```sh
docker exec apivo-qa-api apivo connect-network -backfill-from 2026-09-01
docker restart apivo-qa-api
sleep 15
docker logs --since 1m apivo-qa-api 2>&1 | grep -E 'scheduler started|registered|NO AFFILIATE'
```

**The restart is not optional.** The api resolves its publisher account
once, at startup: an api that started before the account existed logged
`NO AFFILIATE NETWORK IS CONNECTED` at ERROR and registered only its three
global jobs — neither sweep and no catalogue import — and it will stay
that way until it starts again. The reconciler will not fight a plain
restart; it only recreates a container whose configuration changed. The
grep should show `affiliate network sweeps registered`,
`catalogue import registered` and `scheduler started` with `jobs: 6`, and
no `NO AFFILIATE` line.

The scheduled import runs every six hours, and its first run lands at a
random point in the first 36 minutes after the api starts — which on an
environment that redeploys on every merge is a first run that keeps moving.
Do not wait for it. The same import runs by hand, under the same lock, and
reports what it wrote and which routes a band can now be published on,
named by the network's own id for the retailer:

```sh
docker exec apivo-qa-api apivo import-catalogue
```

The report lists up to 25 publishable routes; a real network has more, and
this lists them all:

```sh
apivoctl psql qa
```

```sql
select n.id as network, mn.external_merchant_id, m.slug, mn.status
  from cashback.merchant_network mn
  join cashback.merchant m on m.id = mn.merchant_id
  join cashback.network n on n.id = mn.network_id
 order by 1, 2;
```

On the fixture network `FIXM-77` is a live retailer. A band at 15% of the
sale with the member's default share, on a tracking template that can be
any absolute URL — the fixture appends its click reference to whatever it
is given:

```sh
docker exec apivo-qa-api apivo publish-offer -merchant FIXM-77 -rate-bps 1500 \
  -deeplink 'https://outdoor.fixture.invalid/'
```

The command reports the route it published on and the click-out to try;
`deploy/k8s/README.md` walks through every flag. From here the member flow
is the one `make cashback-demo` runs locally, against QA's Supabase tokens
instead of the demo's stand-in.

### Moving QA to a real network

Swap `NETWORKS=fixture` for the driver and add its block —
`NETWORK_LINKWISE_ACCOUNT_ID`, `NETWORK_LINKWISE_API_KEY`,
`NETWORK_LINKWISE_API_SECRET`, `NETWORK_LINKWISE_SOURCE_LANGUAGE=el` — then
`apivoctl deploy qa` and `connect-network` again for the new account. Read
the caution at `NETWORKS` in `api.env.example` first: QA polls that account
every fifteen minutes for as long as the keys are in the file. And the
attribution canary (below, #524) refuses the sweep after three reports that
matched no click while none has ever matched one — on a real network, the
first thing worth watching for.

---

## Later: production

Not in this pass, and nothing above touches it. When there is something to
protect:

1. A second VPS. Production is alone on its host — not for load, but because
   QA runs whatever was merged minutes ago and a host is a shared blast
   radius.
2. A Supabase EU project, and `DATABASE_URL` with **`sslmode=verify-full`**.
3. `apivo.com` and `www.apivo.com` DNS, and the origin certificate extended
   to cover them.
4. `provision.sh` with `APIVO_HOST_ROLE=prod`.
5. `APIVO_PROD_URL=https://apivo.com` in `environments.env` — last, for the
   same reason as step 7.

And before **any** of it is publicly reachable, the editorial rate limit has
to exist in Go. See
[ENVIRONMENTS.md](ENVIRONMENTS.md#required-before-anything-is-publicly-reachable).

---

# When cashback goes wrong

Everything above brings hosts up. This part is for a host that is already
up and is doing something wrong with money.

Read the shape of it first, because it decides how much time you have:

| Symptom | How bad | Time you have |
|---|---|---|
| The zero-sum check is failing | **Incident.** The ledger and the wallets disagree | None. Stop writes if you cannot explain it in ten minutes |
| The attribution canary is refusing | **Incident.** Every purchase through a network is being lost | Hours. Members are being told they will be paid |
| The catalogue emptied | **Incident.** Members see no retailers, and the routes say they left | Hours. The next complete import is the remedy, not the deadline |
| A payout is stuck | Serious. One member's money is in flight | Hours |
| The poller has stalled | Serious, and silent. Commissions are not arriving | Hours, but the backlog grows |
| The unattributed queue is growing | Ordinary, unless it is growing *fast* | Days |

Two rules that hold in every section below:

- **Never edit a money row by hand.** Not an entry, not a payout, not a
  ledger transfer. Every one of the invariants C-1 to C-7 is enforced by the
  database, and a hand-edit that gets past one of them got past it because
  you disabled the thing that was protecting you. Reversals **insert**; they
  never update.
- **A refusal is information.** This system is built to refuse rather than
  to guess, so an error naming a constraint is telling you which rule you
  are about to break. Read it before you work around it.

## The zero-sum check is failing (C-1)

```sh
make cashback-verify-ledger
```

C-1 is the one invariant that lives outside our own schema (ADR-0002), which
is exactly why it is checked continuously rather than trusted. That target
proves two different things, and they fail for different reasons. Find out
which one is red before doing anything else.

**Do not** adjust the ledger to match the wallet, or the wallet to match the
ledger. You do not yet know which is right.

### Every currency sums to zero — and one does not

```sql
select currency, net_minor from cashback.ledger_zero_sum where net_minor <> 0;
```

The ledger's own postings do not net. Nothing in the application can cause
this: a transfer's postings are written together, inside one transaction,
by the ledger driver, and the ledger refuses an unbalanced one. So a row
here is the substrate disagreeing with itself, not a bug in the crediting
path — an ADR-0002 exit-route conversation, not a runbook step. Escalate,
and do not post anything else until somebody has looked.

### A wallet does not match the ledger

This is the common one, and it has a known cause worth learning.

The money moves **before** the state is recorded. `earnings/statemachine.go`
posts the transfer first and records the transition and its link second,
and it says so in two error sentinels you will find in the log:

- `ErrNotRecorded` — *"the transfer posted but the transition could not be
  recorded"*;
- `ErrEntryMoved` — the entry left the state its caller had read, so the
  code deliberately returns **having posted and not recorded**, on the
  grounds that recording a transition the entry did not make would be worse.

So the divergence is always the same shape: **money in the ledger with no
transition behind it.** The opposite — a transition with no posting — is
close to unrepresentable, because the transition, its link and the entry's
new state are one commit or none.

Look for the posting, not for the transition:

```sql
select t.ref, t.posted_at, t.idempotency_key
  from ledger.transfer t
  left join cashback.ledger_link ll on ll.ledger_transfer_ref = t.ref
 where ll.ledger_transfer_ref is null
 order by t.posted_at desc
 limit 20;
```

Read the result with the schema in mind: `ledger_link` records **entry**
transitions only, so a transfer that belongs to something else — the
house-account seeding a balance, anything posted by hand — appears here
legitimately. What you are looking for is a transfer whose idempotency key
names an entry, with no link beside it and a `ErrNotRecorded` in the log at
the same moment.

The fix is to **replay**, never to edit: the transfer key is derived from
the move, so re-running it re-posts to the same transfer and the second
attempt is refused rather than duplicating the money.

## The catalogue emptied

Members see no retailers; `merchant_network.status` is `left_network` across
the board.

This is the failure contract rule 8 exists to prevent: **an import reads
absence as departure.** A retailer missing from the catalogue answer is
reconciled to `left_network`, its offers stop being published, and members
see an emptied catalogue. So a truncated answer — a network that returned
two pages of five and then hung up — looks exactly like every retailer
leaving at once.

An adapter must yield an error when it stops early, and `MarkRoutesNotSeen`
must only run after an iteration that ended with no error at all. If the
catalogue emptied, one of those two failed.

```sql
-- What changed, and when. All at one timestamp is the tell.
select status, count(*), min(retrieved_at), max(retrieved_at)
  from cashback.merchant_network
 group by status;
```

If every departure carries one `retrieved_at`, it was one bad import, not
the network.

**There is nothing to restore from.** `merchant_network` keeps no history:
`MarkRoutesNotSeen` updates `status` in place and `UpsertRoute`'s
`ON CONFLICT` overwrites it, so the previous value is recorded nowhere. Do
not go looking for an audit table.

What puts it back is **a complete import**, and that is the good news:
`TestAReturningRetailerComesBack` is exactly this case, and a later import
that returns the retailer moves the route from `left_network` back to
`active` with no operator action at all. So the sequence is: find out why
the answer was short, fix that, and let the next import run. The danger is
not the next import — it is running one while the network is *still*
answering short, which re-confirms the departures. If you cannot fix the
cause quickly, stop the import job until you can.

## A payout is stuck

A request moves `awaiting_approval` → `approved` → `paid`, and the `payout`
beside it moves `submitted` → `settled`. Stuck almost always means the rail
did not confirm, not that we did not send.

```sql
select r.id, r.state as request_state, r.amount_minor, r.currency, r.requested_at,
       p.state as payout_state, p.approved_by, p.rail, p.rail_reference, p.submitted_at
  from cashback.withdrawal_request r
  left join cashback.payout p on p.request_id = r.id
 where r.state not in ('paid', 'rejected')
   and r.requested_at < now() - interval '24 hours'
 order by r.requested_at;
```

- `payout.approved_by` **is** the approval (C-4) — the row is the approval,
  and there is no separate flag to check. A request still at
  `awaiting_approval` with no payout beside it is not stuck; it is one
  nobody has approved, and the queue is where it belongs.
- A request at `approved` with a `submitted` payout and no `settled_at` is
  the real stuck case: we sent, the rail has not answered.
- **Never re-run a payout to unstick it.** Exactly-once is enforced by the
  database, so a second attempt is refused — and that refusal is the correct
  outcome, not an obstacle. Read it: it tells you the first one landed.
- If the rail genuinely lost it, the fix is a **reversal and a new
  withdrawal**, never an edit to the old row.

## The poller has stalled

The quiet one. Nothing errors; commissions simply stop arriving, and the
first person to notice is a member whose purchase never appeared.

A cursor that has not moved is the signal:

```sql
select na.external_publisher_id, n.id as network, n.active,
       na.cursor_at, na.trailing_cursor_at
  from cashback.network_account na
  join cashback.network n on n.id = na.network_id
 order by na.cursor_at;
```

`cursor_at` advances **only after a window is fully persisted** (FR-031), so
a cursor that has not moved means no window completed — not that nothing was
reported. Look for: the job not running at all (scheduler capacity, or a
lock held by a process that died), a credential the network now refuses, or
one window that fails every time and blocks the ones behind it.

**Never move a cursor forward by hand.** The span between where it was and
where you put it is never re-read, and every transaction in it stays pending
for ever. Migration `0023` refuses to move a backfill start for the same
reason.

`ErrNetworkRefused` is terminal and means a credential — retrying is an
infinite loop with a frozen cursor. `ErrNetworkUnavailable` and
`ErrNetworkRateLimited` are retryable and will clear on their own.

**One trap worth knowing.** `cashback.network` carries
`max_query_window_days` and `rate_limit_per_minute`, and **editing them
changes nothing today** — the adapters use compiled-in constants, and the
one read of the row discards it. If a network is complaining that we are
hammering it, lowering that column will not help; the limit is a release.
This is recorded as a defect in
`specs/004-multi-network-linkwise/research.md` §4.7.

## The unattributed queue is growing

A report we cannot tie to a click. Ordinary in small numbers — members clear
cookies, references expire — and a signal when it is a *proportion* rather
than a count.

```sql
select count(*) filter (where detected_at > now() - interval '24 hours') as today,
       count(*) filter (where resolved_at is null) as open,
       count(*) as total
  from cashback.unattributed_transaction;
```

Growing fast, on a network that was previously fine, usually means the click
reference stopped surviving the round trip: an offer's `deeplink_template`
was edited, or the network's `click_ref_param` is wrong. Both are
configuration, and both lose money silently on **every** click while they
last — the member clicks, buys, and nothing comes back.

A network that has *never* been fine is the case below: the attribution
canary refuses the forward sweep for it, so it does not depend on somebody
noticing a proportion.

Check one live offer end to end before you assume the network is at fault:

```sql
select o.id, o.deeplink_template, n.click_ref_param
  from cashback.offer o
  join cashback.merchant_network mn on mn.id = o.merchant_network_id
  join cashback.network n on n.id = mn.network_id
 where o.valid_from <= now()
   and coalesce(o.valid_to, 'infinity'::timestamptz) > now();
```

Attributing by hand from the queue is a legitimate operator action and the
queue exists for it. Attributing *in bulk* to clear a backlog is not: each
one is a decision about whose purchase it was, and C-2 means the entry rests
on that decision for ever.

## The attribution canary is refusing (#524)

```
job failed  job=network-poll:linkwise:CD20  error="networks: attribution has never succeeded at this network: 3 distinct transaction(s) dated after the first click (…) went unattributed and 0 were ever credited to a click; …"
```

Every forward sweep, for as long as it stands. **The poll itself succeeded**:
the reports are stored, the cursor moved, and the queue has them. What the
line says is that since the first member clicked through this network, not
one reported purchase has come back matched to a click, and enough have now
come back unmatched to rule out chance.

That has two known causes, and they look identical from here:

- the tracking URL carries our reference in a parameter the network does
  not read — every report arrives with **no** reference at all;
- the parameter is right but the network cuts the reference short — every
  report arrives with a reference that matches **no** click.

Tell them apart from the queue, which already has the evidence:

```sql
select nt.external_id, nt.click_ref, nt.transacted_at
  from cashback.unattributed_transaction u
  join cashback.network_transaction nt on nt.id = u.network_transaction_id
 where nt.network_id = 'linkwise'
 order by nt.transacted_at desc
 limit 10;
```

`click_ref` null on every row is the first cause: fix `click_ref_param`.
A value on every row that is a **prefix** of one of ours is the second:
compare it byte for byte against `cashback.click.click_ref`, and the length
at which it stops is the network's maximum — which is a code change to the
reference minting and a decision about collision risk, so raise it on #524
rather than shortening anything by hand.

```sql
update cashback.network set click_ref_param = 'subid1' where id = 'linkwise';
```

No restart. The next forward sweep re-counts, and the canary stays refusing
until the first purchase made *after* the fix comes back matched — which,
for a network with a validation delay, can be days. That is the canary
working, not the fix failing: the queued reports from before the fix will
never match, and they are what an operator attributes by hand from the
queue once the cause is known.

Once one credit to a click exists at the network the canary retires for
good, and later unattributed reports are the ordinary case above.

## A network is unreachable

Members can still click — the catalogue is ours, and a click-out only needs
the offer row and the adapter's deeplink builder. What stops is ingestion.

That is the designed behaviour and it is safe: cursors do not advance, so
nothing is skipped, and the windows are re-read when the network returns.
The backlog is bounded by the network's own maximum window, so a long outage
means several windows rather than one enormous one.

What to check, in order: is it us (credential, clock, egress) or them; is
the failure terminal (`ErrNetworkRefused` — a credential, and retries will
never clear it) or retryable; and is the outage longer than the backfill
horizon, in which case the span at the far end needs a deliberate re-read
rather than a cursor edit.

**Do not disable the network row to stop the noise.** `network.active` is
what lets members click through; switching it off takes the catalogue down
as well as the poller, and the poller was already failing safely.
