import { describe, expect, it } from 'vitest';

import {
  createReaderApi,
  probeEmptyPlaces,
  ReaderApiError,
  type FrontPageData,
  type ReaderApi,
} from './api';
import type { Place } from './axes';
import { FRONT_FIXTURES } from './fixtures';

const MUNICH: Place = {
  slug: 'munich',
  endonym: 'München',
  endonymLang: 'de',
  scope: 'city',
  selectable: true,
  parents: ['Bayern', 'Deutschland'],
};
const BAVARIA: Place = {
  slug: 'bavaria',
  endonym: 'Bayern',
  endonymLang: 'de',
  scope: 'region',
  selectable: false,
  parents: ['Deutschland'],
};

describe('the fixture client (no API_BASE_URL)', () => {
  const api = createReaderApi(undefined);

  it('scopes by both axes: language AND any followed place', async () => {
    const { items } = await api.front({ lang: 'el', places: ['munich'] });
    expect(items.length).toBeGreaterThan(0);
    for (const item of items) {
      expect(item.lang).toBe('el');
      expect(item.places).toContain('munich');
    }
  });

  it('answers newest first across places (US1-AC1)', async () => {
    const { items } = await api.front({ lang: 'el', places: ['munich', 'greece'] });
    const times = items.map((item) => item.published_at);
    expect(times).toEqual([...times].sort().reverse());
    const slugs = new Set(items.flatMap((item) => [...item.places]));
    expect(slugs.has('munich')).toBe(true);
    expect(slugs.has('greece')).toBe(true);
  });

  it('answers an empty list, never an error, for a place with nothing (US1-AC3)', async () => {
    const { items, next_cursor } = await api.front({ lang: 'el', places: ['bavaria'] });
    expect(items).toEqual([]);
    expect(next_cursor).toBeNull();
  });

  it('respects the limit', async () => {
    const { items } = await api.front({ lang: 'el', places: ['munich', 'greece'], limit: 2 });
    expect(items).toHaveLength(2);
  });

  it('treats an empty base URL like an absent one', async () => {
    const { items } = await createReaderApi('').front({ lang: 'de', places: ['munich'] });
    expect(items.length).toBeGreaterThan(0);
  });

  it('finds an article by id, with the approval time on the record', async () => {
    const first = FRONT_FIXTURES[0];
    const article = await api.article(first?.id ?? '');
    expect(article?.headline).toBe(first?.headline);
    expect(article?.approved_at).toBeTruthy();
  });

  it('answers null for an unknown id — the contract 404', async () => {
    await expect(api.article('00000000-0000-4000-8000-000000000000')).resolves.toBeNull();
  });
});

describe('the HTTP client (API_BASE_URL set)', () => {
  const page: FrontPageData = {
    items: [FRONT_FIXTURES[0] as FrontPageData['items'][number]],
    next_cursor: null,
  };

  function respondingWith(response: Response): { fetched: URL[]; fetchImpl: typeof fetch } {
    const fetched: URL[] = [];
    const fetchImpl: typeof fetch = (input) => {
      fetched.push(new URL(input instanceof URL ? input.href : String(input)));
      return Promise.resolve(response);
    };
    return { fetched, fetchImpl };
  }

  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  it('speaks the contract: lang, repeatable place, limit — and strips trailing slashes', async () => {
    const { fetched, fetchImpl } = respondingWith(jsonResponse(page));
    const api = createReaderApi('http://api:8080/', fetchImpl);
    const data = await api.front({ lang: 'el', places: ['munich', 'greece'] });

    expect(data.items).toHaveLength(1);
    const url = fetched.at(0);
    expect(url?.pathname).toBe('/api/v1/front');
    expect(url?.searchParams.get('lang')).toBe('el');
    expect(url?.searchParams.getAll('place')).toEqual(['munich', 'greece']);
    expect(url?.searchParams.get('limit')).toBe('20');
  });

  it('sends an explicit limit through', async () => {
    const { fetched, fetchImpl } = respondingWith(jsonResponse(page));
    await createReaderApi('http://api:8080', fetchImpl).front({
      lang: 'de',
      places: ['munich'],
      limit: 5,
    });
    expect(fetched.at(0)?.searchParams.get('limit')).toBe('5');
  });

  it('surfaces a non-2xx answer as a ReaderApiError with the status', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'unknown place' }, 400));
    const api = createReaderApi('http://api:8080', fetchImpl);
    await expect(api.front({ lang: 'el', places: ['munich'] })).rejects.toMatchObject({
      name: 'ReaderApiError',
      status: 400,
    });
  });

  it('rejects a body without an items array', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ nonsense: true }));
    const api = createReaderApi('http://api:8080', fetchImpl);
    await expect(api.front({ lang: 'el', places: ['munich'] })).rejects.toBeInstanceOf(
      ReaderApiError,
    );
  });

  it('fetches an article by id at the contract path', async () => {
    const detail = FRONT_FIXTURES[0];
    const { fetched, fetchImpl } = respondingWith(jsonResponse(detail));
    const api = createReaderApi('http://api:8080', fetchImpl);
    const article = await api.article(detail?.id ?? '');
    expect(article?.id).toBe(detail?.id);
    expect(fetched.at(0)?.pathname).toBe(`/api/v1/articles/${detail?.id ?? ''}`);
  });

  it('reads the contract 404 as null — withdrawn and unknown look identical', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'not found' }, 404));
    const api = createReaderApi('http://api:8080', fetchImpl);
    await expect(api.article('anything')).resolves.toBeNull();
  });

  it('surfaces other article errors as ReaderApiError', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ title: 'boom' }, 500));
    const api = createReaderApi('http://api:8080', fetchImpl);
    await expect(api.article('anything')).rejects.toMatchObject({ status: 500 });
  });

  it('rejects an article body without an id', async () => {
    const { fetchImpl } = respondingWith(jsonResponse({ nonsense: true }));
    const api = createReaderApi('http://api:8080', fetchImpl);
    await expect(api.article('anything')).rejects.toBeInstanceOf(ReaderApiError);
  });

  it('rejects a half-shaped article — every contract field is checked at runtime', async () => {
    const detail = FRONT_FIXTURES[0];
    const mistyped = { ...detail, headline: 42 };
    const { fetchImpl } = respondingWith(jsonResponse(mistyped));
    await expect(
      createReaderApi('http://api:8080', fetchImpl).article('x'),
    ).rejects.toBeInstanceOf(ReaderApiError);

    const { places: _places, ...withoutPlaces } = detail as NonNullable<typeof detail>;
    const { fetchImpl: fetchImpl2 } = respondingWith(jsonResponse(withoutPlaces));
    await expect(
      createReaderApi('http://api:8080', fetchImpl2).article('x'),
    ).rejects.toBeInstanceOf(ReaderApiError);
  });
});

describe('probeEmptyPlaces', () => {
  it('flags a followed place with nothing published at all (US1-AC3)', async () => {
    const api = createReaderApi(undefined);
    const { items } = await api.front({ lang: 'el', places: ['munich', 'bavaria'] });
    const empty = await probeEmptyPlaces(api, 'el', [MUNICH, BAVARIA], items);
    expect(empty.map((place) => place.slug)).toEqual(['bavaria']);
  });

  it('does not flag a place merely crowded out of the shared first page', async () => {
    // Bavaria has content, but the combined first page happens to hold
    // only Munich items — the probe must clear it.
    const bavariaItem = { ...FRONT_FIXTURES[0], places: ['bavaria'] };
    const crowded: ReaderApi = {
      front: (query) =>
        Promise.resolve({
          items: query.places.includes('bavaria') ? [bavariaItem] : [],
          next_cursor: null,
        } as FrontPageData),
      article: () => Promise.resolve(null),
    };
    const munichOnlyPage = (await createReaderApi(undefined).front({
      lang: 'el',
      places: ['munich'],
    })).items;
    const empty = await probeEmptyPlaces(crowded, 'el', [MUNICH, BAVARIA], munichOnlyPage);
    expect(empty).toEqual([]);
  });

  it('never probes a place already visible on the page', async () => {
    let probes = 0;
    const counting: ReaderApi = {
      front: () => {
        probes += 1;
        return Promise.resolve({ items: [], next_cursor: null });
      },
      article: () => Promise.resolve(null),
    };
    const page = (await createReaderApi(undefined).front({ lang: 'el', places: ['munich'] }))
      .items;
    await probeEmptyPlaces(counting, 'el', [MUNICH], page);
    expect(probes).toBe(0);
  });
});
