# HTTP Contract Delta: The Operator Surface for Two Networks

**Feature**: [../spec.md](../spec.md) | **Base**: `002/contracts/http-api.md`

All operator endpoints live behind `/api/v1/cashback/ops/`
(`ops/handler.go:21`), behind the existing auth gate, and are added to
`routes()` — which `Patterns()` derives and
`TestEveryRegisteredRouteIsReachable` proves is not bookkeeping that
drifted. **No member-facing endpoint changes.** Which network carries a
retailer is not a member-facing fact.

---

## 1. Existing endpoints gain a network filter

Every queue that can contain rows from two networks becomes filterable by
one, because "work through the unattributed queue" stops being one task the
day two networks feed it (FR-102).

| Endpoint | Change |
|---|---|
| `GET …/unattributed` | Optional `?network=<id>`; every row **names** its network |
| `GET …/held` | Optional `?network=<id>`; every row names the network of the report behind the entry |
| `GET …/reconciliation/runs/{id}/differences` | Every row names its network. A run is already per statement, and a statement is already per network |
| `GET …/exports/ledger`, `…/exports/reconciliation` | A network column in both JSON and CSV. Existing consumers see one more column, never a changed one |

`POST …/reconciliation/runs` takes the **network** and the publisher's own
account identifier rather than a raw `network_account_id` UUID
(`ops/reconciliation_http.go:59,98`). With one account an operator could
find that UUID; with two, reconciling either means a `psql` session first,
and the endpoint is the one place a mis-typed UUID reconciles the wrong
network's statement.

`?network=` naming a network that is not connected is **400**, not an empty
list — an empty list is indistinguishable from "no work", and a typo in a
network id is the likeliest way to be told there is none.

### The unattributed reason becomes discriminating (FR-098)

Today an unmatched report is unattributed. With two networks there are three
distinct causes and three distinct operator actions:

| Reason | What happened | What an operator does |
|---|---|---|
| `no_reference` | The network reported no click reference at all | Attribute by hand, or dismiss |
| `unknown_reference` | A reference that matches no click we ever issued | Suspect the network, or an expired click |
| `foreign_network` | A reference that matches a click issued through **another** network | **Nothing** — this is the correct outcome of two networks reporting one purchase (FR-096). Dismiss it and expect the sibling report to have been credited |
| `route_cannot_attribute` | The route carries no click reference by design (rule 10) | Stop publishing the route, or accept it as unattributable |

`foreign_network` is the one that matters most and is easiest to get wrong.
Without it, the correct behaviour of a two-network deployment looks exactly
like a bug, and an operator's instinct — attribute it by hand — would create
the second credit the database now refuses.

---

## 2. New — the connected networks (FR-103)

```http
GET /api/v1/cashback/ops/networks
```

```json
{
  "networks": [
    {
      "id": "awin",
      "display_name": "Awin",
      "active": true,
      "driver_shipped": true,
      "click_ref_param": "clickref",
      "max_query_window_days": 31,
      "rate_limit_per_minute": 360,
      "accounts": [
        {
          "external_publisher_id": "123456",
          "credential_ref": "awin",
          "credential_present": true,
          "cursor_at": "2026-09-03T04:00:00Z",
          "trailing_cursor_at": "2026-05-27T04:00:00Z",
          "last_poll_at": "2026-09-03T06:00:00Z",
          "last_poll_outcome": "ok"
        }
      ]
    }
  ]
}
```

Three fields answer the questions that are currently unanswerable without
reading logs, and each maps to a real failure:

- **`driver_shipped`** — the row exists in `cashback.network` but the binary
  has no adapter for it. That is exactly the two-switch disagreement this
  feature removes (`plan.md` D-B), and until the registry lands it is a
  network that can be seeded and then refuses to start.
- **`credential_present`** — whether the configuration block
  `credential_ref` names resolved. **Never the credential, never a prefix of
  it, never its length.** A boolean (ADR-0003).
- **`last_poll_outcome`** — a network that is configured, active and silent
  is the failure mode with no symptom; a cursor that has not moved is how it
  is seen.

`GET`, read-only, no state. It is a listing, and the thing it lists is
configuration plus cursors.

---

## 3. New — move the published route (FR-099, FR-101)

```http
POST /api/v1/cashback/ops/merchants/{id}/route
{ "network": "linkwise", "reason": "better validation rate over 60 days" }
```

- **`reason` is required and must not be blank**, matching FR-061 and every
  other operator action in this surface. A rate decision nobody wrote down
  is a rate decision nobody can revisit.
- The acting operator comes from the auth gate, never from the body.
- **409** if the target route is not `active` or cannot be attributed — the
  database refuses it (`merchant_network_preferred_is_publishable`), and the
  endpoint reports the refusal rather than translating it into a 500.
- **404** if the retailer has no route on that network.
- The change is recorded: who, when, from which network, to which, and why.

### Why an endpoint and not a rate rule

Automatic arbitration on the higher member rate is **Q10**, and its recorded
default is *no*. A catalogue that changes under members because a rate moved
is a product decision. This endpoint is the safe half: a human moves the
route and says why.

---

## 4. What does **not** change

| Thing | Why |
|---|---|
| Member-facing catalogue, wallet, click-out | A member sees a retailer and a rate. The network is ours to know |
| `POST …/reconciliation/runs` | A statement is already per network; the run already carries it |
| Auth, problem+json shape, pagination | Unchanged, and the new endpoints use them as they stand |
| `openapi.json` generation | The document is served from the same source; the C-6 money guard (T125) walks whatever is added, so a new money field in these responses is held to `Money` automatically |
