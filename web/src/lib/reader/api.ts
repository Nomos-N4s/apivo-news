import type { ReadingLanguage } from './axes';
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
