import { beforeEach, describe, expect, it, vi } from 'vitest';

import { READING_LANGUAGES } from '../../lib/reader/axes';
import { faqStrings } from '../../lib/faq/strings';

/**
 * The FAQ page, rendered for real (issue #528).
 *
 * `.astro` files are outside the coverage gate, so nothing else in CI renders
 * this page. What these cases protect is the render contract: every question
 * and every referenced answer actually reaches the HTML, the language
 * switcher marks one language and links the other, and nothing leaks a value
 * the template never had.
 */

const RENDER_TIMEOUT_MS = 20_000;

/** Values that mean a template read something that was not there. */
const LEAKS = ['undefined', 'NaN', '[object Object]', 'href=""', 'href="#"'];

async function renderFaq(lang: string): Promise<string> {
  vi.resetModules();
  // `astro:env/server` resolves to nothing under the container, and
  // brandOnHand refuses to fall back to the fixture brand when APP_ENV is
  // prod — so the environment is supplied here, as a development one.
  vi.doMock('astro:env/server', () => ({
    APP_ENV: 'dev',
    BRAND_DIR: '',
    PUBLIC_APP_VERSION: undefined,
  }));
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create();
  // brandOnHand memoises per process; forget it so a mocked environment is
  // not answered from a previous test's resolution.
  const { forgetBrandOnHand } = await import('../../lib/brand/load');
  forgetBrandOnHand();
  const Faq = (await import('./faq.astro')).default;
  return container.renderToString(Faq, { params: { lang } });
}

describe.each(READING_LANGUAGES)('the FAQ in %s', (lang) => {
  beforeEach(() => {
    vi.resetModules();
  });

  it(
    'renders every question',
    async () => {
      const html = await renderFaq(lang);

      for (const section of faqStrings(lang).sections) {
        expect(html, `section heading ${section.heading}`).toContain(section.heading);
        for (const entry of section.entries) {
          expect(html, `question "${entry.question}"`).toContain(entry.question);
        }
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'renders every answer the catalogues already ship',
    async () => {
      // Read from faqStrings rather than retyped, so this case cannot
      // disagree with the module about what the page is meant to say.
      const html = await renderFaq(lang);

      for (const section of faqStrings(lang).sections) {
        for (const entry of section.entries) {
          for (const paragraph of entry.answer) {
            expect(html, `answer "${paragraph.slice(0, 40)}…"`).toContain(paragraph);
          }
        }
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'leaks no value it never had',
    async () => {
      const html = await renderFaq(lang);

      for (const leak of LEAKS) {
        expect(html, `rendered ${leak}`).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'marks one language and links the other',
    async () => {
      const html = await renderFaq(lang);
      const other = lang === 'el' ? 'de' : 'el';

      expect(html.match(/aria-current="true"/g) ?? []).toHaveLength(1);
      expect(html).toContain(`href="/${other}/faq"`);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'reaches contact without changing the reader language (FR-009)',
    async () => {
      const html = await renderFaq(lang);
      const other = lang === 'el' ? 'de' : 'el';

      expect(html).toContain(`href="/${lang}/contact"`);
      // The only link to the other language is the switcher itself.
      const foreign = html.match(new RegExp(`href="/${other}/[^"]*"`, 'g')) ?? [];
      expect(foreign).toEqual([`href="/${other}/faq"`]);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'is not dressed as a member surface',
    async () => {
      // MemberBar's links are place-scoped; this route has no place axis,
      // and choosing one for the reader is what Principle VII forbids.
      const html = await renderFaq(lang);

      expect(html).not.toContain('section-links');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'offers no assistant and names no shell',
    async () => {
      // The board's closing panel hands off to the evreos assistant. Neither
      // exists here, and the canvas gives the assistant to the shell.
      const html = await renderFaq(lang);

      expect(html.toLowerCase()).not.toContain('evreos');
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('a language this application does not serve', () => {
  it(
    'is rewritten to the not-found page rather than rendered',
    async () => {
      // FR-015: only the alpha languages mount. The page answers by calling
      // `Astro.rewrite('/404')`, and the container carries no route table to
      // rewrite INTO — so reaching that error is the proof the guard fired.
      // Asserting the throw is the only way to observe the guard here; a
      // rendered heading would mean it did not.
      await expect(renderFaq('fr')).rejects.toThrow(/route \/404/);
    },
    RENDER_TIMEOUT_MS,
  );
});
