import { experimental_AstroContainer as AstroContainer } from 'astro/container';
import { describe, expect, it } from 'vitest';

import { cashbackStrings } from '../../../../i18n/cashback';
import { CATALOGUE_FIXTURES } from '../../../../lib/cashback/fixtures';
import { READING_LANGUAGES } from '../../../../lib/reader/axes';
import Merchant from './[slug].astro';

/**
 * The retailer page (issue #598), which had no rendered test at all.
 *
 * Two things it exists to pin. A refused click-out now says WHICH refusal,
 * because the six the api distinguishes want different moves and exactly one
 * of them — not having opted in — is the member's to act on. And every
 * published band has a button, because a click is tracked against an offer
 * and a band nobody can press is a rate nobody can earn.
 */

const RENDER_TIMEOUT_MS = 20_000;

/** A fixture retailer with more than one band, if the fixtures carry one. */
const MULTI = CATALOGUE_FIXTURES.find((m) => m.rates.length > 1);
/** One with exactly one, which is nearly every real retailer. */
const SINGLE = CATALOGUE_FIXTURES.find((m) => m.rates.length === 1);

async function render(path: string): Promise<string> {
  const container = await AstroContainer.create();
  const url = new URL(path, 'https://example.invalid');
  const [, lang, place, , slug] = url.pathname.split('/');
  return container.renderToString(Merchant, {
    request: new Request(url),
    params: { lang: lang ?? '', place: place ?? '', slug: slug ?? '' },
  });
}

describe.each(READING_LANGUAGES)('a refused click-out in %s', (lang) => {
  const t = cashbackStrings(lang);
  const slug = SINGLE?.slug ?? CATALOGUE_FIXTURES[0]?.slug ?? 'agora';

  it(
    'sends a member who has not opted in to the screen that fixes it',
    async () => {
      // The one refusal of the six they can act on. #568 made the api answer
      // 403 for it; #593 built the screen; this is the link between them.
      const html = await render(`/${lang}/munich/cashback/${slug}?clickout=403`);

      expect(html).toContain(t.clickoutNotJoined);
      expect(html).toContain(`href="/${lang}/munich/cashback/join"`);
      expect(html).toContain(t.optIn);
    },
    RENDER_TIMEOUT_MS,
  );

  it.each([
    [429, (s: typeof t) => s.clickoutTooMany],
    [409, (s: typeof t) => s.clickoutOfferGone],
    [502, (s: typeof t) => s.clickoutShopUnreachable],
  ])('says what %d actually was', async (status, sentence) => {
    const html = await render(`/${lang}/munich/cashback/${slug}?clickout=${status}`);

    expect(html).toContain(sentence(t));
    expect(html).toMatch(/role="alert"/);
  });

  it(
    'falls back to the general sentence for a status it has no words for',
    async () => {
      const html = await render(`/${lang}/munich/cashback/${slug}?clickout=418`);

      expect(html).toContain(t.clickoutFailed);
    },
    RENDER_TIMEOUT_MS,
  );

  it.each(['', 'banana', '-1', '200'])(
    'renders no refusal at all for ?clickout=%s',
    async (value) => {
      // A query parameter is somebody else's input. Anything that is not a
      // refusal status renders nothing rather than an empty band.
      const html = await render(`/${lang}/munich/cashback/${slug}?clickout=${value}`);

      expect(html).not.toContain(t.clickoutFailed);
      expect(html).not.toContain(t.clickoutNotJoined);
    },
  );

  it(
    'says nothing about a click-out when none was refused',
    async () => {
      const html = await render(`/${lang}/munich/cashback/${slug}`);

      expect(html).not.toContain(t.clickoutFailed);
      expect(html).not.toContain(t.clickoutNotJoined);
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('every published band is reachable', () => {
  it('is worth testing at all', () => {
    // If the fixtures ever stop carrying a multi-band retailer, the case
    // below would pass by rendering nothing. Fail loudly instead.
    expect(MULTI, 'no fixture retailer has more than one band').toBeDefined();
  });

  it(
    'gives a retailer with two bands a button for each offer',
    async () => {
      const slug = MULTI?.slug ?? '';
      const html = await render(`/el/munich/cashback/${slug}`);

      for (const rate of MULTI?.rates ?? []) {
        expect(html, `no control carries offer ${rate.offer_id}`).toContain(rate.offer_id);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'adds no second control to a retailer that publishes one band',
    async () => {
      // Nearly every retailer. The table gains no column and the page looks
      // exactly as it did.
      const slug = SINGLE?.slug ?? '';
      const html = await render(`/el/munich/cashback/${slug}`);

      // Targeted at the per-row control alone. Not the word — the primary
      // call to action `shopAtTracking` contains the same verb — and not
      // `name="offer_id"`, which that form's own hidden input carries.
      expect(html).not.toContain('open-band');
      expect(html).not.toContain('open-cell');
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('a language this application does not serve', () => {
  it(
    'is rewritten to the not-found page rather than rendered',
    async () => {
      await expect(render('/fr/munich/cashback/agora')).rejects.toThrow(/route \/404/);
    },
    RENDER_TIMEOUT_MS,
  );
});
