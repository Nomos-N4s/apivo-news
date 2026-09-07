import type { APIRoute } from 'astro';
import { API_BASE_URL, APP_ENV, PUBLIC_APP_VERSION } from 'astro:env/server';

import {
  CashbackApiError,
  CashbackConfigurationError,
  createCashbackApi,
} from '../../../lib/cashback/api';
import { isSameOrigin } from '../../../lib/csrf';
import { sessionOf } from '../../../lib/editorial/session';
import { safeReturnPath } from '../../../lib/member/access';

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
 * there is no open redirect on the way OUT.
 *
 * There was one on the way BACK. This docstring reasoned carefully about the
 * success path and left the failure path taking a `back` field off the form
 * and checking it with `back.startsWith('/')` — which `//evil.example`
 * passes, being protocol-relative and off this site. `safeReturnPath`
 * (#564) is the check that was already written for this, and refuses that,
 * `/\host`, control characters and anything absurdly long.
 *
 * A failure renders as a refusal on the merchant page rather than sending
 * the member onward untracked, because an untracked purchase earns nothing
 * and looks exactly like a tracked one until the cashback fails to appear.
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

  /**
   * Where a refusal sends them, with the outcome attached.
   *
   * Built through `URL` rather than by concatenating a `?`: `back` carries no
   * query today, and the day it does, string-joining produces
   * `…?a=1?clickout=409` — a second `?` that every parser reads as part of
   * the first parameter's value.
   */
  const backTo = (status: number): string => {
    const target = new URL(
      safeReturnPath(typeof back === 'string' ? back : null, '/'),
      url.origin,
    );
    target.searchParams.set('clickout', String(status));
    return `${target.pathname}${target.search}`;
  };

  try {
    // Inside the try, because it throws. A deployment with APP_ENV=prod and
    // no API_BASE_URL threw CashbackConfigurationError straight out of this
    // route: an Astro error page to somebody who had just pressed "open the
    // shop", instead of a refusal on the page they came from.
    const session = sessionOf(request);
    const api = createCashbackApi(API_BASE_URL, {
      appEnv: APP_ENV,
      token: session.token,
      appVersion: PUBLIC_APP_VERSION,
    });
    const { redirect_url } = await api.clickout(offerId);
    return redirect(redirect_url, 303);
  } catch (error) {
    // A misconfigured deployment is otherwise silent: the member sees the
    // same refusal as a network fault and only an operator can tell them
    // apart, so the operator gets told.
    if (error instanceof CashbackConfigurationError) {
      console.error(`cashback: ${error.message}`);
    }
    // 403 (not opted in), 409 (expired offer or inactive merchant), 429 and
    // 401 are all outcomes the member has to be told about, and none of them
    // is a reason to send them to the shop anyway. The merchant page turns
    // the status into a sentence; anything that is not the api refusing is a
    // 502, which is what "we could not reach the shop" means here.
    const status = error instanceof CashbackApiError ? error.status : 502;
    return redirect(backTo(status), 303);
  }
};
