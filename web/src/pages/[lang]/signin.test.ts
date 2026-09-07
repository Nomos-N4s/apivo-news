import { describe, expect, it, vi } from 'vitest';

import { memberStrings } from '../../lib/member/strings';
import { READING_LANGUAGES } from '../../lib/reader/axes';
import { uiStrings } from '../../lib/reader/strings';

/**
 * The member sign-in page, rendered for real (issues #529, #564).
 *
 * This file used to assert an absence: no form, no method, no action, three
 * disabled controls. That was the right test for a page whose whole job was
 * to draw a sign-in that did not work and say so, and it is the wrong test
 * for the page that replaced it. It has been rewritten rather than relaxed —
 * the new promise is as specific as the old one and is pinned as tightly.
 *
 * What the page promises now:
 *
 *   * the email path works, so there IS a form and it posts
 *   * the shell path does not, so its button is still natively `disabled`
 *   * no password field, ever, in either state
 *   * a deployment with no auth configured says so and sends nothing
 *   * the language switcher moves the language and no other axis (FR-009)
 */

const RENDER_TIMEOUT_MS = 20_000;

const LEAKS = ['undefined', 'NaN', '[object Object]', 'href=""', 'href="#"'];

/** A deployment with an auth provider configured, and one without. */
const CONFIGURED = {
  APP_ENV: 'dev',
  BRAND_DIR: '',
  PUBLIC_APP_VERSION: undefined,
  PUBLIC_SUPABASE_URL: 'https://project.invalid',
  PUBLIC_SUPABASE_ANON_KEY: 'anon-key-for-a-render',
};

const UNCONFIGURED = { ...CONFIGURED, PUBLIC_SUPABASE_URL: '', PUBLIC_SUPABASE_ANON_KEY: '' };

async function renderSignIn(
  lang: string,
  options: { query?: string; env?: Record<string, unknown> } = {},
): Promise<string> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => options.env ?? CONFIGURED);
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create();
  const { forgetBrandOnHand } = await import('../../lib/brand/load');
  forgetBrandOnHand();
  const SignIn = (await import('./signin.astro')).default;
  const url = `https://example.invalid/${lang}/signin${options.query ?? ''}`;
  return container.renderToString(SignIn, { params: { lang }, request: new Request(url) });
}

describe.each(READING_LANGUAGES)('member sign-in in %s', (lang) => {
  it(
    'offers a form that posts, because the email path works now',
    async () => {
      const html = await renderSignIn(lang);
      const m = memberStrings(lang);

      expect(html).toContain('<form');
      expect(html).toMatch(/method="post"/i);
      expect(html).toContain(`action="/${lang}/signin"`);
      expect(html).toContain(m.sendLink);
      expect(html).toContain(m.emailPathNote);
      // Enabled: a required email field a member can actually type into.
      expect(html).toMatch(/<input[^>]*type="email"/);
      expect(html).not.toMatch(/<input[^>]*type="email"[^>]*disabled/);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'keeps the shell path disabled, and says so beside it',
    async () => {
      const html = await renderSignIn(lang);
      const m = memberStrings(lang);

      // The native attribute, not a class that looks the part: `disabled`
      // is what reaches assistive technology and what stops a click.
      expect(html).toMatch(new RegExp(`<button[^>]*disabled[^>]*>\\s*${m.continueWithApp}`));
      expect(html).toContain(m.tokenExchangeNote);
      expect(html).toContain(m.notWorkingTitle);
      // As a heading, not a bold span: it is the landmark by which a
      // screen-reader user reaches what this page still owes.
      expect(html).toMatch(new RegExp(`<h2[^>]*>\\s*${m.notWorkingTitle}\\s*</h2>`));
      expect(html).toContain(m.notWorkingBody);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'asks for no password, in any state',
    async () => {
      // The board draws none and the shell note rules one out in as many
      // words. No password is also why this product owns no reset screen.
      for (const query of ['', '?outcome=expired', '?outcome=refused']) {
        const html = await renderSignIn(lang, { query });
        expect(html, query).not.toContain('type="password"');
        expect(html, query).not.toContain('autocomplete="current-password"');
        expect(html, query).not.toContain('autocomplete="new-password"');
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'says a deployment with no auth configured sends nothing, and disables the field',
    async () => {
      const html = await renderSignIn(lang, { env: UNCONFIGURED });
      const m = memberStrings(lang);

      expect(html).toContain(m.notConfigured);
      expect(html).toMatch(/<input[^>]*type="email"[^>]*disabled/);
      // A refusal a member can hear: the band carries role="alert" rather
      // than sitting in the page as ordinary prose.
      expect(html).toMatch(/role="alert"/);
    },
    RENDER_TIMEOUT_MS,
  );

  it.each([
    ['expired', (lang: (typeof READING_LANGUAGES)[number]) => memberStrings(lang).linkExpired],
    ['refused', (lang: (typeof READING_LANGUAGES)[number]) => memberStrings(lang).sendRefused],
  ])(
    'renders the %s outcome a redirect handed it',
    async (outcome, sentence) => {
      // /auth/confirm renders nothing and has no language of its own, so it
      // hands an outcome to this page and this page owns the sentence.
      const html = await renderSignIn(lang, { query: `?outcome=${outcome}` });

      expect(html).toContain(sentence(lang));
      expect(html).toMatch(/role="alert"/);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'ignores an outcome it has no sentence for, rather than rendering the word',
    async () => {
      const html = await renderSignIn(lang, { query: '?outcome=haxxed' });

      expect(html).not.toContain('haxxed');
      expect(html).not.toMatch(/role="alert"/);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'carries a return path through the form and the switcher, and refuses one it would not honour',
    async () => {
      const kept = await renderSignIn(lang, {
        query: '?next=%2Fel%2Fmunich%2Fcashback%2Fwallet',
      });
      expect(kept).toContain('next=%2Fel%2Fmunich%2Fcashback%2Fwallet');

      // An open redirect is the one thing a `next` parameter is for, if
      // nobody checks it. `//evil.example` starts with a slash.
      const refused = await renderSignIn(lang, { query: '?next=%2F%2Fevil.example' });
      expect(refused).not.toContain('evil.example');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'leaks no value it never had',
    async () => {
      const html = await renderSignIn(lang);

      for (const leak of LEAKS) {
        expect(html, `rendered ${leak}`).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'is not dressed as a member surface',
    async () => {
      // MemberBar's links are the member surfaces, and somebody on this page
      // has not got one yet.
      const html = await renderSignIn(lang);

      expect(html).not.toContain('section-links');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'names no vendor',
    async () => {
      const html = await renderSignIn(lang);

      // Both names, because the page once reached for a neighbouring string
      // that carried the second one and this case did not notice.
      for (const vendor of ['evreos', 'supabase']) {
        expect(html.toLowerCase(), `rendered ${vendor}`).not.toContain(vendor);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'marks one language, links the other, and changes no axis (FR-009)',
    async () => {
      const html = await renderSignIn(lang);
      const other = lang === 'el' ? 'de' : 'el';

      expect(html.match(/aria-current="true"/g) ?? []).toHaveLength(1);
      expect(html).toContain(`href="/${other}/signin"`);
      expect(html).toContain(`href="/${lang}/munich+greece"`);
      // The switcher is the only link into the other language.
      const foreign = html.match(new RegExp(`href="/${other}/[^"]*"`, 'g')) ?? [];
      expect(foreign).toEqual([`href="/${other}/signin"`]);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'labels the email field with the label this repository already owns',
    async () => {
      const html = await renderSignIn(lang);

      expect(html).toContain(uiStrings(lang).emailLabel);
      expect(html).toContain('for="member-email"');
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
      await expect(renderSignIn('fr')).rejects.toThrow(/route \/404/);
    },
    RENDER_TIMEOUT_MS,
  );
});
