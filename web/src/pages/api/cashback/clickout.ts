import type { APIRoute } from 'astro';
import { API_BASE_URL, APP_ENV } from 'astro:env/server';

import { CashbackApiError, createCashbackApi } from '../../../lib/cashback/api';
import { isSameOrigin } from '../../../lib/csrf';
import { editorSession } from '../../../lib/editorial/session';

/**
 * The click-out: the one place a member leaves for a shop.
 *
 * It is a POST rather than a link because it creates a row. `POST
 * /clickouts` commits the click and its rate snapshot **before** it answers,
 * so a member who arrives at the shop is a member whose click exists; a
 * GET that a prefetcher or a crawler could follow would manufacture clicks
 * nobody made, and every one of them would be evidence in a later
 * attribution.
 *
 * The redirect is a 303 to the target the API returned, and only to that:
 * the offer id names the deeplink, the member never supplies a URL, and so
 * there is no open redirect to close. A failure renders as a plain refusal
 * on the merchant page rather than sending the member onward untracked,
 * because an untracked purchase earns nothing and looks exactly like a
 * tracked one until the cashback fails to appear.
 */
export const POST: APIRoute = async ({ request, redirect, url }) => {
  if (!isSameOrigin(request, url.origin)) {
    return new Response('cross-origin click-out refused', { status: 403 });
  }

  const form = await request.formData();
  const offerId = form.get('offer_id');
  const back = form.get('back');
  if (typeof offerId !== 'string' || offerId === '') {
    return new Response('offer_id is required', { status: 400 });
  }

  const session = editorSession(request);
  const api = createCashbackApi(API_BASE_URL, { appEnv: APP_ENV, token: session.token });

  try {
    const { redirect_url } = await api.clickout(offerId);
    return redirect(redirect_url, 303);
  } catch (error) {
    // 401, 409 (expired offer or inactive merchant) and 429 are all
    // outcomes the member has to be told about, and none of them is a
    // reason to send them to the shop anyway.
    const status = error instanceof CashbackApiError ? error.status : 502;
    const target = typeof back === 'string' && back.startsWith('/') ? back : '/';
    return redirect(`${target}?clickout=${status}`, 303);
  }
};
