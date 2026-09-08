import type { PayoutDestination } from '../cashback/types';

/**
 * Where a member stands with payouts (issue #610).
 *
 * The design boards draw two chips, verified and not. There are three
 * states, and the one they leave out is the one every member is in:
 * **no destination at all**. Nothing in this repository can create one —
 * the canvas draws no form and none is built — so `none` is not an edge
 * case here, it is the normal answer, and a profile that showed "not
 * verified" for it would describe a destination that does not exist.
 *
 * The rule is `verified_at`, never `verified_method`. Both are documented as
 * moving together, but only the first is what FR-051 turns on; reading the
 * method would make a row with a timestamp and a null method — which the
 * type permits — silently unverified.
 */
export type PayoutStanding = 'verified' | 'unverified' | 'none';

/**
 * The standing a list of destinations adds up to.
 *
 * One verified destination is enough: money can reach them. That is why
 * this is `some` and not "the first one" — the api returns a collection in
 * no promised order, so picking `[0]` would make the answer depend on the
 * order rows come back in.
 */
export function payoutStanding(
  destinations: readonly PayoutDestination[],
): PayoutStanding {
  if (destinations.length === 0) {
    return 'none';
  }
  return destinations.some((destination) => destination.verified_at !== null)
    ? 'verified'
    : 'unverified';
}

/**
 * The name to show, or null when there is none worth showing.
 *
 * `display_name` is a plain column with no NOT NULL default behind it, and
 * the register path fills it from whatever the provider supplied — which is
 * often nothing. A blank or whitespace-only name must render as "not set"
 * rather than as an empty row that looks like a rendering fault.
 */
export function displayName(name: string | undefined | null): string | null {
  if (typeof name !== 'string') {
    return null;
  }
  const trimmed = name.trim();
  return trimmed === '' ? null : trimmed;
}
