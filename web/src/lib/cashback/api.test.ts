import { describe, expect, it, vi } from 'vitest';

import {
  CashbackApiError,
  CashbackConfigurationError,
  createCashbackApi,
} from './api';

const BASE = 'http://api.internal';

/** A fetch that records its calls and answers with a canned response. */
function stubFetch(
  responder: (url: string, init: RequestInit | undefined) => Response,
): { fetch: typeof fetch; calls: { url: string; init: RequestInit | undefined }[] } {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  const impl = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString();
    calls.push({ url, init });
    return Promise.resolve(responder(url, init));
  });
  return { fetch: impl as unknown as typeof fetch, calls };
}

const json = (body: unknown, status = 200): Response =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

describe('createCashbackApi configuration', () => {
  it('answers from fixtures with no base URL in development, and says so', () => {
    const api = createCashbackApi(undefined);
    expect(api.source).toBe('fixture');
  });

  it('refuses to serve invented balances in a deployed environment', () => {
    expect(() => createCashbackApi(undefined, { appEnv: 'prod' })).toThrow(
      CashbackConfigurationError,
    );
    expect(() => createCashbackApi('', { appEnv: 'prod' })).toThrow(/invented/);
  });

  it('refuses an APP_ENV it cannot read, base URL or not', () => {
    expect(() => createCashbackApi(undefined, { appEnv: 'production' })).toThrow(
      CashbackConfigurationError,
    );
    expect(() => createCashbackApi(BASE, { appEnv: 'staging' })).toThrow(
      /neither "dev" nor "prod"/,
    );
  });

  it('builds an HTTP client when a base URL is set', () => {
    const api = createCashbackApi(BASE, { appEnv: 'prod' });
    expect(api.source).toBe('api');
  });
});

describe('the fixture client', () => {
  it('filters entries by state', async () => {
    const api = createCashbackApi(undefined);
    const all = await api.entries();
    const confirmed = await api.entries({ state: 'confirmed' });
    expect(confirmed.items.length).toBeGreaterThan(0);
    expect(confirmed.items.length).toBeLessThan(all.items.length);
    expect(confirmed.items.every((entry) => entry.state === 'confirmed')).toBe(true);
  });

  it('shows a reversal beside the credit it reverses, never instead of it', async () => {
    const { items } = await createCashbackApi(undefined).entries();
    const reversal = items.find((entry) => entry.reversal_of_id !== null);
    expect(reversal).toBeDefined();
    expect(items.some((entry) => entry.entry_id === reversal?.reversal_of_id)).toBe(true);
    expect(reversal?.reason).toBeTruthy();
  });

  it('reserves more than was asked for, because whole entries are reserved', async () => {
    const request = await createCashbackApi(undefined).requestWithdrawal('d1', 1500, 'EUR');
    expect(request.state).toBe('awaiting_approval');
    expect(request.reserved_amount.minor).toBeGreaterThan(1500);
  });

  it('carries a retailer whose bands have lapsed, with no rate invented', async () => {
    const merchant = await createCashbackApi(undefined).merchant('mikri-patrida');
    expect(merchant).not.toBeNull();
    expect(merchant?.rates).toEqual([]);
  });

  it('answers null for an unknown retailer', async () => {
    expect(await createCashbackApi(undefined).merchant('nothing-here')).toBeNull();
  });

  it('never claims to know a confirmation time', async () => {
    const merchant = await createCashbackApi(undefined).merchant('agora');
    expect(merchant?.typical_confirmation_days).toBeNull();
  });

  it('matches the catalogue search against the name', async () => {
    const api = createCashbackApi(undefined);
    const hits = await api.catalogue({ lang: 'el', places: ['munich'], q: 'agora' });
    expect(hits.items).toHaveLength(1);
    expect(hits.items[0]?.slug).toBe('agora');
  });
});

describe('the HTTP client', () => {
  it('sends the bearer token on every call', async () => {
    const stub = stubFetch(() => json({ items: [], next_cursor: null }));
    const api = createCashbackApi(BASE, { fetch: stub.fetch, token: 'jwt-here' });
    await api.withdrawals();
    const headers = stub.calls[0]?.init?.headers as Record<string, string>;
    expect(headers['Authorization']).toBe('Bearer jwt-here');
  });

  it('sends language and place as separate parameters', async () => {
    const stub = stubFetch(() => json({ items: [], next_cursor: null }));
    await createCashbackApi(BASE, { fetch: stub.fetch }).catalogue({
      lang: 'el',
      places: ['munich', 'greece'],
    });
    const url = new URL(stub.calls[0]?.url ?? '');
    expect(url.searchParams.get('lang')).toBe('el');
    expect(url.searchParams.getAll('place')).toEqual(['munich', 'greece']);
  });

  it('reads a never-opted-in member as null rather than an error', async () => {
    const stub = stubFetch(() => json({ title: 'not found' }, 404));
    const participation = await createCashbackApi(BASE, { fetch: stub.fetch }).participation();
    expect(participation).toBeNull();
  });

  it('reads an unknown retailer as null', async () => {
    const stub = stubFetch(() => json({ title: 'not found' }, 404));
    expect(await createCashbackApi(BASE, { fetch: stub.fetch }).merchant('nope')).toBeNull();
  });

  it('carries the problem document and its code off a refusal', async () => {
    const stub = stubFetch(() =>
      json(
        {
          title: 'Insufficient confirmed balance',
          code: 'insufficient_confirmed_balance',
          shortfall: { minor: 500, currency: 'EUR' },
          threshold: { minor: 1000, currency: 'EUR' },
        },
        409,
      ),
    );
    const api = createCashbackApi(BASE, { fetch: stub.fetch });
    await expect(api.requestWithdrawal('d1', 500, 'EUR')).rejects.toMatchObject({
      status: 409,
    });
    try {
      await api.requestWithdrawal('d1', 500, 'EUR');
    } catch (error) {
      expect(error).toBeInstanceOf(CashbackApiError);
      const failure = error as CashbackApiError;
      expect(failure.code).toBe('insufficient_confirmed_balance');
      expect(failure.problem?.['shortfall']).toEqual({ minor: 500, currency: 'EUR' });
    }
    expect.assertions(4);
  });

  it('survives a failure whose body is not JSON', async () => {
    const stub = stubFetch(() => new Response('<html>502</html>', { status: 502 }));
    const api = createCashbackApi(BASE, { fetch: stub.fetch });
    await expect(api.wallet()).rejects.toMatchObject({ status: 502, problem: null });
  });

  it('posts a withdrawal as minor units and a currency, never a decimal', async () => {
    const stub = stubFetch(() =>
      json({ request_id: 'w1', state: 'awaiting_approval', reserved_amount: { minor: 1840, currency: 'EUR' } }, 201),
    );
    await createCashbackApi(BASE, { fetch: stub.fetch }).requestWithdrawal('d1', 1500, 'EUR');
    const body = JSON.parse(String(stub.calls[0]?.init?.body)) as Record<string, unknown>;
    expect(body['amount']).toEqual({ minor: 1500, currency: 'EUR' });
    expect(JSON.stringify(body)).not.toMatch(/\d+\.\d/);
  });

  it('asks the withdrawal queue only for what is awaiting approval', async () => {
    const stub = stubFetch(() => json({ items: [], next_cursor: null }));
    await createCashbackApi(BASE, { fetch: stub.fetch }).withdrawalsAwaitingApproval();
    expect(stub.calls[0]?.url).toContain('state=awaiting_approval');
  });

  it('encodes a run id into the differences path', async () => {
    const stub = stubFetch(() => json({ items: [], next_cursor: null }));
    await createCashbackApi(BASE, { fetch: stub.fetch }).differences('a/b');
    expect(stub.calls[0]?.url).toContain('/runs/a%2Fb/differences');
  });

  it('mounts every path under the cashback base', async () => {
    const stub = stubFetch(() => json({ items: [], next_cursor: null }));
    const api = createCashbackApi(BASE, { fetch: stub.fetch });
    await api.held();
    await api.unattributed();
    await api.reconciliationRuns();
    for (const call of stub.calls) {
      expect(call.url.startsWith(`${BASE}/api/v1/cashback/`)).toBe(true);
    }
  });

  it('tolerates a base URL with a trailing slash', async () => {
    const stub = stubFetch(() => json({ pending: { minor: 0, currency: 'EUR' } }));
    await createCashbackApi(`${BASE}/`, { fetch: stub.fetch }).wallet();
    expect(stub.calls[0]?.url).toBe(`${BASE}/api/v1/cashback/wallet`);
  });
});
