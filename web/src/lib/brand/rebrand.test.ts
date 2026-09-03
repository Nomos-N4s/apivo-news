import { experimental_AstroContainer as AstroContainer } from 'astro/container';
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

import Catalogue from '../../pages/[lang]/[place]/cashback/index.astro';
import Merchant from '../../pages/[lang]/[place]/cashback/[slug].astro';
import Wallet from '../../pages/[lang]/[place]/cashback/wallet.astro';
import Withdraw from '../../pages/[lang]/[place]/cashback/withdraw.astro';
import Held from '../../pages/ops/held.astro';
import Reconciliation from '../../pages/ops/reconciliation.astro';
import Unattributed from '../../pages/ops/unattributed.astro';
import Withdrawals from '../../pages/ops/withdrawals.astro';
import { BRAND_FILE_NAME, parseBrand } from './index';

/**
 * Rebrandability, as a test that goes red (SC-007).
 *
 * The constitution's claim is that a rebrand is a configuration change plus
 * an asset swap, and that "rebrandability is a test that goes red, not a
 * claim in a document". `scripts/lint-brand-literals.sh` greps the source
 * for the current brand's strings; this renders the surfaces and looks at
 * what actually reaches a browser, which is a different question. A colour
 * written into a scoped `<style>` block is not a brand *name*, so the lint
 * does not see it — and it is exactly the thing a rebrand has to change.
 *
 * **Scope, stated rather than implied.** This covers the cashback surfaces.
 * The shared masthead and site footer render the product name directly,
 * because this repository shipped a news product before it had a brand
 * configuration; driving those to zero is issue #275, and until it lands a
 * whole-page assertion over the member surfaces would fail on chrome this
 * change did not write. So the member pages are asserted over their own
 * `<main>`, and the operator pages — which have no shared chrome — whole.
 */

const FIXTURE_BRAND = parseBrand(
  readFileSync(
    new URL(`../../../../internal/platform/brand/testdata/fixture/${BRAND_FILE_NAME}`, import.meta.url),
    'utf8',
  ),
);

async function render(
  page: Parameters<AstroContainer['renderToString']>[0],
  path: string,
): Promise<string> {
  const container = await AstroContainer.create();
  const url = new URL(path, 'http://localhost');
  const [, lang, place, , slug] = url.pathname.split('/');
  return container.renderToString(page, {
    request: new Request(url),
    params: { lang: lang ?? '', place: place ?? '', slug: slug ?? '' },
  });
}

/** The page's own body, with the shared masthead and footer removed. */
function ownContent(html: string): string {
  const main = /<main[\s\S]*?<\/main>/.exec(html);
  const styles = [...html.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)].map((m) => m[1] ?? '');
  return `${main?.[0] ?? ''}\n${styles.join('\n')}`;
}

const MEMBER_SURFACES = [
  ['wallet', Wallet, '/el/munich/cashback/wallet'],
  ['catalogue', Catalogue, '/el/munich/cashback'],
  ['merchant', Merchant, '/el/munich/cashback/agora'],
  ['withdrawal', Withdraw, '/el/munich/cashback/withdraw'],
] as const;

const OPERATOR_SURFACES = [
  ['unattributed', Unattributed, '/ops/unattributed'],
  ['held', Held, '/ops/held'],
  ['withdrawal approvals', Withdrawals, '/ops/withdrawals'],
  ['reconciliation', Reconciliation, '/ops/reconciliation'],
] as const;

/** The `<style>` blocks of a component or page, as written. */
function styleBlocks(relativePath: string): string {
  const source = readFileSync(new URL(`../../${relativePath}`, import.meta.url), 'utf8');
  return [...source.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)]
    .map((match) => match[1] ?? '')
    .join('\n');
}

/**
 * Every file this change added that carries styles.
 *
 * Asserted against the SOURCE rather than the render, and the difference
 * matters: Astro extracts scoped styles into a stylesheet at build time, so
 * a container render carries markup and no CSS at all. A "no hex in the
 * rendered output" assertion would therefore pass on a page whose every
 * colour was hardcoded, which is the one failure this test exists to catch.
 */
const STYLED_SOURCES = [
  'pages/[lang]/[place]/cashback/wallet.astro',
  'pages/[lang]/[place]/cashback/index.astro',
  'pages/[lang]/[place]/cashback/[slug].astro',
  'pages/[lang]/[place]/cashback/withdraw.astro',
  'pages/ops/held.astro',
  'pages/ops/unattributed.astro',
  'pages/ops/withdrawals.astro',
  'pages/ops/reconciliation.astro',
  'components/CashbackNav.astro',
  'components/OpsChrome.astro',
  'components/RateBadge.astro',
  'layouts/OpsLayout.astro',
];

describe('the cashback surfaces carry no colour of their own', () => {
  it.each(STYLED_SOURCES)('resolves every colour in %s from a token', (relativePath) => {
    const css = styleBlocks(relativePath);
    // A literal colour is a colour a rebrand cannot change. The theme's
    // colours arrive as CSS custom properties from the brand configuration,
    // so these surfaces must reach for a var() and never for a hex.
    expect(css).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(css).not.toMatch(/\b(?:rgb|hsl)a?\(/);
    expect(css).not.toMatch(/\b(?:aqua|fuchsia|lime|maroon|navy|olive|teal|silver)\b/);
  });

  it('does reach for the tokens, so the assertion above is not vacuous', () => {
    for (const relativePath of STYLED_SOURCES) {
      expect(styleBlocks(relativePath), relativePath).toMatch(/var\(--/);
    }
  });

  it('names no font family of its own either', () => {
    // Typography is brand configuration too - --font-heading and
    // --font-body come from the same block the colours do. The monospace
    // stack is the exception and is deliberate: a reference, an IBAN and a
    // network id are read character by character, which is a property of
    // the data rather than a decision about the brand.
    for (const relativePath of STYLED_SOURCES) {
      const css = styleBlocks(relativePath).replace(
        /font-family:\s*ui-monospace[^;]*;/g,
        '',
      );
      expect(css, relativePath).not.toMatch(/font-family:\s*(?!var\()/);
    }
  });
});

describe('the operator surfaces name no brand at all', () => {
  const brandStrings = [
    FIXTURE_BRAND.name,
    FIXTURE_BRAND.legal.entity,
    FIXTURE_BRAND.domains.primary,
    FIXTURE_BRAND.support.general,
    FIXTURE_BRAND.payout.descriptor,
  ];

  it.each(OPERATOR_SURFACES)('renders %s without one', async (_name, page, path) => {
    const rendered = await render(page, path);
    // Nothing here should print a brand value, which is why swapping the
    // brand cannot break these pages: they have no opinion about it. A
    // future surface that DOES need the name must take it from the loaded
    // brand, and this test is where that shows up as a deliberate change.
    for (const value of brandStrings) {
      expect(rendered).not.toContain(value);
    }
  });
});

describe('the member surfaces name no brand in their own content', () => {
  const brandStrings = [
    FIXTURE_BRAND.name,
    FIXTURE_BRAND.legal.entity,
    FIXTURE_BRAND.domains.primary,
    FIXTURE_BRAND.support.general,
  ];

  it.each(MEMBER_SURFACES)('renders %s without one', async (_name, page, path) => {
    // Over the page's own <main>, not the whole document. The shared
    // masthead and footer print the product name directly - this
    // repository shipped a news product before it had a brand
    // configuration - and driving those to zero is issue #275. Asserting
    // over the whole page would fail on chrome this change did not write,
    // and the honest scope is what these pages themselves render.
    const own = ownContent(await render(page, path));
    expect(own).not.toBe('');
    for (const value of brandStrings) {
      expect(own).not.toContain(value);
    }
  });
});

describe('the fixture brand is loadable and complete', () => {
  it('parses, so a rebrand has something to swap in', () => {
    expect(FIXTURE_BRAND.id).not.toBe('');
    expect(FIXTURE_BRAND.theme.colours.accent).toMatch(/^#[0-9a-f]{3,8}$/i);
    expect(FIXTURE_BRAND.defaults.currency).toMatch(/^[A-Z]{3}$/);
  });

  it('is denominated in a currency the money formatter can render', () => {
    // SC-007's point is that a different brand renders every surface, and a
    // brand carries its own currency default. A formatter that only knew
    // the euro would fail here rather than in production.
    expect(FIXTURE_BRAND.defaults.currency).not.toBe('EUR');
  });
});
