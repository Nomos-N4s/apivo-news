# Cashback member screens — master prompt for finishing them

> **In scope for `apivo-news`**, unlike the two files beside it. This is
> the briefing an engineer needs to finish the member-facing cashback
> screens in `web/` against the backend as it actually stands, and to take
> the product to its first real transaction: one person, one purchase at a
> Linkwise retailer, money in a wallet. Every fact below was verified on QA
> on 2026-09-07 and every gap is a gap on that date. QA redeploys on every
> merge to `main`, so re-verify anything more than a few merges old before
> building on it.

## How to use this file

Paste everything from **"The prompt"** to the end into a fresh session in
this repository, or point the session at this path. The sections before
it are the facts the prompt rests on; an engineer who wants to argue with
the prompt argues with those. Keep the file current: a screen that lands
is a line that changes here, in the same pull request.

## Where the backend is

Each of these was exercised on QA, `https://ra1ze.com`, with real
containers and a real Supabase token:

- **Cashback is on in QA.** The compose overlay runs the Blnk ledger and
  Redis beside the api; the brand definition is mounted and loaded; the
  fixture network is connected; its catalogue is imported; one rate band
  is published — 15% on the fixture retailer `FIXM-77`, slug
  `fixture-outdoor-co`, offer `0be7b5b7-eccd-4507-ab69-8a44d0a404e8`.
- **The whole member flow works over the API.** A member created in the
  nonprod Supabase project authenticated, opted in under terms `0.1.0`,
  clicked out and received a click reference and a redirect URL, and read
  a wallet at zero against the €20 payout threshold. The web has never
  been through that flow, because the web cannot sign a member in.
- **The fixture network never reports a sale**, so no wallet on QA will
  move until QA polls Linkwise. The Linkwise adapter is built, recorded
  against the real API and conformant (`specs/004-multi-network-linkwise`,
  T242–T248). The attribution canary (#524) refuses the sweep after three
  reports that matched no click while none ever has — the alarm for a
  click reference that did not survive the round trip.
- **Operators have their commands.** `apivo connect-network`,
  `apivo import-catalogue` and `apivo publish-offer` are subcommands of
  the deployed binary; the sequence for a host is
  [docs/RUNBOOK.md](../RUNBOOK.md), "Switching cashback on — QA first".
- **Locally, the whole stack runs without Docker.** `make cashback-demo`
  starts Postgres-backed api and web with a JWKS stand-in that mints
  member and operator tokens, seeds the fixture catalogue and walks the
  member flow with curl. It is the end-to-end harness the screens should
  be developed and tested against.

## The contract

**Where it lives.** `specs/002-apivo-cashback-alpha/contracts/http-api.md`
states intent; `api/openapi.json` is validated in CI and served by the
binary; `web/src/lib/cashback/types.ts` is hand-written against the first
and checked by nothing. A change to one is a change to all three, in one
pull request.

**Reaching it.** Server-side the web calls `API_BASE_URL` + `/api/v1/…`,
and compose sets `API_BASE_URL` to the api container of the same
environment. From the browser the same origin serves both: the edge
routes `/api/*`, `/healthz` and `/readyz` to the api and everything else
to the web. On pull-request previews and wherever `API_BASE_URL` is
unset outside production, the client answers from
`web/src/lib/cashback/fixtures.ts` and every page says so.

**Authentication.** A bearer token from the nonprod Supabase project,
verified against `JWKS_URL`; its subject must have a row in `account`,
or the answer is 401 (`ErrUnknownAccount`). **There is no anonymous
cashback surface** (FR-023): every route below answers 401 without a
token, the catalogue included. Operator routes additionally require
`account.role = 'operator'`. Errors are `application/problem+json`.

**Member routes**, under `/api/v1/cashback`:

| Route | What it does | State |
|---|---|---|
| `GET /participation` | the caller's opt-in; 404 when they never opted in | live |
| `POST /participation` `{terms_version}` | opt in; the version must be the brand's current one; 503 with no brand | live |
| `DELETE /participation` | leave | live |
| `GET /catalogue?lang&place…&q&limit&cursor` | retailers for a language and places, rates in the merchant-page shape | **not implemented — B2, #545** |
| `GET /merchants/{slug}?lang` | one retailer, every band published for them | live |
| `POST /clickouts` `{offer_id}` | `{click_ref, redirect_url, expires_at}`; the band is snapshotted onto the click (FR-013) | live; does not yet refuse a member who has not opted in — **B3**, T254 |
| `GET /wallet` | pending, confirmed, reserved, paid out, threshold | live |
| `GET /wallet/entries?state&limit&cursor` | the statement, reversals with reasons | live |
| `GET` · `POST /payout-destinations` `{kind, details}` | 201 with `verified_at: null`; verification is a separate flow that **does not exist — B6, #548** | live / gap |
| `POST /withdrawals` · `GET /withdrawals` · `GET /withdrawals/{id}` | ask to be paid; 409 with `threshold`/`shortfall` below €20 or on an unverified destination | live |
| `GET /export` | the caller's own history, JSON or CSV | live |

**Operator routes**, under `/api/v1/cashback/ops`: `held` (list,
`{id}/release`, `{id}/reject`), `unattributed` (list, `{id}/dismiss`),
`withdrawals` (`{id}/approve`, `{id}/reject`, `{id}/settle`),
`reconciliation/runs` (create, `{id}/differences`,
`differences/{id}/resolve`) and `exports/ledger` ·
`exports/reconciliation`. All live.

**Accounts.** `/api/v1/account/` today serves only `GET tours`. Nothing
creates an `account` row for a new Supabase user: editors are seeded by
`apivo-seed-editors`, and the one QA member was made by hand with the
service-role key and an `insert`. That is **B1 (#544)**, and until it lands
every QA member is made that way.

## What the web has today

Surveyed on 2026-09-07. Paths under `web/src`.

**Authentication mechanics, built for editors and reusable.**
`@supabase/ssr` with the SDK's own httpOnly cookies
(`lib/editorial/supabase.ts`, `lib/editorial/session.ts`);
`middleware.ts` resolves the session on editor, cashback, ops and
`/api/cashback` paths and pages read it back with
`editorSession(Astro.request)`; the cashback client takes
`session.token`. The web reads the role from the token's
`app_metadata.role`; the api reads it from `account.role`. Two sources,
and a member exists only when both agree — a plain member has no
`app_metadata.role` and is `reader` in both.

**Wired to the api, conditional on a token only an editor or operator
can produce today:** the catalogue shelf with search
(`pages/[lang]/[place]/cashback/index.astro`, calling the endpoint that
does not exist yet); the retailer page with a click-out form posting to
`/api/cashback/clickout` (`[slug].astro`); the wallet with totals, a
filtered statement, per-entry detail and reversals (`wallet.astro`); the
three-step withdrawal with threshold and shortfall handling
(`withdraw.astro`); and all four operator queues under `pages/ops/`
behind `lib/cashback/ops-guard.ts`.

**Stubbed:** registration and consent (`pages/[lang]/register.astro`,
`lib/account/consent.ts` — the form fields are disabled and the consent
history is a fixture); every cashback surface on previews or without
`API_BASE_URL`.

**Named, not mocked:** member sign-in exists as a screen with every
control disabled. `pages/[lang]/signin.astro` (#531, merged the same
hour as this file) draws the two paths the design board draws — an email
link, and a token from the app shell exchanged for a session of our own
— and prints on its face that neither is wired. No password anywhere, by
design. `register.astro` takes the same posture for sign-up.

**Missing entirely:** anything working behind those two screens (the
front page's sign-in button is disabled and quotes that state; the only
live credential form is `pages/[lang]/editor/signin.astro`); an opt-in
screen (`api.optIn()` and its strings exist, nothing calls them; the
wallet's not-participating band links to the catalogue instead); auth
gating (no page redirects to a sign-in; unauthenticated calls surface as
503 or empty states); adding or verifying a payout destination (the
withdrawal only lists verified ones); sign-out for members; any
end-to-end test (Vitest only, no browser tests).

**Two breaks on the deployed host that are not the web's fault:** the
edge sends `/api/*` to the Go api, so the web's own
`/api/cashback/clickout` and `/api/tour/*` never reached the web on
Hetzner until **B4 (#546)** narrowed the edge matcher to `/api/v1/*`; and the web container is given no `BRAND_DIR`, so in
production mode the legal pages and anything rendering the brand refuse
rather than print a fixture company (**B5, #547**).

## The gaps, in order, and who owns them

**Backend and deployment** — the Go side of this repository, one issue
and one pull request each, none of them the frontend's to build:

- **B1 — self-registration (#544).** `POST /api/v1/account`: a verified token
  whose subject has no row creates one with role `reader`, from the
  token's `sub` and `email`; a second call answers the existing row.
  This is what a sign-up screen calls after Supabase has created the
  user, and what makes a member exist to the api. Contract to be written
  into `http-api.md` and `openapi.json` before the screen is built.
- **B2 — `GET /catalogue` (#545).** Documented, called by the web, absent.
- **B3 — FR-110 (spec 004, T254).** The click-out refuses a member who has not opted in
  Until it lands the screens must not offer a click to
  a member without a participation.
- **B4 — edge routing (#546): landed.** The edge now sends `/api/v1/*`,
  `/healthz` and `/readyz` to the api and everything else to the web, on
  environments and previews alike, and `validate.sh` proves both halves.
  The web's own `/api/cashback/clickout` and `/api/tour/*` reach the web.
  On a host provisioned before this, the Caddy snippets must be copied
  to `/opt/apivo/caddy/` and the edge reloaded; the runbook says how.
- **B5 — the web's brand (#547).** Mount `/etc/apivo/<env>/brand` into the web
  container as the overlay now does for the api, and set its `BRAND_DIR`.
- **B6 — payout destination verification (#548).** FR-051 says a destination is
  verified before it is paid to and the contract says verification is a
  separate flow; no endpoint performs it. Needed before the first
  withdrawal, not before the first transaction.

**Frontend**, in the order that shortens the path to a real transaction:

- **F1 — member sign-in, on the design's terms.** Bring
  `pages/[lang]/signin.astro` and `register.astro` to life without
  changing what they promise. The email path is a link sent by Supabase
  Auth (`signInWithOtp`; the nonprod project's own mailer is rate-limited
  but enough for QA) and never a password. The shell path is a token
  exchanged for a session of our own, and stays disabled with its copy
  until there is a shell to hand one over. A first link creates the
  Supabase user, so sign-up and sign-in are one flow; the callback lands
  in the same `@supabase/ssr` cookie session the editor sign-in uses and
  then calls B1, which is what makes the person a member. The front
  page's sign-in button comes alive in the same change, because the page
  quotes its state. Sign-out. No password means no reset.
- **F2 — gating.** An unauthenticated request to any `/{lang}/{place}/cashback/…`
  page redirects to sign-in with a return path; a 401 from the api does
  the same; `/ops` without the operator role is a 403 page, not a queue
  with its chrome. No page shows a 503 for a missing token.
- **F3 — opt-in.** `GET /participation` answering 404 renders the terms
  the member is accepting — the version and the document from the web's
  own brand, which is the same file the api reads — and
  `POST /participation` with that version. A 503 there means the
  deployment has no brand, and the page says so rather than retrying.
- **F4 — catalogue on the real endpoint**, once B2 lands, keeping the
  fixture for development. Language and place stay separate parameters
  (constitution VII); `name_is_fallback` renders "shown in German" rather
  than pretending.
- **F5 — click-out, end to end in a browser**: the retailer page's form,
  the web endpoint, the 303 to `redirect_url`, and the failure banner —
  reachable on QA now that B4 has landed, and refusing to render the
  button for a member without a participation until B3 exists.
- **F6 — wallet against real entries.** Already wired; verify with the
  first pending entry that the statement, the detail pane and the
  lifecycle read correctly, and that a reversal shows its reason.
- **F7 — payout destination.** A screen to add one
  (`POST /payout-destinations`), an "awaiting verification" state, and
  the withdrawal picking it up once B6 verifies it.
- **F8 — an end-to-end test of the member journey**, sign-up to
  click-out, running against `make cashback-demo` locally and in CI. The
  repository has no browser test today; Playwright with the pre-installed
  Chromium is the expected shape, and the demo's minted tokens are how
  the test signs in without a Supabase project.
- **F9 — the two languages and the brand lint.** Every new string in
  `el` and `de` (`READING_LANGUAGES`); the brand-literal lint's remaining
  budget entries are all under `web/` and belong to this work (T128).

## The finish line: one real transaction

**Preconditions.** B1, B2, B4 and F1–F5 merged. QA switched from the
fixture network to Linkwise: `NETWORKS=linkwise` and its four keys in
`/etc/apivo/qa/api.env`, `apivoctl deploy qa`, `apivo connect-network`,
a restart, `apivo import-catalogue`, and `apivo publish-offer` on a
retailer whose programme allows deeplinking — the runbook has the exact
sequence and the caution: QA polls a real publisher account every fifteen
minutes for as long as the keys are there, and the credentials are the
founder's to place and to remove.

**The test.** A person signs up on `https://ra1ze.com`, opts in, opens
the retailer, clicks out, lands on the retailer through `go.linkwi.se`,
and buys something cheap and returnable. Then the forward sweep, every
fifteen minutes, reads what Linkwise reports; the earnings matcher
attributes the report by its click reference; an entry appears in the
wallet as **pending**. That entry, citing that click, is the finish
line. How long Linkwise takes to report a sale is not recorded anywhere
in this repository (`specs/004-multi-network-linkwise/research.md`,
§5.2 lists what is unknown) — hours to a day is the expectation, and
the test is not failed until a day has passed.

**Reading the result.** `GET /wallet/entries` shows the entry;
`/ops/unattributed` shows a report that matched no click — the symptom of
a click reference that did not survive the redirect, which is what the
canary (#524) exists to catch; `docker logs apivo-qa-api` shows the
sweeps. Confirmation comes weeks later when the retailer validates the
sale, and a withdrawal needs €20 confirmed and a verified destination
(B6), so neither is part of this test.

## Rules of the house

- The constitution, `.specify/memory/constitution.md`, wins over
  everything, including this file. Every branch is `xcoder/<slug>`;
  every commit is signed and solely authored; no assistant or vendor is
  named anywhere; commit small; one issue per pull request; every
  intermediate commit builds and passes.
- Before pushing web changes: `cd web && npm test && npm run check`,
  then the brand-literal lint. Before pushing Go changes:
  `make vet && make test-unit`, then the three ref and author lints
  `CLAUDE.md` lists.
- No anonymous cashback surface. Language and place are separate.
  Money is minor units with a currency (`lib/cashback/money.ts`), never a
  float. Everything brand-shaped comes from the brand file, never a
  literal. Cashback routes carry `noindex` and are out of the sitemap.
- The contract changes in three places at once or not at all. Backend
  first or in the same pull request; never a screen built against an
  endpoint that is only planned.
- QA is the truth: a merge to `main` is live at `https://ra1ze.com`
  within two minutes. Previews (`pr-<n>.ra1ze.com`) run on fixtures and
  prove layout, not integration.

## The prompt

You are finishing the member-facing cashback screens of `apivo-news`,
an Astro frontend under `web/` over a Go api, so that one real person
can sign up, opt in, click out to a Linkwise retailer and see a pending
entry in their wallet. Read `docs/master-prompts/cashback-frontend.md`
first and take its facts as the state of the world; where you find the
code disagrees with it, the code is right and the file needs a line
changed in your pull request.

Work the frontend list in order — F1 sign-in on the design's terms, an
email link and never a password, F2 gating, F3
opt-in, F4 catalogue, F5 click-out, F6 wallet, F7 payout destination,
F8 the end-to-end test, F9 languages and the brand lint — and do not
build against a backend gap: B1, B2, B4 and B5 are the Go side's to
land, and the file says which screen waits on which. Develop and test
against `make cashback-demo`, which runs the whole stack locally and
mints member tokens; prove integration on QA, not on previews.

Every branch is `xcoder/<slug>`. Every commit is signed, solely
authored, small, and names its issue. No assistant or vendor is named in
a commit, a comment, a description or a document. Run `npm test` and
`npm run check` in `web/` and the brand-literal lint before every push.
Strings ship in `el` and `de`. Nothing brand-shaped is a literal. No
cashback page is reachable without a member, and no click is offered to
a member who has not opted in.

When a screen works against QA with a real member, say so in the pull
request with what you ran and what you saw, and update this file.
