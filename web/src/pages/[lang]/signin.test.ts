import { describe, expect, it, vi } from 'vitest';

import { memberStrings } from '../../lib/member/strings';
import { READING_LANGUAGES } from '../../lib/reader/axes';
import { uiStrings } from '../../lib/reader/strings';

/**
 * The member sign-in page, rendered for real (issue #529).
 *
 * Most of these cases assert an ABSENCE, which is unusual and is the point:
 * the page's whole job is to draw a sign-in that does not work and say so.
 * A form that posts to itself, an OAuth href, a password field or a
 * "check your email" would each turn an honest screen into a lie, and none
 * of them would fail a type check.
 */

const RENDER_TIMEOUT_MS = 20_000;

const LEAKS = ['undefined', 'NaN', '[object Object]', 'href=""', 'href="#"'];

async function renderSignIn(lang: string): Promise<string> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => ({
    APP_ENV: 'dev',
    BRAND_DIR: '',
    PUBLIC_APP_VERSION: undefined,
  }));
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create();
  const { forgetBrandOnHand } = await import('../../lib/brand/load');
  forgetBrandOnHand();
  const SignIn = (await import('./signin.astro')).default;
  return container.renderToString(SignIn, { params: { lang } });
}

describe.each(READING_LANGUAGES)('member sign-in in %s', (lang) => {
  it(
    'renders every control disabled',
    async () => {
      // The native attribute, not a class that looks the part: `disabled`
      // is what reaches assistive technology and what stops a click.
      const html = await renderSignIn(lang);
      const m = memberStrings(lang);

      expect(html).toContain(m.continueWithApp);
      expect(html).toContain(m.sendLink);
      // Two buttons and one input.
      expect((html.match(/disabled/g) ?? []).length).toBeGreaterThanOrEqual(3);
      expect(html).toMatch(/<input[^>]*type="email"[^>]*disabled|<input[^>]*disabled[^>]*type="email"/);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'makes no submittable claim',
    async () => {
      const html = await renderSignIn(lang);

      // A form with no action posts to itself; the page reloads and the
      // reader concludes something was sent.
      expect(html).not.toContain('<form');
      expect(html).not.toMatch(/\smethod=/i);
      expect(html).not.toMatch(/\saction=/i);
      // No password takes this route, and nothing here sends mail.
      expect(html).not.toContain('type="password"');
      expect(html).not.toContain('mailto:');
      expect(html).not.toMatch(/href="[^"]*(oauth|auth\/callback|\/auth\/)/i);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'says what is not wired, in the reader language',
    async () => {
      const html = await renderSignIn(lang);
      const t = uiStrings(lang);
      const m = memberStrings(lang);

      expect(html).toContain(m.notWorkingTitle);
      // As a heading, not a bold span. Every control here is disabled and
      // so out of the tab order; the heading outline is the only way a
      // screen-reader user reaches what this page exists to say.
      expect(html).toMatch(
        new RegExp(`<h2[^>]*>\s*${m.notWorkingTitle}\s*</h2>`),
      );
      expect(html).toContain(m.notWorkingBody);
      // Read from the catalogues rather than retyped, so this case cannot
      // disagree with the page about what it promises.
      expect(html).toContain(m.frontPageButtonState(t.signIn));
      // signInPending is that button's title, not its face. Quoting it
      // here would tell a reader to look for words the button does not
      // show unless they hover it.
      expect(html).not.toContain(m.frontPageButtonState(t.signInPending));
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'leaks no value it never had',
    async () => {
      const html = await renderSignIn(lang);

      for (const leak of LEAKS) {
        // `href="#"` is exactly how a decorative OAuth button would appear.
        expect(html, `rendered ${leak}`).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'is not dressed as a member surface',
    async () => {
      // MemberBar's links are the member surfaces; the subject of this page
      // is that membership is not available yet.
      const html = await renderSignIn(lang);

      expect(html).not.toContain('section-links');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'names no vendor',
    async () => {
      const html = await renderSignIn(lang);

      // Both names, because the page reached for a neighbouring string
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
