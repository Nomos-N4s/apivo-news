import { describe, expect, it } from 'vitest';

import {
  createEditorialApi,
  EditorialApiError,
  formatItemCost,
  formatSpend,
  spendPercent,
} from './api';
import { PROVENANCE_FIXTURES, QUEUE_FIXTURES } from './fixtures';

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
    expect(page.holds?.queued_untranslated).toBeGreaterThan(0);
    expect(page.spend?.cap_microusd).toBeGreaterThan(0);
  });

  it('never fakes an approval — nothing is recorded and it says why', async () => {
    const outcome = await api.approve('any-source-item', 'any-translation', 'attr');
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
      'Originally published by X.',
    );

    expect(outcome.recorded).toBe(true);
    expect(outcome.article_id).toBe('a1');
    const body = JSON.parse(String(calls.at(0)?.init?.body)) as Record<string, unknown>;
    expect(body['translation_id']).toBe('tr-1');
    expect(body['source_item_id']).toBeUndefined();
    expect(body['publish']).toBe(true);
    // The contract requires a non-blank attribution (FR-008).
    expect(body['attribution']).toBe('Originally published by X.');
    expect(calls.at(0)?.init?.method).toBe('POST');
  });

  it('approves an untranslated origin with source_item_id only', async () => {
    const { calls, fetchImpl } = respondingWith(jsonResponse({ article_id: 'a2' }, 201));
    await createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve('src-2', null, 'attr');

    const body = JSON.parse(String(calls.at(0)?.init?.body)) as Record<string, unknown>;
    expect(body['source_item_id']).toBe('src-2');
    expect(body['translation_id']).toBeUndefined();
  });

  it('surfaces a 409 (origin already approved) rather than claiming success', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'conflict' }, 409));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve('src', null, 'attr'),
    ).rejects.toMatchObject({ status: 409 });
  });

  it('rejects an approval body without an article id', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ ok: true }, 201));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve('src', null, 'attr'),
    ).rejects.toBeInstanceOf(EditorialApiError);
  });
});

describe('editorial endpoints not deployed yet', () => {
  // API_BASE_URL is one address for the whole Go API, but the reader
  // endpoints (T024) can ship before the editorial ones (T019/T020). A
  // 404 must therefore mean "not deployed", not "the screen is broken".
  it('falls back to fixtures when the queue endpoint 404s', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'not found' }, 404));
    const page = await createEditorialApi('http://api:8080', 'jwt', fetchImpl).queue();
    expect(page.items.length).toBeGreaterThan(0);
  });

  it('reports not-recorded rather than erroring when approvals 404', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'not found' }, 404));
    const outcome = await createEditorialApi('http://api:8080', 'jwt', fetchImpl).approve(
      'src',
      null,
      'attr',
    );
    expect(outcome.recorded).toBe(false);
  });

  it('still surfaces a real failure (500) as an error', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'boom' }, 500));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).queue(),
    ).rejects.toMatchObject({ status: 500 });
  });

  it('falls back to fixtures when the source list 404s', async () => {
    // POST /editorial/sources exists on the API; the list does not, so a
    // 404 there must not take the screen down.
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'not found' }, 404));
    const page = await createEditorialApi('http://api:8080', 'jwt', fetchImpl).sources();
    expect(page.sources.length).toBeGreaterThan(0);
  });

  it('still surfaces 401 on the source list — that is a real answer', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'unauthorised' }, 401));
    await expect(
      createEditorialApi('http://api:8080', null, fetchImpl).sources(),
    ).rejects.toMatchObject({ status: 401 });
  });

  it('accepts a contract-compliant queue carrying only items', async () => {
    // No holds, no spend — the screen must not assume the proposed fields.
    const { fetchImpl } = respondingWith(jsonResponse({ items: [] }));
    const page = await createEditorialApi('http://api:8080', 'jwt', fetchImpl).queue();
    expect(page.items).toEqual([]);
    expect(page.spend).toBeUndefined();
    expect(page.holds).toBeUndefined();
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

describe('the audit trace', () => {
  it('answers a provenance record from fixtures', async () => {
    const record = await createEditorialApi(undefined).provenance(
      'a41e7c92-08d5-4d1b-9d6c-1f0b7e3a55c1',
    );
    expect(record?.approval.approver_name).not.toBe('');
    expect(record?.source_item.usage_rule_snapshot).toBe('extract_and_link');
    expect(record?.events?.length).toBeGreaterThan(0);
  });

  it('carries a null translation when the target locale already matched', async () => {
    const record = await createEditorialApi(undefined).provenance(
      'd57b1f30-6c92-4a44-b8e1-95ac2f7d0e63',
    );
    expect(record?.translation).toBeNull();
  });

  it('never fakes a withdrawal — publication cannot end unrecorded (FR-016)', async () => {
    const outcome = await createEditorialApi(undefined).withdraw('any', 'publisher request');
    expect(outcome.recorded).toBe(false);
    expect(outcome.withdrawn_at).toBeUndefined();
    // The reason must describe withdrawal, not approval.
    expect(outcome.reason).toContain('publication did not end');
  });

  it('reads a 404 trace as null rather than throwing', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'not found' }, 404));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).provenance('missing'),
    ).resolves.toBeNull();
  });

  it('returns null for an id that matches nothing, never another article', async () => {
    const api = createEditorialApi(undefined);
    await expect(api.provenance('00000000-0000-4000-8000-000000000000')).resolves.toBeNull();
  });

  it('shows a trace on first visit, when no id has been given yet', async () => {
    const record = await createEditorialApi(undefined).provenance('');
    expect(record?.article_id).not.toBe('');
  });

  it('makes no request for an empty id — no /articles//provenance', async () => {
    const { calls, fetchImpl } = respondingWith(jsonResponse({}));
    const record = await createEditorialApi('http://api:8080', 'jwt', fetchImpl).provenance('');
    expect(record).toBeNull();
    expect(calls).toHaveLength(0);
  });

  it('rejects a half-shaped provenance rather than crashing the audit', async () => {
    const partial = { article_id: 'x', approval: {}, source: {}, source_item: {} };
    const { fetchImpl } = respondingWith(jsonResponse(partial));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).provenance('x'),
    ).rejects.toBeInstanceOf(EditorialApiError);
  });

  it('rejects a record whose timestamps would format as Invalid Date', async () => {
    const bad = {
      ...PROVENANCE_FIXTURES[0],
      approval: { approver_name: 'E', approver_email: 'e@x', approved_at: 'not a date' },
    };
    const { fetchImpl } = respondingWith(jsonResponse(bad));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).provenance('x'),
    ).rejects.toBeInstanceOf(EditorialApiError);
  });

  it('calls the contract paths for trace and withdrawal', async () => {
    const trace = respondingWith(jsonResponse(PROVENANCE_FIXTURES[0]));
    await createEditorialApi('http://api:8080', 'jwt', trace.fetchImpl).provenance('x');
    expect(trace.calls.at(0)?.url.pathname).toBe('/api/v1/editorial/articles/x/provenance');

    const wd = respondingWith(jsonResponse({ article_id: 'x', withdrawn_at: 'now' }));
    await createEditorialApi('http://api:8080', 'jwt', wd.fetchImpl).withdraw('x', 'because');
    expect(wd.calls.at(0)?.url.pathname).toBe('/api/v1/editorial/articles/x/withdrawal');
    expect(wd.calls.at(0)?.init?.method).toBe('POST');
    expect(JSON.parse(String(wd.calls.at(0)?.init?.body))['reason']).toBe('because');
  });
});

describe('the source list payload', () => {
  const validCycle = { retrieved: 1, duplicates_skipped: 0, failures: [] };
  const validSource = {
    id: 's1',
    name: 'X',
    feed_path: '/rss',
    language: 'de',
    jurisdiction: 'DE',
    usage_rule: 'extract_and_link',
    permission_evidence: null,
    active: true,
    last_polled_at: '2026-08-14T06:12:00Z',
  };

  it('accepts a well-formed list', async () => {
    const { fetchImpl } = respondingWith(
      jsonResponse({ sources: [validSource], cycle: validCycle }),
    );
    const page = await createEditorialApi('http://api:8080', 'jwt', fetchImpl).sources();
    expect(page.sources).toHaveLength(1);
  });

  it('rejects a body whose poll cycle is missing — the screen dereferences it', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ sources: [validSource] }));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).sources(),
    ).rejects.toBeInstanceOf(EditorialApiError);
  });

  it('rejects a last_polled_at the screen could not format', async () => {
    // A non-null invalid timestamp would throw RangeError inside Intl
    // mid-render; rejecting here becomes the page's calm 503 instead.
    const { fetchImpl } = respondingWith(
      jsonResponse({
        sources: [{ ...validSource, last_polled_at: 'not a date' }],
        cycle: validCycle,
      }),
    );
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).sources(),
    ).rejects.toBeInstanceOf(EditorialApiError);
  });

  it('accepts a never-polled source, whose timestamp is null', async () => {
    const { fetchImpl } = respondingWith(
      jsonResponse({
        sources: [{ ...validSource, last_polled_at: null }],
        cycle: validCycle,
      }),
    );
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).sources(),
    ).resolves.toBeTruthy();
  });
});

describe('sources', () => {
  it('lists configured feeds and the deduplicating poll cycle (FR-014)', async () => {
    const page = await createEditorialApi(undefined).sources();
    expect(page.sources.length).toBeGreaterThan(0);
    expect(page.cycle.duplicates_skipped).toBeGreaterThan(0);
    expect(page.cycle.failures.length).toBeGreaterThan(0);
  });

  it('shows no source with a full_text rule — unreachable without evidence (FR-004)', async () => {
    const page = await createEditorialApi(undefined).sources();
    for (const source of page.sources) {
      expect(source.usage_rule).toBe('extract_and_link');
      expect(source.permission_evidence).toBeNull();
    }
  });

  it('never fakes a configured source', async () => {
    const outcome = await createEditorialApi(undefined).addSource({
      name: 'X',
      url: 'https://x.example/rss',
      language: 'de',
      jurisdiction: 'DE',
      licence_terms: 'terms',
    });
    expect(outcome.recorded).toBe(false);
    expect(outcome.reason).toContain('no source was configured');
  });

  it('never sends a usage rule — the contract does not accept one', async () => {
    const { calls, fetchImpl } = respondingWith(jsonResponse({ source_id: 's1' }, 201));
    await createEditorialApi('http://api:8080', 'jwt', fetchImpl).addSource({
      name: 'X',
      url: 'https://x.example/rss',
      language: 'de',
      jurisdiction: 'DE',
      licence_terms: 'terms',
    });
    const body = JSON.parse(String(calls.at(0)?.init?.body)) as Record<string, unknown>;
    expect(body['usage_rule']).toBeUndefined();
    expect(body['name']).toBe('X');
    expect(calls.at(0)?.url.pathname).toBe('/api/v1/editorial/sources');
  });

  it('surfaces a 409 duplicate feed URL rather than claiming success', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'duplicate' }, 409));
    await expect(
      createEditorialApi('http://api:8080', 'jwt', fetchImpl).addSource({
        name: 'X',
        url: 'https://x.example/rss',
        language: 'de',
        jurisdiction: 'DE',
        licence_terms: 't',
      }),
    ).rejects.toMatchObject({ status: 409 });
  });
});
