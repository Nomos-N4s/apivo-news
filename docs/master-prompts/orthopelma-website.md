# Orthopelma site prototype — master prompt

> **Planning artifact only.** This is a one-off pitch prototype for a
> partner business, built and hosted **outside** `apivo-news` — its own
> tiny repository (suggested: `Nomos-N4s/orthopelma-site`), a static
> vhost on the existing Hetzner box, nothing touching the Apivo deploy
> pipeline or product scope. The file lives here so the plan survives
> the session that wrote it.

## Context — facts from the founder, to take as given

- The business is an orthopaedic shoe technology shop ("Orthopelma") in
  Germany; the current site is <https://www.ost-orthopelma.de/>, built
  on Wix, visibly dated, its footer still reading "©2021 … Created with
  Wix.com".
- We registered **orthopelma.de** for the owner — the cleaner name is
  part of the pitch.
- The published email is `info@ost-richter.de`: the name of the shop
  where the owner trained, inherited when its previous owner handed the
  business over. Display it unchanged, but structure the template so the
  later swap to an `orthopelma.de` address is a one-variable edit.
- The team photo on the current site shows two people who have not
  worked there for years. Never reuse the current imagery of people.
- The owner's one stated requirement — the reason he tolerates Wix — is
  that **he can change the front-page opening hours himself, easily.**
  The prototype must demonstrate that this stays true without Wix.
- He opens a **second shop on 1 October**; the site should be able to
  announce it.
- He picks a new logo colour early next week, so brand colours must be
  swappable in seconds — and showing him options is part of the pitch.
- Audience: patients 40+, German-speaking, many referred by doctors,
  many comfortably off. Trust and legibility beat flash.
- The price-raise and cashback/price-lock campaign is **deliberately
  absent** from this site. The website pitch and the pricing pitch are
  separate conversations.

## How to use this file

Paste the master prompt below whole into a fresh session in the new
repository — it is deliberately one-shot buildable in a weekend. If
spec-kit artifacts are wanted anyway: everything through "Deliverables
and acceptance" is `/speckit-specify` input; "Technical direction" and
"Hosting" are `/speckit-plan` input.

---

## The master prompt

**Mission.** Build and deploy a static prototype website for
"Orthopelma", a German orthopaedic shoe technology shop, good enough
that the owner says yes to leaving Wix for his new domain. It will be
judged in three seconds on his phone, so it must look dramatically
better than the current site at first glance — calm, professional,
trustworthy — and hold up under a slower read.

**Step 0 — content inventory, before any design.** Crawl the current
site — start at <https://www.ost-orthopelma.de/> and
<https://www.ost-orthopelma.de/leere-seite-2> — and lift every fact:
business name and legal form, address, phone, email, opening hours, the
services offered, any wording about health-insurance billing. Facts are
reused verbatim; nothing is invented. Where a fact is missing, render an
honest placeholder marked `[Platzhalter — vom Inhaber bestätigen]`. If
the crawl is blocked from the executing environment (it was from the
session that wrote this file), ask for the facts to be pasted in rather
than guessing.

**Language and tone.** Every visitor-facing word in German, Sie-Form,
plain and warm-professional, no anglicisms. Trust over marketing:
craftsmanship, experience, familiarity with prescriptions and health
insurance. No claims the current site does not itself make.

**Structure — one page, plus two legal stubs:**

1. **Header** — logo placeholder, phone number, hours at a glance.
2. **Hero** — what the shop does for people's feet, in one sentence a
   70-year-old patient would repeat; primary action: call; secondary:
   directions.
3. **Announcement slot** — the new shop opening on 1 October, shown or
   hidden by a single boolean in the config described below.
4. **Öffnungszeiten** — prominent, and THE demonstration feature: all
   opening hours live in one small, clearly labelled block in a single
   config file (JSON, or a marked block at the top of the page source —
   one obvious place), rendered everywhere hours appear. Document the
   owner's future 60-second edit-and-publish path. Do not build a CMS.
5. **Leistungen** — the services from the content inventory, each with
   one short benefit line and a restrained icon; the insurance line only
   as the current site words it.
6. **Vertrauen / Über uns** — the craft story without people photos
   (the current ones are stale; new ones are on the ask-list below).
   Neutral workshop and material imagery, or clean illustration.
7. **Kontakt** — address, phone, the current email
   (`info@ost-richter.de`, one-variable swap ready), and a static map
   image linking out to a route planner. No embedded third-party map —
   it keeps the privacy story clean and the page fast.
8. **Footer** — Impressum and Datenschutz stubs clearly marked
   "Entwurf"; and no site-builder credit line of any kind.

**Brand mechanics — the pitch feature.** All colours are CSS custom
properties in one labelled block. Ship a small, discreet
prototype-only palette switcher offering three tasteful directions, so
the owner picks his new logo colour *on* the site. Typography large and
calm: base size at least 18px, generous line height, WCAG 2.1 AA
contrast throughout — the audience is 40+, sometimes low-vision, and
accessibility here is a selling point, not a checkbox.

**Technical direction.** Static output, boring on purpose: hand-rolled
HTML and CSS with a few lines of JavaScript, or a static-site generator
only if it genuinely helps — nothing that needs a server process.
Self-hosted fonts and assets; zero third-party requests at runtime; no
cookies, no analytics, nothing to consent to. Performance: Lighthouse
≥ 95 in all four categories; under 500 KB before images; images in
AVIF/WebP, sized and lazy-loaded. Responsive from 320px up, checked on
a real phone. `<meta name="robots" content="noindex">` while it is a
prototype — it must not compete with the live site in search.

**Hosting.** Serve it as a static site from the existing Hetzner box's
Caddy — its own site block, reusing the hardening snippet, entirely
separate from the Apivo application deploys. Preferred hostname:
`preview.orthopelma.de` — we control the domain, and the owner seeing
his new name live is part of the pitch. Fallback if DNS is not ready in
time: a subdomain of an Apivo-controlled domain. HTTPS either way.
Deliver a one-command redeploy path; rsync of a folder is fine.

**Deliverables and acceptance.**

- The live URL, reviewed phone-first.
- The palette switcher working; the hours demonstrably editable in one
  place in under a minute.
- Lighthouse results showing the four ≥ 95 scores.
- A short note in German to send the owner with the link: what he is
  looking at; that colours, texts and photos are placeholders he
  controls; and the ask-list — logo file and chosen colour; fresh
  photos (workshop, storefront, the people who actually work there
  now); confirmed services, hours and insurance wording; the Impressum
  and Datenschutz details; and, when he is ready, the go-ahead to point
  `orthopelma.de` at the new site.

**Non-goals.** No CMS, no online booking, no shop, no multi-language,
no cashback or pricing-campaign content anywhere on the site, and no
changes of any kind to the live Wix site, its DNS, or the email setup.
