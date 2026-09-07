import { describe, expect, it, vi } from 'vitest';

import { cashbackStrings } from '../../i18n/cashback';
import { memberStrings } from '../../lib/member/strings';
import { profileStrings } from '../../lib/profile/strings';
import { READING_LANGUAGES } from '../../lib/reader/axes';
import { uiStrings } from '../../lib/reader/strings';

/**
 * The member profile, rendered (issue #610).
 *
 * Two promises are pinned harder than the rest.
 *
 * The page must render for somebody who IS signed in. #583 shipped because
 * every case in a suite rendered as a stranger — the one visitor the page
 * was already right for — so the signed-in branch here is reached by filing
 * a session under the request, exactly as `signin.test.ts` now does.
 *
 * And it must not invent. Four fields the design boards draw have no source
 * in this repository; a case below asserts the page says nothing about them
 * rather than filling them with something plausible.
 */

const RENDER_TIMEOUT_MS = 20_000;

const LEAKS = ['undefined', 'NaN', '[object Object]', 'href=""', 'href="#"'];

/** A preview or a development run: no api, so the client answers fixtures. */
const FIXTURES = {
  APP_ENV: 'dev',
  API_BASE_URL: undefined,
  BRAND_DIR: '',
  PUBLIC_APP_VERSION: undefined,
};

/**
 * A deployment with an api named, so the client is live rather than fixtures.
 * `.invalid` never resolves (RFC 2606), which is exactly the outage wanted.
 *
 * APP_ENV stays `dev`. `prod` would be the more faithful deployment and it
 * demands a BRAND_DIR — correctly, since the legal notices would otherwise
 * name a company that does not exist — and this suite is about the profile,
 * not about the brand loader.
 */
const LIVE = { ...FIXTURES, API_BASE_URL: 'https://api.invalid' };

async function load(env: Record<string, unknown>): Promise<{
  container: Awaited<ReturnType<typeof importContainer>>;
  Profile: unknown;
}> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => env);
  const container = await importContainer();
  const { forgetBrandOnHand } = await import('../../lib/brand/load');
  forgetBrandOnHand();
  const Profile = (await import('./profile.astro')).default;
  return { container, Profile };
}

async function importContainer(): ReturnType<
  typeof import('astro/container').experimental_AstroContainer.create
> {
  const { experimental_AstroContainer } = await import('astro/container');
  return experimental_AstroContainer.create();
}

async function request(
  lang: string,
  options: { env?: Record<string, unknown>; signedInAs?: string; cookie?: string } = {},
): Promise<Request> {
  const headers = new Headers();
  if (options.cookie !== undefined) {
    headers.set('cookie', options.cookie);
  }
  const req = new Request(`https://example.invalid/${lang}/profile`, { headers });
  if (options.signedInAs !== undefined) {
    const { rememberSession } = await import('../../lib/editorial/session');
    rememberSession(req, {
      displayName: options.signedInAs,
      email: options.signedInAs,
      role: 'reader',
      token: 'a-live-token',
      authenticated: true,
    });
  }
  return req;
}

async function render(
  lang: string,
  options: { env?: Record<string, unknown>; signedInAs?: string; cookie?: string } = {},
): Promise<string> {
  const { container, Profile } = await load(options.env ?? FIXTURES);
  const req = await request(lang, options);
  return container.renderToString(Profile as never, { params: { lang }, request: req });
}

async function respond(
  lang: string,
  options: { env?: Record<string, unknown>; signedInAs?: string } = {},
): Promise<Response> {
  const { container, Profile } = await load(options.env ?? FIXTURES);
  const req = await request(lang, options);
  return container.renderToResponse(Profile as never, { params: { lang }, request: req });
}

describe.each(READING_LANGUAGES)('the profile in %s', (lang) => {
  const t = profileStrings(lang);
  const ui = uiStrings(lang);

  it(
    'names the three sections the boards draw',
    async () => {
      const html = await render(lang);

      expect(html).toContain(t.identityHeading);
      expect(html).toContain(t.readingHeading);
      expect(html).toContain(t.activityHeading);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'shows the address, under the label this repository already owns',
    async () => {
      // Not a second email label. `member/strings.ts` states the rule and
      // `profile/strings.ts` keeps it; this is the render that proves it.
      const html = await render(lang);

      expect(html).toContain(ui.emailLabel);
      expect(html).toContain('member@example.test');
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'says where the address came from, and not what the boards say',
    async () => {
      // The correction. The boards tell the reader we keep none of this
      // because a shell hands us a token; there is no shell and the address
      // is in our own account row.
      const html = await render(lang);

      expect(html).toContain(t.identityNote);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'offers the way out, posting to the page that owns sign-out',
    async () => {
      const html = await render(lang);

      expect(html).toContain(memberStrings(lang).signOut);
      expect(html).toMatch(/method="post"/i);
      expect(html).toContain('name="action" value="signout"');
      expect(html).toContain(`action="/${lang}/signin`);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'moves the language and no other axis',
    async () => {
      // FR-009. The switcher changes one axis; a place must not ride along.
      const html = await render(lang);
      const other = lang === 'el' ? 'de' : 'el';

      expect(html).toContain(`href="/${other}/profile"`);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'offers a way to choose places when none has been recorded',
    async () => {
      // No preference cookie: the place-scoped links have no place, so the
      // page offers the screen that sets one rather than inventing München.
      const html = await render(lang);

      expect(html).toContain(t.choosePlaces);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'uses the places the reader last chose, and keeps this URL’s language',
    async () => {
      // The cookie carries both axes. Only the places are taken from it —
      // a German reader on /de/profile must not be handed Greek links.
      const html = await render(lang, { cookie: 'reader_axes=/el/munich' });

      expect(html).toContain('munich');
      expect(html).toContain(`/${lang}/munich/cashback`);
      expect(html).not.toContain(`/${lang === 'el' ? 'de' : 'el'}/munich/cashback`);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'states one of the three payout standings, never a blank chip',
    async () => {
      const html = await render(lang);
      const c = cashbackStrings(lang);
      const said = [c.verifiedPayoutsOn, t.payoutNotVerified, t.payoutNone].filter((s) =>
        html.includes(s),
      );

      expect(t.payoutLabel).toBeTruthy();
      expect(html).toContain(t.payoutLabel);
      expect(said.length, 'no payout standing was stated').toBeGreaterThan(0);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'leaks no value it never had',
    async () => {
      const html = await render(lang);

      for (const leak of LEAKS) {
        expect(html, `rendered ${leak}`).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('what the boards draw and this page has no source for', () => {
  it(
    'invents no member-since date, no theme, no device count, no consent tally',
    async () => {
      // Each of these has a board field and no column, route or setting
      // behind it. The page is silent rather than plausible.
      const html = await render('el');

      // A consent tally would read "N ... 3", the boards' "1 of 3 purposes".
      expect(html).not.toMatch(/\b\d+\s*\/\s*3\b/);
      // `account.Profile` carries no created date, so no year may be claimed.
      expect(html).not.toMatch(/Μέλος από/);
      for (const absent of ['Θέμα', 'Συσκευές', 'σκοπούς']) {
        expect(html, `mentions ${absent}`).not.toContain(absent);
      }
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('the fence', () => {
  it(
    'sends a stranger to sign in, and back here afterwards',
    async () => {
      // Outside the middleware's fence, which matches cashback paths by
      // their two leading segments. This route keeps its own.
      const response = await respond('el', { env: LIVE });

      expect(response.status).toBe(303);
      expect(response.headers.get('location')).toContain('/el/signin');
      expect(response.headers.get('location')).toContain(
        `next=${encodeURIComponent('/el/profile')}`,
      );
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'lets a preview through with no session at all',
    async () => {
      // A pull-request preview has no api and therefore no sign-in. Fencing
      // it would make this screen unreviewable, which is where it is
      // reviewed.
      const response = await respond('el');

      expect(response.status).toBe(200);
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('a signed-in member whose api is not answering', () => {
  it(
    'still says who they are signed in as, and says the rest is missing',
    async () => {
      // The whole reason the account read never throws. `api.invalid` does
      // not resolve, so both the account read and the figures fail.
      const html = await render('el', { env: LIVE, signedInAs: 'someone@example.test' });
      const t = profileStrings('el');

      expect(html).toContain(t.identityHeading);
      expect(html).toContain(t.accountUnavailable);
      expect(html).toContain(cashbackStrings('el').pageUnavailable);
      // And no crash, and no invented figures.
      for (const leak of LEAKS) {
        expect(html).not.toContain(leak);
      }
    },
    RENDER_TIMEOUT_MS,
  );
});

describe('a language this application does not serve', () => {
  it(
    'is rewritten to the not-found page rather than rendered',
    async () => {
      await expect(render('fr')).rejects.toThrow(/route \/404/);
    },
    RENDER_TIMEOUT_MS,
  );
});
