import type { APIRoute } from 'astro';
import { APP_ENV } from 'astro:env/server';

import { composeFrontPageTarget, isReadingLanguage } from '../lib/reader/axes';
import {
  FIRST_RUN_SETUP_PATH,
  PREFERENCE_COOKIE,
  rememberPreference,
  setupPath,
} from '../lib/reader/preference';
import { isSecureRequest } from '../lib/secure-request';

/**
 * The setup form's destination: `GET /go?lang=el&place=munich&place=greece`
 * answers 302 to the composed front page — the URL scheme is the only
 * state the axes have (FR-009) — and stores that front page as the one
 * `/` opens from now on (issue #133). The reader has just answered both
 * questions explicitly, which is the only thing that ever writes the
 * preference here.
 *
 * A submission with no valid place returns to the setup page (the form
 * works without JavaScript, so nothing stops an empty selection);
 * anything without a mounted language returns to setup as well, in the
 * language the first run is asked in. Neither case guesses a front page:
 * an unanswerable submission goes back to the question.
 */
export const GET: APIRoute = ({ cookies, request, url, redirect }) => {
  const langParam = url.searchParams.get('lang');
  const target = composeFrontPageTarget(langParam, url.searchParams.getAll('place'));
  if (target !== null) {
    rememberPreference(cookies, target, {
      current: cookies.get(PREFERENCE_COOKIE)?.value,
      // The request and APP_ENV, never `url`: this container is reached
      // over plain HTTP behind the Worker, so `url` says http:// on an
      // https-only deployment (lib/secure-request.ts).
      secure: isSecureRequest(request, APP_ENV),
    });
    return redirect(target, 302);
  }
  if (langParam !== null && isReadingLanguage(langParam)) {
    return redirect(setupPath(langParam), 302);
  }
  return redirect(FIRST_RUN_SETUP_PATH, 302);
};
