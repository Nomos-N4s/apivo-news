import type { APIContext } from 'astro';
import { describe, expect, it } from 'vitest';

import { PREFERENCE_COOKIE } from '../lib/reader/preference';
import { GET } from './go';

interface CookieWrite {
  readonly key: string;
  readonly value: string;
  readonly secure: boolean;
}

/**
 * The minimal request context this endpoint reads: the query it was
 * submitted with, the cookie the browser already carries, and a redirect
 * that records where it was pointed. The rest of APIContext is
 * render-pipeline state a unit test neither needs nor can construct.
 */
function makeContext(
  query: string,
  storedPreference?: string,
): { context: APIContext; writes: CookieWrite[] } {
  const writes: CookieWrite[] = [];
  const url = new URL(`/go${query}`, 'http://localhost:4321');
  const context = {
    url,
    // The endpoint reads the request, not the URL, to decide whether the
    // browser reached us over TLS — behind the Worker the two disagree
    // (src/lib/secure-request.ts).
    request: new Request(url),
    cookies: {
      get: (): { value: string } | undefined =>
        storedPreference === undefined ? undefined : { value: storedPreference },
      set: (key: string, value: string, options: { secure: boolean }): void => {
        writes.push({ key, value, secure: options.secure });
      },
    },
    redirect: (path: string, status?: number): Response =>
      new Response(null, { status: status ?? 302, headers: { location: path } }),
  };
  return { context: context as unknown as APIContext, writes };
}

async function submit(
  query: string,
  storedPreference?: string,
): Promise<{ location: string | null; status: number; writes: CookieWrite[] }> {
  const { context, writes } = makeContext(query, storedPreference);
  const response = await GET(context);
  return { location: response.headers.get('location'), status: response.status, writes };
}

describe('GET /go', () => {
  it('lands the completed setup on the chosen front page', async () => {
    const { location, status } = await submit('?lang=de&place=munich&place=greece');
    expect(status).toBe(302);
    expect(location).toBe('/de/munich+greece');
  });

  it('remembers that front page, and stores nothing else', async () => {
    const { writes } = await submit('?lang=de&place=munich&place=greece');
    // secure is false here because the test context is the development
    // shape: plain http, no forwarded scheme, APP_ENV unset. The
    // deployed answer is asserted in lib/secure-request.test.ts and in
    // the preference cookie's own suite.
    expect(writes).toEqual([
      { key: PREFERENCE_COOKIE, value: '/de/munich+greece', secure: false },
    ]);
  });

  it('leaves an unchanged preference alone', async () => {
    const { writes } = await submit('?lang=el&place=munich', '/el/munich');
    expect(writes).toEqual([]);
  });

  it('returns an empty selection to setup in the language it chose, remembering nothing', async () => {
    const { location, writes } = await submit('?lang=de');
    expect(location).toBe('/de/setup');
    expect(writes).toEqual([]);
  });

  it('returns an unmounted language to setup rather than to some other paper', async () => {
    const unmounted = await submit('?lang=en&place=munich');
    expect(unmounted.location).toBe('/el/setup');
    expect(unmounted.writes).toEqual([]);

    const nothing = await submit('');
    expect(nothing.location).toBe('/el/setup');
    expect(nothing.writes).toEqual([]);
  });

  it('refuses a place the setup screen does not offer', async () => {
    // bavaria is addressable but not selectable: as form input it is not
    // an answer to the question that was asked.
    const { location, writes } = await submit('?lang=el&place=bavaria');
    expect(location).toBe('/el/setup');
    expect(writes).toEqual([]);
  });
});
