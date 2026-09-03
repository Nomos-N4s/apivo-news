# Quickstart: Running Two Networks Locally

**Feature**: [spec.md](spec.md) | **Plan**: [plan.md](plan.md)

This is the 002 quickstart plus the parts that only exist once a second
network does. Everything below runs **with no credentials** — the two
networks are `fixture` and `fixture2`, and the point of the second fixture
is that this page is runnable in CI (T224).

Where a step depends on a task that has not landed, the command says so
itself: the `Makefile` targets refuse with the task number and issue rather
than failing obscurely.

---

## 1. Bring the stack up

```sh
make cashback-up                 # postgres, redis, blnk
make cashback-seed               # one network, two merchants, three bands
```

## 2. Configure two networks

```sh
export NETWORKS=fixture,fixture2

export NETWORK_FIXTURE_ACCOUNT_ID=fixture-publisher-1
export NETWORK_FIXTURE_SOURCE_LANGUAGE=en

export NETWORK_FIXTURE2_ACCOUNT_ID=fixture-publisher-2
export NETWORK_FIXTURE2_SOURCE_LANGUAGE=el
```

No `NETWORK_*_API_KEY` for either: `NeedsCredentials()` is false for a
fixture driver, which is the whole point of it.

**Raise the pool.** Two networks is nine scheduled jobs, and the capacity
check refuses to start below `2 × 9 + 2 = 20`:

```sh
export DATABASE_URL="postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable&pool_max_conns=20"
```

Getting this wrong is the *good* failure — the check names the number it
needs. Getting it wrong before this feature was a deadlock under load.

## 3. What you should see at startup

```text
INFO  network connected            network=fixture  account=fixture-publisher-1
INFO  network connected            network=fixture2 account=fixture-publisher-2
INFO  scheduler registered 9 jobs
```

Nine, not six: a forward sweep, a trailing sweep and a catalogue import per
network, plus the zero-sum check, the settlement sweep and the earnings
lifecycle.

```sh
curl -sH "Authorization: Bearer $OPS_TOKEN" \
  localhost:8080/api/v1/cashback/ops/networks | jq '.networks[] | {id, active, driver_shipped, accounts: [.accounts[].credential_present]}'
```

---

## Validation scenarios

These continue the 002 quickstart's V1–V10. Each is a `make
cashback-scenario NAME=…` target once T251 lands; until then each is
reproducible by hand from the steps given.

### V11 — Two networks, two catalogues, one deployment (SC-020, US1)

1. Start with both networks configured as above.
2. Wait for both catalogue imports.
3. `select network_id, count(*) from cashback.merchant_network group by 1;`

**Pass**: both networks have routes. **Fail**: one network's jobs never ran
— check for a duplicate job name, which is what T221 removes.

### V12 — A report from the wrong network credits nobody (SC-021, US2, C-2)

This is the scenario the whole feature turns on.

1. Click out through `fixture` and capture the `click_ref`.
2. Make `fixture2` report a transaction citing **that same reference**.

**Pass**:
- no entry is opened;
- the report appears in `GET …/ops/unattributed` with reason
  `foreign_network`;
- the row names `fixture2`, and the click it *nearly* matched is not
  disclosed to the operator queue as attributable.

**Then** have `fixture` report the same reference:

- **that** one credits normally.

**Fail**: two entries, or one entry citing a click from the other network.
Either is one purchase credited twice, and it is the reason `0033` exists.

### V13 — The database refuses the second credit (SC-021, Principle VIII)

Not through the API — directly, because the point is that the *database*
holds the rule:

```sql
insert into cashback.entry (…, click_id, network_transaction_id, …)
values (…, '<the same click>', '<a different report>', …);
```

**Pass**: `23505`, constraint `entry_click_id_idx`.
**Fail**: a second row. The application-level check is not the guarantee.

### V14 — A retailer on both networks publishes one route (SC-022, US3)

1. Seed one merchant with a route on each network.
2. `select network_id, preferred, status from cashback.merchant_network where merchant_id = …;`

**Pass**: exactly one `preferred`. Then set the preferred route's status to
`left_network`:

**Pass**: the surviving active route holds the slot, and the catalogue
publishes it. **Fail**: the retailer publishes nothing, or publishes the
dead route.

### V15 — An unattributable route is never published (SC-026, FR-107)

1. Set `can_attribute = false` on the preferred route.

**Pass**: `23514`, constraint `merchant_network_preferred_is_publishable` —
the update is refused, and the retailer's route must be moved before that
route can be marked unattributable.

**Why this matters more than it looks**: nothing about an unattributable
route fails. The member clicks, buys, and the network pays us. Only the
member is never credited, and every log line is green.

### V16 — Moving the published route is a recorded decision (FR-099, FR-101)

```sh
curl -X POST -H "Authorization: Bearer $OPS_TOKEN" \
  -d '{"network":"fixture2","reason":"better validation rate over 60 days"}' \
  localhost:8080/api/v1/cashback/ops/merchants/$ID/route
```

**Pass**: 200, the route moves, and who/when/from/to/why are recorded. An
empty `reason` is **400**. A route that is paused or unattributable is
**409**, carrying the database's refusal rather than a 500.

### V17 — A third network costs one package (SC-024, US5)

```sh
go test ./internal/arch/ -run TestNetworkIsolation
```

**Pass**: green, **unmodified**, with two adapter packages present. That
file is SC-008's proof, and it passing untouched as adapters are added is
the only honest form the claim can take.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `the connection pool allows MaxConns=14, but 9 jobs need 20` | The pool was sized for one network | Raise `pool_max_conns`; the message carries the number |
| `NETWORK_DRIVER is no longer read` | The old flat configuration | `NETWORKS` plus `NETWORK_<DRIVER>_*` — see [contracts/config.md](contracts/config.md) |
| A network is in `NETWORKS` but never polls | Its block is incomplete | The ERROR line names the network and the key. The deployment stays up on purpose |
| A `NETWORK_FOO_*` block is ignored | `foo` is not in `NETWORKS` | Presence never implies intent; add it to the list |
| Everything green, members never credited | A route that cannot carry a click reference | `select external_merchant_id from cashback.merchant_network where not can_attribute;` |
