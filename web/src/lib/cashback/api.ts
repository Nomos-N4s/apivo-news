import { APP_ENV_DEV, APP_ENV_PROD, parseAppEnv } from '../app-env';
import {
  CATALOGUE_FIXTURES,
  CATALOGUE_LIST_FIXTURES,
  DESTINATION_FIXTURES,
  DIFFERENCE_FIXTURES,
  ENTRY_FIXTURES,
  HELD_FIXTURES,
  RECONCILIATION_RUN_FIXTURES,
  UNATTRIBUTED_FIXTURES,
  WALLET_FIXTURE,
  WITHDRAWAL_APPROVAL_FIXTURES,
  WITHDRAWAL_FIXTURES,
} from './fixtures';
import type {
  CatalogueItem,
  EntryPage,
  EntryState,
  HeldEntry,
  MerchantDetail,
  OperatorPage,
  Participation,
  PayoutDestination,
  Problem,
  ReconciliationDifference,
  ReconciliationRun,
  UnattributedTransaction,
  WalletTotals,
  Withdrawal,
  WithdrawalForApproval,
  WithdrawalRequest,
} from './types';

/**
 * Typed client for the cashback API
 * (`specs/002-apivo-cashback-alpha/contracts/http-api.md`).
 *
 * The Astro server is the only public HTTP surface; the Go API is not
 * publicly routable, so every call here happens server-side with the
 * member's own bearer token. There is no anonymous cashback surface at all
 * (FR-023) — an anonymous click can never be back-attributed, so it is
 * never created.
 *
 * Structure follows the reader and editorial clients: one interface, an
 * HTTP implementation, a fixture implementation, and a `source` field so
 * every page can say which it is showing. The refusal in a deployed
 * environment is sharper here than it is for the reader. Serving invented
 * articles misleads somebody about who wrote something; serving an invented
 * balance tells them they are owed money they are not owed, and they will
 * act on it.
 */

/** Whether the answers are the API's or the built-in fixtures'. */
export type CashbackSource = 'api' | 'fixture';

/** A non-2xx answer, carrying the problem document where the API sent one. */
export class CashbackApiError extends Error {
  readonly status: number;
  readonly problem: Problem | null;

  constructor(message: string, status: number, problem: Problem | null = null) {
    super(message);
    this.name = 'CashbackApiError';
    this.status = status;
    this.problem = problem;
  }

  /**
   * The machine-readable refusal, where the API named one.
   *
   * Pages branch on this rather than on the status: the two balance
   * refusals share `insufficient_confirmed_balance` across two different
   * walls, and an unverified destination is a 409 like several other
   * things.
   */
  get code(): string | null {
    return typeof this.problem?.code === 'string' ? this.problem.code : null;
  }
}

/** A refusal to build a client at all, because it would answer with invented money. */
export class CashbackConfigurationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'CashbackConfigurationError';
  }
}

/** Query for the catalogue. Language and place stay separate (constitution VII). */
export interface CatalogueQuery {
  readonly lang: string;
  readonly places: readonly string[];
  readonly q?: string;
  readonly limit?: number;
  readonly cursor?: string;
}

/** Query for the member's entry list. */
export interface EntryQuery {
  readonly state?: EntryState;
  readonly limit?: number;
  readonly cursor?: string;
}

/** The surface the pages consume. */
export interface CashbackApi {
  readonly source: CashbackSource;

  /** Null where the member has never opted in — the 404 is not an error. */
  participation(): Promise<Participation | null>;
  optIn(termsVersion: string): Promise<Participation>;

  catalogue(query: CatalogueQuery): Promise<OperatorPage<CatalogueItem>>;
  /** Null for an unknown or inactive retailer — one answer for both. */
  merchant(slug: string, lang?: string): Promise<MerchantDetail | null>;

  wallet(): Promise<WalletTotals>;
  entries(query?: EntryQuery): Promise<EntryPage>;

  destinations(): Promise<readonly PayoutDestination[]>;
  requestWithdrawal(destinationId: string, amountMinor: number, currency: string): Promise<WithdrawalRequest>;
  withdrawals(): Promise<readonly Withdrawal[]>;

  /* Operator surfaces. */
  unattributed(): Promise<OperatorPage<UnattributedTransaction>>;
  held(): Promise<OperatorPage<HeldEntry>>;
  withdrawalsAwaitingApproval(): Promise<OperatorPage<WithdrawalForApproval>>;
  reconciliationRuns(): Promise<OperatorPage<ReconciliationRun>>;
  differences(runId: string): Promise<OperatorPage<ReconciliationDifference>>;
}

/** Contract default; the API caps at 100. */
const DEFAULT_LIMIT = 20;

const BASE_PATH = '/api/v1/cashback';

function page<T>(items: readonly T[]): OperatorPage<T> {
  return { items, next_cursor: null };
}

function fixtureApi(): CashbackApi {
  return {
    source: 'fixture',
    participation: () =>
      Promise.resolve({
        status: 'active',
        opted_in_at: '2026-06-01T00:00:00Z',
        terms_version: 'fixture-1',
        default_currency: 'EUR',
      }),
    optIn: (termsVersion) =>
      Promise.resolve({
        status: 'active',
        opted_in_at: new Date(0).toISOString(),
        terms_version: termsVersion,
        default_currency: 'EUR',
      }),
    catalogue: (query) => {
      const needle = query.q?.trim().toLowerCase();
      const items =
        needle === undefined || needle === ''
          ? CATALOGUE_LIST_FIXTURES
          : CATALOGUE_LIST_FIXTURES.filter((item) =>
              item.name.toLowerCase().includes(needle),
            );
      return Promise.resolve(page(items.slice(0, query.limit ?? DEFAULT_LIMIT)));
    },
    merchant: (slug) =>
      Promise.resolve(CATALOGUE_FIXTURES.find((item) => item.slug === slug) ?? null),
    wallet: () => Promise.resolve(WALLET_FIXTURE),
    entries: (query) => {
      const state = query?.state;
      const items =
        state === undefined
          ? ENTRY_FIXTURES
          : ENTRY_FIXTURES.filter((entry) => entry.state === state);
      return Promise.resolve({ items, next_cursor: null });
    },
    destinations: () => Promise.resolve(DESTINATION_FIXTURES),
    requestWithdrawal: (_destinationId, amountMinor, currency) =>
      Promise.resolve({
        request_id: 'fx-withdrawal-new',
        state: 'awaiting_approval',
        // The fixture reserves more than was asked for, because the real
        // one does: whole entries, oldest first. A preview that returned
        // the requested figure would teach the screen a shape the API
        // never sends.
        reserved_amount: { minor: amountMinor + 340, currency },
      }),
    withdrawals: () => Promise.resolve(WITHDRAWAL_FIXTURES),
    unattributed: () => Promise.resolve(page(UNATTRIBUTED_FIXTURES)),
    held: () => Promise.resolve(page(HELD_FIXTURES)),
    withdrawalsAwaitingApproval: () => Promise.resolve(page(WITHDRAWAL_APPROVAL_FIXTURES)),
    reconciliationRuns: () => Promise.resolve(page(RECONCILIATION_RUN_FIXTURES)),
    differences: () => Promise.resolve(page(DIFFERENCE_FIXTURES)),
  };
}

function httpApi(baseUrl: string, fetchImpl: typeof fetch, token: string | null): CashbackApi {
  const base = baseUrl.replace(/\/+$/, '') + BASE_PATH;

  const headers = (extra: Record<string, string> = {}): Record<string, string> => {
    const built: Record<string, string> = { Accept: 'application/json', ...extra };
    if (token !== null && token !== '') {
      built['Authorization'] = `Bearer ${token}`;
    }
    return built;
  };

  /**
   * Reads a problem document off a failed response.
   *
   * A body that is not JSON, or is JSON that is not an object, produces
   * null rather than throwing: the caller is already handling a failure,
   * and a parse error on top of it would replace a usable status code with
   * a stack trace.
   */
  const problemOf = async (response: Response): Promise<Problem | null> => {
    try {
      const body: unknown = await response.json();
      return typeof body === 'object' && body !== null ? (body as Problem) : null;
    } catch {
      return null;
    }
  };

  const request = async <T>(
    path: string,
    init: RequestInit = {},
    allow404 = false,
  ): Promise<T | null> => {
    const response = await fetchImpl(`${base}${path}`, {
      ...init,
      headers: headers(init.body === undefined ? {} : { 'Content-Type': 'application/json' }),
    });
    if (allow404 && response.status === 404) {
      return null;
    }
    if (!response.ok) {
      const problem = await problemOf(response);
      throw new CashbackApiError(
        `cashback API answered ${response.status} for ${path}`,
        response.status,
        problem,
      );
    }
    return (await response.json()) as T;
  };

  const required = async <T>(path: string, init?: RequestInit): Promise<T> =>
    (await request<T>(path, init)) as T;

  const listQuery = (params: Record<string, string | number | undefined>): string => {
    const search = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== '') {
        search.set(key, String(value));
      }
    }
    const query = search.toString();
    return query === '' ? '' : `?${query}`;
  };

  return {
    source: 'api',
    participation: () => request<Participation>('/participation', {}, true),
    optIn: (termsVersion) =>
      required<Participation>('/participation', {
        method: 'POST',
        body: JSON.stringify({ terms_version: termsVersion }),
      }),
    catalogue: (query) => {
      const search = new URLSearchParams();
      search.set('lang', query.lang);
      for (const place of query.places) {
        search.append('place', place);
      }
      if (query.q !== undefined && query.q !== '') {
        search.set('q', query.q);
      }
      search.set('limit', String(query.limit ?? DEFAULT_LIMIT));
      if (query.cursor !== undefined) {
        search.set('cursor', query.cursor);
      }
      return required<OperatorPage<CatalogueItem>>(`/catalogue?${search.toString()}`);
    },
    merchant: (slug, lang) =>
      request<MerchantDetail>(
        `/merchants/${encodeURIComponent(slug)}${listQuery({ lang })}`,
        {},
        true,
      ),
    wallet: () => required<WalletTotals>('/wallet'),
    entries: (query) =>
      required<EntryPage>(
        `/wallet/entries${listQuery({
          state: query?.state,
          limit: query?.limit ?? DEFAULT_LIMIT,
          cursor: query?.cursor,
        })}`,
      ),
    destinations: async () =>
      (await required<OperatorPage<PayoutDestination>>('/payout-destinations')).items,
    requestWithdrawal: (destinationId, amountMinor, currency) =>
      required<WithdrawalRequest>('/withdrawals', {
        method: 'POST',
        body: JSON.stringify({
          destination_id: destinationId,
          amount: { minor: amountMinor, currency },
        }),
      }),
    withdrawals: async () => (await required<OperatorPage<Withdrawal>>('/withdrawals')).items,
    unattributed: () => required<OperatorPage<UnattributedTransaction>>('/ops/unattributed'),
    held: () => required<OperatorPage<HeldEntry>>('/ops/held'),
    withdrawalsAwaitingApproval: () =>
      required<OperatorPage<WithdrawalForApproval>>(
        '/ops/withdrawals?state=awaiting_approval',
      ),
    reconciliationRuns: () => required<OperatorPage<ReconciliationRun>>('/ops/reconciliation/runs'),
    differences: (runId) =>
      required<OperatorPage<ReconciliationDifference>>(
        `/ops/reconciliation/runs/${encodeURIComponent(runId)}/differences`,
      ),
  };
}

/** How a cashback client is built. */
export interface CashbackApiOptions {
  /** The member's or operator's bearer token; every endpoint requires one. */
  readonly token?: string | null;
  /** Injected in tests; production uses the platform's `fetch`. */
  readonly fetch?: typeof fetch;
  /** `APP_ENV`. In `prod` an absent base URL is a refusal, never a fallback. */
  readonly appEnv?: string | undefined;
}

/**
 * Builds the client.
 *
 * With a base URL, requests go to the Go API. Without one the fixtures
 * answer — and that is a development convenience which must never reach a
 * member, because the fixtures state a balance. So:
 *
 *   - in `APP_ENV=prod` an absent base URL is REFUSED here, and the page
 *     fails rather than rendering. A deployment that cannot reach its API
 *     knows nothing about anybody's money, and saying nothing is the only
 *     truthful alternative to saying what is owed.
 *   - anywhere else the client answers but reports `source: 'fixture'`, so
 *     every surface can mark what it is showing.
 *
 * An `APP_ENV` that is neither value is refused before either branch, for
 * the reason the reader client gives: `prod` is a spelling, not a meaning,
 * and reading `production` as "not prod" would hand a deployed member a
 * wallet full of invented figures.
 */
export function createCashbackApi(
  baseUrl: string | undefined,
  options: CashbackApiOptions = {},
): CashbackApi {
  const appEnv = parseAppEnv(options.appEnv);
  if (appEnv === null) {
    throw new CashbackConfigurationError(
      `APP_ENV is ${JSON.stringify(options.appEnv)}, which is neither "${APP_ENV_DEV}" nor "${APP_ENV_PROD}". A value this application cannot read is not development: it would serve a deployed member a wallet of invented balances. Set APP_ENV to "${APP_ENV_PROD}" on a deployment, or leave it unset.`,
    );
  }
  if (baseUrl === undefined || baseUrl === '') {
    if (appEnv === APP_ENV_PROD) {
      throw new CashbackConfigurationError(
        'API_BASE_URL is not set in a deployed environment (APP_ENV=prod): the cashback surfaces would answer from built-in fixtures, whose balances and entries are invented. Set API_BASE_URL to the deployment origin that routes to the Go API.',
      );
    }
    return fixtureApi();
  }
  return httpApi(baseUrl, options.fetch ?? fetch, options.token ?? null);
}
