import { createServerClient, type CookieOptions } from '@supabase/ssr';
import type { SupabaseClient } from '@supabase/supabase-js';
import type { AstroCookies } from 'astro';
import { PUBLIC_SUPABASE_ANON_KEY, PUBLIC_SUPABASE_URL } from 'astro:env/server';

import type { Database } from '../database.types';
import {
  astroCookieOptions,
  editorSessionFrom,
  NO_EDITOR_SESSION,
  parseCookieHeader,
  supabaseConfig,
  type EditorSession,
} from './session';

/**
 * The Supabase Auth glue: everything in this module is a thin wrapper over
 * the SDK, kept in one file so the rest of the editorial code deals in
 * `EditorSession` and never in auth vendor types.
 *
 * The session lives in cookies, read and written through Astro's own
 * cookie store, which is why every entry point here takes the request and
 * the store together — a refreshed access token must land back in the
 * browser or the editor is signed out an hour later.
 */

/** Whether this deployment has Supabase Auth configured at all. */
export function authConfigured(): boolean {
  return supabaseConfig(PUBLIC_SUPABASE_URL, PUBLIC_SUPABASE_ANON_KEY) !== null;
}

/**
 * A request-scoped Supabase client, or null when no auth is configured.
 *
 * Never share one across requests: it holds the caller's session.
 */
export function authClient(
  request: Request,
  cookies: AstroCookies,
): SupabaseClient<Database> | null {
  const config = supabaseConfig(PUBLIC_SUPABASE_URL, PUBLIC_SUPABASE_ANON_KEY);
  if (config === null) {
    return null;
  }
  const secure = new URL(request.url).protocol === 'https:';
  return createServerClient<Database>(config.url, config.anonKey, {
    cookies: {
      getAll: (): { name: string; value: string }[] =>
        parseCookieHeader(request.headers.get('cookie')),
      setAll: (cookiesToSet: { name: string; value: string; options: CookieOptions }[]): void => {
        for (const { name, value, options } of cookiesToSet) {
          cookies.set(name, value, astroCookieOptions(options, secure));
        }
      },
    },
  });
}

/**
 * The auth error statuses that mean "this visitor simply has no valid
 * session" — a missing, expired or rejected token. Everything else is
 * the provider failing, which must not read as normal signed-out
 * traffic.
 */
const EXPECTED_AUTH_STATUSES: ReadonlySet<number> = new Set([400, 401, 403]);

/**
 * Logs an auth-resolution failure that is not a plain missing/invalid
 * session. Name and message only — never the cookie header, the token or
 * the SDK's payload, which carry credentials. During an auth outage
 * every editorial route resolves to "nobody is signed in", and without
 * this line that outage would be indistinguishable from signed-out
 * traffic.
 */
function logAuthFailure(detail: string): void {
  console.error(`editorial auth: session resolution failed (${detail}); resolving to nobody`);
}

/**
 * Resolves who is signed in, against the auth server.
 *
 * `getUser()` rather than the cookie's own contents: a cookie is whatever
 * the browser sent, and this screen puts a name next to the word
 * "approver". Only the auth server's answer is evidence. It also refreshes
 * an expired access token, which `setAll` above writes back.
 *
 * A failure of any kind resolves to "nobody is signed in". That is the
 * honest reading — an identity we could not confirm is not an identity —
 * and it costs the editor a sign-in, never a false attribution. But the
 * two failures are logged apart: an absent session is normal traffic; a
 * provider or network failure is an outage the operator must be able to
 * see.
 */
export async function resolveEditorSession(
  request: Request,
  cookies: AstroCookies,
): Promise<EditorSession> {
  const client = authClient(request, cookies);
  if (client === null) {
    return NO_EDITOR_SESSION;
  }
  try {
    const { data: userData, error } = await client.auth.getUser();
    if (error !== null) {
      if (!EXPECTED_AUTH_STATUSES.has(error.status ?? 0)) {
        logAuthFailure(`${error.name}: status ${String(error.status)}`);
      }
      return NO_EDITOR_SESSION;
    }
    if (userData.user === null) {
      return NO_EDITOR_SESSION;
    }
    // The token the editorial API is called with. Read after getUser so
    // any refresh has already happened and this is the live one.
    const { data: sessionData } = await client.auth.getSession();
    return editorSessionFrom(userData.user, sessionData.session?.access_token ?? null);
  } catch (error) {
    // Thrown, not returned: a network failure or an SDK bug, never a
    // routine signed-out visitor.
    logAuthFailure(error instanceof Error ? `${error.name}: ${error.message}` : String(error));
    return NO_EDITOR_SESSION;
  }
}
