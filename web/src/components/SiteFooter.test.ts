import { beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * These cases render a real Astro component through the container, and
 * the first one pays the whole cold start: a module-registry reset, the
 * container import, its creation, and a first render. Idle that is about
 * two seconds; on a loaded machine it crosses vitest's five-second
 * default and the suite goes red over work that was merely slow (#132).
 * The bound stays well above the real cost so a genuine hang still fails.
 */
const RENDER_TIMEOUT_MS = 20_000;

// The footer's one piece of logic: whether this deployment can name the
// release it is running (issue #119). It is rendered for real here - the
// Astro container renders the component itself - because the bug being
// pinned is a render bug: an empty PUBLIC_APP_VERSION drew the separator
// with nothing after it, so the page showed "© 2026 epiloYES ·" and the
// reader was told there is a version that the deployment cannot name.
//
// PUBLIC_APP_VERSION is declared `access: 'secret'`, i.e. read from the
// environment at request time through astro:env/server. Under the container
// that module resolves to nothing whatever process.env holds, so the value
// is supplied by mocking the module - the same seam the component imports.

async function renderFooter(version: string | undefined): Promise<string> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => ({ PUBLIC_APP_VERSION: version }));
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create();
  const SiteFooter = (await import('./SiteFooter.astro')).default;
  return container.renderToString(SiteFooter, { props: { lang: 'de' } });
}

/** The rendered version chip, or undefined when the footer drew none. */
function versionChip(html: string): string | undefined {
  const match = html.match(/<span class="version"[^>]*>([^<]*)<\/span>/);
  return match?.[1];
}

/**
 * The copyright line's own text - the element the version chip sits in.
 * Asserted whole, because the bug was punctuation left behind inside it;
 * the footer's legal line uses the same separator character legitimately,
 * so only this element's content answers the question.
 */
function copyLine(html: string): string {
  const match = html.match(/<span class="copy"[^>]*>(.*?)<\/span><\/div>/);
  if (match?.[1] === undefined) {
    throw new Error(`the footer rendered no copyright line: ${html}`);
  }
  return match[1];
}

describe('SiteFooter version', () => {
  beforeEach(() => {
    vi.doUnmock('astro:env/server');
  });

  it(
    'renders the deployed version after a separator',
    async () => {
      const html = await renderFooter('v1.2.3');
      expect(versionChip(html)).toBe('· v1.2.3');
    },
    RENDER_TIMEOUT_MS,
  );

  it('trims the value it renders', async () => {
    const html = await renderFooter('  v1.2.3\n');
    expect(versionChip(html)).toBe('· v1.2.3');
  });

  it(
    'renders nothing at all when no version is set',
    async () => {
      const html = await renderFooter(undefined);
      expect(versionChip(html)).toBeUndefined();
      expect(copyLine(html)).toMatch(/^© \d{4} epiloYES$/);
    },
    RENDER_TIMEOUT_MS,
  );

  // The regression: an empty string is not a version, and neither is
  // whitespace. Both must render exactly what an unset variable renders -
  // not a separator with nothing after it.
  it.each([
    ['empty', ''],
    ['whitespace', '   '],
  ])(
    'renders no dangling separator for a %s version',
    async (_name, value) => {
      const html = await renderFooter(value);
      expect(versionChip(html)).toBeUndefined();
      expect(copyLine(html)).toMatch(/^© \d{4} epiloYES$/);
    },
    RENDER_TIMEOUT_MS,
  );
});
