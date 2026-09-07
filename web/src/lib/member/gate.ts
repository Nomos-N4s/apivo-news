import { CashbackApiError } from '../cashback/api';
import { isReadingLanguage, type ReadingLanguage } from '../reader/axes';

/**
 * Who may see a cashback surface, and what happens to everybody else
 * (issue #570).
 *
 * There is no anonymous cashback surface — every route answers 401 without a
 * token, the catalogue included (FR-023). That is a property of the whole
 * product, not of any one page, so the fence stands in the middleware beside
 * the crawler fence rather than being remembered separately by each of the
 * four member pages. A fifth page added later is gated by existing.
 *
 * FIXTURE MODE IS EXEMPT FROM IT, and that exemption is the reason this is a
 * decision rather than a one-line check. A pull-request preview runs with no
 * api and nobody signed in, and its entire job is to render the screens for
 * review; redirecting there would make previews useless for the one thing
 * they exist for. So the question is not "is somebody signed in" but "is
 * there an api here that would refuse them".
 */

/** Whether this visitor must sign in before a cashback page will render. */
export function needsSignIn(authenticated: boolean, fixtures: boolean): boolean {
  return !fixtures && !authenticated;
}

/**
 * The reading language of a member cashback page, or null for anything else.
 *
 * Only `/{lang}/{place}/cashback…` is matched. `/ops` has a role of its own to
 * check and no language in its path, and `/api/cashback/…` is posted to by a
 * form — answering a POST with a redirect to a sign-in page would lose what
 * was posted, so that route keeps its own refusal.
 *
 * A language this application does not serve answers null rather than a
 * default: the page's own guard rewrites those to /404, and a fence that
 * redirected them to a Greek sign-in would be inventing a reader's language
 * on the way past (FR-009, FR-015).
 */
export function cashbackPageLanguage(pathname: string): ReadingLanguage | null {
  const match = /^\/([^/]+)\/[^/]+\/cashback(?:\/|$)/.exec(pathname);
  const lang = match?.[1];
  return lang !== undefined && isReadingLanguage(lang) ? lang : null;
}

/**
 * Whether a failure from the cashback client was the api declining to
 * recognise the caller.
 *
 * 401 and 403 both mean it: 401 is no token or one that does not verify, 403
 * is a token that verifies for somebody this route is not for.
 *
 * Note what pages must NOT do with this: redirect. Past the middleware fence
 * there IS a session, so a 401 here means the api does not recognise a person
 * who is signed in — an account row that was never created (#567), or a token
 * this deployment's JWKS will not verify. Sending them to sign in would
 * return them to the same 401 and the same redirect, forever. The page says
 * so instead, and offers the sign-in as a choice rather than taking it.
 */
export function isUnauthorized(error: unknown): boolean {
  return error instanceof CashbackApiError && (error.status === 401 || error.status === 403);
}
