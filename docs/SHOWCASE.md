# Running epiloYES from nothing, in front of an audience

A runbook for building the paper live: empty database to published article to audit
trail, in about fifteen minutes of talking. It doubles as the from-scratch setup
guide, because a demo that needs a special path is a demo that proves nothing.

Everything below runs against the local stack. Nothing here touches the Supabase
database.

---

## Before the camera starts

**1. Demo pacing.** The poll and translation loops default to fifteen minutes,
which is right in production and fatal on camera. In `.env`:

```
POLL_INTERVAL=1m
TRANSLATION_INTERVAL=1m
```

That is the only change, and it is configuration rather than a special demo mode:
the same binary, the same code paths, a shorter clock. Set it back afterwards.

**2. Reset to nothing.**

```bash
docker compose -p apivo-news down
docker volume rm apivo-news_pgdata
docker compose -p apivo-news up -d --build
```

The api migrates the empty database on boot. Watch it happen:

```bash
docker compose -p apivo-news logs -f api
```

**3. Provision yourself.** The account row is what turns a signed-in person into an
approver — the database refuses approvals from anyone without it.

```bash
docker exec -i apivo-news-postgres-1 psql -U apivo -d apivo -c \
  "insert into account (id, email, display_name, role) values \
   ('<your Supabase user id>', '<your email>', '<Your Name>', 'editor');"
```

Co-founders get the same statement with their own Supabase user ids, once each of
them exists in Authentication → Users. (Issue #127 replaces this with
`apivo provision-editor`.)

**4. Check the front page is honestly empty**: <http://127.0.0.1:4321/el/munich+greece>
should say nothing is published yet, for both places. That empty state is the
opening shot.

---

## The demo

### Beat 1 — sign in (30 seconds)

<http://127.0.0.1:4321/el/editor/signin>

The sign-in page states the rule before anyone types: approval records your name
permanently beside the article. Sign in.

### Beat 2 — register the licensed sources (3 minutes)

On the sources screen, add these four. The licence terms field is the point of the
beat: whatever you type is snapshotted onto every item this feed ever yields, and
the audit shows that text forever — so it says what is actually true today, which
is that no publisher has granted anything yet.

| Name | URL | Lang | Jurisdiction |
|---|---|---|---|
| ERT News | `https://www.ertnews.gr/feed/` | el | GR |
| in.gr | `https://www.in.gr/feed/` | el | GR |
| Abendzeitung München | `https://www.abendzeitung-muenchen.de/storage/rss/rss/muenchen-news-abendzeitung.xml` | de | DE |
| tz München | `https://www.tz.de/muenchen/rssfeed.rdf` | de | DE |

Licence terms, for all four — adjust the publisher name:

> No reuse licence obtained. Registered for internal demonstration only: bounded
> extract with attribution and linkback, pending a licensing conversation with
> \<publisher\>.

Abendzeitung is the honest exception worth mentioning aloud: its RSS page does
publish conditions (free for private, non-commercial use; commercial use by prior
permission), so its terms text can say that instead.

### Beat 3 — the pipeline runs itself (2 minutes)

Nothing to click. Within a minute the poller fetches all four feeds, and the log
tells the story better than any slide:

```bash
docker compose -p apivo-news logs -f api
```

`retrieved` lines, then `translated` lines carrying `cost_microusd` — roughly
$0.0001 an article. Say the number out loud; then say that the ledger the database
maintains would halt the month at $25 without anyone watching.

### Beat 4 — approve (4 minutes)

The review queue now holds real Greek and Munich news. Open one item and show what
the editor sees *before* deciding: the original title, the source's own words, the
retrieval time, the licence snapshot, the content hash, and — for translated rows —
the model, the prompt version and what that translation cost.

Approve one. Two things to narrate:

- **Places**: an article must name at least one place, because an article no reader
  can reach is not published, it is lost. The database refuses one without.
- **Attribution**: the block is frozen at approval, permanently. Nobody can edit it
  afterwards, including you, including me.

### Beat 5 — the reader sees a newspaper (2 minutes)

<http://127.0.0.1:4321/el/munich+greece> — the article is there, in Greek, for the
places it was approved for. Switch the language axis; switch places. Two axes,
never crossed: a Greek speaker in Munich reads Munich news in Greek.

### Beat 6 — the part nobody else has (3 minutes)

Editor → audit → paste the article id. In one query, under a second: the source,
the licence terms as they stood *at retrieval* (not as they stand now), the
content hash, the model, the prompt version, the cost, the named human who
approved it, when, and every lifecycle event in order.

This is the promise the whole system is built around: every published sentence
traceable in under five minutes. Measured in CI on every build, it answers in
about twelve milliseconds.

### Beat 7 — withdrawal, if there is time (1 minute)

Withdraw the article with a reason. It leaves the reader site immediately and stays
in the audit forever, with the reason recorded. Nothing is ever deleted; the record
of what we published, and why we stopped, survives.

---

## Afterwards

- Restore `POLL_INTERVAL` and `TRANSLATION_INTERVAL` to `15m`.
- Rotate the Workers AI token if it has been on screen.
- For co-founders testing the deployed instance: each needs a Supabase Auth user
  and an account row with `role = 'editor'`, and every approval they make carries
  their name — which is the point.

## If something goes wrong on camera

- **Queue empty after a minute**: check the api log for `polling source failed`.
  A feed that answers 403 to a datacentre IP is the usual cause; the source row
  records the error and the other three keep working.
- **A screen shows a 503 band**: the api is not reachable from the web container;
  `docker compose -p apivo-news ps` and restart the api.
- **Sign-in bounces back**: the account row is missing or its id does not match the
  Supabase user id. That is the one failure the screens cannot fix themselves.
