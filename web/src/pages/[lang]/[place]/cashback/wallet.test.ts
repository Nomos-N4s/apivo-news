import { experimental_AstroContainer as AstroContainer } from 'astro/container';
import { describe, expect, it } from 'vitest';

import Wallet from './wallet.astro';
import { cashbackStrings, languageName } from '../../../../i18n/cashback';

const t = cashbackStrings('el');

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

  /*
   * The three facts `GET /wallet/entries` sends that this page used not to
   * be told (#554). Each is asserted on the rendered page rather than on the
   * client, for the same reason the reversal above is: a client that carries
   * the field and a page that drops it is exactly the failure worth catching.
   */

  it('says which language a name it could not translate is in', async () => {
    // fx-entry-2's retailer has no Greek copy, so the German name answered.
    const html = await render('/el/munich/cashback/wallet?entry=fx-entry-2');
    expect(html).toContain(t.shownInLanguage(languageName('el', 'de')));
  });

  it('names the rule holding a held entry back, not merely that it is held', async () => {
    const html = await render('/el/munich/cashback/wallet?entry=fx-entry-6');
    expect(html).toContain('basket_above_threshold');
    expect(html).toContain(t.holdRule);
  });

  it('says there is no retailer rather than leaving the space blank', async () => {
    // Attributed by hand: no click, so no route back to a shop (FR-034).
    const html = await render('/el/munich/cashback/wallet?entry=fx-entry-7');
    expect(html).toContain(t.noMerchant);
    expect(html).toContain(t.noMerchantExplained);
  });

  it('offers reserved as a state a member can read', async () => {
    const html = await render('/el/munich/cashback/wallet');
    // `declined` was never a state the api produces; `reserved` always was.
    expect(html).not.toContain('Απορρίφθηκε');
    expect(t.entryStates.reserved.trim()).not.toBe('');
  });
});
