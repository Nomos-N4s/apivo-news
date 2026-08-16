import { APP_ENV_DEV, APP_ENV_PROD, parseAppEnv } from '../app-env';
import { isReadingLanguage, type Place, type ReadingLanguage } from './axes';
import { FRONT_FIXTURES } from './fixtures';

/**
 * Typed client for the public reader API
 * (`specs/001-epiloyes-alpha/contracts/http-api.md`).
 *
 * The Astro server is the API's first consumer: pages call this client
 * server-side (`API_BASE_URL` — the api service address under compose and
 * Kubernetes, the deployment's own public origin on Cloudflare, where the
 * containers share no private network). An unset base URL serves the
 * built-in fixtures through the same interface, so going live is an
 * environment change rather than a code change — but never silently, and
 * never in a deployed environment: see `createReaderApi`.
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

/**
 * A refusal to build a reader client at all, because the configuration it
 * was given would make it answer with data nobody published.
 */
export class ReaderConfigurationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ReaderConfigurationError';
  }
}

/**
 * Where a client's answers come from.
 *
 * The editorial client has carried this distinction since it had fixtures;
 * the reader path had none, which is how a deployment with no
 * `API_BASE_URL` could present invented publishers and an invented
 * approver as the public record, unmarked. A reader client now always says
 * which it is, and the pages say it out loud.
 */
export type ReaderSource = 'api' | 'fixture';

/** The reader API surface the pages consume. */
export interface ReaderApi {
  /** Whether these answers are the API's or the built-in fixtures'. */
  readonly source: ReaderSource;
  front(query: FrontQuery): Promise<FrontPageData>;
  /** One published article; null when the contract answers 404. */
  article(id: string): Promise<ArticleDetail | null>;
}

/** Contract default; the API caps at 100. */
const DEFAULT_LIMIT = 20;

function fixtureApi(): ReaderApi {
  return {
    source: 'fixture',
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
    source: 'api',
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
      if (!isArticleDetail(body)) {
        throw new ReaderApiError('reader API answered with a malformed article body');
      }
      return body;
    },
  };
}

/**
 * Runtime check of the article payload — external JSON must not reach the
 * page half-shaped and crash the render; a malformed body is the client's
 * error (ReaderApiError), not a 500.
 */
function isArticleDetail(body: unknown): body is ArticleDetail {
  if (typeof body !== 'object' || body === null) {
    return false;
  }
  const record = body as Record<string, unknown>;
  return (
    typeof record['id'] === 'string' &&
    typeof record['headline'] === 'string' &&
    typeof record['extract'] === 'string' &&
    typeof record['lang'] === 'string' &&
    isReadingLanguage(record['lang']) &&
    Array.isArray(record['places']) &&
    record['places'].every((slug) => typeof slug === 'string') &&
    typeof record['attribution'] === 'string' &&
    typeof record['source_url'] === 'string' &&
    typeof record['published_at'] === 'string' &&
    typeof record['approved_at'] === 'string'
  );
}

/** How a reader client is built; both parts have working defaults. */
export interface ReaderApiOptions {
  /** Injected in tests; production uses the platform's `fetch`. */
  readonly fetch?: typeof fetch;
  /**
   * `APP_ENV`. In a deployed environment (`prod`) an absent base URL is a
   * refusal, never a fallback — see `createReaderApi`.
   */
  readonly appEnv?: string | undefined;
}

/**
 * Builds the client. With a base URL, requests go to the Go API.
 *
 * Without one, the built-in fixtures answer — and that is a development
 * convenience which must never reach a reader. The fixtures are complete
 * articles from publishers that do not exist, and their attribution blocks
 * name an editor who does not exist as the approver. A product whose one
 * promise is that no published sentence exists without a real named
 * approver cannot serve them to the public, so:
 *
 *   - in `APP_ENV=prod` an absent base URL is REFUSED here. The page fails
 *     rather than rendering, which is the honest outcome: a deployment
 *     that cannot reach its API has nothing to show, and showing nothing
 *     is the only truthful alternative to showing the record.
 *   - anywhere else the client still answers, but says `source: 'fixture'`
 *     so every page can mark what it is showing. Silence was the bug.
 *
 * An `APP_ENV` that is neither value is refused before either branch is
 * reached, whether or not a base URL is set. `prod` is a spelling, not a
 * meaning: read `production` as "not prod" and the refusal above never
 * fires, the fixtures answer a deployed reader, and the cookies lose
 * their `Secure` attribute with them (lib/secure-request.ts). The Go
 * binary refuses to start on the same value, and this is the same refusal
 * at the same boundary.
 */
export function createReaderApi(
  baseUrl: string | undefined,
  options: ReaderApiOptions = {},
): ReaderApi {
  const appEnv = parseAppEnv(options.appEnv);
  if (appEnv === null) {
    throw new ReaderConfigurationError(
      `APP_ENV is ${JSON.stringify(options.appEnv)}, which is neither "${APP_ENV_DEV}" nor "${APP_ENV_PROD}". A value this application cannot read is not development: it would serve a deployed reader from built-in fixtures, whose publishers and approving editor are invented. Set APP_ENV to "${APP_ENV_PROD}" on a deployment, or leave it unset.`,
    );
  }
  if (baseUrl === undefined || baseUrl === '') {
    if (appEnv === APP_ENV_PROD) {
      throw new ReaderConfigurationError(
        'API_BASE_URL is not set in a deployed environment (APP_ENV=prod): the reader would answer from built-in fixtures, whose publishers and approving editor are invented. Set API_BASE_URL to the deployment origin that routes to the Go API.',
      );
    }
    return fixtureApi();
  }
  return httpApi(baseUrl, options.fetch ?? fetch);
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
