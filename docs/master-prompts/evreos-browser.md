# Evreos — master prompt for the browser programme

> **Planning artifact only.** Nothing in this file is in scope for
> `apivo-news`: the constitution excludes the browser, mobile apps and the
> remaining ecosystem mini-apps from both current products, and this file
> does not change that. It is the complete planning input for a
> **dedicated repository**. It lives here because this repository is where
> founder-level decisions are recorded, and because everything Evreos will
> eventually need from the Apivo backend is a founder decision to be taken
> here — listed at the end of Block C as dependencies, never as
> assumptions.

## What Evreos is

Evreos is Apivo's own distribution channel: a featherweight,
privacy-first browser for Windows, macOS and Linux, written in Rust, that
carries the Apivo super-app as built-in apps — epiloYES (news) and
cashback first; coupons, fuel prices, grocery offers, price comparison
and TV/radio later. The consumer proposition must stand entirely on its
own — a fast browser that respects the machine and the person — and that
honesty is the strategy: a browser people keep because it is good is
worth more than any funnel we could rent, and it is the only kind that
survives being examined.

Two forces make it worth building now:

1. **Owned distribution.** Every Apivo surface today reaches members
   through channels somebody else prices — app stores, search, social. A
   default browser is the one client a member opens daily without being
   asked.
2. **Offline partners.** The first cohort is not organic: partner
   businesses (first, a German orthopaedic shop with thousands of
   patients and a partner-funded price-lock cashback programme) hand
   their customers a concrete reason to install. The browser is where
   those members then meet the rest of the ecosystem.

**Timeline honesty.** The partner's new shop opens on 1 October and
cannot wait for a browser. The pilot runs on the existing web surfaces;
Evreos absorbs the cashback surface when it ships. Evreos's job for the
pilot cohort is month-two retention, not day-one delivery — and its
backend prerequisite (offline partner campaigns, dependency D4 below) is
a decision this repository has not yet taken.

## How to use this file

1. Create the dedicated repository (suggested name: `Nomos-N4s/evreos`).
   Naming sits beside founder question Q9; before anything public carries
   the name, check the Evreos trademark and domain situation.
2. Bootstrap spec-kit in it the way this repository is set up
   (`.specify/` plus the speckit skills).
3. `/speckit-constitution` with **Block A**.
4. `/speckit-specify` with **Block B**.
5. `/speckit-clarify` — the open decisions at the end of Block B are the
   questions it should press on; they are answered by the founder, never
   silently resolved by a spec.
6. `/speckit-plan` with **Block C**.
7. `/speckit-tasks`, `/speckit-analyze`, then implement.

Each block is self-contained and can be pasted into the new repository
without this file around it.

---

## Block A — input for `/speckit-constitution`

Evreos is a consumer browser and super-app shell built by a solo founder.
Its constitution exists to make two things structural rather than
habitual: the performance discipline that is the product's identity, and
the trust posture that is its licence to carry money surfaces. Draft the
constitution from these principles:

1. **Sole authorship and signed commits (non-negotiable).** Every commit
   is authored solely by the founder. No AI attribution trailers, no
   mention of AI assistance in commit messages, PR descriptions, code
   comments or documentation. Conventional Commits referencing the issue;
   one PR per issue; nothing lands directly on `main`. Enforce with the
   same commit-msg hook and server-side commit-hygiene CI job the Apivo
   repository uses.

2. **Featherweight is law.** Hard budgets — download size, installed
   size, cold start, shell memory overhead, idle CPU, chrome input
   latency — live in one budget file in the repository and are enforced
   by CI gates that fail the build on regression. Budgets only move by
   founder decision, and the default direction is tighter. Every feature
   states its byte and millisecond cost in its PR; a feature that cannot
   justify its cost is not added. Preferring deletion is a merge
   argument.

3. **Rust core, no bundled engine.** The shell is stable Rust. Electron,
   CEF and any bundled Chromium are permanently rejected — they are the
   thing Evreos exists not to be. Rendering goes through an
   `Engine` interface defined by the shell (the consumer), with the
   system-webview implementation as default and a headless test
   implementation kept working from day one, so the seam is proved by a
   second implementation rather than asserted — leaving room for a pure
   Rust engine as an experimental third backend when one is
   daily-drivable.

4. **Browser first, super-app second.** Signed out, with every Apivo
   surface ignored, Evreos must be a genuinely good private browser —
   that is the default experience. Apivo surfaces are discoverable,
   opt-in and dismissible. Nothing is ever injected into a web page
   without an explicit user action for that occasion, and affiliate
   attribution is never attached silently or claimed for a purchase the
   member's click did not lead to — the failure that ended the Honey
   extension's trust is the canonical counter-example. A violation here
   is a release blocker, not a bug.

5. **All money is server-side.** The browser renders ledger-derived
   state and requests actions; it never computes a balance, never builds
   an affiliate deeplink, never holds money logic. The cashback
   invariants (double entry, evidence, approver-gated payouts,
   exactly-once) live behind the Apivo API and are not re-implemented,
   approximated or cached-as-truth in the client.

6. **Privacy by default, GDPR by construction.** Browsing works fully
   signed out. Telemetry and crash reporting are opt-in, aggregate and
   EU-hosted. No browsing history leaves the machine. No fingerprinting,
   no install-referrer tricks: partner attribution is a claim code the
   member deliberately scans or types.

7. **Language and place are independent axes.** UI strings live in
   catalogues keyed by BCP-47 primary language subtags (German, Greek and
   English at launch); place is never fused into a locale value. This is
   the same principle the Apivo constitution carries, for the same
   reason.

8. **Rebrandable shell.** No brand name, colour, endpoint or support
   address hardcoded outside one brand configuration; a fixture brand
   builds in CI. This keeps partner-branded distributions possible
   without promising them.

9. **Apps are content, not releases.** First-party apps ship as signed,
   versioned surfaces delivered server-side; a browser release is only
   ever for the shell and its engine integration. An app declares its
   capabilities in its signed manifest and can never widen them from
   inside; anything page-adjacent additionally requires the user's
   per-app grant.

10. **Accessibility is not optional.** The first real cohort is 40+.
    WCAG 2.1 AA on every shell surface, full keyboard operation, UI
    scaling to 200%, and correct international text input (German and
    Greek at minimum) are release criteria, not polish.

---

## Block B — input for `/speckit-specify`

Specify **Evreos v1**: a featherweight, privacy-first desktop browser
(Windows, macOS, Linux) that doubles as the shell for Apivo's super-app,
with two built-in apps at launch — epiloYES (news) and cashback.

### Why this product

- People with ordinary machines experience mainstream browsers as bloat:
  slow starts, gigabytes of memory, background churn. "The browser that
  respects your machine" is an honest, testable wedge.
- Apivo needs distribution it owns. News and cashback today reach
  members only through channels priced by others; a browser someone
  keeps open all day is a standing relationship, not a rented visit.
- Offline partner businesses are ready to hand their customers a reason
  to install (first pilot: a German orthopaedic shop, thousands of
  patients, a partner-funded price-lock cashback programme). The pilot
  itself launches on existing web surfaces — Evreos is where those
  members land next, so retention beats launch-day scope.

### Personas

- **The partner-referred member** — German, often 40+, not technical,
  arrives via a QR code or a staff recommendation with a cashback claim
  to redeem. Needs an install → sign-in → claim path with no dead ends,
  large type and honest language.
- **The efficiency-seeker** — resents what mainstream browsers do to an
  ordinary laptop; evaluates cold start, memory and default privacy in
  the first ten minutes; the organic wedge audience and the harshest
  reviewer.
- **The partner business** — funds a campaign for its own customers and
  wants it administered credibly; never operates the browser itself.
- **The Apivo operator (founder)** — publishes and updates apps,
  merchants and campaigns without cutting a browser release; watches
  budgets, funnels and retention.

### v1 scope — browser

Table stakes done properly, and no more: tabs with session restore and
background-tab suspension; an omnibox combining search, history and
bookmarks; bookmarks, history, downloads; find-in-page; zoom and UI
scaling; per-site permission prompts (camera, microphone, location,
notifications); private windows using the engine's ephemeral mode;
tracker/ad blocking on by default with a visible per-site toggle; PDF
viewing via the engine; dark and light themes; keyboard shortcuts;
import of bookmarks and history from Chrome, Firefox and Edge;
default-browser registration on each OS; signed auto-update with staged
rollout. Password handling in v1 is the platform's autofill where the
engine provides it — a built-in manager is explicitly later.

### v1 scope — super-app platform and launch apps

- A home surface and dock where built-in apps live; each app is a
  sandboxed, remotely updatable surface with a typed, capability-scoped
  bridge to the shell.
- **epiloYES app (mandatory):** the existing reader — front page,
  article pages, language and place switching — presented as a
  first-class app, not a pinned tab.
- **Cashback app (mandatory):** browse the merchant catalogue (language
  and place as separate parameters); open an offer through a tracked
  click-out; see the wallet with pending, confirmed and payable amounts
  exactly as the ledger reports them, with copy that explains honestly
  why pending exists (we pay when the network pays); request a
  withdrawal and follow its status. The claim-a-campaign flow for
  offline partners is designed in the UX but ships disabled until the
  backend exists (dependency D4).
- **Apivo ID:** one account (the existing Supabase-auth identity) across
  apps; required for money surfaces, never for browsing.
- An `evreos://` deep-link scheme so a QR code can open the claim flow
  after install.

### Fast-follow and later (name them, do not spec them)

Fast-follow: coupons app, fuel-price app, Android. Later: grocery
offers, price comparison, TV/radio hub, cross-device sync, iOS,
partner-branded builds. Each becomes real only when its Apivo-side
product domain exists.

### Success criteria

Starting budgets — placeholders to ratify or replace during clarify, and
thereafter tighten-only:

- Download ≤ 20 MB and installed footprint ≤ 60 MB per platform.
- Cold start to an interactive window ≤ 800 ms warm, ≤ 2 s first run,
  on named reference hardware (a 2020 mid-range x86 laptop and an M1
  Air).
- Shell overhead ≤ 150 MB RSS beyond engine processes at 10 tabs; total
  footprint on a scripted 10-tab workload ≥ 40% below Chrome on the
  same machine and sites.
- Idle: < 0.5% CPU, no wake storms, suspended background tabs.
- Chrome interactions (tab switch, omnibox keystroke) render within a
  16 ms frame.
- Business: ≥ 25% of patients pitched at the pilot counter install and
  complete a claim; D30 retention ≥ 20%; ≥ 1 cashback activation per
  active member per month.
- A public benchmark page whose methodology and scripts are published —
  a competitor rerunning our harness must get our numbers.

### Non-goals for v1

Chrome/WebExtensions compatibility; a built-in password manager; iOS;
cross-device sync; VPN; crypto or web3 surfaces; AI sidebars; and —
permanently, from the constitution — ad injection, silent affiliate
attribution, and server-side collection of browsing history.

### Open decisions for `/speckit-clarify` (founder answers)

- **Q-E1 Platform order.** The pilot demographic is phone-leaning; is
  Android needed before desktop polish, and what does the mobile path
  cost in the chosen stack?
- **Q-E2 Default search.** Which engine ships default, and what
  monetization posture (default-search revenue is a real browser income
  stream and a values statement).
- **Q-E3 Distribution channels.** Direct download only at first, or
  winget/Homebrew/Flatpak/store listings from beta?
- **Q-E4 Password story.** Is platform autofill acceptable for v1?
- **Q-E5 Import scope.** Bookmarks and history only, or passwords too?
- **Q-E6 Telemetry floor.** What minimal opt-in set, if any, ships in
  beta?
- **Q-E7 Brand.** Evreos trademark/domain clearance; standalone consumer
  brand vs "by Apivo".
- **Q-E8 Partner-branded builds.** Kept-possible only (rebrandability
  principle), or a v1 requirement for the pilot?
- **Q-E9 Targets.** Confirm or replace every placeholder number above.

---

## Block C — input for `/speckit-plan`

### Mandates

- Stable Rust, one Cargo workspace, no nightly features on the release
  path.
- Targets from day one: Windows, macOS and Linux, each on x86_64 and
  aarch64; CI cross-builds the full matrix on every PR.
- The budget file (for example `budgets.toml`) is read by CI gates from
  M0: artifact size measured per target, cold start via hyperfine on OS
  runners, memory via a scripted-session harness. A red budget fails the
  merge.
- Every external dependency sits behind an interface defined by its
  consumer, swappable in bounded time and proved by a second working
  implementation — the engine, the update transport, telemetry, the
  blocker. Licenses are gated (Apache/MIT-class preferred; file-level
  copyleft like MPL acceptable; anything stronger needs a recorded
  founder decision).

### The engine decision (take it first, record it as ADR-0001 of the new repository)

**Default: operating-system webviews via `wry`/`tao` — WebView2 on
Windows, WKWebView on macOS, WebKitGTK on Linux.** The engine's bytes,
memory and security patches belong to the OS; that is the only honest
route to the size and startup budgets, it ships this year, and the same
stack has a proven mobile path (Tauri 2) for Q-E1. The shell talks to it
only through the `Engine` trait, beside a headless test implementation
that keeps the seam real; Servo's embedding story (for example the Verso
work) is tracked as the future experimental third backend — the pure
Rust engine without betting v1 on web compat it does not yet have.

Rejected, with reasons recorded: bundled Chromium/CEF/Electron (the
budgets die, and it is the thing Evreos exists not to be); an own engine
(a decade); Servo as the v1 default (not daily-drivable yet — revisit
trigger: it becomes so).

Accepted costs, named: per-OS engine differences are ours to absorb in a
QA matrix; deep extension APIs are unavailable (aligned with non-goals);
page-rendering performance belongs to the OS engine, which constrains
what we may claim (see benchmark honesty).

### Spikes before any product code (M0, each with a pass/fail exit)

- **S1 Interception:** filter-list blocking and tracked click-out on all
  three platforms — WebResourceRequested on WebView2, content rule lists
  plus navigation delegation on WKWebView, the WebKitGTK interception
  APIs — driven by one `adblock-rust` core with EasyList plus German and
  Greek regional lists.
- **S2 Tab model:** webview-per-tab with a suspension policy; memory
  measured at 10 and 50 tabs, not assumed.
- **S3 Cold-start floor:** a bare shell window per OS on the reference
  hardware — does the 800 ms budget survive WebView2 and WebKitGTK
  initialisation? Budgets move only on this evidence, by founder
  sign-off.
- **S4 Chrome UI:** the tab strip and omnibox built twice — a Rust-native
  toolkit (iced or egui; gpui if its license posture fits) versus
  web-tech chrome in a dedicated shell webview — measured on input
  latency, RSS, accessibility, and international text input (German dead
  keys, Greek layout). This spike decides the shell UI; the decision is
  an ADR.
- **S5 Onboarding:** `evreos://` deep link registered on all three OSes
  and a QR claim-code flow end-to-end, with attribution carried by the
  code the member scans — no install-referrer fingerprinting.
- **S6 Updates:** ed25519-signed update applied through a staged-rollout
  dry run on all three platforms.

### Architecture

Workspace crates along these seams: the binary and composition root; the
shell UI; the `Engine` trait and its wry implementation plus the headless
test engine; tabs and session state (SQLite); the blocker
(`adblock-rust` and list updating); the app platform (manifests,
sandboxed surfaces, the bridge); the commerce client; identity; the
updater; opt-in telemetry. Boundaries carried by an architecture test,
as in the Apivo repository.

- **App platform:** an app is a signed (ed25519), versioned manifest
  plus a web surface served from Apivo infrastructure, cached locally
  with an offline fallback. The bridge is typed JSON-RPC over the
  engine's messaging channel; capabilities come from the manifest and,
  for anything page-adjacent, a per-app user grant. An app can never
  widen its own capabilities.
- **Launch apps consume existing surfaces:** epiloYES is the existing
  Astro reader; the cashback app is a client of the published HTTP API —
  the OpenAPI document the binary serves is the contract, and the client
  is generated from it, never hand-written on both sides.
- **Money:** rendered verbatim from the API — pending, confirmed,
  declined, reversed — with no client-side computation; click-outs go
  through the server redirect that carries the click reference, so
  deeplink construction and network vocabulary stay in the backend
  adapters where they already live.
- **Identity:** the native PKCE flow against the existing auth; tokens
  in the OS keychain (DPAPI, Keychain, libsecret); signed-out browsing
  untouched by any of it.
- **Updates and packaging:** three channels (nightly, beta, stable),
  staged percentages, delta where the platform allows; MSI and winget;
  notarized DMG and a Homebrew cask; AppImage, deb, rpm and Flatpak.
  The release pipeline proves the published artifact names its own
  version before a channel moves — the same posture as the Apivo deploy.
- **Telemetry:** opt-in, aggregate, EU-hosted; crash minidumps behind a
  separate opt-in.

### Benchmark honesty

With system webviews, page rendering and web compat belong to the OS
engine. The public benchmark page therefore measures what is ours —
download and installed size, cold start, shell overhead, idle behaviour,
chrome latency, and total scripted-workload footprint against Chrome and
Firefox — with methodology and scripts in the repository. No
cherry-picking; reproducibility is the marketing claim.

### Milestones, each with an exit criterion

- **M0** — spikes S1–S6 concluded, budget harness gating CI, budgets
  ratified.
- **M1** — a daily driver: the founder and partners go a week without
  reaching for Chrome; budgets green.
- **M2** — app platform live with the epiloYES app; an app update
  reaches users with no browser release.
- **M3** — Apivo ID plus the cashback app against the fixture network:
  catalogue → click-out → a pending entry visible in the wallet.
- **M4** — withdrawals, packaging and auto-update complete; public beta
  opens.
- **M5** — Android go/no-go: the mobile path assessed against the pilot
  demographics and Q-E1; an explicit decision, not a drift.

### Dependencies on the Apivo backend — founder decisions taken in `apivo-news`, assumed by nothing above

- **D1** A compatibility policy for the public API once shipped clients
  consume it (deprecation windows, versioning).
- **D2** Auth configuration for a native client (PKCE, audiences, token
  lifetimes).
- **D3** App-manifest hosting and signing infrastructure.
- **D4** Offline partner campaigns — in-store claims, escrowed partner
  funding, per-campaign exposure caps. **Currently excluded by the
  constitution** ("in-store cashback", "card-linked offers"): the
  Orthopelma price-lock pilot depends on this decision, first on the web
  surfaces, in Evreos second.
- **D5** Each further mini-app (coupons, fuel, grocery, comparison,
  TV/radio) becomes its own product domain per ADR-0001 when the founder
  schedules it; Evreos only ever renders surfaces those domains publish.
- **D6** The default-search and monetization posture (Q-E2) — a revenue
  decision, not an engineering one.
