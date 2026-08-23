import { API_BASE_URL } from 'astro:env/server';
import type { APIRoute } from 'astro';

import { isCursor, isTourId, writeTourProgress } from '../../../lib/account/tours';
import { resolveEditorSession } from '../../../lib/editorial/supabase';

/**
 * Records tour progress on behalf of the page script.
 *
 * This exists so the BEARER TOKEN STAYS ON THE SERVER. Every editorial
 * screen builds its API client in frontmatter and renders the result; the
 * token is never in the document and never in a page script. Letting the
 * tour write directly to /api/v1/account/tours would have meant putting it
 * in a data attribute, where any script on the page — and anything that
 * ever manages to inject one — could read a credential that approves
 * articles. A guided tour is not worth that, so the browser talks to this
 * same-origin endpoint and the session stays on this side.
 *
 * POST rather than PUT because this is a form-shaped action on a resource
 * this endpoint does not itself represent; the PUT it forwards is the
 * idempotent one.
 *
 * Answers 204 whether or not the record was made. The caller is a tour
 * step, it has already written the browser's own copy, and there is
 * nothing useful it could do with a failure — retrying would interrupt the
 * thing the tour is explaining. A failure that matters shows up as
 * progress that did not follow somebody to their second device, and the
 * log on the API side is where it is diagnosed.
 */
export const prerender = false;

export const POST: APIRoute = async ({ params, request, cookies }) => {
  const tourId = params['tour'] ?? '';
  if (!isTourId(tourId)) {
    return new Response(null, { status: 400 });
  }

  // Resolved here rather than read from the middleware's cache: that
  // middleware runs on /{lang}/editor/* only (isEditorialPath), and this
  // endpoint is deliberately not under it — a route that exists to keep a
  // credential server-side should not depend on somebody remembering to
  // widen a path pattern.
  //
  // Nobody signed in has nothing to record against. Not an error: the
  // first two steps of the editorial tour are on the sign-in screen, where
  // there is no account yet and localStorage is the only place progress
  // can live.
  const session = await resolveEditorSession(request, cookies);
  if (!session.authenticated || session.token === null) {
    return new Response(null, { status: 204 });
  }

  let cursor: unknown;
  try {
    cursor = ((await request.json()) as { cursor?: unknown }).cursor;
  } catch {
    return new Response(null, { status: 400 });
  }
  if (typeof cursor !== 'string' || !isCursor(cursor)) {
    return new Response(null, { status: 400 });
  }

  await writeTourProgress(API_BASE_URL, session.token, tourId, cursor);
  return new Response(null, { status: 204 });
};
