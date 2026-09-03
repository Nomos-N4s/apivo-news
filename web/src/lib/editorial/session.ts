/**
 * The editor identity behind the editorial screens.
 *
 * The approver is never anonymous: `article.approved_by` is NOT NULL and
 * migration 0002 additionally requires that account to hold the editor
 * role, so every editorial screen names who is signed in. That name now
 * comes from Supabase Auth (research D7): the middleware resolves the
 * cookie-backed session once per request, against the auth server, and
 * this module maps what it answered onto the shape the screens read.
 *
 * `editorSession()` is unchanged for its callers — still synchronous,
 * still taking the request — because the request object is the key the
 * middleware filed the resolved identity under. Resolution needs to write
 * cookies (a refreshed token has to land back in the browser) and a page
 * is the wrong place for that, which is why it happens in middleware and
 * this seam is a lookup.
 */

/**
 * The roles `account.role` allows, as migrations 0002 and 0019 constrain it.
 *
 * An editor is not an operator with fewer permissions and an operator is not
 * an editor with more; the Go side deliberately carries two separate
 * `Require…` functions rather than one parameterised by role, and this union
 * is the same decision on this side.
 */
export type AccountRole = 'reader' | 'editor' | 'operator';

/** The signed-in person, as the editorial screens need them. */
export interface EditorSession {
  readonly displayName: string;
  readonly email: string;
  /**
   * Mirrors `account.role`. Only 'editor' may approve an article (the 0002
   * trigger), and only 'operator' may approve a payout or act on a cashback
   * queue (the 0019 trigger). The union is the database's `check` constraint
   * verbatim, because a role this side cannot name is one a screen cannot
   * gate on: an operator whose claim mapped to 'reader' would be shown the
   * reader's cashback surfaces and refused by the API on every operator call
   * without the screen ever being able to say why.
   */
  readonly role: AccountRole;
  /** The bearer token for the editorial API; null when nobody is signed in. */
  readonly token: string | null;
  /**
   * Whether a Supabase session actually backs this identity. The screens
   * surface this rather than implying a real sign-in — a named approver
   * that nobody authenticated is exactly the claim this product must not
   * make.
   */
  readonly authenticated: boolean;
}

/**
 * Nobody is signed in: no name to show, no token to send.
 *
 * There is deliberately no placeholder name here. A screen that prints a
 * name is stating who is at the keyboard, and this value is what it says
 * when it does not know.
 */
export const NO_EDITOR_SESSION: EditorSession = {
  displayName: '',
  email: '',
  role: 'reader',
  token: null,
  authenticated: false,
};

/**
 * The part of a Supabase user the screens read. Narrower than the SDK's
 * type on purpose: this module maps claims, and stating which ones keeps
 * the mapping honest about what it depends on.
 */
export interface SupabaseUserClaims {
  readonly email?: string | null | undefined;
  readonly user_metadata?: Record<string, unknown> | null | undefined;
  readonly app_metadata?: Record<string, unknown> | null | undefined;
}

/** A metadata entry, when it is a non-blank string. */
function metadataString(
  metadata: Record<string, unknown> | null | undefined,
  key: string,
): string | null {
  const value = metadata?.[key];
  return typeof value === 'string' && value.trim() !== '' ? value : null;
}

/**
 * The name to print. Supabase spells the display name three ways
 * depending on how the user was created, and the email is the last
 * resort — an address is at least genuinely this person's.
 */
function displayNameOf(user: SupabaseUserClaims, email: string): string {
  return (
    metadataString(user.user_metadata, 'display_name') ??
    metadataString(user.user_metadata, 'full_name') ??
    metadataString(user.user_metadata, 'name') ??
    email
  );
}

/**
 * The role, from the auth user's app metadata.
 *
 * `account.role` is the authority and the database enforces it; the web
 * container never reads that table, so this is what the token asserts,
 * not what the schema decided. Anything other than an explicit 'editor'
 * reads as 'reader': under-claiming authority is safe, and claiming it is
 * the failure this product cannot afford. The screens use it to name the
 * role, never to permit anything — the approval gate is the trigger in
 * migration 0002.
 */
function roleOf(user: SupabaseUserClaims): AccountRole {
  const claimed = metadataString(user.app_metadata, 'role');
  return claimed === 'editor' || claimed === 'operator' ? claimed : 'reader';
}

/**
 * Maps a resolved Supabase user and its access token onto the session.
 *
 * A user without a token is not a session: the screens would name a
 * person while every call to the editorial API went out unauthenticated
 * and came back 401. Either both are present or nobody is signed in.
 */
export function editorSessionFrom(
  user: SupabaseUserClaims | null | undefined,
  accessToken: string | null | undefined,
): EditorSession {
  if (
    user === null ||
    user === undefined ||
    accessToken === null ||
    accessToken === undefined ||
    accessToken === ''
  ) {
    return NO_EDITOR_SESSION;
  }
  const email = typeof user.email === 'string' ? user.email : '';
  return {
    displayName: displayNameOf(user, email),
    email,
    role: roleOf(user),
    token: accessToken,
    authenticated: true,
  };
}

/** The SDK-shaped cookie options `astroCookieOptions` translates. */
export interface SdkCookieOptions {
  readonly domain?: string | undefined;
  readonly path?: string | undefined;
  readonly expires?: Date | undefined;
  readonly maxAge?: number | undefined;
  readonly sameSite?: boolean | 'lax' | 'strict' | 'none' | undefined;
  readonly httpOnly?: boolean | undefined;
  readonly secure?: boolean | undefined;
}

/** What Astro's cookie store is asked to write. */
export interface AstroCookieWriteOptions {
  readonly domain?: string;
  readonly path?: string;
  readonly expires?: Date;
  readonly maxAge?: number;
  readonly sameSite?: boolean | 'lax' | 'strict' | 'none';
  readonly httpOnly: boolean;
  readonly secure: boolean;
}

/**
 * Translates the SDK's cookie options into the subset Astro writes,
 * then overrides two of them. These two overrides are this product's own
 * security decision, which is why the function lives here, in measured
 * code, rather than in the SDK adapter.
 *
 * `httpOnly` is forced on — whatever the SDK asked: nothing in this
 * application is a Supabase browser client, so no page script has any
 * reason to read the session, and an unreadable cookie is one fewer
 * thing an injected script can steal. The SDK's own default is false,
 * for apps that do need it.
 *
 * `secure` is passed in rather than hard-coded, and the one authority on
 * it is `isSecureRequest` (lib/secure-request.ts): `APP_ENV=prod` means
 * the deployment is https-only and the answer is true, whatever the
 * proxied request URL says. It is NOT read from the request's own
 * protocol — the Worker proxies to this container over plain HTTP, so
 * that reading was false on every deployed shape and this cookie, which
 * carries a refresh token, shipped without `Secure`.
 */
export function astroCookieOptions(
  options: SdkCookieOptions,
  secure: boolean,
): AstroCookieWriteOptions {
  return {
    ...(options.domain === undefined ? {} : { domain: options.domain }),
    ...(options.path === undefined ? {} : { path: options.path }),
    ...(options.expires === undefined ? {} : { expires: options.expires }),
    ...(options.maxAge === undefined ? {} : { maxAge: options.maxAge }),
    ...(options.sameSite === undefined ? {} : { sameSite: options.sameSite }),
    httpOnly: true,
    secure,
  };
}

/** Where the auth provider lives, once both halves are configured. */
export interface SupabaseConfig {
  readonly url: string;
  readonly anonKey: string;
}

/**
 * The auth configuration, or null when this deployment has none.
 *
 * Half a configuration is no configuration: a project URL without a key
 * cannot authenticate anyone, and treating it as "configured" would send
 * editors to a sign-in page that can only fail.
 */
export function supabaseConfig(
  url: string | undefined,
  anonKey: string | undefined,
): SupabaseConfig | null {
  const trimmedUrl = url?.trim() ?? '';
  const trimmedKey = anonKey?.trim() ?? '';
  if (trimmedUrl === '' || trimmedKey === '') {
    return null;
  }
  return { url: trimmedUrl, anonKey: trimmedKey };
}

/**
 * Reads the request's `Cookie` header as name/value pairs.
 *
 * Astro's cookie store can be asked for a name but not enumerated, and
 * the Supabase client needs every cookie it might have written — the
 * session is chunked across several when the token is large.
 */
export function parseCookieHeader(header: string | null): { name: string; value: string }[] {
  if (header === null || header.trim() === '') {
    return [];
  }
  return header
    .split(';')
    .map((pair) => {
      const separator = pair.indexOf('=');
      if (separator < 1) {
        return null;
      }
      const name = pair.slice(0, separator).trim();
      // Values are written percent-encoded; a malformed one is a cookie
      // we did not write, so it passes through rather than throwing.
      const raw = pair.slice(separator + 1).trim();
      let value = raw;
      try {
        value = decodeURIComponent(raw);
      } catch {
        value = raw;
      }
      return name === '' ? null : { name, value };
    })
    .filter((cookie): cookie is { name: string; value: string } => cookie !== null);
}

/**
 * Identities resolved during this request, keyed by the request itself.
 *
 * Weak so an entry lives exactly as long as the request does; nothing has
 * to remember to clear it, and no session can outlive its exchange.
 */
const RESOLVED = new WeakMap<Request, EditorSession>();

/** Files the identity the middleware resolved, for this request only. */
export function rememberEditorSession(request: Request, session: EditorSession): void {
  RESOLVED.set(request, session);
}

/**
 * The current editor session.
 *
 * An unresolved request answers "nobody is signed in" rather than
 * throwing: failing closed keeps a missing middleware from becoming a
 * screen that names an approver it cannot vouch for.
 */
export function editorSession(request: Request): EditorSession {
  return RESOLVED.get(request) ?? NO_EDITOR_SESSION;
}
