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

async function renderFooter(
  version: string | undefined,
  role?: 'reader' | 'editor' | 'operator',
): Promise<string> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => ({ PUBLIC_APP_VERSION: version }));
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create();
  const SiteFooter = (await import('./SiteFooter.astro')).default;

  // The footer reads the session the middleware filed against the request,
  // so the request is the seam: file one here, or file nothing and get the
  // fail-closed answer a plain reader page gets.
  const request = new Request('https://example.invalid/de/munich');
  if (role !== undefined) {
    const { rememberSession } = await import('../lib/editorial/session');
    rememberSession(request, {
      displayName: 'A. Operator',
      email: 'op@example.invalid',
      role,
      token: 'token',
      authenticated: true,
    });
  }
  return container.renderToString(SiteFooter, { props: { lang: 'de' }, request });
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

describe('the legal notices', () => {
  // For a month this line printed "Impressum · Datenschutz · Kontakt —
  // vor dem öffentlichen Start erforderlich" as dead text in front of
  // readers, because the pages did not exist (issue #64). They exist now.
  // The regression this pins is the return of that state: a German-facing
  // service that names its Impressum without linking it is in the same
  // position as one that never mentioned it.
  it(
    'links all three rather than naming them',
    async () => {
      const html = await renderFooter('1.0.0');

      expect(html).toContain('href="/de/impressum"');
      expect(html).toContain('href="/de/privacy"');
      expect(html).toContain('href="/de/contact"');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'no longer says they are still owed',
    async () => {
      const html = await renderFooter('1.0.0');

      expect(html).not.toMatch(/erforderlich|εκκρεμούν/);
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('the operator way in', () => {
  // Nothing linked /ops from any surface: an operator typed the URL. That is
  // the same failure the editorial link was added to fix in b998473.
  it(
    'is offered to an operator',
    async () => {
      const html = await renderFooter('1.0.0', 'operator');

      expect(html).toContain('/ops/unattributed');
      expect(html).toContain('Betrieb');
    },
    RENDER_TIMEOUT_MS,
  );

  it.each(['reader', 'editor'] as const)('is not offered to a %s', async (role) => {
    const html = await renderFooter('1.0.0', role);

    expect(html).not.toContain('/ops/');
  });

  it(
    'is not offered on a page where no identity was resolved',
    async () => {
      // The middleware resolves identity only on the editorial and cashback
      // paths, so a plain reader page files nothing. The footer must fail
      // closed there rather than assume the last role it saw.
      const html = await renderFooter('1.0.0');

      expect(html).not.toContain('/ops/');
    },
    RENDER_TIMEOUT_MS,
  );
});
