import { beforeAll, describe, expect, it } from 'vitest';

import { PLACE_CATALOG, READING_LANGUAGES, frontPagePath } from '../lib/reader/axes';
import { uiStrings } from '../lib/reader/strings';

/**
 * The chrome components, rendered for real (issue #64 follow-up).
 *
 * `.astro` files are excluded from the coverage gate — deliberately, they
 * are mostly layout — so nothing in CI renders them at all. Three defects
 * reached a live preview through that hole in one afternoon: a footer that
 * printed "Imprint · Privacy · Contact — still owed" as dead text for a
 * month after the pages existed; `padding: var(--space-7)` naming a token
 * the scale never carried, which computes to `0`; and a link that set no
 * font-size and inherited the body's, rendering at 15px between two 11.5px
 * neighbours.
 *
 * A static test catches the second (see styles/tokens.test.ts). This one
 * catches the shape of the first and third: it renders each component in
 * both alpha languages and asserts what the markup must never contain — a
 * leaked `undefined`, an empty href, a stringified object — plus, per
 * component, the links that are the reason it exists.
 *
 * It is deliberately not a snapshot suite. A snapshot of a design still
 * being drawn is a test that fails on every intentional change and is
 * therefore updated without being read.
 */

const RENDER_TIMEOUT_MS = 20_000;

type Container = { renderToString: (component: unknown, options: unknown) => Promise<string> };

let container: Container;

// Imported by name rather than by interpolated path: vite's
// dynamic-import-vars cannot analyse `./${name}.astro` and warns that a
// variable import may not read its own directory. An explicit map is also
// the list of what this suite covers, which is worth being able to read.
const COMPONENTS: Record<string, () => Promise<{ default: unknown }>> = {
  Masthead: () => import('./Masthead.astro'),
  AxisBar: () => import('./AxisBar.astro'),
  FixtureNotice: () => import('./FixtureNotice.astro'),
  EmptyPlaceBand: () => import('./EmptyPlaceBand.astro'),
  ProvenanceDisclosure: () => import('./ProvenanceDisclosure.astro'),
  RecordNotice: () => import('./RecordNotice.astro'),
  SiteFooter: () => import('./SiteFooter.astro'),
};

beforeAll(async () => {
  const { experimental_AstroContainer } = await import('astro/container');
  container = (await experimental_AstroContainer.create()) as unknown as Container;
}, RENDER_TIMEOUT_MS);

async function render(name: string, props: Record<string, unknown>): Promise<string> {
  const load = COMPONENTS[name];
  if (load === undefined) {
    throw new Error(`chrome.test.ts has no import for ${name}`);
  }
  return container.renderToString((await load()).default, { props });
}

/** Values that mean a template read something that was not there. */
const LEAKS = ['undefined', 'NaN', '[object Object]', 'href=""', 'href="#"'];

const places = PLACE_CATALOG.filter((place) => place.selectable).slice(0, 2);

/** One representative prop set per component, per language. */
function casesFor(lang: (typeof READING_LANGUAGES)[number]): readonly [string, Record<string, unknown>][] {
  const t = uiStrings(lang);
  return [
    ['Masthead', { homeHref: frontPagePath(lang, ['munich', 'greece']) }],
    ['AxisBar', { lang, places }],
    ['FixtureNotice', { title: t.fixtureNoticeTitle, body: t.fixtureNoticeBody }],
    ['EmptyPlaceBand', { title: t.emptyPlaceTitle, body: t.emptyPlaceBody('München') }],
    [
      'ProvenanceDisclosure',
      {
        label: t.provenance,
        rows: [
          { term: t.source, value: 'Abendzeitung' },
          { term: t.attribution, value: 'example.invalid', href: 'https://example.invalid/a' },
        ],
      },
    ],
    [
      'RecordNotice',
      {
        notice: { tone: 'recorded', label: t.approved, body: t.reassurance, record: ['id=1'] },
      },
    ],
    ['SiteFooter', { lang }],
  ];
}

describe.each(READING_LANGUAGES)('chrome rendered in %s', (lang) => {
  it.each(casesFor(lang).map(([name, props]) => [name, props] as const))(
    '%s renders without leaking a value it never had',
    async (name, props) => {
      const html = await render(name, props);

      expect(html.length).toBeGreaterThan(0);
      for (const leak of LEAKS) {
        expect(html, `${name} rendered ${leak}`).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('the links each chrome component exists to carry', () => {
  it(
    'the footer reaches the three legal notices, about, and the editorial way in',
    async () => {
      // Every one of these was added because somebody could not get
      // somewhere: the editorial link because staff were typing the URL
      // (commit b998473), the legal three because they were named but not
      // linked (#64).
      const html = await render('SiteFooter', { lang: 'de' });

      for (const href of [
        '/de/impressum',
        '/de/privacy',
        '/de/contact',
        '/de/about',
        '/de/editor/signin',
      ]) {
        expect(html, `footer no longer links ${href}`).toContain(`href="${href}"`);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'the masthead leads home, in the language it was given',
    async () => {
      // FR-009: chrome never changes an axis the reader did not touch, so
      // the German masthead may not lead to a Greek front page.
      const html = await render('Masthead', { homeHref: frontPagePath('de', ['munich']) });

      expect(html).toContain('href="/de/munich"');
      expect(html).not.toContain('href="/el/');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'the axis bar keeps the two axes separate and names each place in its own language',
    async () => {
      const html = await render('AxisBar', { lang: 'el', places });

      // Principle VII: two axes, never one locale. Switching language must
      // hold the places, so the German link carries the same place segment.
      expect(html).toContain('href="/de/munich+greece"');
      // WCAG 3.1.2: an endonym inside a page of the other language carries
      // its own lang, so "München" is announced as German on a Greek page.
      expect(html).toContain('lang="de"');
      for (const place of places) {
        expect(html, `axis bar dropped ${place.slug}`).toContain(place.endonym);
      }
    },
    RENDER_TIMEOUT_MS,
  );
});
