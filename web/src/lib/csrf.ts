/**
 * Same-origin checking for state-changing form posts.
 *
 * The editorial screens submit plain HTML forms, and the server handler
 * calls the editorial API with the editor's own credentials. Once
 * `editorSession()` reads a cookie-backed Supabase session, a cross-site
 * form could otherwise drive a privileged action — approving or
 * withdrawing an article — using a logged-in editor's browser. That is
 * the classic CSRF shape, and this product's whole claim is that an
 * approval names the person who made it.
 *
 * The check goes in before real approvals exist rather than after, so
 * enabling auth cannot silently open the hole. It is a floor, not a
 * ceiling: a per-session token belongs here too once there are sessions
 * to bind one to.
 */

/**
 * Whether a state-changing request came from this site.
 *
 * `Origin` is sent by browsers on every form POST and cannot be forged by
 * page script, so it is the check that matters. `Referer` is the fallback
 * for the rare client that omits `Origin`. A request carrying neither is
 * refused: for a form post, absence is not evidence of good faith.
 */
export function isSameOrigin(request: Request, siteOrigin: string): boolean {
  const origin = request.headers.get('origin');
  if (origin !== null && origin !== '') {
    return origin === siteOrigin;
  }
  const referer = request.headers.get('referer');
  if (referer === null || referer === '') {
    return false;
  }
  try {
    return new URL(referer).origin === siteOrigin;
  } catch {
    return false;
  }
}
