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

const MUNICH: Place = { slug: 'munich', endonym: 'München', scope: 'city', selectable: true };
const BAVARIA: Place = { slug: 'bavaria', endonym: 'Bayern', scope: 'region', selectable: false };

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
    };
    const page = (await createReaderApi(undefined).front({ lang: 'el', places: ['munich'] }))
      .items;
    await probeEmptyPlaces(counting, 'el', [MUNICH], page);
    expect(probes).toBe(0);
  });
});
