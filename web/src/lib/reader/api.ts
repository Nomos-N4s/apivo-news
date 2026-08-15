import type { Place, ReadingLanguage } from './axes';
import { FRONT_FIXTURES } from './fixtures';

/**
 * Typed client for the public reader API
 * (`specs/001-epiloyes-alpha/contracts/http-api.md`).
 *
 * The Astro server is the API's first consumer: pages call this client
 * server-side over the internal network (`API_BASE_URL`, set in compose).
 * Until the reader endpoints land (T023/T024), an unset base URL serves
 * the built-in fixtures through the same interface, so going live is an
 * environment change, not a code change.
 */

/** One front-page item — the `GET /api/v1/front` item shape, contract-verbatim. */
export interface FrontItem {
  readonly id: string;
  readonly headline: string;
  readonly extract: string;
  readonly lang: ReadingLanguage;
  /** Slugs of the places the article relates to (many-to-many). */
  readonly places: readonly string[];
  /** The composed attribution block, rendered verbatim (FR-008). */
  readonly attribution: string;
  /** The original article at the publisher — the outbound link (SC-008). */
  readonly source_url: string;
  /** ISO 8601. */
  readonly published_at: string;
}

/** A `GET /api/v1/front` page. */
export interface FrontPageData {
  readonly items: readonly FrontItem[];
  readonly next_cursor: string | null;
}

/**
 * The `GET /api/v1/articles/{id}` payload: the front-item shape plus the
 * approval time. Withdrawn or unpublished articles are a 404 by contract —
 * the existence of unpublished work is not public — so the client answers
 * null and the page renders not-found, never a withdrawn state (issue #52).
 */
export interface ArticleDetail extends FrontItem {
  /** ISO 8601 — when the named editor approved (the public record). */
  readonly approved_at: string;
}

/** Query for the front-page feed; `places` maps to the repeatable `place` param. */
export interface FrontQuery {
  readonly lang: ReadingLanguage;
  readonly places: readonly string[];
  readonly limit?: number;
}

/** A non-2xx answer or an unusable body from the reader API. */
export class ReaderApiError extends Error {
  readonly status: number | undefined;

  constructor(message: string, status?: number) {
    super(message);
    this.name = 'ReaderApiError';
    this.status = status;
  }
}

/** The reader API surface the pages consume. */
export interface ReaderApi {
  front(query: FrontQuery): Promise<FrontPageData>;
  /** One published article; null when the contract answers 404. */
  article(id: string): Promise<ArticleDetail | null>;
}

/** Contract default; the API caps at 100. */
const DEFAULT_LIMIT = 20;

function fixtureApi(): ReaderApi {
  return {
    front(query: FrontQuery): Promise<FrontPageData> {
      const items = FRONT_FIXTURES.filter(
        (item) =>
          item.lang === query.lang &&
          item.places.some((slug) => query.places.includes(slug)),
      )
        .sort((a, b) => b.published_at.localeCompare(a.published_at))
        .slice(0, query.limit ?? DEFAULT_LIMIT);
      return Promise.resolve({ items, next_cursor: null });
    },
    article(id: string): Promise<ArticleDetail | null> {
      return Promise.resolve(FRONT_FIXTURES.find((item) => item.id === id) ?? null);
    },
  };
}

function httpApi(baseUrl: string, fetchImpl: typeof fetch): ReaderApi {
  const base = baseUrl.replace(/\/+$/, '');
  return {
    async front(query: FrontQuery): Promise<FrontPageData> {
      const url = new URL(`${base}/api/v1/front`);
      url.searchParams.set('lang', query.lang);
      for (const slug of query.places) {
        url.searchParams.append('place', slug);
      }
      url.searchParams.set('limit', String(query.limit ?? DEFAULT_LIMIT));
      const response = await fetchImpl(url, {
        headers: { Accept: 'application/json' },
      });
      if (!response.ok) {
        throw new ReaderApiError(
          `reader API answered ${response.status} for ${url.pathname}`,
          response.status,
        );
      }
      const body: unknown = await response.json();
      if (
        typeof body !== 'object' ||
        body === null ||
        !Array.isArray((body as { items?: unknown }).items)
      ) {
        throw new ReaderApiError('reader API answered without an items array');
      }
      return body as FrontPageData;
    },
    async article(id: string): Promise<ArticleDetail | null> {
      const url = new URL(`${base}/api/v1/articles/${encodeURIComponent(id)}`);
      const response = await fetchImpl(url, {
        headers: { Accept: 'application/json' },
      });
      if (response.status === 404) {
        return null;
      }
      if (!response.ok) {
        throw new ReaderApiError(
          `reader API answered ${response.status} for ${url.pathname}`,
          response.status,
        );
      }
      const body: unknown = await response.json();
      if (typeof body !== 'object' || body === null || !('id' in body)) {
        throw new ReaderApiError('reader API answered without an article body');
      }
      return body as ArticleDetail;
    },
  };
}

/**
 * Builds the client. With a base URL, requests go to the Go API; without
 * one, the fixtures answer (development preview until T023/T024 — compose
 * and production always set `API_BASE_URL`).
 */
export function createReaderApi(
  baseUrl: string | undefined,
  fetchImpl: typeof fetch = fetch,
): ReaderApi {
  if (baseUrl === undefined || baseUrl === '') {
    return fixtureApi();
  }
  return httpApi(baseUrl, fetchImpl);
}

/**
 * The followed places with nothing published at all (US1-AC3). Absence
 * from the combined first page is not evidence of emptiness — a prolific
 * place can crowd another out of a shared, newest-first, limited page —
 * so each absent place is probed with its own single-item query before
 * the page claims "nothing published yet".
 */
export async function probeEmptyPlaces(
  api: ReaderApi,
  lang: ReadingLanguage,
  followed: readonly Place[],
  pageItems: readonly FrontItem[],
): Promise<readonly Place[]> {
  const absent = followed.filter(
    (place) => !pageItems.some((item) => item.places.includes(place.slug)),
  );
  const probes = await Promise.all(
    absent.map(async (place) => ({
      place,
      hasAny: (await api.front({ lang, places: [place.slug], limit: 1 })).items.length > 0,
    })),
  );
  return probes.filter((probe) => !probe.hasAny).map((probe) => probe.place);
}
