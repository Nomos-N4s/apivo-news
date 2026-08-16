/**
 * The reader's remembered axes — which language they read, which places
 * they follow (issue #133).
 *
 * The two axes have always lived in the URL and nowhere else, which is why
 * `/` had no answer to give and sent every visitor to the same flagship
 * front page. This module is the one place that remembers, and it
 * remembers the smallest possible thing: the front-page path the reader
 * chose, `/el/munich+greece`. A language subtag and a list of place slugs,
 * stored verbatim, so the value is legible to the reader in their own
 * browser and says exactly what it does.
 *
 * On the cookie itself: it is written only in response to the reader's own
 * explicit action — completing setup, or opening a front page they
 * navigated to — and it holds only what they chose. No identifier, no
 * counter, no timestamp, nothing derived from the visitor and nothing that
 * could join this visit to another. That makes it a functional preference,
 * strictly necessary to deliver the paper the reader asked for, and not a
 * cookie that needs consent. The consent story (issue #91) covers
 * purposes that do carry a record of a person; this deliberately carries
 * none, and the test suite asserts that rather than trusting the comment.
 *
 * Reading it back is a parse, never a guess: a stored value naming a place
 * the catalog does not have, or a language the alpha does not mount, reads
 * as no preference at all. The caller then asks the reader again. A paper
 * the reader did not choose must never appear because a slug went stale —
 * the honest-record rule applied to preferences.
 */

import {
  frontPagePath,
  parseAxes,
  type Axes,
  type ReadingLanguage,
} from './axes';

/**
 * The cookie name. First-party, unprefixed: `__Host-` would be stricter
 * still, but it mandates `Secure`, and the development stack is plain
 * http — a preference that silently stops working outside production is
 * worse than one whose name carries no promise.
 */
export const PREFERENCE_COOKIE = 'reader_axes';

/**
 * A year. Long enough that a reader who visits each season is still
 * greeted by their own paper, short enough that an abandoned browser
 * forgets. Nothing renews it but the reader's own next visit.
 */
export const PREFERENCE_MAX_AGE_SECONDS = 60 * 60 * 24 * 365;

/** What the cookie is written with. */
export interface PreferenceCookieOptions {
  readonly path: string;
  readonly maxAge: number;
  readonly sameSite: 'lax';
  readonly httpOnly: boolean;
  readonly secure: boolean;
}

/**
 * The cookie's options.
 *
 * `httpOnly` because nothing in the reader is a client-side app: every
 * page is server-rendered and reads this on the server, so no page script
 * has any reason to see it. `sameSite: 'lax'` so a link from elsewhere
 * still opens the reader's own paper — the preference has no
 * state-changing power to protect, only a destination to remember.
 *
 * `secure` is passed in, and the one authority on it is `isSecureRequest`
 * (lib/secure-request.ts): `APP_ENV=prod` means the deployment is
 * https-only and the answer is true, `X-Forwarded-Proto` — which the
 * Cloudflare Worker stamps — answers elsewhere, and the request's own URL
 * answers last. It is NOT read from `Astro.url`: the Worker proxies to
 * this container over plain HTTP and @astrojs/node builds `Astro.url`
 * from the socket, so that reading was false on every deployed shape and
 * this cookie shipped without `Secure` while the comment here promised
 * otherwise. The editorial session cookie is written from the same
 * signal, so the two cannot drift apart.
 */
export function preferenceCookieOptions(secure: boolean): PreferenceCookieOptions {
  return {
    path: '/',
    maxAge: PREFERENCE_MAX_AGE_SECONDS,
    sameSite: 'lax',
    httpOnly: true,
    secure,
  };
}

/**
 * The stored preference, or null when there is none to honour.
 *
 * Null covers every unreadable case identically — absent, malformed, a
 * language that is not a reading language, a slug outside the catalog —
 * because they all mean the same thing to the caller: this browser has not
 * told us which paper to open, so ask.
 */
export function readPreference(raw: string | undefined | null): Axes | null {
  if (raw === undefined || raw === null) {
    return null;
  }
  const path = raw.trim();
  if (!path.startsWith('/')) {
    return null;
  }
  const segments = path.slice(1).split('/');
  if (segments.length !== 2) {
    return null;
  }
  return parseAxes(segments[0], segments[1]);
}

/** The slice of Astro's cookie store this module writes through. */
export interface PreferenceCookieStore {
  set(key: string, value: string, options: PreferenceCookieOptions): void;
  delete(key: string, options: { path: string }): void;
}

/** What the caller knows about the request it is answering. */
export interface PreferenceWrite {
  /** The value this request already carries, so an unchanged preference is not rewritten. */
  readonly current: string | undefined;
  /** Whether the browser reached us over TLS — from `isSecureRequest`. */
  readonly secure: boolean;
}

/**
 * Remembers a front page as the one `/` should open.
 *
 * The path is stored as it stands, so the cookie and the URL can never
 * describe different papers. A path this module could not read back is
 * refused rather than written: storing a value we would later have to
 * discard would send the reader to setup with no way to tell why.
 */
export function rememberPreference(
  cookies: PreferenceCookieStore,
  frontPage: string,
  write: PreferenceWrite,
): void {
  if (write.current === frontPage) {
    return;
  }
  if (readPreference(frontPage) === null) {
    return;
  }
  cookies.set(PREFERENCE_COOKIE, frontPage, preferenceCookieOptions(write.secure));
}

/** The front-page path a parsed preference names. */
export function preferenceFrontPage(preference: Axes): string {
  return frontPagePath(
    preference.lang,
    preference.places.map((place) => place.slug),
  );
}

/**
 * Drops a preference we cannot honour. A stored value naming a place that
 * no longer exists is not a preference, and keeping it would make every
 * later visit re-decide the same unanswerable question.
 */
export function forgetPreference(cookies: PreferenceCookieStore): void {
  cookies.delete(PREFERENCE_COOKIE, { path: '/' });
}

/** The setup screen for a reading language. */
export function setupPath(lang: ReadingLanguage): string {
  return `/${lang}/setup`;
}

/**
 * The language the first-run question is asked in.
 *
 * A first visit has told us nothing, and `Accept-Language` is deliberately
 * not consulted: a browser header is a configuration, not a choice, and
 * this screen exists precisely to let the reader make the choice. Greek is
 * the paper's own language and the setup screen names both options in
 * their own script, so neither reader is stranded — and whichever they
 * pick is theirs from then on.
 */
export const FIRST_RUN_LANGUAGE: ReadingLanguage = 'el';

/** Where `/` sends a reader who has not chosen yet. */
export const FIRST_RUN_SETUP_PATH = setupPath(FIRST_RUN_LANGUAGE);
