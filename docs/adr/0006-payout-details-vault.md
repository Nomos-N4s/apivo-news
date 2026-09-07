# ADR-0006: Payout details vault — OpenBao, KV, held outside this database

- **Status**: Accepted (2026-09-07)
- **Date**: 2026-09-07
- **Deciders**: founder
- **Related**: [0002](0002-cashback-money-substrate.md), [0003](0003-affiliate-network-integration.md)

## Context

`cashback.payout_destination` stores a **reference** and never a member's
bank details. That is a rule about the database, and it needs somewhere for
the details to be instead. `payout.DetailsVault` is the port that names that
place, and it has shipped without an implementation since T095 for a stated
reason: *"a vault is somebody's KMS, somebody's secrets manager, or a
payment processor's own tokenisation endpoint, and picking one here would be
picking it for every deployment."*

The consequence is that `POST /api/v1/cashback/payout-destinations` answers
503 on every deployment. No destination can be recorded, so no withdrawal
can name one, so the payout half of the product does not work. FR-051's
verification flow (B6, #548) sits behind this rather than beside it.

Two constraints shape the choice, and both come from decisions already made
rather than from preference.

**The rail is a person.** FR-052 requires a manual rail, `manual.New()` is
what the composition root wires today, and that rail moves no money and
never resolves a reference. So an operator making the bank transfer has to
read the IBAN somewhere. Whatever holds the details must let a human read
them, or that interface becomes work this decision creates.

**The port is one-way.** `DetailsVault` can put details in and get a
reference back, and cannot read them out again, because *"nothing in this
product ever needs to — a rail resolves the reference itself"*. Reading is
therefore always outside this codebase, in whatever the vault's own
interface is.

## Decision

**OpenBao, using the KV v2 secrets engine as the store.**

The adapter lives in `internal/cashback/payout/openbao/`, imports its port
and nothing else, and is wired from configuration by the composition root —
the same shape ADR-0003 fixed for network adapters and ADR-0002 for the
ledger. A deployment with no vault configured keeps the 503 and starts
normally.

The details never enter this database in any form, not even encrypted. The
reference is a random path, so it carries no part of what it points at.

The vault validates the details against the destination kind, because the
port says it is the only thing positioned to: a `sepa` destination is an
IBAN, structurally checked and verified against its mod-97 checksum.

### How it was decided

Four options, eight criteria, weighted for an alpha with no payout volume:
shipping and running it cheaply count for more than scale.

| Criterion | Weight | OpenBao KV | OpenBao transit | Infisical KV | Provider |
|---|---|---|---|---|---|
| Time to a first working withdrawal | 5 | 3 | 2 | 4 | 1 |
| Operator can read the IBAN to pay | 5 | 5 | 1 | 5 | 3 |
| Burden of running it | 4 | 2 | 3 | 3 | 5 |
| Reversibility if the decision changes | 4 | 4 | 4 | 4 | 2 |
| Security if the database leaks | 3 | 5 | 4 | 5 | 5 |
| Security if the vault leaks | 3 | 2 | 3 | 2 | 5 |
| Who carries compliance and liability | 3 | 2 | 2 | 2 | 5 |
| Fit at thousands of members | 2 | 3 | 5 | 3 | 5 |
| **Weighted total, out of 145** | | **97** | **80** | **106** | **103** |

The scores are an argument, not a measurement, and the interesting parts are
where they are decisive rather than where they are close.

**Envelope encryption loses, and not on security.** It scores worst because
the rail is a person and nothing would let them read the IBAN. That gap is
an operator interface this repository would have to build, and it closes by
itself the day a provider becomes the rail.

**The top three are inside the noise.** Halve the weights on time and
running burden and the provider wins; double the security weights and it
wins outright. So the table's real claim is narrow: optimising to ship
favours self-hosting, and optimising for liability favours paying somebody.

**OpenBao over Infisical**, despite scoring lower, for two reasons that the
table cannot hold. It is what the founder prefers, and the gap between them
is entirely operational rather than security. And its transit engine is the
migration path if envelope encryption is later chosen, which makes that an
engine change rather than a vendor change — so this decision does not
foreclose the one deliberately left open.

## Consequences

- The payout surface works end to end for the first time: a member can
  record a destination, and an operator can read it to pay.
- A new operational dependency. OpenBao must be run, unsealed after restart,
  backed up, and upgraded. It is the first thing in this deployment whose
  loss is unrecoverable rather than merely disruptive: lose the vault and
  every stored destination is gone, while every row referencing one remains.
  Backup is therefore not optional and the runbook says so.
- The blast radius of a database compromise stays zero for bank details, and
  the blast radius of a vault compromise is all of them. That is the trade
  being made, and it is the right way round while the database is the more
  exposed of the two.
- Apivo holds personal data it did not hold before, so the GDPR obligations
  and the breach liability are the deployment's. A provider would have
  absorbed them.
- `verified_method` records how each destination was verified, so
  operator-verified and provider-verified destinations stay distinguishable
  if a provider arrives later.

## Alternatives considered

| Alternative | Rejected because |
|---|---|
| **OpenBao transit, ciphertext in this database** | The rail is a person and nothing lets them read the IBAN. It also puts the details, encrypted, in the column the port exists to keep them out of. Reconsider when a provider becomes the rail. |
| **Infisical KV** | Scores highest and is genuinely lighter to run, but its transit-equivalent path is less direct, so choosing it would make the envelope question a vendor migration. It also stores into Postgres, so a deployment pointing it at the existing instance would erase the database-leak advantage that put it top. |
| **A payment provider's tokenisation** | Answers storage, verification and the rail in one choice, and carries the liability. Rejected for now on time and cost: contracts and onboarding stand between the decision and a first withdrawal, and the fees precede the revenue. This is the option to revisit first. |
| **pgcrypto in this database** | The cheapest option and the one the port explicitly forecloses: the details would be in this database, which is what "somewhere this database is not" refuses. |
| **HashiCorp Vault** | The same product before its licence changed to BUSL. OpenBao is the fork of the last MPL-2.0 release, under Linux Foundation governance, with no restriction on commercial use. |

## Revisit triggers

- **Withdrawal volume outgrows a person.** The trigger is the number of
  withdrawals per month, not revenue: a human doing hundreds of transfers
  arrives before the money that pays a provider does.
- **A provider is adopted.** Verification becomes the provider's answer,
  transit becomes viable because nobody reads an IBAN by hand any more, and
  this record is superseded rather than amended.
- **Destinations reach a scale where a secrets engine strains.** KV holds one
  entry per member; its access-control and audit models are project-shaped,
  not row-shaped. Envelope encryption is the answer, and by then its cost
  will have moved.
- **The vault's operational burden proves worse than budgeted.** Infisical is
  the same decision with a lighter runtime, and the port makes it a package
  swap.
