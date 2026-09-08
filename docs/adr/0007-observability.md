# ADR-0007: Observability — OpenTelemetry in the binary, Grafana LGTM beside it

- **Status**: Accepted (2026-09-07)
- **Date**: 2026-09-07
- **Deciders**: founder
- **Related**: [0001](0001-super-app-architecture.md), [0002](0002-cashback-money-substrate.md), [0003](0003-affiliate-network-integration.md), [0006](0006-payout-details-vault.md)

## Context

This repository has **no telemetry**. Not "some, unevenly" — none. `go.mod`
names no tracing, metrics or instrumentation library of any kind; there is no
`/metrics` endpoint; nothing propagates a trace context across a function
call, let alone a process boundary.

What it has instead is good, and is the reason the absence has not hurt yet:
`internal/platform/logging` builds a JSON slog handler that redacts any
attribute whose key names a secret, and every module logs deliberately.
`/healthz` and `/readyz` answer. Container logs go to the Docker `json-file`
driver with rotation.

The gap is what happens to those logs: nothing reads them. They sit on the
host until an operator opens a shell. That is not a hypothetical — it is the
documented procedure. `docs/master-prompts/cashback-frontend.md`, describing
how to read the result of the first real transaction:

> `docker logs apivo-qa-api` shows the sweeps.

And it is why [#607](https://github.com/Nomos-N4s/apivo-news/issues/607)
exists. That issue proposes a database table recording when each sweep last
ran and how it went, because nothing else can answer it. A poll's outcome is
a telemetry question, and the absence of telemetry was about to be paid for
in schema.

### The four questions

The founder's four, stated 2026-09-07, in the order they will be needed:

1. **Watch the first real transaction.** QA is about to be switched to
   Linkwise. A person will click out, buy something, and then hours pass
   before the network reports it. The chain — click issued, redirect taken,
   report polled, reference matched, entry created — crosses a process
   restart and a scheduled job, and today no single artefact spans it.
2. **Know the moment money misbehaves.** C-1's zero-sum check, the outbox's
   dead letters, held credits, reconciliation differences, the attribution
   canary. Each of these already *detects*; none of them *tells anybody*.
3. **The ordinary production picture.** Latency, errors, saturation, for the
   newspaper as much as the wallet — one binary serves both.
4. **An audit trail that can be queried.** Distinct from the durable record,
   which already exists and is not in question: `domain_event` and the ledger
   are the truth. What is missing is a way to *ask them a question* without
   a psql session.

### The constitutional collision

Architecture Constraints, before this record:

> A **self-hosted open-source ledger may run as a sidecar service** beside the
> binary, with its supporting infrastructure, where it carries a correctness
> burden we would otherwise write ourselves. **This is the single permitted
> exception to one-process-per-application** […]

A telemetry backend does not fit that sentence. It carries no correctness
burden for this domain — it observes, it does not compute anything the
product depends on — and it is not one service but several.

Two further things are true and should be said rather than skirted.

**ADR-0006 never addressed this clause.** The payout vault shipped as a
second sidecar without the question being put. Its rationale — key management
is a burden we would otherwise write ourselves — is a good one and fits the
clause's *spirit* exactly, but the clause says "ledger" and says "single",
and nobody argued the gap. That is a governance debt this record settles
rather than inherits.

**Widening a rule to admit the thing you want is the shape of a bad
amendment.** The safeguard has to arrive with the widening, not after it,
which is why the amendment carries the request-path constraint below.

## Decision

**OpenTelemetry inside the binary. Grafana LGTM beside it. Two invariants
that are not negotiable.**

### 1. The instrumentation is OpenTelemetry, and it is ours

The OpenTelemetry Go SDK, in `internal/platform/telemetry`, owned the way
`internal/platform/logging` is owned. Traces, metrics and logs share one
vocabulary and one context. Nothing above that package names OpenTelemetry:
callers pass `context.Context` and the package does the rest, which is the
same rule the ledger port, the network adapters and `payout.DetailsVault`
already follow — *"every external dependency sits behind a consumer-defined
interface, swappable in under five engineer-days"*.

The point of choosing a **vendor-neutral instrumentation standard** rather
than a backend's own SDK is precisely that: the backend below becomes a
deployment decision, reversible without touching a line of domain code. That
is not a theoretical benefit here. It is what makes the second half of this
decision safe to get wrong.

**A deployment with no collector configured is a working deployment.** The
provider degrades to a no-op, says so once at start-up, and serves — the same
stance `cmd/apivo/vault.go` takes for an unconfigured vault, and for the same
reason: an observability outage must never be an availability outage.

### 2. The backend is Grafana LGTM

Grafana Alloy as the single collector, Prometheus for metrics, Loki for logs,
Tempo for traces, Grafana to read them. All Apache-2.0, no user cap, no
revenue cap — the same licensing bar ADR-0002 set for the ledger.

Prometheus rather than Mimir: Mimir solves a scale problem this deployment
does not have and will not have for a long time. Alloy rather than promtail
plus an OpenTelemetry Collector: one agent that scrapes, receives OTLP and
tails the existing json-file logs is one agent to run, and those logs are
already being written whether or not anything reads them.

It is deployed as a compose overlay listed in `COMPOSE_FILE`, the pattern the
cashback overlay established and the vault overlay (#601) confirmed. Listing
the overlay is the whole decision; an environment that has not listed it runs
without telemetry and is not broken.

### Invariant 1 — a span attribute is a new way for a secret to leave

`internal/platform/logging` redacts by key name. It redacts **logs**. Nothing
in this repository has ever needed to think about any other egress, because
there has never been another one.

A span attribute is one. Auto-instrumentation puts request URLs, SQL text and
headers onto spans by default, and in this system those carry, respectively:
member account ids, IBAN-shaped strings on their way to the vault, and bearer
tokens. ADR-0003 keeps network credentials out of the database and the
repository; it would be absurd to let them out through a trace instead.

So the telemetry package applies **the same `secretKeyMarkers` list** the
logging package already uses — reused, not copied, so the two cannot drift —
as a span processor, and additionally suppresses raw SQL, query strings, the
`Authorization` header, and request and response bodies. This is asserted by
a test that feeds a span every forbidden shape and reads the exported payload
back, in the manner of the existing check that the operator networks endpoint
never emits a credential's shape.

### Invariant 2 — telemetry is never on the request path

Exporters are batched and asynchronous, with a bounded queue that **drops**
when full rather than blocking, short export timeouts and a bounded shutdown.
A collector that is down, full or slow produces a dropped-spans counter and
nothing else. It does not add a millisecond to a wallet read, and it does not
take the newspaper down.

This invariant is also the one that makes the constitution amendment
defensible, and it is stated there as a general rule about sidecars rather
than only here about this one.

## Consequences

**The instrumentation is the durable part.** Backends are swappable; the
decision about *what is worth measuring* is not, and it is the expensive one.
This is why the work is sequenced instrumentation-first even though the
founder's route is governance-first: the ADR and the amendment gate it, and
then the binary is instrumented before anything is deployed.

**Five more containers, roughly 1.5–2 GB before tuning**, on a host already
running two application stacks, a database, the ledger and its worker and
queue, the vault, the edge, and up to five preview environments. Retention is
therefore short by default — days, not weeks — and the runbook must say
plainly that on an 8 GB box this displaces preview capacity. Telemetry goes
to QA first, and staging and production only when the box is sized for it.

**Logs stay where they are, and gain trace ids.** The slog handler is not
replaced — its redaction is load-bearing and a bridge that bypassed it would
undo Invariant 1 on the first line it wrote. It is wrapped so that a log line
emitted inside a span carries that span's ids. Alloy tails the same json-file
output that exists today. The change to the log pipeline is additive.

**`domain_event` remains the record.** Telemetry observes the stream; it never
becomes it. A trace expires with its retention window and a `domain_event`
row does not, and any question whose answer must survive that window is a
question for the database. #607 stays open for exactly this reason: a span
tells a human with Grafana how the last sweep went, and does not tell
`GET /ops/networks` after three days.

**A governance debt is paid.** The amendment names the vault, so ADR-0006's
sidecar is argued rather than assumed.

## Alternatives considered

**SigNoz.** Apache-2.0, traces, metrics and logs in one product on
ClickHouse, one UI, materially less to operate than four Grafana components.
Rejected on ecosystem depth rather than on merit: dashboards, exporters,
runbooks and hiring all assume Prometheus and Grafana, and the operator of
this system for the foreseeable future is one person who benefits more from
the well-trodden path than from fewer containers. ClickHouse is also not
meaningfully lighter than Prometheus plus Loki at this volume.

**Prometheus and Grafana only.** Two containers, roughly 300 MB, covering
question 2 and question 3 completely. Rejected because it answers neither
question 1 nor question 4: no distributed tracing means the click-to-entry
chain stays unreconstructable, which is the thing the founder most wants to
watch, and no log aggregation means the audit trail stays a shell session.
Retained as the fallback if the resource bill proves unaffordable — dropping
Loki and Tempo later is a compose edit, because the instrumentation does not
change.

**A hosted backend** (Grafana Cloud's free tier, Honeycomb, Datadog).
Rejected on two grounds that are both this repository's existing positions:
telemetry from this system carries member ids and money movements, and
EU-jurisdiction data handling is a constraint the constitution already sets
for the database and the host; and a free tier that later prices itself is
the "no user or revenue cap" trap ADR-0002 was explicit about. OTLP export
means this can be revisited without code changes if the operational burden
proves too high.

**ELK / OpenSearch.** Rejected: heavier than LGTM for this scale, and Elastic's
licensing history is exactly the risk ADR-0002's Apache/MIT bar exists to
avoid.

**Do nothing; keep reading `docker logs`.** Rejected by the founder,
explicitly, on 2026-09-07. Worth recording that it was a defensible position
while the product moved no real money — and stops being one the day it does.

## Revisit triggers

- **The resource bill bites.** If telemetry displaces preview capacity or
  destabilises QA, drop to Prometheus and Grafana alone; the instrumentation
  is unaffected.
- **A second host, or Kubernetes.** This record assumes one box. A second
  changes the collector topology and probably argues for Mimir over
  Prometheus.
- **Retention becomes a compliance question.** If a regulator or a partner
  requires longer retention of anything telemetry holds, that thing belongs
  in `domain_event` instead, and this record does not change.
- **The scrub list is found insufficient.** If a secret ever reaches a span,
  this record's Invariant 1 failed and the failure is a defect of the highest
  severity, not a tuning matter.
