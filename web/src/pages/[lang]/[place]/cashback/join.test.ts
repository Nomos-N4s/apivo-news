import { experimental_AstroContainer as AstroContainer } from 'astro/container';
import { describe, expect, it } from 'vitest';

import { cashbackStrings } from '../../../../i18n/cashback';
import { TERMS_TEXT_VERSION } from '../../../../lib/legal/terms';
import { READING_LANGUAGES } from '../../../../lib/reader/axes';
import Join from './join.astro';

/**
 * The opt-in screen, rendered (issue #592).
 *
 * With no `API_BASE_URL` the client answers from fixtures, which report every
 * member as already participating — so without the fixture-mode branch this
 * page would render its "nothing to do" state and nothing else, on every
 * preview and every development run. That branch is what these cases mostly
 * exercise, and it is the state a reviewer sees.
 */

const RENDER_TIMEOUT_MS = 20_000;

async function render(path: string): Promise<string> {
  const container = await AstroContainer.create();
  const url = new URL(path, 'https://example.invalid');
  const [, lang, place] = url.pathname.split('/');
  return container.renderToString(Join, {
    request: new Request(url),
    params: { lang: lang ?? '', place: place ?? '' },
  });
}

describe.each(READING_LANGUAGES)('the opt-in in %s', (lang) => {
  it(
    'asks for the consent, and names the version being accepted',
    async () => {
      const html = await render(`/${lang}/munich/cashback/join`);
      const t = cashbackStrings(lang);

      expect(html).toContain(t.joinHeading);
      expect(html).toContain(t.optIn);
      // The fixture brand's version, which is what this render is against.
      expect(html).toMatch(/Με τη συμμετοχή αποδέχεσαι|Mit der Teilnahme nimmst du/);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'links the terms it is asking somebody to accept',
    async () => {
      // Accepting a version whose text is one click away is the least this
      // screen owes: the whole reason /{lang}/terms exists.
      const html = await render(`/${lang}/munich/cashback/join`);

      expect(html).toContain(cashbackStrings(lang).readTerms);
      expect(html).toContain(`href="/${lang}/terms"`);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'posts to itself, and only same-origin',
    async () => {
      const html = await render(`/${lang}/munich/cashback/join`);

      expect(html).toContain('<form');
      expect(html).toMatch(/method="post"/i);
      expect(html).toContain(`action="/${lang}/munich/cashback/join"`);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'refuses a cross-origin post rather than recording a consent',
    async () => {
      // A consent is a record with money behind it. `isSameOrigin` compares
      // hosts and refuses a request carrying neither Origin nor Referer.
      const container = await AstroContainer.create();
      const url = new URL(`https://example.invalid/${lang}/munich/cashback/join`);
      const response = await container.renderToResponse(Join, {
        request: new Request(url, {
          method: 'POST',
          headers: { origin: 'https://evil.invalid' },
        }),
        params: { lang, place: 'munich' },
      });

      expect(response.status).toBe(403);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'carries the reader’s two axes into every link it offers',
    async () => {
      // Language and place travel separately and neither is chosen on the
      // reader's behalf (Principle VII, FR-009).
      const html = await render(`/${lang}/munich+greece/cashback/join`);

      expect(html).toContain(`href="/${lang}/munich+greece/cashback"`);
      expect(html).not.toContain('/el/munich/cashback/join');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'leaks no value it never had',
    async () => {
      const html = await render(`/${lang}/munich/cashback/join`);

      for (const leak of ['undefined', 'NaN', '[object Object]', 'href=""']) {
        expect(html, `rendered ${leak}`).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'never posts the string "undefined" as somebody’s recorded consent',
    async () => {
      // `documents` is a map. A brand without a terms document must offer no
      // button at all rather than send the api a version that is not one.
      const html = await render(`/${lang}/munich/cashback/join`);

      expect(html).not.toContain('undefined');
      expect(TERMS_TEXT_VERSION).not.toBe('undefined');
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('a language this application does not serve', () => {
  it(
    'is rewritten to the not-found page rather than rendered',
    async () => {
      await expect(render('/fr/munich/cashback/join')).rejects.toThrow(/route \/404/);
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('a place this application does not serve', () => {
  it(
    'is rewritten too, rather than rendering an opt-in for nowhere',
    async () => {
      await expect(render('/el/atlantis/cashback/join')).rejects.toThrow(/route \/404/);
    },
    RENDER_TIMEOUT_MS,
  );
});
