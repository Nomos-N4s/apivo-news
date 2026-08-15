import type { APIRoute } from 'astro';

import {
  composeFrontPageTarget,
  DEFAULT_FRONT_PAGE,
  isReadingLanguage,
} from '../lib/reader/axes';

/**
 * The setup form's destination: `GET /go?lang=el&place=munich&place=greece`
 * answers 302 to the composed front page — the URL scheme is the only
 * state the axes have (FR-009). A submission with no valid place returns
 * to the setup page (the form works without JavaScript, so nothing stops
 * an empty selection); anything without a mounted language falls back to
 * the flagship journey.
 */
export const GET: APIRoute = ({ url, redirect }) => {
  const langParam = url.searchParams.get('lang');
  const target = composeFrontPageTarget(langParam, url.searchParams.getAll('place'));
  if (target !== null) {
    return redirect(target, 302);
  }
  if (langParam !== null && isReadingLanguage(langParam)) {
    return redirect(`/${langParam}/setup`, 302);
  }
  return redirect(DEFAULT_FRONT_PAGE, 302);
};
