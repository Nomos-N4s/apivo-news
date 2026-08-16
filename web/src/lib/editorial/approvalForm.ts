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

export type ApprovalFormResult =
  | { readonly ok: true; readonly submission: ApprovalSubmission }
  | { readonly ok: false; readonly refusal: ApprovalFormRefusal };

/**
 * Parses the approval form's fields. The place checkboxes arrive as
 * repeated `place` entries; duplicates collapse to one because a checkbox
 * group cannot mean a slug twice, and the API rejects a repeated slug.
 */
export function parseApprovalForm(form: FormData): ApprovalFormResult {
  const sourceItemId = String(form.get('source_item_id') ?? '');
  if (sourceItemId === '') {
    return { ok: false, refusal: 'no-item' };
  }
  const rawTranslationId = form.get('translation_id');
  const translationId =
    typeof rawTranslationId === 'string' && rawTranslationId !== '' ? rawTranslationId : null;
  const attribution = String(form.get('attribution') ?? '').trim();
  if (attribution === '') {
    return { ok: false, refusal: 'no-attribution' };
  }
  const places: string[] = [];
  for (const value of form.getAll('place')) {
    const slug = String(value).trim();
    if (slug !== '' && !places.includes(slug)) {
      places.push(slug);
    }
  }
  if (places.length === 0) {
    return { ok: false, refusal: 'no-place' };
  }
  return { ok: true, submission: { sourceItemId, translationId, attribution, places } };
}
