import { beforeAll, describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from '../lib/reader/axes';

/**
 * The member section bar, rendered for real.
 *
 * The defect it exists to close: the reader's front page drew a bare
 * masthead with no section links, while the cashback pages drew a bar that
 * included a link back to the news. A member could therefore reach the news
 * from the catalogue and never the catalogue from the news — the one
 * direction nobody tested, because nothing renders `.astro` files in CI.
 *
 * These cases assert the three destinations and the current-page marker in
 * every alpha language, which is the whole contract of a nav.
 */

const RENDER_TIMEOUT_MS = 20_000;

type Container = { renderToString: (component: unknown, options: unknown) => Promise<string> };

let container: Container;
let MemberBar: unknown;

beforeAll(async () => {
  const { experimental_AstroContainer } = await import('astro/container');
  container = (await experimental_AstroContainer.create()) as unknown as Container;
  MemberBar = (await import('./MemberBar.astro')).default;
}, RENDER_TIMEOUT_MS);

const SLUGS = ['munich', 'greece'];
const SECTIONS = ['news', 'catalogue', 'wallet', 'merchant'] as const;

function render(props: Record<string, unknown>): Promise<string> {
  return container.renderToString(MemberBar, { props });
}

describe.each(READING_LANGUAGES)('the member bar in %s', (lang) => {
  const home = `/${lang}/munich+greece`;

  it.each(SECTIONS)(
    'reaches every section while on %s',
    async (current) => {
      const html = await render({ lang, slugs: SLUGS, current });

      expect(html, 'no way to the news').toContain(`href="${home}"`);
      expect(html, 'no way to the catalogue').toContain(`href="${home}/cashback"`);
      expect(html, 'no way to the wallet').toContain(`href="${home}/cashback/wallet"`);
    },
    RENDER_TIMEOUT_MS,
  );

  it.each(SECTIONS)(
    'marks exactly one destination as the current page on %s',
    async (current) => {
      // Never two — which reads to a screen reader as being in two places —
      // and never none, which is what the news page had while the bar
      // hard-coded `on: false` for its own news link.
      const html = await render({ lang, slugs: SLUGS, current });

      expect(html.match(/aria-current="page"/g) ?? []).toHaveLength(1);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'keeps the reader on the places they chose',
    async () => {
      // FR-009: chrome never changes an axis the reader did not touch, so
      // every section link carries the same place segment it was given.
      const html = await render({ lang, slugs: ['munich'], current: 'news' });

      expect(html).toContain(`href="/${lang}/munich/cashback"`);
      expect(html).not.toContain('munich+greece');
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('what the bar does not do', () => {
  it(
    'names itself for the nav it is, not for one of its destinations',
    async () => {
      // It was labelled "cashback" while it served one product. On the news
      // page that would announce a reader into the cashback nav.
      const html = await render({ lang: 'de', slugs: SLUGS, current: 'news' });

      expect(html).toContain('aria-label="Bereiche"');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'renders no aside and no dateline unless given one',
    async () => {
      const html = await render({ lang: 'de', slugs: SLUGS, current: 'news' });

      expect(html).not.toContain('class="aside"');
      expect(html).not.toContain('class="date"');
      expect(html).not.toContain('undefined');
    },
    RENDER_TIMEOUT_MS,
  );
});
