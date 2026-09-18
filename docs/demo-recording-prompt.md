# Master prompt: record the epiloYES demo

Paste everything below the line into Claude Cowork (or any agent with screen
control). It assumes the local stack is already running — if it is not, the
prompt says how to start it.

---

You are helping record a product demo video. The software is **epiloYES**, a
multilingual local newspaper for Greek communities abroad, running locally on
this machine. The audience is three co-founders who have never seen it work.
Your job is to drive the browser and narrate; the founder will capture the
screen.

## Before you start

The stack should already be up. Verify:

```
curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:4321/el/munich+greece
```

`200` means ready. If it is not, run `docker compose -p apivo-news up -d` from
`C:\Users\madym\repos\apivo-news` and wait for three healthy containers.

Use **`127.0.0.1`, never `localhost`** — a stale process may hold the IPv6
address.

Open the browser at a clean, readable window size (1440×900 or larger), zoom
at 100%, and close unrelated tabs. Do not show the terminal except where a beat
below calls for it.

## What this software is, in one paragraph

News feeds are polled; each retrieved item is stored with the publisher's
licence terms **snapshotted at the moment of retrieval**; a machine translation
is made with its cost recorded; a **named human editor** approves it; only then
is it published. Every published article can be traced back to its source,
licence, model, prompt and approver in one query. The database — not the
application — enforces those rules.

## The recording, seven beats, about fifteen minutes

Narrate what you are doing and why it matters. Never claim something the screen
does not show.

### 1. The empty paper (30s)

Open <http://127.0.0.1:4321/el/munich+greece>.

It says nothing has been published yet, for both places. Say plainly: this is a
real empty database, not a mock — everything that follows will be built live.

### 2. Sign in (30s)

Open <http://127.0.0.1:4321/el/editor/signin>.

Read the line on the screen aloud: approval records your name permanently
beside the article. **The founder types their own credentials — do not ask for
them and do not type them yourself.** Wait for them.

### 3. Register the licensed sources (3 min)

Go to the sources screen. Add these four, one at a time:

| Name | URL | Lang | Jurisdiction |
|---|---|---|---|
| ERT News | `https://www.ertnews.gr/feed/` | el | GR |
| in.gr | `https://www.in.gr/feed/` | el | GR |
| Abendzeitung München | `https://www.abendzeitung-muenchen.de/storage/rss/rss/muenchen-news-abendzeitung.xml` | de | DE |
| tz München | `https://www.tz.de/muenchen/rssfeed.rdf` | de | DE |

For licence terms use, adjusting the publisher name:

> No reuse licence obtained. Registered for internal demonstration only:
> bounded extract with attribution and linkback, pending a licensing
> conversation with \<publisher\>.

**This is a beat, not paperwork.** Say why the text matters: it is snapshotted
onto every item this feed ever yields, and the audit will show it for good. The
system is designed so the record cannot flatter you.

### 4. The pipeline runs itself (2 min)

Nothing to click. Show the log:

```
docker compose -p apivo-news logs -f api
```

Within a minute: `retrieved` lines, then `translated` lines carrying
`cost_microusd`. Point at the cost — roughly **$0.0001 per article** — and say
that the database keeps a running ledger and halts the month at $25 without
anyone watching.

Return to the browser once items are flowing.

### 5. Approve (4 min)

Open the review queue. Before approving anything, show what the editor sees:
the original title, the source's own words, when it was retrieved, the licence
snapshot, the content hash, and — on translated rows — the model, the prompt
version and what that translation cost.

Approve one item. Narrate two things:

- **Places.** An article must name at least one place, because an article no
  reader can reach is not published, it is lost. The database refuses one
  without.
- **Attribution.** It freezes at approval, permanently. Nobody can edit it
  afterwards — not the editor, not the developer, not the database owner.

**Approve both a Greek original and its German translation of different
stories**, so beat 6 can switch languages and show both.

### 6. The reader sees a newspaper (2 min)

Open <http://127.0.0.1:4321/el/munich+greece> — the approved Greek article is
there. Then <http://127.0.0.1:4321/de/munich+greece> for the German one.

Say what this shows: language and place are **independent axes, never crossed**.
A Greek speaker in Munich reads Munich news in Greek. That is the product.

### 7. The part nobody else has (3 min)

Go to the editor's audit screen and paste an article id.

In one query, under a second: the source, the licence terms **as they stood at
retrieval** — not as they stand now — the content hash, the model, the prompt
version, the cost, the named human who approved it, when, and every lifecycle
event in order.

Close on this: every published sentence is traceable in under five minutes.
It is measured on every build, and it answers in about twelve milliseconds.

### Optional closer — withdrawal (1 min)

Withdraw the article with a reason. It leaves the reader site at once and stays
in the audit forever, reason recorded. Nothing is ever deleted: the record of
what was published, and why it stopped, survives.

## Rules while recording

- **Never type or ask for passwords, tokens or connection strings.** If a
  credential is needed, hand control back to the founder.
- **Do not show** the terminal's environment, `.env`, or any dashboard with
  keys visible. Beat 4's log is fine — it prints no secrets.
- If something fails on camera, **say so and keep going**. The API log names
  its own failures clearly, and a feed that answers 403 to a datacentre IP is a
  normal thing that the other three sources survive. A demo that admits a
  failure is more convincing than one that cuts away.
- Do not speed up or fake any step. If the poller takes ninety seconds, that is
  ninety seconds of a real system doing real work — fill it with beat 4's
  explanation.

## If a beat breaks

- **Queue still empty after a minute**: check the api log for
  `polling source failed`. A blocked feed records its error and the others keep
  working; carry on with whatever arrived.
- **A screen shows a 503 band**: the api is unreachable from the web container.
  `docker compose -p apivo-news ps`, restart the api, reload.
- **Sign-in bounces back**: the editor account row is missing. Stop and tell the
  founder — it is the one failure the screens cannot fix themselves.
