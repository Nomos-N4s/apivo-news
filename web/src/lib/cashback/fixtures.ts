import type {
  CatalogueItem,
  HeldEntry,
  MerchantDetail,
  PayoutDestination,
  ReconciliationDifference,
  ReconciliationRun,
  UnattributedTransaction,
  WalletEntry,
  WalletTotals,
  Withdrawal,
  WithdrawalForApproval,
} from './types';

/**
 * The development preview's data.
 *
 * Every surface that renders these says so out loud, and `createCashbackApi`
 * refuses to serve them in a deployed environment — the same refusal the
 * reader client makes, for a sharper reason. An invented publisher misleads
 * a reader about who wrote something; an invented balance tells somebody
 * they are owed money they are not owed.
 *
 * Two rules these fixtures keep, because a preview that breaks them teaches
 * the wrong shape:
 *
 *   1. **Online tracked links only.** The design mockups lean heavily on
 *      card-linked in-store purchases, which the constitution puts out of
 *      scope and for which no evidence path exists (issue #445). A fixture
 *      showing one would be a screen nothing in the backend can produce.
 *   2. **The totals close.** confirmed + pending + reserved and the entry
 *      list agree, so a preview never demonstrates arithmetic the real
 *      wallet cannot reproduce.
 */

const EUR = 'EUR';

/** The fixture entries, newest first. */
export const ENTRY_FIXTURES: readonly WalletEntry[] = [
  {
    entry_id: 'fx-entry-1',
    merchant_name: 'Agora',
    merchant_name_language: 'el',
    merchant_name_is_fallback: false,
    transacted_at: '2026-08-21T14:12:00Z',
    sale_amount: { minor: 8430, currency: EUR },
    cashback_amount: { minor: 422, currency: EUR },
    state: 'pending',
    expected_confirmation_at: '2026-09-20T00:00:00Z',
    hold_rule: null,
    reversal_of_id: null,
    reason: null,
  },
  {
    entry_id: 'fx-entry-2',
    merchant_name: 'Efimeria',
    merchant_name_language: 'de',
    merchant_name_is_fallback: true,
    transacted_at: '2026-08-18T09:30:00Z',
    sale_amount: { minor: 4600, currency: EUR },
    cashback_amount: { minor: 184, currency: EUR },
    state: 'confirmed',
    expected_confirmation_at: null,
    hold_rule: null,
    reversal_of_id: null,
    reason: null,
  },
  {
    entry_id: 'fx-entry-3',
    merchant_name: 'Agora',
    merchant_name_language: 'el',
    merchant_name_is_fallback: false,
    transacted_at: '2026-07-12T18:04:00Z',
    sale_amount: { minor: 2210, currency: EUR },
    cashback_amount: { minor: 111, currency: EUR },
    state: 'reversed',
    expected_confirmation_at: null,
    hold_rule: null,
    reversal_of_id: null,
    reason: null,
  },
  {
    // The reversal of fx-entry-3, and its own row. The pair is the point:
    // US3 scenario 2 requires both the credit and the reversal to be
    // visible, with the reason, so the preview carries a pair rather than a
    // single tidy entry.
    entry_id: 'fx-entry-4',
    merchant_name: 'Agora',
    merchant_name_language: 'el',
    merchant_name_is_fallback: false,
    transacted_at: '2026-07-19T11:00:00Z',
    sale_amount: { minor: 2210, currency: EUR },
    cashback_amount: { minor: -111, currency: EUR },
    state: 'reversed',
    expected_confirmation_at: null,
    hold_rule: null,
    reversal_of_id: 'fx-entry-3',
    reason: 'The shop refunded the order.',
  },
  {
    entry_id: 'fx-entry-5',
    merchant_name: 'Kritikos',
    merchant_name_language: 'el',
    merchant_name_is_fallback: false,
    transacted_at: '2026-06-28T08:15:00Z',
    sale_amount: { minor: 1740, currency: EUR },
    cashback_amount: { minor: 87, currency: EUR },
    state: 'paid',
    expected_confirmation_at: null,
    hold_rule: null,
    reversal_of_id: null,
    reason: null,
  },
  {
    // Held, and naming the rule holding it. The totals do not count held
    // money at all, so a member who sees this row and no matching figure is
    // reading the truth rather than a rounding error.
    entry_id: 'fx-entry-6',
    merchant_name: 'Agora',
    merchant_name_language: 'el',
    merchant_name_is_fallback: false,
    transacted_at: '2026-08-24T16:40:00Z',
    sale_amount: { minor: 31200, currency: EUR },
    cashback_amount: { minor: 1560, currency: EUR },
    state: 'held',
    expected_confirmation_at: null,
    hold_rule: 'basket_above_threshold',
    reversal_of_id: null,
    reason: null,
  },
  {
    // Attributed by hand, so there is no click and no retailer to name
    // (FR-034). The contract sends null here and the page has to have
    // something to say; a preview that never produces the null would let a
    // blank line reach a member unnoticed.
    entry_id: 'fx-entry-7',
    merchant_name: null,
    merchant_name_language: null,
    merchant_name_is_fallback: false,
    transacted_at: '2026-08-02T10:05:00Z',
    sale_amount: { minor: 5900, currency: EUR },
    cashback_amount: { minor: 295, currency: EUR },
    state: 'pending',
    expected_confirmation_at: null,
    hold_rule: null,
    reversal_of_id: null,
    reason: null,
  },
];

/** Totals that agree with the entries above. */
export const WALLET_FIXTURE: WalletTotals = {
  pending: { minor: 422, currency: EUR },
  confirmed: { minor: 184, currency: EUR },
  reserved: { minor: 0, currency: EUR },
  paid_out: { minor: 87, currency: EUR },
  payout_threshold: { minor: 1000, currency: EUR },
};

/** Fixture merchants. Online, tracked-link, with dated bands. */
export const CATALOGUE_FIXTURES: readonly MerchantDetail[] = [
  {
    merchant_id: 'fx-merchant-1',
    slug: 'agora',
    name: 'Agora',
    name_language: 'el',
    name_is_fallback: false,
    summary: 'Greek groceries, delivered across Germany and Austria.',
    country: 'DE',
    terms: 'Delivery within Germany and Austria only. Marketplace sellers are the seller of record.',
    typical_confirmation_days: null,
    rates: [
      {
        offer_id: 'fx-offer-1',
        kind: 'percent',
        bps: 500,
        conditions: 'Applies to the basket total excluding delivery.',
        exclusions: 'Gift cards and deposits are excluded.',
        valid_to: null,
      },
    ],
  },
  {
    merchant_id: 'fx-merchant-2',
    slug: 'efimeria',
    name: 'Efimeria',
    name_language: 'de',
    name_is_fallback: true,
    summary: 'Online pharmacy shipping to Germany.',
    country: 'DE',
    terms: 'Prescription medicines are dispensed under German pharmacy law and are never eligible.',
    typical_confirmation_days: null,
    rates: [
      {
        offer_id: 'fx-offer-2',
        kind: 'percent',
        bps: 400,
        conditions: null,
        exclusions: 'Prescription items are excluded by law; the rest tracks normally.',
        valid_to: '2026-12-31T23:59:59Z',
      },
    ],
  },
  {
    merchant_id: 'fx-merchant-3',
    slug: 'kritikos',
    name: 'Kritikos',
    name_language: 'el',
    name_is_fallback: false,
    summary: 'Bakery goods shipped weekly.',
    country: 'GR',
    terms: 'Shipped weekly from Crete. Perishable goods are not returnable.',
    typical_confirmation_days: null,
    rates: [
      {
        offer_id: 'fx-offer-3',
        kind: 'fixed',
        amount: { minor: 250, currency: EUR },
        conditions: 'One credit per order.',
        exclusions: null,
        valid_to: null,
      },
    ],
  },
  {
    // A retailer whose bands have lapsed. 200 with an empty list, not 404:
    // a shop that pays nothing today is still a shop that exists, and the
    // catalogue has to render it without inventing a rate.
    merchant_id: 'fx-merchant-4',
    slug: 'mikri-patrida',
    name: 'Mikri Patrida',
    name_language: 'el',
    name_is_fallback: false,
    summary: 'Deli and traiteur, ships within Bavaria.',
    country: 'DE',
    terms: null,
    typical_confirmation_days: null,
    rates: [],
  },
];

/** The listing view of the same merchants. */
export const CATALOGUE_LIST_FIXTURES: readonly CatalogueItem[] = CATALOGUE_FIXTURES;

/** One verified destination and one that is not — the 409 path has a subject. */
export const DESTINATION_FIXTURES: readonly PayoutDestination[] = [
  {
    destination_id: 'fx-destination-1',
    kind: 'sepa',
    verified_at: '2026-07-02T10:00:00Z',
    verified_method: 'micro_deposit',
    created_at: '2026-06-28T09:12:00Z',
  },
  {
    // Added later and never proved. The withdrawal offers it and refuses to
    // let it be chosen, which is the state a member most needs drawn.
    destination_id: 'fx-destination-2',
    kind: 'sepa',
    verified_at: null,
    verified_method: null,
    created_at: '2026-08-14T17:41:00Z',
  },
];

/** A withdrawal in the state every withdrawal starts in. */
export const WITHDRAWAL_FIXTURES: readonly Withdrawal[] = [
  {
    request_id: 'fx-withdrawal-1',
    destination_id: 'fx-destination-1',
    state: 'awaiting_approval',
    amount: { minor: 1500, currency: EUR },
    requested_at: '2026-08-24T09:46:00Z',
    decided_at: null,
    decision_reason: null,
    payout_reference: null,
  },
  {
    request_id: 'fx-withdrawal-2',
    destination_id: 'fx-destination-1',
    state: 'rejected',
    amount: { minor: 2000, currency: EUR },
    requested_at: '2026-07-30T12:00:00Z',
    decided_at: '2026-08-01T08:20:00Z',
    decision_reason: 'The destination name did not match the account holder.',
    payout_reference: null,
  },
];

/** Operator queues. */
export const UNATTRIBUTED_FIXTURES: readonly UnattributedTransaction[] = [
  {
    id: 'fx-unattributed-1',
    network_id: 'fx-network-1',
    external_id: 'NX-55120',
    sale: { minor: 8430, currency: EUR },
    commission: { minor: 843, currency: EUR },
    transacted_at: '2026-08-21T14:12:00Z',
    attributable: true,
  },
  {
    // The network named a reference that matched no click. Only dismissal
    // is lawful here, and the queue says so rather than offering a button
    // the database will refuse.
    id: 'fx-unattributed-2',
    network_id: 'fx-network-1',
    external_id: 'NX-55121',
    sale: { minor: 3100, currency: EUR },
    commission: { minor: 310, currency: EUR },
    transacted_at: '2026-08-22T08:02:00Z',
    attributable: false,
  },
];

export const HELD_FIXTURES: readonly HeldEntry[] = [
  {
    id: 'fx-held-1',
    account_id: 'fx-account-2',
    network_transaction_id: 'fx-nt-9',
    click_id: 'fx-click-9',
    amount: { minor: 4200, currency: EUR },
    held_since: '2026-08-19T06:00:00Z',
    hold_rule: 'basket_above_threshold',
    hold_reason: 'Basket above the review threshold for a first purchase.',
    network_id: 'fx-network-1',
    external_id: 'NX-55090',
    report_status: 'pending',
    sale: { minor: 84000, currency: EUR },
    commission: { minor: 8400, currency: EUR },
    transacted_at: '2026-08-18T21:40:00Z',
  },
];

export const WITHDRAWAL_APPROVAL_FIXTURES: readonly WithdrawalForApproval[] = [
  {
    request_id: 'fx-withdrawal-1',
    account_id: 'fx-account-1',
    account_email: 'member@example.test',
    // More than any round number a member would have typed, because the
    // real one is: entries are reserved whole, so the figure that leaves
    // the ledger is rarely the figure that was asked for.
    reserved_amount: { minor: 1840, currency: EUR },
    destination: {
      ...(DESTINATION_FIXTURES[0] as PayoutDestination),
      details_ref: 'openbao:secret/cashback/payout-destinations/fx01',
    },
    requested_at: '2026-08-24T09:46:00Z',
  },
];

export const RECONCILIATION_RUN_FIXTURES: readonly ReconciliationRun[] = [
  {
    id: 'fx-run-1',
    network_account_id: 'fx-network-account-1',
    period: { start: '2026-07-01T00:00:00Z', end: '2026-08-01T00:00:00Z' },
    imported_at: '2026-08-03T07:15:00Z',
    difference_count: 3,
    open_count: 2,
  },
];

export const DIFFERENCE_FIXTURES: readonly ReconciliationDifference[] = [
  {
    id: 'fx-difference-1',
    kind: 'reported_not_paid',
    network_transaction_id: 'fx-nt-1',
    transaction_id: 'NX-54001',
    expected: { minor: 843, currency: EUR },
    actual: null,
    delta: { minor: -843, currency: EUR },
    superseded: false,
    resolution: null,
  },
  {
    id: 'fx-difference-2',
    kind: 'amount_mismatch',
    network_transaction_id: 'fx-nt-2',
    transaction_id: 'NX-54002',
    expected: { minor: 500, currency: EUR },
    actual: { minor: 430, currency: EUR },
    delta: { minor: -70, currency: EUR },
    superseded: false,
    resolution: null,
  },
  {
    id: 'fx-difference-3',
    kind: 'paid_not_reported',
    network_transaction_id: null,
    transaction_id: 'NX-54003',
    expected: null,
    actual: { minor: 220, currency: EUR },
    delta: { minor: 220, currency: EUR },
    superseded: true,
    resolution: {
      resolution: 'explained',
      resolved_by: 'operator@example.invalid',
      reason: 'The network restated the report and the payment matches it.',
      resolved_at: '2026-08-05T11:20:00Z',
    },
  },
];
