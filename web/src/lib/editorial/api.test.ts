import { describe, expect, it } from 'vitest';

import {
  createEditorialApi,
  EditorialApiError,
  formatItemCost,
  formatSpend,
  spendPercent,
} from './api';
import { QUEUE_FIXTURES } from './fixtures';

function respondingWith(response: Response): {
  calls: { url: URL; init: RequestInit | undefined }[];
  fetchImpl: typeof fetch;
} {
  const calls: { url: URL; init: RequestInit | undefined }[] = [];
  const fetchImpl: typeof fetch = (input, init) => {
    calls.push({ url: new URL(input instanceof URL ? input.href : String(input)), init });
    return Promise.resolve(response);
  };
  return { calls, fetchImpl };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('the fixture client (no API_BASE_URL)', () => {
  const api = createEditorialApi(undefined);

  it('answers the queue with the pipeline holds and the ledger', async () => {
    const page = await api.queue();
    expect(page.items.length).toBeGreaterThan(0);
    expect(page.holds.queued_untranslated).toBeGreaterThan(0);
    expect(page.spend.cap_microusd).toBeGreaterThan(0);
  });

  it('never fakes an approval — nothing is recorded and it says why', async () => {
    const outcome = await api.approve('any-source-item', 'any-translation');
    expect(outcome.recorded).toBe(false);
    expect(outcome.article_id).toBeUndefined();
    expect(outcome.reason).toContain('not implemented');
  });

  it('treats an empty base URL like an absent one', async () => {
    const page = await createEditorialApi('').queue();
    expect(page.items.length).toBeGreaterThan(0);
  });
});

describe('the HTTP client (API_BASE_URL set)', () => {
  const queueBody = {
    items: QUEUE_FIXTURES,
    holds: { queued_untranslated: 0, skipped_over_ceiling: 0 },
    spend: { month: '2026-08', spent_microusd: 0, cap_microusd: 25_000_000 },
  };

  it('calls the contract path and carries the editor token', async () => {
    const { calls, fetchImpl } = respondingWith(jsonResponse(queueBody));
    await createEditorialApi('http://api:8080/', 'jwt-token', fetchImpl).queue();

    expect(calls.at(0)?.url.pathname).toBe('/api/v1/editorial/queue');
    const headers = calls.at(0)?.init?.headers as Record<string, string>;
    expect(headers['Authorization']).toBe('Bearer jwt-token');
  });

  it('omits the Authorization header when there is no token', async () => {
    const { calls, fetchImpl } = respondingWith(jsonResponse(queueBody));
    await createEditorialApi('http://api:8080', null, fetchImpl).queue();
    const headers = calls.at(0)?.init?.headers as Record<string, string>;
    expect(headers['Authorization']).toBeUndefined();
  });

  it('surfaces 401/403 as an EditorialApiError with the status', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'forbidden' }, 403));
    const api = createEditorialApi('http://api:8080', 'jwt', fetchImpl);
    await expect(api.queue()).rejects.toMatchObject({
      name: 'EditorialApiError',
      status: 403,
    });
  });

  it('rejects a queue body without an items array', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ nonsense: true }));
    await expect(
      createEditorialApi('http://api:8080', null, fetchImpl).queue(),
    ).rejects.toBeInstanceOf(EditorialApiError);
  });

  it('approves a translated origin with translation_id only (contract XOR)', async () => {
    const { calls, fetchImpl } = respondingWith(
      jsonResponse({ article_id: 'a1', approved_by: 'Eleni', approved_at: 'now' }, 201),
    );
    const outcome = await createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve(
      'src-1',
      'tr-1',
    );

    expect(outcome.recorded).toBe(true);
    expect(outcome.article_id).toBe('a1');
    const body = JSON.parse(String(calls.at(0)?.init?.body)) as Record<string, unknown>;
    expect(body['translation_id']).toBe('tr-1');
    expect(body['source_item_id']).toBeUndefined();
    expect(body['publish']).toBe(true);
    expect(calls.at(0)?.init?.method).toBe('POST');
  });

  it('approves an untranslated origin with source_item_id only', async () => {
    const { calls, fetchImpl } = respondingWith(jsonResponse({ article_id: 'a2' }, 201));
    await createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve('src-2', null);

    const body = JSON.parse(String(calls.at(0)?.init?.body)) as Record<string, unknown>;
    expect(body['source_item_id']).toBe('src-2');
    expect(body['translation_id']).toBeUndefined();
  });

  it('surfaces a 409 (origin already approved) rather than claiming success', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'conflict' }, 409));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve('src', null),
    ).rejects.toMatchObject({ status: 409 });
  });

  it('rejects an approval body without an article id', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ ok: true }, 201));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve('src', null),
    ).rejects.toBeInstanceOf(EditorialApiError);
  });
});

describe('ledger formatting', () => {
  it('reads the ledger in dollars, matching the cost_microusd column', () => {
    expect(formatSpend(9_200_000)).toBe('$9.20');
    expect(formatSpend(25_000_000)).toBe('$25.00');
  });

  it('shows a per-item cost below a cent at three decimals', () => {
    expect(formatItemCost(4000)).toBe('$0.004');
    expect(formatItemCost(0)).toBe('$0.000');
  });

  it('computes the bar fill and clamps an overrun to 100', () => {
    expect(spendPercent({ month: 'm', spent_microusd: 5_000_000, cap_microusd: 25_000_000 })).toBe(
      20,
    );
    expect(
      spendPercent({ month: 'm', spent_microusd: 40_000_000, cap_microusd: 25_000_000 }),
    ).toBe(100);
  });

  it('never divides by a zero cap', () => {
    expect(spendPercent({ month: 'm', spent_microusd: 1, cap_microusd: 0 })).toBe(0);
  });
});
