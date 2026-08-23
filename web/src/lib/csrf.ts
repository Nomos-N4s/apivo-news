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
 *
 * ---------------------------------------------------------------------------
 * The comparison is on HOST, not on the whole origin
 *
 * @astrojs/node builds `Astro.url` from the socket and the Host header and
 * ignores X-Forwarded-Proto, so behind TLS termination `Astro.url.origin` is
 * `http://<host>` while the browser sends `Origin: https://<host>`. Compared
 * whole, every editorial form post fails.
 *
 * Three proxies compensated for that by REWRITING the header — Caddy's
 * (apivo-routes) and the Worker's rewriteSameSiteOriginHeaders. Each needs
 * the site's hostname at parse time, which a wildcard site does not have:
 * `header_up` will not expand a placeholder in its search argument, and the
 * regexp form does not substitute in Caddy 2.10 (tested with `$1`, `{re.1}`
 * and `{http.regexp.<name>.1}`). So previews had no rewrite, and their
 * editorial form posts — sign-in included — were refused. That was recorded
 * as costing nothing "because a preview has no auth to sign in with", which
 * stopped being true the moment previews got auth.
 *
 * Comparing hosts makes the check independent of every proxy, which
 * snippets.caddy and docs/ENVIRONMENTS.md both named as the better fix. It
 * does not weaken it: CSRF is about WHICH SITE posted, and the host is that
 * answer. `https://evil.example` and `https://ra1ze.com` differ on host. The
 * only thing given up is refusing a post from this same host over http —
 * which requires the site to be served over http at all, and
 * `isSecureRequest` (APP_ENV=prod) plus the redirect at the edge is what
 * settles that, rather than a string comparison here.
 */
export function isSameOrigin(request: Request, siteOrigin: string): boolean {
  const expected = hostOf(siteOrigin);
  if (expected === null) {
    return false;
  }
  const origin = request.headers.get('origin');
  if (origin !== null && origin !== '') {
    return hostOf(origin) === expected;
  }
  const referer = request.headers.get('referer');
  if (referer === null || referer === '') {
    return false;
  }
  return hostOf(referer) === expected;
}

/** The host (name and port) of a URL, or null when it is not one. */
function hostOf(value: string): string | null {
  try {
    const host = new URL(value).host;
    return host === '' ? null : host;
  } catch {
    return null;
  }
}
