import { experimental_AstroContainer as AstroContainer } from 'astro/container';
import { describe, expect, it } from 'vitest';

import Wallet from './wallet.astro';

/**
 * The wallet page, rendered (T085).
 *
 * US3 scenario 2: a reversed entry shows both the credit and the reversal,
 * with a reason. It is asserted against the rendered page rather than
 * against the client, because the requirement is about what a member can
 * see — a client that returns both rows and a page that renders one is
 * exactly the failure this test exists to catch.
 *
 * With no API_BASE_URL the page answers from fixtures, which carry the pair
 * on purpose.
 */
async function render(path: string): Promise<string> {
  const container = await AstroContainer.create();
  const url = new URL(path, 'http://localhost');
  const [, lang, place] = url.pathname.split('/');
  return container.renderToString(Wallet, {
    request: new Request(url),
    params: { lang: lang ?? '', place: place ?? '' },
  });
}

describe('the wallet page', () => {
  it('renders the credit and its reversal, both of them', async () => {
    const html = await render('/el/munich/cashback/wallet');
    // The fixture pair: a credit of 1,11 € and its reversal of -1,11 €.
    expect(html).toContain('+1,11');
    expect(html).toMatch(/[-−]1,11/);
  });

  it('gives the reversal its reason, in the record the member reads', async () => {
    const html = await render('/el/munich/cashback/wallet');
    expect(html).toContain('The shop refunded the order.');
  });

  it('names the entry it reverses, so the pair is readable as a pair', async () => {
    const html = await render('/el/munich/cashback/wallet');
    expect(html).toContain('fx-entry-3');
  });
});
