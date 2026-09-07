import { describe, expect, it, vi } from 'vitest';

import { TERMS_TEXT_VERSION, termsStrings } from '../../lib/legal/terms';
import { READING_LANGUAGES } from '../../lib/reader/axes';

/**
 * The cashback terms page, rendered for real (issue #589).
 *
 * Two things it exists to catch. First, that every clause the catalogue
 * carries actually reaches the page — a terms page that silently drops a
 * clause is worse than one that never had it. Second, the version check: the
 * brand names a version and this repository holds the words, and a member
 * must never be shown one and asked to accept the other.
 */

const RENDER_TIMEOUT_MS = 20_000;

const LEAKS = ['undefined', 'NaN', '[object Object]', 'href=""', 'href="#"'];

/** No BRAND_DIR: the fixture brand answers, as it does on a development run. */
const FIXTURE_ENV = { APP_ENV: 'dev', BRAND_DIR: '', PUBLIC_APP_VERSION: undefined };

async function renderTerms(
  lang: string,
  env: Record<string, unknown> = FIXTURE_ENV,
): Promise<string> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => env);
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create();
  const { forgetBrandOnHand } = await import('../../lib/brand/load');
  forgetBrandOnHand();
  const Terms = (await import('./terms.astro')).default;
  return container.renderToString(Terms, {
    params: { lang },
    request: new Request(`https://example.invalid/${lang}/terms`),
  });
}

describe.each(READING_LANGUAGES)('the cashback terms in %s', (lang) => {
  it(
    'renders every clause the catalogue carries',
    async () => {
      // Read from termsStrings rather than retyped, so this case cannot
      // disagree with the module about what the page is meant to say.
      const html = await renderTerms(lang);
      const t = termsStrings(lang);

      expect(html).toContain(t.heading);
      expect(html).toContain(t.intro);
      for (const clause of t.clauses) {
        expect(html, `heading "${clause.heading}"`).toContain(clause.heading);
        for (const line of [...clause.body, ...clause.points]) {
          expect(html, `${clause.heading}: "${line}"`).toContain(line);
        }
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'gives every clause a real heading, not a bold span',
    async () => {
      // Thirteen clauses are what somebody navigates by, and a span is not
      // navigable. One h1 for the page, an h2 for each clause.
      const html = await renderTerms(lang);
      const t = termsStrings(lang);

      expect(html.match(/<h1[^>]*>/g) ?? []).toHaveLength(1);
      expect(html.match(/<h2[^>]*>/g) ?? []).toHaveLength(t.clauses.length);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'names the version the words were written for',
    async () => {
      const html = await renderTerms(lang);

      expect(html).toContain(termsStrings(lang).versionLine(TERMS_TEXT_VERSION));
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'says so when the brand is a fixture, and does not cry drift over one',
    async () => {
      /*
       * The fixture brand names a version picked for a test, not a
       * deployment's claim about the terms it holds anybody to. Comparing
       * against it would refuse every development run and every preview for
       * no reason, so the check is skipped and the page says whose brand it
       * is instead.
       */
      const html = await renderTerms(lang);
      const t = termsStrings(lang);

      expect(html).toContain(t.fixtureTitle);
      expect(html).toContain(t.fixtureBody);
      expect(html).not.toContain(t.driftTitle);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'refuses to vouch for words the deployment does not name, and says both numbers',
    async () => {
      /*
       * The case the whole version check exists for. A real brand directory
       * makes `source` a deployment's rather than a fixture's, and this one
       * names a version these words were not written against — so the page
       * says so, names both numbers, and the opt-in built on it will not
       * collect consent while they disagree.
       */
      const html = await renderTerms(lang, {
        APP_ENV: 'dev',
        BRAND_DIR: '../internal/platform/brand/testdata/fixture',
        PUBLIC_APP_VERSION: undefined,
      });
      const t = termsStrings(lang);

      expect(html).toContain(t.driftTitle);
      expect(html).toContain(TERMS_TEXT_VERSION);
      // The brand's own number, which is not this text's.
      expect(html).toContain('3.1.0');
      // A deployment's brand is not a fixture, so that note is absent.
      expect(html).not.toContain(t.fixtureBody);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'leaks no value it never had',
    async () => {
      const html = await renderTerms(lang);

      for (const leak of LEAKS) {
        expect(html, `rendered ${leak}`).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'is readable without a member: no member bar, no sign-in wall',
    async () => {
      // Somebody reads this BEFORE joining. `isCashbackPath` needs two
      // segments before `cashback`, so this route is outside the fence —
      // and it must not dress itself as a member surface either.
      const html = await renderTerms(lang);

      expect(html).not.toContain('section-links');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'marks one language, links the other, and changes no axis (FR-009)',
    async () => {
      const html = await renderTerms(lang);
      const other = lang === 'el' ? 'de' : 'el';

      expect(html.match(/aria-current="true"/g) ?? []).toHaveLength(1);
      expect(html).toContain(`href="/${other}/terms"`);
      expect(html).toContain(`href="/${lang}/munich+greece"`);
      const foreign = html.match(new RegExp(`href="/${other}/[^"]*"`, 'g')) ?? [];
      expect(foreign).toEqual([`href="/${other}/terms"`]);
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('a language this application does not serve', () => {
  it(
    'is rewritten to the not-found page rather than rendered',
    async () => {
      // The container carries no route table to rewrite into, so reaching
      // that error is the proof the FR-015 guard fired.
      await expect(renderTerms('fr')).rejects.toThrow(/route \/404/);
    },
    RENDER_TIMEOUT_MS,
  );
});
