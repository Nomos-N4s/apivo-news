import type { ReadingLanguage } from '../reader/axes';
import { QUEUE_FIXTURES, SPEND_FIXTURE } from './fixtures';

/**
 * Typed client for the editorial API
 * (`specs/001-epiloyes-alpha/contracts/http-api.md`).
 *
 * Same seam as the reader client: with `API_BASE_URL` set, requests go to
 * the Go API over the internal network; without it, typed fixtures answer
 * through the same interface. The editorial endpoints do not exist yet
 * (T019 #22, T020 #23), so today every deployment takes the fixture path.
 */

/**
 * One row of `GET /api/v1/editorial/queue`.
 *
 * The first block is the contract's payload, verbatim. The second is what
 * the 1g review pane needs in order to be a responsible review surface —
 * an editor cannot approve on a translated headline alone; they must see
 * the original text, the lineage that produced the translation, and what
 * it cost. Those fields are marked optional and the pane degrades without
 * them, so this type already matches the endpoint as specified while
 * recording the gap (issue #61).
 */
export interface QueueItem {
  // — the contract's queue payload —
  readonly source_item_id: string;
  readonly translation_id: string | null;
  readonly source_name: string;
  readonly headline_original: string | null;
  readonly headline_translated: string | null;
  readonly extract_translated: string | null;
  /** ISO 8601. */
  readonly retrieved_at: string;
  readonly licence_snapshot: string;

  // — proposed additions, needed by the review pane (issue #61) —
  /** Place slugs the item would publish to; drives the queue kicker. */
  readonly places?: readonly string[];
  /** The original article at the publisher, for "Open source ↗". */
  readonly source_url?: string;
  /** The retrieved original, as evidence beside the translation. */
  readonly extract_original?: string | null;
  readonly original_author?: string | null;
  readonly original_published_at?: string | null;
  /** The DB-computed fingerprint of the retrieved body. */
  readonly content_hash?: string;
  /** Source language, and the target the translation was produced in. */
  readonly source_lang?: string;
  readonly target_lang?: ReadingLanguage | null;
  /** Translation lineage (FR-005) and its provider-reported cost (FR-006). */
  readonly model?: string | null;
  readonly prompt_version?: string | null;
  readonly cost_microusd?: number | null;
}

/**
 * The pipeline states the queue must show but the contract's list does not
 * carry: items held because the translation provider is unavailable, and
 * items skipped for exceeding the per-article ceiling (FR-006, and the
 * spec's translation-outage edge case). Recorded as a gap in issue #61.
 */
export interface PipelineHolds {
  readonly queued_untranslated: number;
  readonly skipped_over_ceiling: number;
}

/**
 * The monthly translation ledger (FR-006). Backed by the `translation_spend`
 * table (migration 0002); no endpoint exposes it yet — issue #61.
 */
export interface SpendLedger {
  readonly month: string;
  readonly spent_microusd: number;
  readonly cap_microusd: number;
}

/** A `GET /api/v1/editorial/queue` page, plus the states the screen needs. */
export interface QueuePage {
  readonly items: readonly QueueItem[];
  readonly holds: PipelineHolds;
  readonly spend: SpendLedger;
}

/**
 * What `POST /api/v1/editorial/approvals` answers. `recorded: false` is not
 * an error state — it is the honest answer while no editorial API exists:
 * the intent was carried, nothing was written. An approval UI must never
 * claim a record it did not create (I-1, FR-007).
 */
export interface ApprovalOutcome {
  readonly recorded: boolean;
  readonly article_id?: string;
  readonly approved_by?: string;
  readonly approved_at?: string;
  readonly published_at?: string | null;
  /** Why nothing was recorded, when `recorded` is false. */
  readonly reason?: string;
}

/** A non-2xx answer or an unusable body from the editorial API. */
export class EditorialApiError extends Error {
  readonly status: number | undefined;

  constructor(message: string, status?: number) {
    super(message);
    this.name = 'EditorialApiError';
    this.status = status;
  }
}

/** The editorial API surface the pages consume. */
export interface EditorialApi {
  queue(): Promise<QueuePage>;
  approve(sourceItemId: string, translationId: string | null): Promise<ApprovalOutcome>;
}

const NOT_WIRED =
  'The editorial API is not implemented yet (T019/T020), so no article was created and no approval was recorded.';

function fixtureApi(): EditorialApi {
  return {
    queue(): Promise<QueuePage> {
      return Promise.resolve({
        items: QUEUE_FIXTURES,
        holds: { queued_untranslated: 3, skipped_over_ceiling: 1 },
        spend: SPEND_FIXTURE,
      });
    },
    approve(): Promise<ApprovalOutcome> {
      // Deliberately not a fake success. Approval is the one action whose
      // whole meaning is that a record exists; pretending otherwise would
      // misrepresent an invariant the database enforces.
      return Promise.resolve({ recorded: false, reason: NOT_WIRED });
    },
  };
}

function httpApi(baseUrl: string, fetchImpl: typeof fetch, token: string | null): EditorialApi {
  const base = baseUrl.replace(/\/+$/, '');
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (token !== null) {
    headers['Authorization'] = `Bearer ${token}`;
  }
  return {
    async queue(): Promise<QueuePage> {
      const response = await fetchImpl(new URL(`${base}/api/v1/editorial/queue`), { headers });
      if (!response.ok) {
        throw new EditorialApiError(
          `editorial API answered ${response.status} for the queue`,
          response.status,
        );
      }
      const body: unknown = await response.json();
      if (
        typeof body !== 'object' ||
        body === null ||
        !Array.isArray((body as { items?: unknown }).items)
      ) {
        throw new EditorialApiError('editorial API answered without an items array');
      }
      return body as QueuePage;
    },
    async approve(sourceItemId: string, translationId: string | null): Promise<ApprovalOutcome> {
      // The contract takes exactly one origin: translation_id XOR
      // source_item_id (400 if both or neither).
      const origin =
        translationId === null
          ? { source_item_id: sourceItemId }
          : { translation_id: translationId };
      const response = await fetchImpl(new URL(`${base}/api/v1/editorial/approvals`), {
        method: 'POST',
        headers: { ...headers, 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...origin, publish: true }),
      });
      if (!response.ok) {
        throw new EditorialApiError(
          `editorial API answered ${response.status} for the approval`,
          response.status,
        );
      }
      const body: unknown = await response.json();
      if (typeof body !== 'object' || body === null || !('article_id' in body)) {
        throw new EditorialApiError('editorial API answered without an article id');
      }
      return { recorded: true, ...(body as Record<string, unknown>) } as ApprovalOutcome;
    },
  };
}

/**
 * Builds the client. With a base URL, requests go to the Go API carrying
 * the editor's token; without one, the fixtures answer and approval
 * reports that nothing was recorded.
 */
export function createEditorialApi(
  baseUrl: string | undefined,
  token: string | null = null,
  fetchImpl: typeof fetch = fetch,
): EditorialApi {
  if (baseUrl === undefined || baseUrl === '') {
    return fixtureApi();
  }
  return httpApi(baseUrl, fetchImpl, token);
}

/**
 * Formats a ledger amount — e.g. 9_200_000 → "$9.20".
 *
 * The column is `cost_microusd` and providers bill in dollars, so the
 * ledger reads in dollars. The mockup drew euros; showing a euro sign over
 * a dollar column would misstate the spend against the cap, so the unit
 * follows the schema until a converted presentation currency is decided
 * (issue #61).
 */
export function formatSpend(microusd: number): string {
  return `$${(microusd / 1_000_000).toFixed(2)}`;
}

/** Formats a per-item translation cost, which is far below a cent — "$0.004". */
export function formatItemCost(microusd: number): string {
  return `$${(microusd / 1_000_000).toFixed(3)}`;
}

/** The ledger's fill, clamped to 0–100 so an overrun cannot break the bar. */
export function spendPercent(spend: SpendLedger): number {
  if (spend.cap_microusd <= 0) {
    return 0;
  }
  return Math.min(100, Math.max(0, (spend.spent_microusd / spend.cap_microusd) * 100));
}
