import type { ReadingLanguage } from '../reader/axes';

/**
 * The plumbing of signing a member in: where they came from, where they are
 * sent back to, and what the sign-in page is being told happened.
 *
 * It lives apart from the page because all of it is decidable from strings
 * and none of it should be decided in a template. A return path in
 * particular is the one parameter where being approximately right is an open
 * redirect: `//evil.example` is a URL a browser follows off this site, and it
 * begins with a slash.
 */

/** Where a member lands when they signed in from nowhere in particular. */
export const DEFAULT_RETURN_PATH = '/';

/**
 * What the sign-in page is being told, in a query parameter, by whatever
 * redirected to it.
 *
 * They are outcomes rather than messages: the page owns the sentence, in the
 * member's own language, and a redirect carrying prose would be a redirect
 * that has to be translated.
 */
export const ACCESS_OUTCOMES = [
  'sent',
  'expired',
  'refused',
  'unconfigured',
  // Both mean the sign-in itself worked and the account behind it did not.
  // `taken` is about the member's address and they can act on it; `unmade`
  // is about this deployment and they cannot.
  'taken',
  'unmade',
  // Signed out, on the page somebody signs in from — which is where the
  // sign-out lands them, and where they would go next anyway.
  'signedout',
] as const;

export type AccessOutcome = (typeof ACCESS_OUTCOMES)[number];

/** Whether a query parameter names an outcome this page knows. */
export function isAccessOutcome(value: string | null | undefined): value is AccessOutcome {
  return value !== null && value !== undefined && (ACCESS_OUTCOMES as readonly string[]).includes(value);
}

/**
 * A return path this site is willing to send somebody to after they sign in.
 *
 * Same-origin paths only, and the check is on the SHAPE of the string rather
 * than on a parsed URL: `new URL(raw, origin)` resolves `//evil.example` to
 * another origin and reports it as valid, which is exactly the mistake.
 *
 * Refused, and why each one:
 *
 *   * anything not starting with `/` — an absolute URL, or a relative path
 *     whose meaning depends on where it is read from.
 *   * `//host` and `/\host` — protocol-relative, and browsers normalise the
 *     backslash to a slash before following it. Both leave this site.
 *   * `/auth…` — the flow's own routes. Landing back in the middle of a
 *     sign-in after finishing one is a loop, not a return.
 *   * a control character, which is how a header or a URL gets split.
 *
 * A refusal is never an error. Somebody arriving with a mangled `next` still
 * wants to sign in, so they sign in and land on the fallback.
 */
export function safeReturnPath(
  raw: string | null | undefined,
  fallback: string = DEFAULT_RETURN_PATH,
): string {
  if (typeof raw !== 'string' || raw === '' || raw.length > 512) {
    return fallback;
  }
  if (!raw.startsWith('/') || raw.startsWith('//')) {
    return fallback;
  }
  if (raw.includes('\\')) {
    return fallback;
  }
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(raw)) {
    return fallback;
  }
  if (raw === '/auth' || raw.startsWith('/auth/')) {
    return fallback;
  }
  return raw;
}

/** The sign-in page for a language, carrying where to come back to. */
export function signInPath(
  lang: ReadingLanguage,
  options: { next?: string | undefined; outcome?: AccessOutcome | undefined } = {},
): string {
  const params = new URLSearchParams();
  const next = safeReturnPath(options.next, '');
  if (next !== '') {
    params.set('next', next);
  }
  if (options.outcome !== undefined) {
    params.set('outcome', options.outcome);
  }
  const query = params.toString();
  return query === '' ? `/${lang}/signin` : `/${lang}/signin?${query}`;
}

/**
 * The absolute URL the auth provider sends the member back to.
 *
 * Absolute because a provider redirects from its own origin and has no idea
 * what ours is, and it must match one this deployment's provider allows. It
 * carries the language, because `/auth/confirm` has none of its own and a
 * failure has to be explained in the language the member was reading.
 */
export function confirmUrl(origin: string, lang: ReadingLanguage, next: string): string {
  const url = new URL('/auth/confirm', origin);
  url.searchParams.set('lang', lang);
  const safe = safeReturnPath(next, '');
  if (safe !== '') {
    url.searchParams.set('next', safe);
  }
  return url.toString();
}
