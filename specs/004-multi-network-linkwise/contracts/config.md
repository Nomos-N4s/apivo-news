# Configuration Contract: N Networks

**Feature**: [../spec.md](../spec.md) | **Plan**: [../plan.md](../plan.md) D-A

## Today

`NetworkConfig` is five flat scalars — `NETWORK_DRIVER`,
`NETWORK_ACCOUNT_ID`, `NETWORK_API_KEY`, `NETWORK_API_SECRET`,
`NETWORK_SOURCE_LANGUAGE` (`config/cashback.go:251-290`). One of each. A
second network has nowhere to go.

That was a decision, not an omission, and its reasoning is worth keeping:

> *'The alpha integrates one network, so there is one credential set rather
> than a per-network map: a second network is a second adapter and a
> **deliberate configuration change**, not something a **wildcard
> environment lookup** should be able to conjure.'*

The design below is the deliberate change, and is still not a wildcard
lookup.

## The surface

```text
NETWORKS=awin,linkwise                   # the ordered list. Nothing else runs.

NETWORK_AWIN_ACCOUNT_ID=<publisher id>   # not a secret; appears in deeplinks
NETWORK_AWIN_API_KEY=<secret>
NETWORK_AWIN_API_SECRET=<secret>         # optional; not every network issues one
NETWORK_AWIN_SOURCE_LANGUAGE=de          # BCP-47 primary subtag, operator-stated

NETWORK_LINKWISE_ACCOUNT_ID=...
NETWORK_LINKWISE_API_KEY=...
NETWORK_LINKWISE_SOURCE_LANGUAGE=el
```

### Rules

| Rule | Behaviour |
|---|---|
| `NETWORKS` is the only thing that enables a network | A `NETWORK_FOO_*` block with `foo` absent from the list is **ignored**, and said so once at INFO. Presence never implies intent |
| Each entry must satisfy `validateNetworkDriver` | Lowercase letters, digits, underscores, starting with a letter — so `NETWORK_<DRIVER>_` is unambiguous |
| Each entry must be a **shipped** driver | A name absent from the registry is refused at startup, listing the drivers that do ship |
| Duplicate entries are refused | Two adapters for one driver is two publisher accounts, which is a different feature (`network_account` already models it; `NETWORKS` does not) |
| A network with a missing key is **named** | ERROR naming the network and the key: `NETWORK_LINKWISE_API_KEY is unset` (FR-091) |
| One incomplete network does not stop the others | It does not start; the deployment does, and the others poll (FR-091) |
| **Zero** usable networks is not a startup failure | Clicks on an existing catalogue, the wallet and the money loop all still run. One ERROR line says no network is polling. This is the stance the sweeps already take on a missing publisher account |
| The old flat keys are **refused**, not aliased | `NETWORK_DRIVER is no longer read; use NETWORKS and NETWORK_<DRIVER>_*`. A silent alias lets a deployment believe it has two networks and run one |

### Why an explicit list rather than inferring from which blocks are present

FR-091 requires an incomplete network be reported **by name**. If presence
implied intent, a typo in `NETWORK_LINKWISE_API_KEY` would make Linkwise
*vanish* — no name to report, nothing to complain about, and a deployment
that silently polls one network while its operator believes it polls two.
Naming the network first is what turns a missing key into a failure with a
name in it.

### Why not indexed keys (`NETWORK_1_DRIVER`)

The index is a second name for something that already has one, and it is
positional: inserting a network at the front silently rebinds every
credential after it. A credential bound to a position rather than to a name
is a credential that can be given to the wrong network by an edit that looks
like reordering.

### Why not one JSON blob

It puts a parser between an operator and a typo. `NETWORK_LINKWISE_API_KEY
is unset` names the key; `invalid character '}' looking for beginning of
object key string` names a byte offset.

## Credentials

Unchanged in substance, and finally exercised (ADR-0003, FR-093):

- Credentials live in the **environment**. Never in the repository, never in
  the database, never in a migration, never in a seed.
- `cashback.network_account.credential_ref` holds **a key into
  configuration** — its comment already says *"never the credential itself
  (ADR-0003)"* (`0011:89-90`). With one network that column had no work to
  do. With two it does: `credential_ref = 'awin'` resolves the
  `NETWORK_AWIN_*` block.
- `APIKey` and `APISecret` stay `Secret`, so they cannot be printed by
  accident, and the per-network validation error names the **key**, never
  the value.

## Connection budget

`CheckCapacity` needs `jobs × 2 + 2` connections — one per job to hold its
advisory lock for the whole run, one for its work, and two left for the rest
of the application (`scheduler/advisorylock.go:109-131`).

`cmd/apivo/main.go:440-446` already writes the arithmetic out for one
network: the zero-sum check alone needs 4, the settlement sweep takes it to
6, the earnings lifecycle to 8, the two network sweeps to 12, and the
catalogue import to **14**.

Each additional network adds **three** jobs — a forward sweep, a trailing
sweep and a catalogue import, all per publisher account:

| Networks | Jobs | `pool_max_conns` floor |
|---|---|---|
| 1 | 6 | 14 |
| 2 | 9 | **20** |
| 3 | 12 | 26 |

`locker.CheckCapacity` asserts this at startup against the jobs **actually
registered** (FR-095), and its error carries the numbers. So a deployment
that adds a network without raising `pool_max_conns` in `DATABASE_URL`
refuses to start, rather than deadlocking jobs against request handlers
under load — which is the failure the check exists to prevent, and it gets
one network closer with every network added.
