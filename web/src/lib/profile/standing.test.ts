import { describe, expect, it } from 'vitest';

import type { PayoutDestination } from '../cashback/types';
import { displayName, payoutStanding } from './standing';

/** A destination row, verified or not. */
function destination(verifiedAt: string | null, id = 'd1'): PayoutDestination {
  return {
    destination_id: id,
    kind: 'sepa',
    verified_at: verifiedAt,
    verified_method: verifiedAt === null ? null : 'micro_deposit',
    created_at: '2026-07-01T10:00:00Z',
  } as PayoutDestination;
}

describe('payoutStanding', () => {
  it('reports no destination as `none`, not as unverified', () => {
    // The state every member is in today, and the one the boards do not
    // draw. Calling it "not verified" would describe a row that is absent.
    expect(payoutStanding([])).toBe('none');
  });

  it('reports a destination nobody has proved as unverified', () => {
    expect(payoutStanding([destination(null)])).toBe('unverified');
  });

  it('reports a proved destination as verified', () => {
    expect(payoutStanding([destination('2026-08-02T09:00:00Z')])).toBe('verified');
  });

  it('finds a verified destination that is not the first one', () => {
    // The api promises no order, so reading `[0]` would make the answer
    // depend on which row came back first.
    const list = [
      destination(null, 'd1'),
      destination(null, 'd2'),
      destination('2026-08-02T09:00:00Z', 'd3'),
    ];

    expect(payoutStanding(list)).toBe('verified');
  });

  it('is verified on the timestamp even when the method is missing', () => {
    // The two are documented as moving together and the type allows them
    // to part. `verified_at` is what FR-051 turns on.
    const row = { ...destination('2026-08-02T09:00:00Z'), verified_method: null };

    expect(payoutStanding([row])).toBe('verified');
  });

  it('stays unverified when every row is unproved', () => {
    expect(payoutStanding([destination(null, 'a'), destination(null, 'b')])).toBe(
      'unverified',
    );
  });
});

describe('displayName', () => {
  it.each([
    ['null', null],
    ['undefined', undefined],
    ['an empty string', ''],
    ['only spaces', '   '],
    ['only a tab', '\t'],
  ])('has no name to show for %s', (_label, value) => {
    expect(displayName(value)).toBeNull();
  });

  it('trims a name that has one', () => {
    expect(displayName('  Nikos  ')).toBe('Nikos');
  });

  it('leaves an ordinary name alone', () => {
    expect(displayName('Nikos A.')).toBe('Nikos A.');
  });
});
