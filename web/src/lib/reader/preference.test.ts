import { describe, expect, it } from 'vitest';

import { isSecureRequest } from '../secure-request';
import { frontPagePath, PLACE_CATALOG, READING_LANGUAGES } from './axes';
import {
  FIRST_RUN_SETUP_PATH,
  forgetPreference,
  preferenceCookieOptions,
  preferenceFrontPage,
  PREFERENCE_COOKIE,
  PREFERENCE_MAX_AGE_SECONDS,
  readPreference,
  rememberPreference,
  setupPath,
  type PreferenceCookieOptions,
  type PreferenceCookieStore,
} from './preference';

/** A cookie store that records what it was asked to do, and nothing else. */
function makeStore(): PreferenceCookieStore & {
  writes: { key: string; value: string; options: PreferenceCookieOptions }[];
  deletes: string[];
} {
  const writes: { key: string; value: string; options: PreferenceCookieOptions }[] = [];
  const deletes: string[] = [];
  return {
    writes,
    deletes,
    set: (key, value, options): void => {
      writes.push({ key, value, options });
    },
    delete: (key): void => {
      deletes.push(key);
    },
  };
}

describe('readPreference', () => {
  it('reads back a stored front page as both axes', () => {
    const preference = readPreference('/el/munich+greece');
    expect(preference?.lang).toBe('el');
    expect(preference?.places.map((place) => place.slug)).toEqual(['munich', 'greece']);
  });

  it('reads a single place and the other reading language', () => {
    expect(readPreference('/de/munich')?.lang).toBe('de');
    expect(readPreference('/de/munich')?.places.map((place) => place.slug)).toEqual(['munich']);
  });

  it('accepts an addressable place the setup screen does not offer', () => {
    // /el/bavaria is a real front page; the cookie may name whatever the
    // URL may name, or the two would drift the moment one was visited.
    expect(readPreference('/el/bavaria')?.places.at(0)?.slug).toBe('bavaria');
  });

  it('refuses a language the alpha does not mount (FR-015)', () => {
    expect(readPreference('/en/munich')).toBeNull();
    expect(readPreference('/fr/munich')).toBeNull();
  });

  it('refuses a place slug the catalog does not have — never a different paper', () => {
    expect(readPreference('/el/atlantis')).toBeNull();
    expect(readPreference('/el/munich+atlantis')).toBeNull();
  });

  it('refuses anything that is not a front-page path', () => {
    expect(readPreference(undefined)).toBeNull();
    expect(readPreference(null)).toBeNull();
    expect(readPreference('')).toBeNull();
    expect(readPreference('el/munich')).toBeNull();
    expect(readPreference('/el')).toBeNull();
    expect(readPreference('/el/munich/a/abc-123')).toBeNull();
    expect(readPreference('/')).toBeNull();
    expect(readPreference('https://elsewhere.example/el/munich')).toBeNull();
  });

  it('tolerates surrounding whitespace', () => {
    expect(readPreference('  /el/munich  ')?.lang).toBe('el');
  });
});

describe('preferenceFrontPage', () => {
  it('round-trips a stored value through the parse', () => {
    const preference = readPreference('/de/greece+munich');
    expect(preference).not.toBeNull();
    expect(preference === null ? '' : preferenceFrontPage(preference)).toBe('/de/greece+munich');
  });
});

describe('rememberPreference', () => {
  it('stores the front page the reader chose', () => {
    const store = makeStore();
    rememberPreference(store, '/de/munich', { current: undefined, secure: false });
    expect(store.writes).toHaveLength(1);
    expect(store.writes.at(0)?.key).toBe(PREFERENCE_COOKIE);
    expect(store.writes.at(0)?.value).toBe('/de/munich');
  });

  it('leaves an unchanged preference alone', () => {
    const store = makeStore();
    rememberPreference(store, '/de/munich', { current: '/de/munich', secure: false });
    expect(store.writes).toEqual([]);
  });

  it('overwrites a preference the reader has moved away from', () => {
    const store = makeStore();
    rememberPreference(store, '/de/munich', { current: '/el/munich+greece', secure: false });
    expect(store.writes.at(0)?.value).toBe('/de/munich');
  });

  it('refuses to store what it could not read back', () => {
    const store = makeStore();
    rememberPreference(store, '/el/atlantis', { current: undefined, secure: false });
    rememberPreference(store, '/en/munich', { current: undefined, secure: false });
    rememberPreference(store, 'nonsense', { current: undefined, secure: false });
    expect(store.writes).toEqual([]);
  });

  it("carries the request's scheme into the cookie", () => {
    const plain = makeStore();
    rememberPreference(plain, '/el/munich', { current: undefined, secure: false });
    expect(plain.writes.at(0)?.options.secure).toBe(false);

    const https = makeStore();
    rememberPreference(https, '/el/munich', { current: undefined, secure: true });
    expect(https.writes.at(0)?.options.secure).toBe(true);
  });
});

describe('the preference cookie', () => {
  it('carries the two axes and no identifier of any kind', () => {
    const store = makeStore();
    rememberPreference(store, frontPagePath('de', ['munich', 'greece']), {
      current: undefined,
      secure: true,
    });

    const written = store.writes.at(0);
    expect(written?.value).toBe('/de/munich+greece');

    // Every character of the value is accounted for: one reading language
    // and place slugs from the catalog, joined by the URL's own separators.
    // Nothing here is derived from the visitor.
    const [, lang, placeSegment] = written?.value.split('/') ?? [];
    expect(READING_LANGUAGES as readonly string[]).toContain(lang);
    const slugs = PLACE_CATALOG.map((place) => place.slug);
    for (const slug of placeSegment?.split('+') ?? []) {
      expect(slugs).toContain(slug);
    }

    // And no second cookie rides along with it.
    expect(store.writes).toHaveLength(1);
  });

  it('is the same value for the same choice, every time — no nonce, no session', () => {
    const first = makeStore();
    const second = makeStore();
    rememberPreference(first, '/el/munich+greece', { current: undefined, secure: false });
    rememberPreference(second, '/el/munich+greece', { current: undefined, secure: false });
    expect(second.writes.at(0)?.value).toBe(first.writes.at(0)?.value);
  });

  // "Verbatim" has to be true on the wire, not merely after a decode:
  // Astro's default encoder would store `%2Fde%2Fmunich%2Bgreece`, which
  // is no longer a sentence the reader can read in their own browser and
  // check against the address bar.
  it('is stored exactly as the front-page path, not percent-encoded', () => {
    const { encode } = preferenceCookieOptions(true);
    for (const value of ['/de/munich+greece', '/el/munich', '/el/munich+greece']) {
      expect(encode(value), value).toBe(value);
    }
  });

  it('is unreadable to page script, first-party, and expires on its own', () => {
    const options = preferenceCookieOptions(true);
    expect(options.httpOnly).toBe(true);
    expect(options.sameSite).toBe('lax');
    expect(options.path).toBe('/');
    expect(options.maxAge).toBe(PREFERENCE_MAX_AGE_SECONDS);
    expect(PREFERENCE_MAX_AGE_SECONDS).toBe(60 * 60 * 24 * 365);
  });
});

describe('forgetPreference', () => {
  it('drops a preference that cannot be honoured', () => {
    const store = makeStore();
    forgetPreference(store);
    expect(store.deletes).toEqual([PREFERENCE_COOKIE]);
  });
});

describe('the secure attribute', () => {
  // The signal is imported from lib/secure-request.ts rather than
  // restated here, so the preference cookie and the editorial session
  // cookie cannot disagree about what "the browser reached us over TLS"
  // means. This is the composition, stated end to end.
  it('is written from the deployment, not from a proxied request URL', () => {
    // What the container actually sees behind the Worker: plain http.
    const proxied = new Request('http://news.example/el/munich');
    expect(new URL(proxied.url).protocol).toBe('http:');

    const store = makeStore();
    rememberPreference(store, '/el/munich', {
      current: undefined,
      secure: isSecureRequest(proxied, 'prod'),
    });
    expect(store.writes.at(0)?.options.secure).toBe(true);
  });

  it('still leaves the plain-http development stack working', () => {
    const store = makeStore();
    rememberPreference(store, '/el/munich', {
      current: undefined,
      secure: isSecureRequest(new Request('http://localhost:4321/el/munich'), 'dev'),
    });
    expect(store.writes.at(0)?.options.secure).toBe(false);
  });
});

describe('the first run', () => {
  it("asks in the paper's own language rather than guessing from a header", () => {
    expect(FIRST_RUN_SETUP_PATH).toBe('/el/setup');
    expect(setupPath('de')).toBe('/de/setup');
  });
});
