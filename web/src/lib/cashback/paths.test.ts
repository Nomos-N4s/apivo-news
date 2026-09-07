import { describe, expect, it } from 'vitest';

import {
  cataloguePath,
  joinPath,
  merchantPath,
  OPS_PATHS,
  placeSegment,
  walletPath,
  withdrawPath,
} from './paths';

describe('the cashback URL scheme', () => {
  it('builds the opt-in under the same two axes', () => {
    expect(joinPath('el', ['munich'])).toBe('/el/munich/cashback/join');
    expect(joinPath('de', ['munich', 'greece'])).toBe('/de/munich+greece/cashback/join');
  });

  it('collides with a retailer slugged the same, and that is the trade', () => {
    // These two build the SAME url, and pinning that is the point: Astro
    // resolves the static route before the dynamic one, so a retailer ever
    // slugged `join` becomes unreachable rather than standing where the
    // opt-in should. That is the right way round for the failure to fall —
    // the worse outcome is a member sent to a shop when they meant to accept
    // the terms — and `wallet` and `withdraw` already make the same trade.
    expect(joinPath('el', ['munich'])).toBe(merchantPath('el', ['munich'], 'join'));
  });

  it('carries language and place as two separate segments', () => {
    expect(cataloguePath('el', ['munich'])).toBe('/el/munich/cashback');
    // Never a combined locale: the two axes are independent (VII).
    expect(cataloguePath('el', ['munich'])).not.toMatch(/el-DE|el_DE/);
  });

  it('joins several places the way the reader pages join them', () => {
    expect(placeSegment(['munich', 'greece'])).toBe('munich+greece');
    expect(walletPath('de', ['munich', 'greece'])).toBe('/de/munich+greece/cashback/wallet');
  });

  it('puts the wallet and the withdrawal under the catalogue prefix', () => {
    expect(walletPath('el', ['munich'])).toBe('/el/munich/cashback/wallet');
    expect(withdrawPath('el', ['munich'])).toBe('/el/munich/cashback/withdraw');
  });

  it('escapes a merchant slug rather than letting it climb the path', () => {
    expect(merchantPath('el', ['munich'], '../wallet')).toBe(
      '/el/munich/cashback/..%2Fwallet',
    );
    expect(merchantPath('el', ['munich'], 'agora')).toBe('/el/munich/cashback/agora');
  });

  it('gives the operator queues no axes at all', () => {
    for (const path of Object.values(OPS_PATHS)) {
      expect(path.startsWith('/ops/')).toBe(true);
      expect(path).not.toMatch(/\/(el|de)\//);
    }
  });
});
