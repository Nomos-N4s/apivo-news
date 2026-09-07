import { describe, expect, it, vi } from 'vitest';

import { registerAccount } from './register';

const BASE = 'https://api.invalid';
const TOKEN = 'a-verified-token';

const ROW = {
  id: '7dd4bd88-511b-4f82-82b5-640f95c1f2fc',
  email: 'member@example.invalid',
  display_name: 'member',
  role: 'reader',
};

/** A fetch that answers once, and records what it was asked. */
function stubFetch(answer: () => Response): {
  fetch: typeof fetch;
  calls: { url: string; init: RequestInit | undefined }[];
} {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  const stub = vi.fn((url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), init });
    return Promise.resolve(answer());
  });
  return { fetch: stub as unknown as typeof fetch, calls };
}

const json = (body: unknown, status: number): Response =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });

describe('registerAccount', () => {
  it('posts to the account route with the bearer token and no body', async () => {
    // No body by design: the subject and the email come out of the token,
    // which is the only place they can honestly come from. A body would be
    // a client asserting an identity the token already proves.
    const stub = stubFetch(() => json(ROW, 201));
    await registerAccount(BASE, TOKEN, stub.fetch);

    expect(stub.calls[0]?.url).toBe('https://api.invalid/api/v1/account');
    expect(stub.calls[0]?.init?.method).toBe('POST');
    expect(stub.calls[0]?.init?.body).toBeUndefined();
    const headers = stub.calls[0]?.init?.headers as Record<string, string>;
    expect(headers['Authorization']).toBe(`Bearer ${TOKEN}`);
  });

  it('strips a trailing slash from the base rather than doubling it', async () => {
    const stub = stubFetch(() => json(ROW, 201));
    await registerAccount(`${BASE}/`, TOKEN, stub.fetch);

    expect(stub.calls[0]?.url).toBe('https://api.invalid/api/v1/account');
  });

  it.each([
    [201, 'registered'],
    [200, 'existing'],
  ])('reads %d as %s, and carries the row back', async (status, outcome) => {
    const stub = stubFetch(() => json(ROW, status));
    const result = await registerAccount(BASE, TOKEN, stub.fetch);

    expect(result.outcome).toBe(outcome);
    expect(result.account?.id).toBe(ROW.id);
    expect(result.account?.role).toBe('reader');
  });

  it('reads 409 as an address already spoken for', async () => {
    // The subject is the identity and the email is unique, so this is one
    // person arriving under a second provider account with an address
    // already held — a thing to name, never to merge.
    const stub = stubFetch(() => json({ status: 409 }, 409));
    const result = await registerAccount(BASE, TOKEN, stub.fetch);

    expect(result.outcome).toBe('email_taken');
    expect(result.account).toBeNull();
  });

  it('reads 400 as a token carrying no address', async () => {
    const stub = stubFetch(() => json({ status: 400 }, 400));

    await expect(registerAccount(BASE, TOKEN, stub.fetch)).resolves.toMatchObject({
      outcome: 'no_email',
    });
  });

  it.each([401, 500, 502, 503])(
    'reads %d as not now rather than not ever',
    async (status) => {
      // A 401 here means the token the exchange just produced did not verify
      // at the api — clock skew, or a JWKS pointing at another project. The
      // member can do nothing about either, and neither is permanent.
      const stub = stubFetch(() => json({ status }, status));

      await expect(registerAccount(BASE, TOKEN, stub.fetch)).resolves.toMatchObject({
        outcome: 'unavailable',
      });
    },
  );

  it('never throws when the api cannot be reached at all', async () => {
    // A sign-in that has already succeeded must not be undone by this call
    // failing. The session is real; the person is signed in.
    const dead = vi.fn(() => Promise.reject(new TypeError('fetch failed')));

    await expect(
      registerAccount(BASE, TOKEN, dead as unknown as typeof fetch),
    ).resolves.toEqual({ outcome: 'unavailable', account: null });
  });

  it.each([undefined, ''])(
    'is unavailable, and calls nothing, when the deployment names no api (%s)',
    async (base) => {
      // A development run or a pull-request preview has no api to register
      // against, and every cashback surface there answers from fixtures.
      const stub = stubFetch(() => json(ROW, 201));
      const result = await registerAccount(base, TOKEN, stub.fetch);

      expect(result.outcome).toBe('unavailable');
      expect(stub.calls).toHaveLength(0);
    },
  );

  it('is unavailable, and calls nothing, without a token', async () => {
    const stub = stubFetch(() => json(ROW, 201));
    const result = await registerAccount(BASE, '', stub.fetch);

    expect(result.outcome).toBe('unavailable');
    expect(stub.calls).toHaveLength(0);
  });

  it('survives a success whose body is not the shape the contract promises', async () => {
    // The outcome is decided by the status. A body that cannot be read is a
    // reason to carry no row, never a reason to fail a sign-in.
    const stub = stubFetch(
      () => new Response('not json', { status: 201, headers: { 'content-type': 'text/plain' } }),
    );
    const result = await registerAccount(BASE, TOKEN, stub.fetch);

    expect(result.outcome).toBe('registered');
    expect(result.account).toBeNull();
  });

  it.each([
    ['a row with no id', { email: 'a@b.invalid' }],
    ['a row with no email', { id: 'x' }],
    ['a list where an object was promised', [ROW]],
    ['null', null],
  ])('carries no row for %s', async (_label, body) => {
    const stub = stubFetch(() => json(body, 200));
    const result = await registerAccount(BASE, TOKEN, stub.fetch);

    expect(result.outcome).toBe('existing');
    expect(result.account).toBeNull();
  });
});
