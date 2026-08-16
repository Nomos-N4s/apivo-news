/**
 * Parsing and validation for the approval form POST — the server side of
 * the editor screen's one privileged action.
 *
 * The place checkboxes have no client JavaScript behind them (the screen
 * needs none), and plain HTML cannot express "at least one of these", so
 * this is where an empty selection is stopped: before the API client is
 * called, with a named reason the screen can show. The Go API enforces the
 * same rule with a 400 — this check is the polite refusal, not the
 * guarantee.
 */

import { PLACE_CATALOG, type Place } from '../reader/axes';
import type { QueueItem } from './api';

/**
 * The places the approval form offers: the reader vocabulary, exactly.
 * Non-selectable catalog entries (Bavaria, Germany) are hierarchy context —
 * setup and register never offer them and the axes parser drops them from
 * URLs, so an article tagged only to one would satisfy the API and the
 * database yet stay unreachable through the product's own follow flow: a
 * quieter version of the unreachability FR-009 exists to end. Same filter
 * as register.astro; publishing to hierarchy places would be a front-page
 * roll-up decision for the spec, not a checkbox default.
 */
export const APPROVAL_PLACES: readonly Place[] = PLACE_CATALOG.filter(
  (place) => place.selectable,
);

/** The two strings the attribution default composes from. */
export interface AttributionStrings {
  readonly originallyPublishedBy: string;
  readonly publicationDateNotSupplied: string;
}

/**
 * The attribution the form pre-fills (FR-008): source name and the
 * publication date the feed DECLARED — `original_published_at`, never
 * `retrieved_at`. The two are different claims, and `article_guard`
 * freezes whatever the editor approves, permanently; a silently
 * substituted retrieval date would be frozen in as the publication date.
 *
 * When the feed declared no date, the absence is said out loud —
 * "publication date not supplied by the feed", in the chrome's language —
 * so the editor decides what to write over a visible gap rather than
 * approving an invented fact. The formatter is only ever called with the
 * declared date; a fallback date does not exist to format.
 */
export function attributionDefault(
  item: Pick<QueueItem, 'source_name' | 'original_published_at'>,
  strings: AttributionStrings,
  formatDate: (iso: string) => string,
): string {
  const published = item.original_published_at;
  if (published === null || published === undefined) {
    return `${strings.originallyPublishedBy} ${item.source_name} (${strings.publicationDateNotSupplied}).`;
  }
  return `${strings.originallyPublishedBy} ${item.source_name}, ${formatDate(published)}.`;
}

/**
 * Whether a queue row carries the evidence the approval rests on (#87).
 *
 * Presence, not truthiness: a null author or publication date is a value
 * the feed genuinely declared (none), while an ABSENT property means the
 * row came from an API that predates the evidence block — and a permanent
 * approval must not be offered over placeholder dashes, which is the exact
 * failure mode the evidence work exists to close. A translated row must
 * additionally carry its lineage: the model and prompt version that
 * produced the text being approved (FR-005), and the recorded cost field
 * (FR-006 — the value may be null, the field may not be missing).
 */
export function hasApprovalEvidence(
  item: Pick<
    QueueItem,
    | 'translation_id'
    | 'source_url'
    | 'extract_original'
    | 'original_author'
    | 'original_published_at'
    | 'content_hash'
    | 'source_lang'
    | 'model'
    | 'prompt_version'
    | 'cost_microusd'
  >,
): boolean {
  if (
    item.source_url === undefined ||
    item.extract_original === undefined ||
    item.original_author === undefined ||
    item.original_published_at === undefined ||
    item.content_hash === undefined ||
    item.source_lang === undefined
  ) {
    return false;
  }
  if (item.translation_id !== null) {
    return item.model != null && item.prompt_version != null && item.cost_microusd !== undefined;
  }
  return true;
}

/** A submission the API client can carry as-is. */
export interface ApprovalSubmission {
  readonly sourceItemId: string;
  /** Null for an untranslated origin — the contract's XOR. */
  readonly translationId: string | null;
  readonly attribution: string;
  /** At least one place slug; the article publishes to these. */
  readonly places: readonly string[];
}

/**
 * Why a POST could not become a submission. `no-place` gets its own name
 * because it is the one refusal with a message of its own on the screen:
 * an article tagged to no place can never appear on any front page.
 */
export type ApprovalFormRefusal = 'no-item' | 'no-attribution' | 'no-place';

/**
 * What the POST did carry, kept through a refusal so the re-render can put
 * it back: the attribution is the one thing on the form typed rather than
 * selected, and the checked boxes are the editor's judgement — a refusal
 * that discarded either would charge the editor for the form's own rule.
 */
export interface StaleFields {
  /** Null when the POST named no item — there is nothing to re-select. */
  readonly sourceItemId: string | null;
  /** Trimmed; null when blank — the default would be put back anyway. */
  readonly attribution: string | null;
  /** Possibly empty — for `no-place` it necessarily is. */
  readonly places: readonly string[];
}

export type ApprovalFormResult =
  | { readonly ok: true; readonly submission: ApprovalSubmission }
  | { readonly ok: false; readonly refusal: ApprovalFormRefusal; readonly stale: StaleFields };

/**
 * Parses the approval form's fields. The place checkboxes arrive as
 * repeated `place` entries; duplicates collapse to one because a checkbox
 * group cannot mean a slug twice, and the API rejects a repeated slug.
 */
export function parseApprovalForm(form: FormData): ApprovalFormResult {
  const sourceItemId = String(form.get('source_item_id') ?? '');
  const rawTranslationId = form.get('translation_id');
  const translationId =
    typeof rawTranslationId === 'string' && rawTranslationId !== '' ? rawTranslationId : null;
  const attribution = String(form.get('attribution') ?? '').trim();
  const places: string[] = [];
  for (const value of form.getAll('place')) {
    const slug = String(value).trim();
    if (slug !== '' && !places.includes(slug)) {
      places.push(slug);
    }
  }
  const refuse = (refusal: ApprovalFormRefusal): ApprovalFormResult => ({
    ok: false,
    refusal,
    stale: {
      sourceItemId: sourceItemId === '' ? null : sourceItemId,
      attribution: attribution === '' ? null : attribution,
      places,
    },
  });
  if (sourceItemId === '') {
    return refuse('no-item');
  }
  if (attribution === '') {
    return refuse('no-attribution');
  }
  if (places.length === 0) {
    return refuse('no-place');
  }
  return { ok: true, submission: { sourceItemId, translationId, attribution, places } };
}
