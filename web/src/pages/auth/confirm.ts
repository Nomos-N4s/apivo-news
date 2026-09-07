/**
 * Where the auth provider sends a member back to after they open the link
 * (issue #564).
 *
 * It renders nothing. It exchanges what arrived for a session, writes the
 * cookies through the same `@supabase/ssr` adapter the editorial sign-in
 * uses, and redirects — onward on success, back to the sign-in page with an
 * outcome on failure. A failure needs a sentence in the member's own
 * language, and this route has no language of its own, so it hands the
 * outcome to a page that does.
 *
 * IT ACCEPTS BOTH SHAPES A PROVIDER MIGHT SEND, and that is deliberate
 * rather than indecisive. Which one arrives depends on the project's email
 * template and on the flow the SDK negotiated:
 *
 *   * `?token_hash=&type=` — the template was pointed at this route with
 *     `{{ .TokenHash }}`, and the token is verified here.
 *   * `?code=` — the default template's `{{ .ConfirmationURL }}` under PKCE,
 *     which lands here with a code to exchange against a verifier this
 *     browser is holding in a cookie.
 *
 * A route that handled one and answered 500 to the other would be a
 * configuration trap: it would work on the deployment it was written against
 * and fail on the next one, with a stack trace instead of a sentence.
 *
 * There is no anonymous outcome. Whatever happens, the member ends up on a
 * page that tells them what happened; nothing here answers with a bare error
 * to a person who was only clicking a link in their mail.
 *
 * AND IT REGISTERS THEM (issue #567). A session is not an account: every
 * cashback route looks the caller up in `account` and answers 401 when there
 * is no row, so a member who got this far and no further would find every
 * money surface failing while nothing looked broken. This is the one moment
 * the web holds a fresh token and knows the sign-in just succeeded, which
 * makes it the only place that call belongs.
 */

import type { APIRoute } from 'astro';
import { API_BASE_URL } from 'astro:env/server';

import { registerAccount } from '../../lib/account/register';
import { authClient } from '../../lib/editorial/supabase';
import { safeReturnPath, signInPath } from '../../lib/member/access';
import { DEFAULT_FRONT_PAGE, isReadingLanguage } from '../../lib/reader/axes';

/**
 * The `type` values a link out of an email can legitimately carry.
 *
 * Narrowed here rather than asserted at the call, because the value comes
 * out of a query string a member's mail client handled and is therefore
 * input. `recovery` is deliberately NOT on the list. The provider will verify
 * one, and this product asks for no password and therefore never sends one,
 * so a `recovery` arriving here did not come from us — which makes refusing
 * it the whole point of narrowing rather than casting.
 */
const EMAIL_OTP_TYPES = ['email', 'magiclink', 'signup', 'invite', 'email_change'] as const;

type EmailOtpType = (typeof EMAIL_OTP_TYPES)[number];

function emailOtpType(value: string | null): EmailOtpType | null {
  if (value === null || value === '') {
    // The template that names this route may leave it off; `email` is what
    // a token hash from a magic link verifies as.
    return 'email';
  }
  return (EMAIL_OTP_TYPES as readonly string[]).includes(value) ? (value as EmailOtpType) : null;
}

export const GET: APIRoute = async ({ request, cookies, redirect, url }) => {
  const asked = url.searchParams.get('lang');
  const lang = asked !== null && isReadingLanguage(asked) ? asked : 'el';
  const next = safeReturnPath(url.searchParams.get('next'), DEFAULT_FRONT_PAGE);

  const client = authClient(request, cookies);
  if (client === null) {
    return redirect(signInPath(lang, { next, outcome: 'unconfigured' }), 303);
  }

  const tokenHash = url.searchParams.get('token_hash');
  const type = url.searchParams.get('type');
  const code = url.searchParams.get('code');

  // `expired` covers a link that was too old, one already spent, and one
  // whose verifier this browser never held — a link forwarded to a phone.
  // They are one sentence to a member, whose next move is the same for all
  // three: ask for another one. Distinguishing them would be describing our
  // plumbing to somebody who wants to read their wallet.
  const expired = redirect(signInPath(lang, { next, outcome: 'expired' }), 303);

  // The session the exchange produced, or null for every way it can fail to
  // produce one. Both shapes answer the same thing, so what follows is
  // written once.
  let token: string | null = null;
  if (tokenHash !== null && tokenHash !== '') {
    const kind = emailOtpType(type);
    if (kind === null) {
      return expired;
    }
    const { data, error } = await client.auth.verifyOtp({ token_hash: tokenHash, type: kind });
    token = error === null ? (data.session?.access_token ?? null) : null;
  } else if (code !== null && code !== '') {
    const { data, error } = await client.auth.exchangeCodeForSession(code);
    token = error === null ? (data.session?.access_token ?? null) : null;
  } else {
    // Neither shape arrived: somebody opened this URL by hand, or a link was
    // truncated on its way through a mail client. Same advice, same sentence.
    return expired;
  }
  if (token === null) {
    return expired;
  }

  // Never throws, whatever the api does. A sign-in that has already
  // succeeded must not be undone by this call.
  const { outcome } = await registerAccount(API_BASE_URL, token);

  // The two refusals that leave a valid session behind which can reach
  // nothing. Dropping it is kinder than keeping it: a member holding a
  // session that 401s everywhere has no way to tell that from a broken site,
  // and signing in again is the move they would try anyway.
  if (outcome === 'email_taken' || outcome === 'no_email') {
    await client.auth.signOut();
    const said = outcome === 'email_taken' ? 'taken' : 'unmade';
    return redirect(signInPath(lang, { next, outcome: said }), 303);
  }

  // `unavailable` lands here with the successes, deliberately. The api being
  // unreachable, or a preview having none at all, is not a reason to refuse
  // somebody the session they just proved they are entitled to.
  return redirect(next, 303);
};
