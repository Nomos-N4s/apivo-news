import { describe, expect, it, vi } from 'vitest';

/**
 * The click-out endpoint (issue #596).
 *
 * It had no test at all, which is how an open redirect sat on its failure
 * path under a docstring explaining why it had none. The route is an
 * `APIRoute`, so its `POST` is called directly — no container, no rendering.
 *
 * With no `API_BASE_URL` the client answers from fixtures, whose `clickout`
 * resolves; the refusal cases therefore drive the failure path by making the
 * api throw rather than by hoping one does.
 */

const ORIGIN = 'https://example.invalid';
const OFFER = '0be7b5b7-eccd-4507-ab69-8a44d0a404e8';

/** A `redirect` with Astro's signature, recording where it was sent. */
function harness(): {
  redirect: (location: string, status?: number) => Response;
  sent: string[];
} {
  const sent: string[] = [];
  return {
    redirect: (location: string, status = 302): Response => {
      sent.push(location);
      return new Response(null, { status, headers: { Location: location } });
    },
    sent,
  };
}

async function post(
  body: Record<string, string>,
  options: { origin?: string | null; env?: Record<string, unknown> } = {},
): Promise<{ response: Response; sent: string[] }> {
  vi.resetModules();
  vi.doMock('astro:env/server', () => options.env ?? { APP_ENV: 'dev', API_BASE_URL: undefined, PUBLIC_APP_VERSION: undefined });
  const { POST } = await import('./clickout');

  const form = new FormData();
  for (const [key, value] of Object.entries(body)) {
    form.set(key, value);
  }
  const headers = new Headers();
  const origin = options.origin === undefined ? ORIGIN : options.origin;
  if (origin !== null) {
    headers.set('origin', origin);
  }
  const url = new URL(`${ORIGIN}/api/cashback/clickout`);
  const { redirect, sent } = harness();
  const response = await POST({
    request: new Request(url, { method: 'POST', body: form, headers }),
    redirect,
    url,
  } as unknown as Parameters<typeof POST>[0]);

  return { response: response as Response, sent };
}

describe('the click-out endpoint', () => {
  it('refuses a cross-origin post', async () => {
    // A click is a row with money behind it, and `isSameOrigin` compares
    // hosts rather than whole origins for the reason csrf.ts sets out.
    const { response } = await post({ offer_id: OFFER }, { origin: 'https://evil.invalid' });

    expect(response.status).toBe(403);
  });

  it('refuses a post carrying no origin or referer at all', async () => {
    const { response } = await post({ offer_id: OFFER }, { origin: null });

    expect(response.status).toBe(403);
  });

  it('refuses a post with no offer', async () => {
    const { response } = await post({ back: '/el/munich/cashback/agora' });

    expect(response.status).toBe(400);
  });

  it('forwards exactly the target the api named, and attaches nothing', async () => {
    const { response, sent } = await post({ offer_id: OFFER });

    expect(response.status).toBe(303);
    expect(sent).toHaveLength(1);
    // The fixture answers `/` on purpose — a preview must never send anybody
    // to a real shop under a click that was never recorded. What this pins
    // is that a SUCCESS forwards the api's target untouched: no outcome
    // parameter, no rewriting, nothing of ours added to a deeplink whose
    // query string is what the network attributes the sale by.
    expect(sent[0]).toBe('/');
    expect(sent[0]).not.toContain('clickout=');
  });

  /*
   * The failure path, and the reason this file exists.
   */

  it.each([
    ['//evil.example', 'protocol-relative, and off this site'],
    ['//evil.example/el/munich', 'the same with a plausible tail'],
    ['https://evil.example/', 'an absolute URL'],
    ['/\\evil.example', 'a backslash a browser normalises to a slash'],
    ['el/munich', 'relative, so it depends on where it is read'],
    ['/auth/confirm', 'back into the sign-in flow'],
  ])('never sends a refusal to %s (%s)', async (back) => {
    const { sent } = await post(
      { offer_id: 'not-a-uuid-so-the-api-refuses', back },
      { env: { APP_ENV: 'prod', API_BASE_URL: '', PUBLIC_APP_VERSION: undefined } },
    );

    expect(sent).toHaveLength(1);
    expect(sent[0]).toBe('/?clickout=502');
    expect(sent[0]).not.toContain('evil.example');
  });

  it('keeps a return path that is genuinely this site’s', async () => {
    const { sent } = await post(
      { offer_id: 'not-a-uuid-so-the-api-refuses', back: '/el/munich/cashback/agora' },
      { env: { APP_ENV: 'prod', API_BASE_URL: '', PUBLIC_APP_VERSION: undefined } },
    );

    expect(sent[0]).toBe('/el/munich/cashback/agora?clickout=502');
  });

  it('adds its outcome to a return path that already carries a query', async () => {
    // `?` used to be appended unconditionally, which built `…?a=1?clickout=x`
    // the moment `back` carried anything.
    const { sent } = await post(
      { offer_id: 'not-a-uuid-so-the-api-refuses', back: '/el/munich/cashback?q=agora' },
      { env: { APP_ENV: 'prod', API_BASE_URL: '', PUBLIC_APP_VERSION: undefined } },
    );

    expect(sent[0]).toBe('/el/munich/cashback?q=agora&clickout=502');
    expect(sent[0]).not.toContain('?q=agora?');
  });

  it('redirects rather than throwing when the deployment names no api', async () => {
    /*
     * APP_ENV=prod with no API_BASE_URL makes createCashbackApi throw. It
     * used to do so from outside the try, so somebody who had just pressed
     * "open the shop" got an Astro error page instead of a refusal on the
     * page they came from.
     */
    const { response, sent } = await post(
      { offer_id: OFFER, back: '/el/munich/cashback/agora' },
      { env: { APP_ENV: 'prod', API_BASE_URL: '', PUBLIC_APP_VERSION: undefined } },
    );

    expect(response.status).toBe(303);
    expect(sent[0]).toBe('/el/munich/cashback/agora?clickout=502');
  });
});
