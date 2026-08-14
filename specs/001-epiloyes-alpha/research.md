# Research & Decisions: epiloYES Alpha

**Feature**: [spec.md](spec.md) | **Date**: 2026-08-14

Each decision: what was chosen, why, and what was rejected. Constitution
constraints (stack, boundaries, invariants) are given and not revisited.

## D1 — Feed parsing: `github.com/mmcdole/gofeed`

- **Decision**: gofeed for RSS/Atom parsing, normalised into the
  ingestion module's own item type at the boundary.
- **Rationale**: mature, handles the RSS/Atom/RDF dialect zoo and
  encoding quirks that real feeds exhibit; one dependency versus a
  hand-rolled parser that would accrete edge cases forever.
- **Rejected**: hand-rolled `encoding/xml` (dialect minefield, no safety
  gain); external ingestion services (violates the monolith decision and
  moves legal evidence outside our transaction boundary).

## D2 — Poll scheduling: in-binary ticker with jitter

- **Decision**: a goroutine per active source inside the monolith,
  `time.Ticker` with per-source jitter, interval from config
  (default 15 min), conditional GET (ETag/Last-Modified) when the feed
  supports it.
- **Rationale**: alpha scale is a handful of feeds; the simplest thing
  that works inside a single binary, trivially testable.
- **Rejected**: external cron + CLI runs (splits process lifecycle),
  queue/job libraries (scale we do not have; new dependency).

## D3 — HTTP routing: standard library `net/http`

- **Decision**: stdlib `ServeMux` with method+path patterns (already in
  use for health endpoints), handlers per module wired in `cmd`.
- **Rationale**: Go ≥1.22 patterns cover the whole contract; zero
  dependencies; boundary-friendly.
- **Rejected**: chi/echo/gin — no capability the contract needs.

## D4 — Supabase JWT validation: `lestrrat-go/jwx/v3`

- **Decision**: validate RS256/ES256 tokens against the project's JWKS
  endpoint with cached, auto-refreshing keys; map `sub` to `account.id`;
  the identity module exposes only `Authenticate(ctx, token) (Identity, error)`.
- **Rationale**: Supabase signs with asymmetric keys and rotates;
  jwx handles JWKS caching/rotation correctly out of the box.
- **Rejected**: `golang-jwt` + hand-rolled JWKS fetch (re-implementing
  rotation), shared-secret HS256 validation (legacy Supabase mode; ties
  us to a symmetric secret).

## D5 — Translation adapter and cost control

- **Decision**: `translation.Translator` interface owned by the
  translation module: `Translate(ctx, req) (Result, error)` where
  `Result` carries headline, extract, model id, prompt version and cost —
  persisted as `translation.headline`, `translation.extract` and the
  lineage/cost columns in the same transaction.
  Providers are adapters in `internal/translation/providers/<name>`,
  selected by config; swapping providers touches one package (well under
  the five-engineer-day bound). Cost control: per-article ceiling and
  monthly cap from config; the module consults the `translation_spend`
  ledger inside the same transaction that records the translation, halts
  the pipeline with a `pipeline.halted` domain event at the cap, and
  skips+flags items whose estimate exceeds the per-article ceiling.
- **Provider shortlist**: see the priced comparison below — founder
  picks at plan review; no provider gets wired before that.
- **Rejected**: hardcoding one provider (violates swappability), spend
  tracking in memory (lost on restart; the ledger is the record).

### Provider shortlist (researched 2026-08-14, official pricing pages)

Volume model: one item ≈ 150 words in / 120 words out; 3,000 items/month,
each into one target language (≈ 630k input + 504k output tokens, or
≈ 2.85M source characters for DeepL).

| Provider | Model/tier | Unit price | Est./month (3k items) | EU/GDPR notes |
|---|---|---|---|---|
| Anthropic | Claude Haiku 4.5 | $1.00/M in, $5.00/M out | ~$3.15 | No EU-processing option on the first-party API (global or US-pinned only); EU-resident inference requires Claude via Vertex AI EU regions |
| Anthropic | Claude Sonnet 5 | $2.00/M in, $10.00/M out | ~$6.30 | Same as above |
| OpenAI | gpt-5.6-luna (small/fast) | $0.20/M in, $1.20/M out | ~$0.73 | EU data residency available (`eu.api.openai.com`); must be selected at Project creation; zero data retention in-region |
| OpenAI | gpt-5.6-terra (quality) | $2.00/M in, $12.00/M out | ~$7.31 | Same as above |
| DeepL | Growth plan | €23.80/mo + €22.00/M chars beyond the included 12M chars/yr | ~€64 steady-state | EU (German) company, GDPR-native; dedicated MT engine, glossaries; German is its home language, Greek fully supported |

Sources: platform.claude.com/docs/en/about-claude/pricing,
developers.openai.com/api/docs/pricing (+ OpenAI data-residency help
pages), deepl.com/en/pro (Growth card). Anthropic/OpenAI batch APIs halve
token prices at ~24 h latency — unsuitable for news headlines.

Caveats that matter for the decision:

- Greek tokenizes heavier than English on some tokenizers; treat LLM
  figures as ±30–50%. Even so, LLMs are an order of magnitude cheaper
  than DeepL at this volume.
- No provider publishes EL↔DE quality benchmarks. Whichever is chosen, a
  small pilot evaluation on real feed items precedes committing (the
  adapter interface makes the pilot cheap).
- If EU-only processing of article text is a hard requirement, the
  first-party Anthropic API is out (Vertex-EU route or drop); OpenAI
  requires a fresh EU-pinned project; DeepL is EU-native. Note the
  content being translated is published news text, not reader personal
  data — reader data never goes to a translation provider under this
  design.
- Legacy cheap DeepL API tiers (free 500k/mo, pay-as-you-go Pro) no
  longer appear on official pages and are reportedly closed to new
  customers; only Growth is assumed available.

**Cap values (founder-approved 2026-08-14)**: per-article ceiling $0.02,
monthly cap $25. Both are configuration, not schema; hitting the monthly
cap halts the pipeline (FR-006).

### Founder direction (2026-08-14): OpenAI-compatible budget inference

The founder directed the shortlist toward cheap OpenAI-API-compatible
hosts (Groq-class) or self-hosting rather than premium first-party APIs.
Consequences:

- The first adapter is a single **OpenAI-compatible** implementation
  with configurable base URL, model and key — it covers Groq, Together,
  DeepInfra, a self-hosted vLLM server and OpenAI itself, switchable by
  config alone.
- Self-hosting proper is uneconomical at alpha volume (any capable GPU
  host exceeds every API option's monthly cost); managed budget
  inference is the operative form of the idea.
- The production model is picked by the founder from the pilot
  evaluation report (T016) — Greek quality is the binding constraint,
  not price.

Verified budget options (official pages, 2026-08-14; monthly = 0.63M in
+ 0.504M out tokens):

| Host | Model | $/1M in / out | Est./month | Notes |
|---|---|---|---|---|
| DeepInfra | gpt-oss-120b | 0.037 / 0.17 | **$0.11** | GDPR "in progress" on their trust page; no processing-location statement |
| DeepInfra | Llama 3.3 70B Turbo | 0.10 / 0.32 | **$0.22** | — |
| Together | Qwen3.5 9B | 0.17 / 0.25 | **$0.23** | no EU-processing statement found |
| Groq | gpt-oss-120b | 0.15 / 0.60 | **$0.40** | Helsinki EU DC exists, but standard API not guaranteed EU-routed — written confirmation needed if it matters |
| Groq | llama-3.3-70b-versatile | 0.59 / 0.79 | **$0.77** | free rate-limited tier available |

Greek-quality caveats that make the pilot mandatory: Llama 3.3 officially
supports German but **not Greek**; gpt-oss models are English-dominant
with no Greek claim; Qwen/Gemma lines claim the broadest language
coverage among budget options. Translated public news text — never
reader personal data — is what leaves the system, which bounds the GDPR
exposure of a non-EU host; the founder decides whether that bound
suffices or an EU-routed option is required.

Endpoints (all OpenAI-compatible): `api.groq.com/openai/v1`,
`api.together.ai/v1`, `api.deepinfra.com/v1/openai`. Prices are
2026-08-14 snapshots; these hosts reprice frequently — recheck at T016.

## D6 — Indexing block: one middleware, portable

- **Decision**: a single Astro server middleware is the enforcement
  point, and it **denies, not just advises**: (a) requests whose
  User-Agent matches the maintained crawler/AI-training/archive signature
  list receive `403` — an actual block, not a directive; (b) `robots.txt`
  (disallow all) and `X-Robots-Tag: noindex, nofollow` on every response
  cover the compliant-crawler and cached-copy cases. One deny list, one
  place, shipped inside the frontend container so the block holds
  identically on Cloudflare and Kubernetes. Cloudflare bot-management
  rules add a second, heuristic fence against UA-spoofing crawlers, but
  only as additive config — never the sole enforcement.
- **Limit named openly**: a crawler that fully impersonates a browser
  defeats origin-level UA blocking everywhere; only heuristic bot
  management (CF-level) catches some of those. The founder decision at
  plan review covers this residual risk alongside the placement.
- **Rationale**: the founder decision says one place, never per-route;
  the constitution says nothing platform-specific in application code. A
  CF-only rule would silently vanish on the Kubernetes path — the
  middleware travels with the artefact.
- **Trade named openly**: this is origin-level, not literally CDN-edge;
  crawlers that ignore robots and headers are only fully stopped by
  CF-level managed rules, which we add as config where available. This
  placement is an explicit founder-decision item at the plan review.
- **API surface**: the Go API is not publicly routable in any deployment
  (internal network only; the Astro server is the sole public surface),
  and it stamps `X-Robots-Tag: noindex, nofollow` on every response as
  defence in depth — already implemented in the platform HTTP server.

## D7 — Editorial UI: Astro pages, same frontend

- **Decision**: `/editor` routes in the existing Astro app, session via
  Supabase Auth JS SDK, calls to the Go API with the user's JWT. Server
  islands only; no SPA framework.
- **Rationale**: one build path (constitution), smallest surface that
  satisfies "review queue, approve, withdraw".
- **Rejected**: separate admin app (second artefact to secure and
  deploy), building editorial into the Go binary as HTML (splits the
  frontend stack decision).

## D8 — TypeScript types from schema

- **Decision**: `supabase gen types typescript --db-url` against the
  migrated local/CI Postgres, committed at `web/src/lib/database.types.ts`,
  with a CI drift job mirroring the sqlc one.
- **Rationale**: same single-source-of-truth guarantee on both sides, as
  the constitution requires.
- **Rejected**: hand-written TS interfaces (constitutionally banned),
  generating at build time only (drift invisible in review).

## D9 — Extract derivation rule

- **Decision**: prefer the feed's own summary; when absent, take the
  first sentences of the retrieved text up to 300 characters, cut at a
  sentence boundary, always suffixed by the source link. The rule lives
  in one deterministic function with table-driven tests; editors see the
  extract before approval. It feeds two consumers: translation input, and
  the read-time extract for untranslated (same-language) articles, whose
  headline is `source_item.original_title`.
- **Rationale**: extract-and-link must stay defensibly "extract", and
  a human signs off every item regardless (I-1).
- **Rejected**: LLM-generated summaries of full text in the alpha
  (higher cost and a subtler licensing posture than quoting a bounded
  extract; revisit only with founder sign-off).

## D10 — Withdrawal mechanics

- **Decision**: per the founder clarification — `withdrawn_at/by/reason`
  columns on `article` (all-or-none CHECK), reader queries filter
  withdrawn out, `article.withdrawn` domain event in the same
  transaction, nothing deleted. Editorial endpoint per the contract.
- **Rejected**: setting `published_at` back to NULL (erases publication
  history), row deletion (destroys the approval record; unthinkable
  given I-1/I-5).
