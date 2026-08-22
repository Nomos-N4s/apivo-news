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
- **The three domains on Cloudflare**, in the same account.
- **A GitHub personal access token** with `read:packages` only. This is the
  one credential the box needs and the only thing it uses it for: pulling
  images. It can read nothing else and write nothing.
- Your **SSH public key** on the Hetzner box, and the **port** sshd listens
  on. If that is not 22, you need it in step 4 or you will lock yourself out.

Supabase is **not** needed for this pass. The free tier is one project and
that project is production's; QA and Staging both run a Postgres container on
the box instead. See [ENVIRONMENTS.md](ENVIRONMENTS.md) for what that costs.

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

## 2. Cloudflare — the origin certificate

**SSL/TLS → Origin Server → Create Certificate.** Take the default (Cloudflare
generates the key), 15 years.

Put **all of these** in the hostname list:

```
ra1ze.com
*.ra1ze.com
reapie.com
```

`*.ra1ze.com` is what makes previews possible — every pull request gets
`pr-<n>.ra1ze.com` and they must all be covered by the one certificate on the
box. A certificate without it means previews serve a TLS error.

Save the two blocks Cloudflare shows you. **The key is shown once.**

You need one certificate covering all the names, not one per domain — the box
runs a single Caddy and presents a single origin certificate.

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
scp origin.pem origin.key root@<vps-ip>:/root/
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

## 5. The Hetzner box — the two database URLs

This is the first of the two ordering constraints: the api will not start
without it.

```sh
# QA
grep APIVO_PG_PASSWORD /etc/apivo/qa/stack.env
# put this in /etc/apivo/qa/api.env, with that password substituted in:
#   DATABASE_URL=postgres://apivo:<password>@postgres:5432/apivo?sslmode=require

# Staging — a DIFFERENT generated password
grep APIVO_PG_PASSWORD /etc/apivo/staging/stack.env
#   DATABASE_URL=postgres://apivo:<password>@postgres:5432/apivo?sslmode=require
```

`sslmode=require` is not decoration. Every environment here runs
`APP_ENV=prod`, and the api **refuses to start** on a `DATABASE_URL` whose
`sslmode` permits an unencrypted session — `disable`, `allow`, `prefer`, or
absent. The host is `postgres`, the compose service name, not `localhost`.

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

Once `https://reapie.com` actually answers, open a pull request setting:

```
APIVO_STAGING_URL=https://reapie.com
```

in [`deploy/hetzner/environments.env`](../deploy/hetzner/environments.env).

This is the second ordering constraint, and it runs the other way: that value
is empty *on purpose* until the host serves. An empty URL is the guard in
`release.yml` that refuses to publish images and move the `:staging` tag
toward an environment that is not there. Filling it in early does not make
staging work sooner — it only deletes the guard, so an `-rc` tag would move
the channel and then fail its probe with nothing to roll back to.

`APIVO_QA_URL` and `APIVO_PREVIEW_DOMAIN` are already set, because neither
gates anything.

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
4. Nothing for GHCR, and nothing to create as a secret: the automatic
   `GITHUB_TOKEN` publishes and deletes image tags.

One consequence of this repository being **public**: pull requests from forks
get a read-only `GITHUB_TOKEN` regardless of what a workflow asks for, so
they cannot publish preview images. `preview.yml` skips both its jobs for
fork pull requests rather than failing them. A maintainer who wants a preview
of an outside contribution pushes the branch to this repository.

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
