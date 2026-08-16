/**
 * One answer to one question: did the browser reach this deployment over
 * TLS?
 *
 * Every cookie this application writes has to answer it — the Supabase
 * session (lib/editorial/supabase.ts) and the reader's preference
 * (lib/reader/preference.ts) — and both used to answer it from
 * `Astro.url.protocol`. That is wrong on every deployed shape we have:
 * the Cloudflare Worker proxies to the container over plain HTTP, and
 * `@astrojs/node` builds `Astro.url` from the socket and the Host header,
 * ignoring `X-Forwarded-Proto` entirely. So `Astro.url` said `http:` on an
 * https-only site and the session cookie shipped without `Secure` — a
 * refresh token that rides the next plain-http request to the same host,
 * and whoever captures it can POST an approval that names a real editor
 * who never approved anything.
 *
 * The answer lives here, once, and it is layered deliberately:
 *
 *   1. **`APP_ENV=prod` is authoritative.** A deployed environment is
 *      https-only — the Worker is the only way in and it is reached over
 *      TLS; docs/RELEASING.md names no other target. So in prod the answer
 *      is yes, and no header, proxy or socket may lower it. That is the
 *      fail-closed direction: the worst case is a `Secure` cookie a
 *      plain-http deployment cannot store, which breaks sign-in loudly
 *      rather than leaking a token quietly.
 *   2. **`X-Forwarded-Proto` otherwise**, which the Worker stamps on every
 *      proxied request (deploy/cloudflare/routing.js) and a Kubernetes TLS
 *      ingress sets by convention. It is trusted only because the sole
 *      route to the container overwrites whatever a client sent.
 *   3. **The request URL last**, which is the truth on the development
 *      stack, where nothing terminates TLS and nothing forwards a scheme.
 *
 * Callers pass `APP_ENV` in rather than reading it here, so this module
 * stays a pure function of its inputs and the tests can state the
 * deployed shape exactly.
 */

import { isProdEnv } from './app-env';

/**
 * The header a TLS terminator states the browser's scheme in. Lower-cased
 * because `Headers.get` is case-insensitive and this constant is also what
 * the Worker writes.
 */
export const FORWARDED_PROTO_HEADER = 'x-forwarded-proto';

/**
 * The scheme a forwarding hop declared, lower-cased, or null when it
 * declared none.
 *
 * The header may carry a list when several proxies appended to it
 * (`https, http`); the first entry is the hop closest to the browser,
 * which is the one that terminated TLS.
 */
export function forwardedProto(raw: string | null | undefined): string | null {
  if (raw === null || raw === undefined) {
    return null;
  }
  const first = raw.split(',')[0]?.trim().toLowerCase() ?? '';
  return first === '' ? null : first;
}

/**
 * Whether the browser reached this deployment over TLS — the one signal
 * the `Secure` attribute of every cookie is written from.
 *
 * @param request The request being answered.
 * @param appEnv `APP_ENV`, as the container was started with.
 */
export function isSecureRequest(request: Request, appEnv: string | undefined | null): boolean {
  if (isProdEnv(appEnv)) {
    return true;
  }
  const forwarded = forwardedProto(request.headers.get(FORWARDED_PROTO_HEADER));
  if (forwarded !== null) {
    return forwarded === 'https';
  }
  return new URL(request.url).protocol === 'https:';
}
