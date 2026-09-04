import type { Money } from './money';

/**
 * The cashback API's payload shapes, contract-verbatim
 * (`specs/002-apivo-cashback-alpha/contracts/http-api.md`).
 *
 * These are hand-written against the HTTP contract rather than generated
 * from the database schema, and the distinction matters: the schema is the
 * shape of the rows, the contract is the shape of the answers, and the
 * contract deliberately publishes less than the schema holds. A merchant
 * row carries the network's commission; the response carries only the share
 * the member earns, because publishing the commission would promise roughly
 * twice what arrives and would publish the margin with it. Generating these
 * from the schema would quietly reintroduce every field the contract chose
 * not to send.
 */

/** `GET /participation` — 404 means never opted in, which is not an error. */
export interface Participation {
  readonly status: 'active' | 'left';
  readonly opted_in_at: string;
  readonly terms_version: string;
  readonly default_currency: string;
}

/**
 * One published rate band.
 *
 * `kind` decides which of the two amount fields is present: a `percent`
 * band carries `bps`, a `fixed` band carries `amount` as money. There is no
 * single `display_rate` string, because a fixed rate IS money and money on
 * this API is always `{ minor, currency }`.
 */
export type Rate =
  | {
      readonly offer_id: string;
      readonly kind: 'percent';
      readonly bps: number;
      readonly conditions: string | null;
      readonly exclusions: string | null;
      readonly valid_to: string | null;
    }
  | {
      readonly offer_id: string;
      readonly kind: 'fixed';
      readonly amount: Money;
      readonly conditions: string | null;
      readonly exclusions: string | null;
      readonly valid_to: string | null;
    };

/** One `GET /catalogue` item. */
export interface CatalogueItem {
  readonly merchant_id: string;
  readonly slug: string;
  readonly name: string;
  /** The language the name is actually written in. */
  readonly name_language: string;
  /**
   * True when no copy exists in the requested language and the retailer's
   * source language answered instead. The page says "shown in German"
   * rather than silently pretending it is Greek.
   */
  readonly name_is_fallback: boolean;
  readonly summary: string | null;
  readonly rates: readonly Rate[];
}

/** `GET /merchants/{slug}`. */
export interface MerchantDetail extends CatalogueItem {
  /**
   * Always null today. Nothing in the schema records it — not on the
   * retailer, not on the route, not on the network — and the contract emits
   * null rather than a plausible constant, because a member reads a number
   * here as "you will be paid in about six weeks".
   */
  readonly typical_confirmation_days: number | null;
}

/** `POST /clickouts` — the tracked click, committed before this is written. */
export interface Clickout {
  readonly click_ref: string;
  readonly redirect_url: string;
  readonly expires_at: string;
}

/**
 * `GET /wallet`. Every total is computed from ledger postings, never from a
 * stored balance (C-1).
 *
 * `reserved` is the one the mockup did not draw and the one a member most
 * needs: money already spoken for by a withdrawal request that has not
 * settled. Without it the arithmetic on the screen does not close.
 */
export interface WalletTotals {
  readonly pending: Money;
  readonly confirmed: Money;
  readonly reserved: Money;
  readonly paid_out: Money;
  /** The floor below which a withdrawal is refused (FR-050). */
  readonly payout_threshold: Money;
}

/** The lifecycle states an entry is published in. */
export type EntryState = 'pending' | 'confirmed' | 'paid' | 'held' | 'reversed' | 'declined';

/** One `GET /wallet/entries` item. */
export interface WalletEntry {
  readonly entry_id: string;
  readonly merchant_name: string;
  readonly transacted_at: string;
  readonly sale_amount: Money;
  readonly cashback_amount: Money;
  readonly state: EntryState;
  readonly expected_confirmation_at: string | null;
  /**
   * Set on a reversal, naming the credit it reverses. Both appear in the
   * list; neither is hidden (US3 scenario 2), which is why this is a
   * pointer rather than a flag that could be used to filter one away.
   */
  readonly reversal_of_id: string | null;
  readonly reason: string | null;
}

/** A page of entries. */
export interface EntryPage {
  readonly items: readonly WalletEntry[];
  readonly next_cursor: string | null;
}

/** A destination a member owns. An unverified one is refused at withdrawal. */
export interface PayoutDestination {
  readonly id: string;
  readonly kind: string;
  /** Masked by the API — the last group of an IBAN, never the whole one. */
  readonly details: string;
  readonly verified_at: string | null;
}

/**
 * `POST /withdrawals` — `awaiting_approval` for **every** request, whatever
 * the amount, because C-4 forbids a payout without a named human approver.
 *
 * `reserved_amount` is not an echo of the requested amount. Cashback is
 * reserved in whole entries, oldest first: an entry cites the network report
 * that evidences it, so there is no half of one to take. Asking for 15.00
 * against entries of 10.00 and 20.00 reserves 30.00, and 30.00 is what the
 * payout pays.
 */
export interface WithdrawalRequest {
  readonly request_id: string;
  readonly state: 'awaiting_approval';
  readonly reserved_amount: Money;
}

/** The withdrawal states a member sees. */
export type WithdrawalState =
  | 'awaiting_approval'
  | 'approved'
  | 'rejected'
  | 'submitted'
  | 'settled'
  | 'failed';

/** `GET /withdrawals` · `GET /withdrawals/{id}`. */
export interface Withdrawal {
  readonly id: string;
  readonly state: WithdrawalState;
  readonly amount: Money;
  readonly reserved_amount: Money;
  readonly destination: PayoutDestination;
  readonly requested_at: string;
  /** Present where an operator refused it — the member reads this. */
  readonly decision_reason: string | null;
  /** Present once settled. */
  readonly payout_reference: string | null;
}

/**
 * An RFC 9457 problem document.
 *
 * The two balance refusals share one code — below the threshold and beyond
 * the confirmed balance are different walls, but a member acts on both the
 * same way — so the figures arrive as extension members and the detail says
 * which wall it was. No amount is spelled into `detail`: minor units
 * rendered as prose read as a price, and the client formats them for the
 * member's language.
 */
export interface Problem {
  readonly type?: string;
  readonly title?: string;
  readonly status?: number;
  readonly detail?: string;
  readonly code?: string;
  readonly shortfall?: Money;
  readonly threshold?: Money;
  readonly confirmed?: Money;
  readonly requested?: Money;
  readonly [extension: string]: unknown;
}

/* ------------------------------------------------------------------ */
/* Operator payloads                                                    */
/* ------------------------------------------------------------------ */

/** A page of any operator queue. */
export interface OperatorPage<T> {
  readonly items: readonly T[];
  readonly next_cursor: string | null;
}

/**
 * `GET /ops/unattributed` — a network transaction that matched no click.
 *
 * `attributable` is served because `entry_evidence_guard` makes an entry
 * with a null click legal only where the network named no reference at all.
 * A report whose reference matched no click can only be dismissed, and an
 * interface that worked this out for itself would offer an action the
 * database is going to refuse.
 */
export interface UnattributedTransaction {
  readonly id: string;
  readonly network_id: string;
  readonly external_id: string;
  readonly sale: Money;
  readonly commission: Money;
  readonly transacted_at: string;
  readonly attributable: boolean;
}

/** `GET /ops/held` — a credit a hold rule kept out of a member's balance. */
export interface HeldEntry {
  readonly id: string;
  readonly account_id: string;
  readonly network_transaction_id: string;
  readonly click_id: string | null;
  readonly amount: Money;
  readonly held_since: string;
  readonly hold_rule: string;
  readonly hold_reason: string;
  readonly network_id: string;
  readonly external_id: string;
  readonly report_status: string;
  readonly sale: Money;
  readonly commission: Money;
  readonly transacted_at: string;
}

/** `GET /ops/withdrawals?state=awaiting_approval`. */
export interface WithdrawalForApproval {
  readonly id: string;
  readonly account_id: string;
  readonly amount: Money;
  readonly reserved_amount: Money;
  readonly destination: PayoutDestination;
  readonly requested_at: string;
}

/** The three kinds of disagreement detection derives from a statement. */
export type DifferenceKind = 'reported_not_paid' | 'amount_mismatch' | 'paid_not_reported';

/** How an operator closed a difference. Neither verdict moves money. */
export type DifferenceResolution = 'explained' | 'absorbed';

/** `GET /ops/reconciliation/runs/{id}/differences`. */
export interface ReconciliationDifference {
  readonly id: string;
  readonly kind: DifferenceKind;
  /** Null where money matched no report at all. */
  readonly network_transaction_id: string | null;
  /** The network's own id, present either way. */
  readonly transaction_id: string;
  readonly expected: Money | null;
  readonly actual: Money | null;
  /** Paid less owed: negative for a shorted report, positive for an overpayment. */
  readonly delta: Money;
  /** The network has restated the report since the difference was filed. */
  readonly superseded: boolean;
  readonly resolution: {
    readonly resolution: DifferenceResolution;
    readonly resolved_by: string;
    readonly reason: string;
    readonly resolved_at: string;
  } | null;
}

/** A reconciliation run, as the runs list and the differences page name it. */
export interface ReconciliationRun {
  readonly id: string;
  readonly network_account_id: string;
  readonly period: { readonly start: string; readonly end: string };
  readonly imported_at: string;
  readonly difference_count: number;
  readonly open_count: number;
}
