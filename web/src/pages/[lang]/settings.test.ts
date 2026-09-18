import { describe, expect, it, vi } from 'vitest';

import { CONSENT_PURPOSES } from '../../lib/account/consent';
import { profileStrings } from '../../lib/profile/strings';
import { READING_LANGUAGES } from '../../lib/reader/axes';
import { uiStrings } from '../../lib/reader/strings';
import { settingsStrings } from '../../lib/settings/strings';

/**
 * The member settings screen, rendered (issue #617).
 *
 * Two promises carry the weight.
 *
 * A consent write that did not happen must SAY it did not happen. There is
 * no consent endpoint; `createAccountApi` answers `recorded: false` with a
 * reason for every write. A screen that swallowed that and looked like it
 * had recorded something would be lying about the one thing on the page
 * that is evidence.
 *
 * And the notifications section must not look like a feature. The seam
 * board's rule for everything in its "Neither, yet" column is: named as
 * owed, never mocked up as working.
 */

const RENDER_TIMEOUT_MS = 20_000;

const LEAKS = ['undefined', 'NaN', '[object Object]', 'href=""', 'href="#"'];

/** A preview or a development run: no api, so the clients answer fixtures. */
const FIXTURES = {
  APP_ENV: 'dev',
  API_BASE_URL: undefined,
  BRAND_DIR: '',
  PUBLIC_APP_VERSION: undefined,
};

/** An api is named, so the cashback client is live. `.invalid` never resolves. */
const LIVE = { ...FIXTURES, API_BASE_URL: 'https://api.invalid' };

async function load(env: Record<string, unknown>): Promise<{
  container: Awaited<ReturnType<typeof makeContainer>>;
  Settings: unknown;
}> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => env);
  const container = await makeContainer();
  const { forgetBrandOnHand } = await import('../../lib/brand/load');
  forgetBrandOnHand();
  const Settings = (await import('./settings.astro')).default;
  return { container, Settings };
}

async function makeContainer(): ReturnType<
  typeof import('astro/container').experimental_AstroContainer.create
> {
  const { experimental_AstroContainer } = await import('astro/container');
  return experimental_AstroContainer.create();
}

interface Options {
  env?: Record<string, unknown>;
  signedInAs?: string;
  cookie?: string;
  post?: Record<string, string>;
  origin?: string | null;
}

async function build(lang: string, options: Options): Promise<Request> {
  const url = `https://example.invalid/${lang}/settings`;
  const headers = new Headers();
  if (options.cookie !== undefined) {
    headers.set('cookie', options.cookie);
  }
  let request: Request;
  if (options.post === undefined) {
    request = new Request(url, { headers });
  } else {
    const form = new FormData();
    for (const [key, value] of Object.entries(options.post)) {
      form.set(key, value);
    }
    const origin = options.origin === undefined ? 'https://example.invalid' : options.origin;
    if (origin !== null) {
      headers.set('origin', origin);
    }
    request = new Request(url, { method: 'POST', body: form, headers });
  }
  if (options.signedInAs !== undefined) {
    // Filed by hand: the middleware does not run in a container render, and
    // a suite where every case is a stranger is how #583 shipped.
    const { rememberSession } = await import('../../lib/editorial/session');
    rememberSession(request, {
      displayName: options.signedInAs,
      email: options.signedInAs,
      role: 'reader',
      token: 'a-live-token',
      authenticated: true,
    });
  }
  return request;
}

async function render(lang: string, options: Options = {}): Promise<string> {
  const { container, Settings } = await load(options.env ?? FIXTURES);
  return container.renderToString(Settings as never, {
    params: { lang },
    request: await build(lang, options),
  });
}

async function respond(lang: string, options: Options = {}): Promise<Response> {
  const { container, Settings } = await load(options.env ?? FIXTURES);
  return container.renderToResponse(Settings as never, {
    params: { lang },
    request: await build(lang, options),
  });
}

describe.each(READING_LANGUAGES)('the settings screen in %s', (lang) => {
  const t = settingsStrings(lang);
  const ui = uiStrings(lang);
  const p = profileStrings(lang);

  it(
    'names the four sections it owns',
    async () => {
      const html = await render(lang);

      expect(html).toContain(p.readingHeading);
      expect(html).toContain(t.payoutsHeading);
      expect(html).toContain(ui.consentHeading);
      expect(html).toContain(t.notificationsHeading);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'offers one switch per consent purpose, over its own history',
    async () => {
      const html = await render(lang);

      for (const purpose of CONSENT_PURPOSES) {
        expect(html, `no control for ${purpose}`).toContain(`value="${purpose}"`);
      }
      expect(html).toContain('aria-pressed');
      expect(html).toContain(ui.consentHistoryHeading);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'sends changing a place to the editor that already exists',
    async () => {
      // The seam board says of places: "This is /setup, which already
      // exists." A second editor here would be a second thing to keep
      // correct about FR-009.
      const html = await render(lang);

      expect(html).toContain(`/${lang}/setup`);
      expect(html).toContain(ui.editPlaces);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'moves the language and no other axis',
    async () => {
      const html = await render(lang);
      const other = lang === 'el' ? 'de' : 'el';

      expect(html).toContain(`href="/${other}/settings"`);
    },
    RENDER_TIMEOUT_MS,
  );

  it(
    'names a place the way the place names itself',
    async () => {
      const html = await render(lang, { cookie: 'reader_axes=/el/munich' });

      expect(html).toContain('München');
      expect(html).not.toMatch(/>\s*munich\s*</);
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

describe('a consent that was not recorded says so', () => {
  it.each(READING_LANGUAGES)('in %s, naming the reason', async (lang) => {
    // The whole point. `setConsent` cannot write — there is no endpoint —
    // so the member is told in the same request rather than being shown a
    // switch that appears to have moved.
    const html = await render(lang, { post: { purpose: 'newsletter', granted: 'true' } });
    const ui = uiStrings(lang);

    expect(html).toContain(ui.notRecorded);
    expect(html).not.toContain(ui.consentRecorded);
    // The reason the fixture gives, which names the schema it did not write.
    expect(html).toMatch(/consent/i);
  });

  it('refuses a cross-origin post rather than acting on it', async () => {
    // A consent is a legal record. `isSameOrigin` compares hosts and
    // refuses a request carrying neither Origin nor Referer.
    const response = await respond('el', {
      post: { purpose: 'newsletter', granted: 'true' },
      origin: 'https://evil.invalid',
    });

    expect(response.status).toBe(403);
  });

  it('refuses a post carrying no origin at all', async () => {
    const response = await respond('el', {
      post: { purpose: 'newsletter', granted: 'true' },
      origin: null,
    });

    expect(response.status).toBe(403);
  });

  it('says nothing about an outcome on a plain visit', async () => {
    const html = await render('el');

    expect(html).not.toContain(uiStrings('el').notRecorded);
  });

  it('ignores a purpose that is not one of ours', async () => {
    // Somebody else's form field. No outcome band, because nothing was
    // attempted — not even a refusal to write.
    const html = await render('el', { post: { purpose: 'tracking', granted: 'true' } });

    expect(html).not.toContain(uiStrings('el').notRecorded);
  });
});

describe('the notifications sketch', () => {
  it.each(READING_LANGUAGES)('is marked a sketch and offers nothing to press in %s', async (lang) => {
    const html = await render(lang);
    const t = settingsStrings(lang);

    expect(html).toContain(t.sketchTitle);
    expect(html).toContain(t.sketchNothingYet);
  });

  it('adds no control of its own', async () => {
    // Every control on this page belongs to consent: three switches, each
    // its own form. A fourth would be the sketch pretending to work.
    const html = await render('el');
    const switches = html.match(/aria-pressed/g) ?? [];

    expect(switches).toHaveLength(CONSENT_PURPOSES.length);
  });
});

describe('what the board draws and this page has no source for', () => {
  it('builds no evreos section, no theme and no device count', async () => {
    // Three invented values and a dead link. The same call #610 made.
    const html = await render('el');

    for (const absent of ['evreos', 'Θέμα', 'Συσκευές']) {
      expect(html, `mentions ${absent}`).not.toContain(absent);
    }
  });
});

describe('the fence', () => {
  it('sends a stranger to sign in, and back here afterwards', async () => {
    const response = await respond('el', { env: LIVE });

    expect(response.status).toBe(303);
    expect(response.headers.get('location')).toContain('/el/signin');
    expect(response.headers.get('location')).toContain(
      `next=${encodeURIComponent('/el/settings')}`,
    );
  });

  it('lets a preview through with no session at all', async () => {
    const response = await respond('el');

    expect(response.status).toBe(200);
  });
});

describe('a signed-in member whose cashback api is not answering', () => {
  it('still shows the consent records, which do not depend on it', async () => {
    // The reason somebody came here. The payout standing is a figure the
    // page can do without; the records are not.
    const html = await render('el', { env: LIVE, signedInAs: 'someone@example.test' });

    expect(html).toContain(uiStrings('el').consentHeading);
    expect(html).toContain(uiStrings('el').consentHistoryHeading);
    expect(html).toContain(profileStrings('el').accountUnavailable);
    for (const leak of LEAKS) {
      expect(html).not.toContain(leak);
    }
  });
});

describe('a language this application does not serve', () => {
  it('is rewritten to the not-found page rather than rendered', async () => {
    await expect(render('fr')).rejects.toThrow(/route \/404/);
  });
});
