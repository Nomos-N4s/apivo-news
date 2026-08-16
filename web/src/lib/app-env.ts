/**
 * `APP_ENV`: which environment this container is running in.
 *
 * The same name and the same two values the Go binary uses
 * (internal/platform/config), and for the same reason — one word decides
 * whether this process believes it is serving the public. It decides three
 * things here: whether the reader may answer from fixtures
 * (lib/reader/api.ts), whether a session or preference cookie is written
 * `Secure` (lib/secure-request.ts), and how loudly a misconfiguration
 * fails.
 *
 * A value that is neither `dev` nor `prod` is not development. The Go
 * binary refuses to start on one; nothing here may quietly read
 * `production` as "not prod" and hand a deployed reader a page of invented
 * publishers, so `parseAppEnv` reports it as unknown and every caller
 * treats unknown as a refusal.
 */

/** Development: fixtures answer, and they say so. */
export const APP_ENV_DEV = 'dev';

/** Deployed and readable by the public. */
export const APP_ENV_PROD = 'prod';

/** The two environments this application knows. */
export type AppEnv = typeof APP_ENV_DEV | typeof APP_ENV_PROD;

/**
 * The environment a raw `APP_ENV` names, or null when it names neither.
 *
 * Unset (and empty, which every environment lookup renders the same way)
 * is development, exactly as the Go binary reads it. Anything else
 * non-empty is null — a value somebody meant, that this application does
 * not understand, and that must therefore stop it rather than steer it.
 */
export function parseAppEnv(raw: string | undefined | null): AppEnv | null {
  if (raw === undefined || raw === null || raw === '') {
    return APP_ENV_DEV;
  }
  if (raw === APP_ENV_DEV || raw === APP_ENV_PROD) {
    return raw;
  }
  return null;
}

/** Whether this container is deployed and serving the public. */
export function isProdEnv(raw: string | undefined | null): boolean {
  return parseAppEnv(raw) === APP_ENV_PROD;
}
