import type { ReadingLanguage } from '../reader/axes';
import {
  POLL_CYCLE_FIXTURE,
  PROVENANCE_FIXTURES,
  QUEUE_FIXTURES,
  SOURCE_FIXTURES,
  SPEND_FIXTURE,
} from './fixtures';

/**
 * Typed client for the editorial API
 * (`specs/001-epiloyes-alpha/contracts/http-api.md`).
 *
 * Same seam as the reader client: with `API_BASE_URL` set, requests go to
 * the Go API over the internal network; without it, typed fixtures answer
 * through the same interface. The editorial endpoints do not exist yet
 * (T019 #22, T020 #23), so today every deployment takes the fixture path.
 */

/** One ended publication in an origin's history. */
export interface QueueWithdrawal {
  readonly article_id: string;
  /** ISO 8601. */
  readonly withdrawn_at: string;
  readonly withdrawn_by: string;
  readonly reason: string;
}

/**
 * One row of `GET /api/v1/editorial/queue`.
 *
 * The first block is the contract's original payload. The second is the
 * evidence the approval rests on — the original text, its author and
 * declared publication date, the fingerprint, the lineage that produced
 * the translation and what it cost — served since #87, because the
 * approval is permanent and the evidence has to be on the screen before
 * the click. The fields stay optional so the pane degrades against an
 * older API instead of crashing; `original_published_at` is the
 * load-bearing one, and its absence must surface VISIBLY, never as a
 * silently substituted retrieval date.
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
  /**
   * True when the origin's only articles were withdrawn: the editor is
   * looking at a correction, not a first approval.
   */
  readonly correction_candidate?: boolean;
  /** The origin's withdrawal history, newest first; [] when fresh. */
  readonly withdrawals?: readonly QueueWithdrawal[];

  // — the evidence block (#87) —
  /** Place slugs the item would publish to; still a gap (issue #61). */
  readonly places?: readonly string[];
  /** The original article at the publisher, for "Open source ↗". */
  readonly source_url?: string;
  /** The retrieved original as bounded prose, beside the translation. */
  readonly extract_original?: string | null;
  readonly original_author?: string | null;
  /**
   * The publication date the feed declared, null when it declared none.
   * The attribution default composes from THIS date — article_guard
   * freezes the attribution at approval, so a substituted retrieval date
   * here would be frozen in as the publication date forever.
   */
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

/**
 * A `GET /api/v1/editorial/queue` page, plus the states the screen needs.
 *
 * Only `items` is in the contract, so `holds` and `spend` are optional:
 * a contract-compliant response carries neither, and the screen must
 * render without them rather than throw on a missing ledger.
 */
export interface QueuePage {
  readonly items: readonly QueueItem[];
  readonly holds?: PipelineHolds;
  readonly spend?: SpendLedger;
  /**
   * True when this page is fixture data rather than the API's answer.
   * The marker in the chrome keys on this — the data itself is the only
   * party that knows its own provenance, since fixtures render both when
   * no API is configured and when the routes answer 404. A real API
   * response never carries the flag.
   */
  readonly fixture?: boolean;
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

/**
 * `GET /api/v1/editorial/articles/{id}/provenance` — the five-minute audit
 * (US5, FR-010), served from the `article_provenance` view (I-5).
 *
 * The legal basis always comes from the retrieval-time snapshots on
 * `source_item`, never from the mutable `source` row: the defence rests on
 * what applied at the time. The `source` object is identity only.
 */
export interface ArticleProvenance {
  readonly article_id: string;
  readonly headline: string;
  readonly places: readonly string[];
  readonly source: {
    readonly name: string;
    readonly feed_url: string;
    readonly jurisdiction: string;
  };
  readonly source_item: {
    readonly source_url: string;
    readonly original_title: string | null;
    readonly retrieved_at: string;
    readonly content_hash: string;
    readonly licence_snapshot: string;
    readonly usage_rule_snapshot: string;
    readonly permission_evidence_snapshot: string | null;
    readonly original_author: string | null;
  };
  readonly translation: {
    readonly model: string;
    readonly prompt_version: string;
    readonly target_locale: string;
    readonly generated_at: string;
    readonly cost_microusd?: number;
  } | null;
  readonly approval: {
    readonly approver_name: string;
    readonly approver_email: string;
    readonly approved_at: string;
  };
  readonly published_at: string | null;
  readonly withdrawal: {
    readonly withdrawn_at: string;
    readonly withdrawn_by: string;
    readonly reason: string;
  } | null;
  /** True when this trace is fixture data; see `QueuePage.fixture`. */
  readonly fixture?: boolean;
  /**
   * The append-only `domain_event` rows for this article (FR-012). Not in
   * the contract's provenance payload — the audit screen needs the chain
   * as events, not only as final values, so this is a proposed addition
   * recorded in issue #67.
   */
  readonly events?: readonly DomainEvent[];
}

/**
 * Runtime check of a provenance payload. The audit screen dereferences
 * nested objects and formats four timestamps, so a partial body would
 * surface as `Invalid Date` or a thrown formatter mid-render. A
 * malformed record is the client's error, not a crashed audit.
 */
function isArticleProvenance(body: unknown): body is ArticleProvenance {
  if (typeof body !== 'object' || body === null) {
    return false;
  }
  const record = body as Record<string, unknown>;
  const source = record['source'] as Record<string, unknown> | undefined;
  const item = record['source_item'] as Record<string, unknown> | undefined;
  const approval = record['approval'] as Record<string, unknown> | undefined;
  const translation = record['translation'] as Record<string, unknown> | null | undefined;
  return (
    typeof record['article_id'] === 'string' &&
    typeof record['headline'] === 'string' &&
    Array.isArray(record['places']) &&
    typeof source === 'object' &&
    source !== null &&
    typeof source['name'] === 'string' &&
    typeof item === 'object' &&
    item !== null &&
    typeof item['source_url'] === 'string' &&
    isTimestamp(item['retrieved_at']) &&
    typeof item['licence_snapshot'] === 'string' &&
    typeof item['usage_rule_snapshot'] === 'string' &&
    typeof approval === 'object' &&
    approval !== null &&
    typeof approval['approver_name'] === 'string' &&
    isTimestamp(approval['approved_at']) &&
    (translation === null ||
      translation === undefined ||
      (typeof translation === 'object' &&
        typeof translation['model'] === 'string' &&
        isTimestamp(translation['generated_at'])))
  );
}

/**
 * A value usable as a date — a string the Date constructor can read.
 *
 * Exported because the screens format wire timestamps through Intl, which
 * throws on an unreadable one: a write that was genuinely recorded must
 * not turn into an error page because its confirmation carried a date the
 * formatter cannot parse.
 */
export function isTimestamp(value: unknown): value is string {
  return typeof value === 'string' && !Number.isNaN(new Date(value).getTime());
}

/** One append-only audit record (FR-012). */
export interface DomainEvent {
  readonly type: string;
  readonly occurred_at: string;
  readonly detail: string;
}

/**
 * A configured feed (mockup 1i) — a `source` row from migration 0001 plus
 * `active` from 0002 and the poll state from 0007, as
 * `GET /api/v1/editorial/sources` serves it (#86).
 */
export interface SourceRow {
  readonly id: string;
  readonly name: string;
  /**
   * The feed URL the crawler polls — the same column the registration
   * wrote, under one name across both source endpoints.
   */
  readonly url: string;
  readonly language: string;
  readonly jurisdiction: string;
  /** `extract_and_link` unless written permission is on record (FR-004). */
  readonly usage_rule: 'extract_and_link' | 'full_text';
  readonly permission_evidence: string | null;
  /** `source.active` (0002): pausing a feed without deleting anything. */
  readonly active: boolean;
  /** ISO 8601; null when the feed has never been polled. */
  readonly last_polled_at: string | null;
}

/**
 * The last poll cycle: how much was retrieved, how much the content
 * fingerprint deduplicated (FR-014), and which feeds failed — the spec's
 * unreachable-feed edge case, where nothing partial is stored. No endpoint
 * carries this either (issue #70).
 */
export interface PollCycle {
  readonly retrieved: number;
  readonly duplicates_skipped: number;
  readonly failures: readonly string[];
}

/** What the sources screen reads: the endpoint's own page shape. */
export interface SourcesPage {
  readonly items: readonly SourceRow[];
  readonly cycle: PollCycle;
  /**
   * Pass back as `cursor` for the next page; null on the last. Optional
   * because the fixture page has no further pages to offer.
   */
  readonly next_cursor?: string | null;
  /** True when this page is fixture data; see `QueuePage.fixture`. */
  readonly fixture?: boolean;
}

/**
 * Runtime check of a source list. The screen dereferences `cycle` and
 * formats every non-null `last_polled_at`, so a body that satisfies only
 * "has a sources array" could still throw mid-render — a validated
 * rejection turns that into the page's own 503 state instead of a 500.
 */
function isSourcesPage(body: unknown): body is SourcesPage {
  if (typeof body !== 'object' || body === null) {
    return false;
  }
  const record = body as Record<string, unknown>;
  const cycle = record['cycle'] as Record<string, unknown> | undefined;
  if (
    typeof cycle !== 'object' ||
    cycle === null ||
    typeof cycle['retrieved'] !== 'number' ||
    typeof cycle['duplicates_skipped'] !== 'number' ||
    !Array.isArray(cycle['failures'])
  ) {
    return false;
  }
  const items = record['items'];
  if (!Array.isArray(items)) {
    return false;
  }
  return items.every((row: unknown) => {
    if (typeof row !== 'object' || row === null) {
      return false;
    }
    const source = row as Record<string, unknown>;
    const polled = source['last_polled_at'];
    return (
      typeof source['name'] === 'string' &&
      typeof source['url'] === 'string' &&
      typeof source['language'] === 'string' &&
      typeof source['usage_rule'] === 'string' &&
      typeof source['active'] === 'boolean' &&
      // Either never polled, or a timestamp the screen can format.
      (polled === null || isTimestamp(polled))
    );
  });
}

/** The outcome of configuring a source. */
export interface SourceOutcome {
  readonly recorded: boolean;
  readonly source_id?: string;
  readonly reason?: string;
}

/**
 * The body `POST /api/v1/editorial/sources` accepts. The usage rule is
 * deliberately absent: new sources are always `extract_and_link`, and an
 * upgrade is a separate founder-gated flow (FR-004, contract). The screen
 * must therefore state the rule, not offer it.
 */
export interface NewSource {
  readonly name: string;
  readonly url: string;
  readonly language: string;
  readonly jurisdiction: string;
  readonly licence_terms: string;
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

/**
 * What `POST /api/v1/editorial/articles/{id}/withdrawal` answers.
 * `recorded: false` carries the same meaning as on approval: the intent
 * was expressed, nothing was written.
 *
 * `reason` is dual-purpose by construction: on a recorded withdrawal it is
 * the justification the database froze (`article.withdrawal_reason`), and
 * on a refused one it is why nothing was written. Either way it is the one
 * line the banner renders — which is why the recorded arm requires it: a
 * confirmation without the frozen reason is not a usable record, and the
 * client refuses it rather than letting the banner render a blank box.
 */
export type WithdrawalOutcome =
  | {
      readonly recorded: true;
      readonly article_id: string;
      readonly withdrawn_at?: string;
      readonly withdrawn_by?: string;
      /** The justification the database froze into `article.withdrawal_reason`. */
      readonly reason: string;
    }
  | {
      readonly recorded: false;
      /** Why nothing was written. */
      readonly reason?: string;
    };

/** The wire shape of a recorded withdrawal (`WithdrawalRecord`). */
interface WithdrawalRecordBody {
  readonly article_id: string;
  readonly withdrawn_at?: string;
  readonly withdrawn_by?: string;
  readonly reason: string;
}

/**
 * Runtime check of a 2xx withdrawal body. The banner's only text is the
 * recorded reason, so a confirmation that lacks it — an API one deploy
 * behind this client still answering the old `{article_id, withdrawn_at,
 * withdrawn_by}` shape — must be refused, not spread into a success state
 * with nothing to show (#85).
 */
function isWithdrawalRecord(body: unknown): body is WithdrawalRecordBody {
  if (typeof body !== 'object' || body === null) {
    return false;
  }
  const record = body as Record<string, unknown>;
  return (
    typeof record['article_id'] === 'string' &&
    record['article_id'] !== '' &&
    typeof record['reason'] === 'string' &&
    record['reason'].trim() !== ''
  );
}

/** The editorial API surface the pages consume. */
export interface EditorialApi {
  queue(): Promise<QueuePage>;
  /**
   * Approval carries the attribution the contract requires (non-blank —
   * it becomes `article.attribution_block`, which FR-008 renders on every
   * published item) and the places the article publishes to (at least one
   * place slug — the front page is scoped by place, so an article tagged
   * to no place can never appear on any of them, and the API refuses the
   * approval with 400).
   */
  approve(
    sourceItemId: string,
    translationId: string | null,
    attribution: string,
    places: readonly string[],
  ): Promise<ApprovalOutcome>;
  /** The audit trace; null when the id matches no article. */
  provenance(articleId: string): Promise<ArticleProvenance | null>;
  withdraw(articleId: string, reason: string): Promise<WithdrawalOutcome>;
  /**
   * One page of the source list. With a cursor, the page that follows the
   * one whose `next_cursor` it was; the fixtures have a single page and
   * ignore it.
   */
  sources(cursor?: string): Promise<SourcesPage>;
  addSource(input: NewSource): Promise<SourceOutcome>;
}

const NOT_WIRED_APPROVAL =
  'The editorial API is not implemented yet (T019/T020), so no article was created and no approval was recorded.';

const NOT_WIRED_SOURCE =
  'The editorial API is not implemented yet (T014), so no source was configured and no polling started.';

const NOT_WIRED_WITHDRAWAL =
  'The editorial API is not implemented yet (T021), so publication did not end and nothing was written to the audit stream.';

/**
 * `API_BASE_URL` is one address for the whole Go API, but the reader and
 * editorial endpoints land as separate tasks (T024 versus T019/T020). A
 * deployment can therefore serve the reader while the editorial routes
 * are still absent, and the editorial screens must not turn that into an
 * error page: a 404 means "not deployed yet", which is exactly the state
 * the fixtures describe.
 */
const NOT_DEPLOYED = 404;

/**
 * True when the response declares the RFC 9457 problem+json content type —
 * the shape every deployed editorial endpoint answers its errors in. A 404
 * without it is the mux's bare not-found, i.e. the route itself is absent.
 */
function isProblemJson(response: Response): boolean {
  return (response.headers.get('Content-Type') ?? '').includes('application/problem+json');
}

/** The problem body's own words, with a generic line when it has none. */
async function problemDetail(response: Response, subject: string): Promise<string> {
  try {
    const body: unknown = await response.json();
    if (typeof body === 'object' && body !== null) {
      const problem = body as Record<string, unknown>;
      const detail = problem['detail'];
      if (typeof detail === 'string' && detail !== '') {
        return detail;
      }
      const title = problem['title'];
      if (typeof title === 'string' && title !== '') {
        return title;
      }
    }
  } catch {
    // An unreadable problem body still identified itself as a refusal;
    // fall through to the generic line.
  }
  return `editorial API answered ${response.status} for ${subject}`;
}

function fixtureApi(): EditorialApi {
  return {
    queue(): Promise<QueuePage> {
      // Every fixture answer declares itself one, so the screens can flag
      // invented numbers as invented no matter why the fixtures rendered.
      return Promise.resolve({
        items: QUEUE_FIXTURES,
        holds: { queued_untranslated: 3, skipped_over_ceiling: 1 },
        spend: SPEND_FIXTURE,
        fixture: true,
      });
    },
    approve(): Promise<ApprovalOutcome> {
      // Deliberately not a fake success. Approval is the one action whose
      // whole meaning is that a record exists; pretending otherwise would
      // misrepresent an invariant the database enforces.
      return Promise.resolve({ recorded: false, reason: NOT_WIRED_APPROVAL });
    },
    provenance(articleId: string): Promise<ArticleProvenance | null> {
      // An empty id is the screen's first visit, with nothing typed yet:
      // show a trace so the page has something to demonstrate. A given
      // id that matches nothing is genuinely not found — falling back to
      // an unrelated article would be the one thing an audit must never
      // do, since the reader would be looking at another item's evidence.
      const found =
        articleId === ''
          ? PROVENANCE_FIXTURES.at(0)
          : PROVENANCE_FIXTURES.find((row) => row.article_id === articleId);
      return Promise.resolve(found === undefined ? null : { ...found, fixture: true });
    },
    withdraw(): Promise<WithdrawalOutcome> {
      // Withdrawal is audited (FR-016); with nothing to audit into, the
      // screen must not suggest publication ended.
      return Promise.resolve({ recorded: false, reason: NOT_WIRED_WITHDRAWAL });
    },
    sources(): Promise<SourcesPage> {
      return Promise.resolve({ items: SOURCE_FIXTURES, cycle: POLL_CYCLE_FIXTURE, fixture: true });
    },
    addSource(): Promise<SourceOutcome> {
      return Promise.resolve({ recorded: false, reason: NOT_WIRED_SOURCE });
    },
  };
}

function httpApi(baseUrl: string, fetchImpl: typeof fetch, token: string | null): EditorialApi {
  const base = baseUrl.replace(/\/+$/, '');
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (token !== null) {
    headers['Authorization'] = `Bearer ${token}`;
  }
  const fixtures = fixtureApi();
  return {
    async queue(): Promise<QueuePage> {
      const response = await fetchImpl(new URL(`${base}/api/v1/editorial/queue`), { headers });
      if (response.status === NOT_DEPLOYED) {
        return fixtures.queue();
      }
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
    async approve(
      sourceItemId: string,
      translationId: string | null,
      attribution: string,
      places: readonly string[],
    ): Promise<ApprovalOutcome> {
      // The contract takes exactly one origin: translation_id XOR
      // source_item_id (400 if both or neither), plus a non-blank
      // attribution, which becomes the article's attribution block, and
      // at least one place slug, which decides whose front pages the
      // article can appear on.
      const origin =
        translationId === null
          ? { source_item_id: sourceItemId }
          : { translation_id: translationId };
      const response = await fetchImpl(new URL(`${base}/api/v1/editorial/approvals`), {
        method: 'POST',
        headers: { ...headers, 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...origin, attribution, publish: true, places }),
      });
      if (response.status === NOT_DEPLOYED) {
        return fixtures.approve(sourceItemId, translationId, attribution, places);
      }
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
    async provenance(articleId: string): Promise<ArticleProvenance | null> {
      // No id means nothing to trace. Interpolating an empty segment
      // would request /articles//provenance and turn a first visit into
      // an error.
      if (articleId === '') {
        return null;
      }
      const url = new URL(
        `${base}/api/v1/editorial/articles/${encodeURIComponent(articleId)}/provenance`,
      );
      const response = await fetchImpl(url, { headers });
      if (response.status === 404) {
        return null;
      }
      if (!response.ok) {
        throw new EditorialApiError(
          `editorial API answered ${response.status} for the provenance trace`,
          response.status,
        );
      }
      const body: unknown = await response.json();
      if (!isArticleProvenance(body)) {
        throw new EditorialApiError('editorial API answered with a malformed provenance record');
      }
      return body;
    },
    async withdraw(articleId: string, reason: string): Promise<WithdrawalOutcome> {
      const url = new URL(
        `${base}/api/v1/editorial/articles/${encodeURIComponent(articleId)}/withdrawal`,
      );
      const response = await fetchImpl(url, {
        method: 'POST',
        headers: { ...headers, 'Content-Type': 'application/json' },
        body: JSON.stringify({ reason }),
      });
      // A 404 carries two meanings on this route. The endpoint itself
      // answers 404 as a domain outcome — no article with this id, or an
      // approved article that never published — and those arrive as RFC
      // 9457 problem+json with the refusal in `detail`. An unmounted
      // editorial prefix (reader deployed, editorial not — the state the
      // fixtures describe) answers the mux's bare 404 instead. Only the
      // bare 404 takes the fixtures' fallback — the same branch queue(),
      // approve() and sources() take, while provenance() maps its 404 to
      // null and addSource() throws. A problem 404 is the deployed API
      // refusing, and its own words are the honest not-recorded reason.
      if (response.status === NOT_DEPLOYED) {
        if (isProblemJson(response)) {
          return { recorded: false, reason: await problemDetail(response, 'the withdrawal') };
        }
        return fixtures.withdraw(articleId, reason);
      }
      if (!response.ok) {
        throw new EditorialApiError(
          `editorial API answered ${response.status} for the withdrawal`,
          response.status,
        );
      }
      const body: unknown = await response.json();
      if (!isWithdrawalRecord(body)) {
        throw new EditorialApiError(
          'editorial API confirmed the withdrawal without the recorded reason',
        );
      }
      return { recorded: true, ...body };
    },
    async sources(cursor?: string): Promise<SourcesPage> {
      const url = new URL(`${base}/api/v1/editorial/sources`);
      // The endpoint's maximum page: the walk to exhaustion exists to
      // show every source, so it takes the fewest round trips the
      // contract allows.
      url.searchParams.set('limit', '100');
      if (cursor !== undefined) {
        url.searchParams.set('cursor', cursor);
      }
      const response = await fetchImpl(url, { headers });
      // A deployment can still serve the reader while the editorial
      // prefix is unmounted, so a 404 here means the read is not deployed
      // rather than that the screen is broken.
      if (response.status === NOT_DEPLOYED) {
        return fixtures.sources();
      }
      if (!response.ok) {
        throw new EditorialApiError(
          `editorial API answered ${response.status} for the source list`,
          response.status,
        );
      }
      const body: unknown = await response.json();
      if (!isSourcesPage(body)) {
        throw new EditorialApiError('editorial API answered with a malformed source list');
      }
      return body;
    },
    async addSource(input: NewSource): Promise<SourceOutcome> {
      // The usage rule is not sent: the contract does not accept it, and
      // the database defaults it to extract_and_link (FR-004).
      const response = await fetchImpl(new URL(`${base}/api/v1/editorial/sources`), {
        method: 'POST',
        headers: { ...headers, 'Content-Type': 'application/json' },
        body: JSON.stringify(input),
      });
      if (!response.ok) {
        throw new EditorialApiError(
          `editorial API answered ${response.status} for the new source`,
          response.status,
        );
      }
      // The 201 names the new source under `id` (contract; the Go handler's
      // sourceResponse), so the outcome reads that field rather than
      // spreading the body and hoping one called `source_id` appears —
      // which never does, leaving every real success with a blank id.
      const body: unknown = await response.json();
      const id = (body as Record<string, unknown> | null)?.['id'];
      // A 201 is a written record even when the body withholds the id, so
      // the outcome stays recorded and the screen omits what it was not
      // told. Denying a source that exists would be the honest-record
      // failure in the other direction.
      return typeof id === 'string' && id !== ''
        ? { recorded: true, source_id: id }
        : { recorded: true };
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
 * The line the banner prints after an approval was recorded: what the
 * response said, and nothing else.
 *
 * A missing `approved_by` renders as absent rather than as the signed-in
 * person. They are not the same claim: the approver is whoever the
 * database wrote into `article.approved_by` (I-1), and printing the name
 * of whoever happened to submit the form over a field the server did not
 * return would attribute one person's approval to another. If the field
 * is missing, what the screen honestly knows is that it does not know.
 */
export function approvalRecordLine(outcome: ApprovalOutcome): string {
  const parts: string[] = [];
  if (outcome.approved_by !== undefined && outcome.approved_by !== '') {
    parts.push(`approved_by = ${outcome.approved_by}`);
  }
  if (outcome.article_id !== undefined && outcome.article_id !== '') {
    parts.push(`article ${outcome.article_id}`);
  }
  return parts.join(' · ');
}

/** The whole source list, as the sources screen renders it. */
export interface SourceList {
  readonly items: readonly SourceRow[];
  readonly cycle: PollCycle;
  /** True when the list is fixture data; see `QueuePage.fixture`. */
  readonly fixture: boolean;
  /**
   * True when the page bound was reached with a cursor still on offer:
   * sources exist beyond `items`, and the screen must say so rather than
   * present the count as the whole registry.
   */
  readonly truncated: boolean;
}

/**
 * Follows `next_cursor` to exhaustion, bounded. The sources screen exists
 * to make the licensing base visible, so rendering page one as the whole
 * registry — the 21st source invisible, the summary counting only what
 * one page held — is the one failure its purpose rules out. Every page is
 * fetched and concatenated; if the bound is hit with a cursor still on
 * offer, the result says so explicitly instead of truncating silently.
 *
 * The bound is generous by construction: pages arrive at the contract's
 * maximum limit of 100 and feeds are registered by hand, so the default
 * covers a registry far beyond reality while still refusing to loop
 * forever on a cursor chain that never exhausts.
 */
export async function allSources(api: EditorialApi, maxPages = 10): Promise<SourceList> {
  const first = await api.sources();
  const items: SourceRow[] = [...first.items];
  let cursor = first.next_cursor ?? null;
  let pages = 1;
  while (cursor !== null && pages < maxPages) {
    const page = await api.sources(cursor);
    items.push(...page.items);
    cursor = page.next_cursor ?? null;
    pages += 1;
  }
  return {
    items,
    // The cycle is one aggregate reading, not a paged list: the first
    // page's copy is the one the screen renders.
    cycle: first.cycle,
    fixture: first.fixture === true,
    truncated: cursor !== null,
  };
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
