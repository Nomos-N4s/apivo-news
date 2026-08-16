/**
 * The row edit form's server side: turning a submitted edit into the
 * patch it actually asks for.
 *
 * The form pre-fills every editable field, so a plain submission carries
 * all four whether the editor touched them or not. Sending all four is
 * not the same request as sending the one that changed: the API diffs
 * supplied values against the CURRENT row, so a field another editor
 * altered between this page's render and this submission would be PATCHed
 * back to the render-time value — the feed URL silently returned to a
 * dead one, a source someone just paused reactivated — and the
 * `source.updated` event would attribute that revert to an editor who
 * never touched the field. `SourcePatch`'s own contract says absent means
 * unchanged; this is where the form honours it.
 *
 * The comparison is against what the form SHOWED, not against the row as
 * it stands now. Those differ exactly when someone else edited in the
 * meantime, and that is the case being fixed: an untouched field must
 * stay absent, and a field the editor really did retype must be sent even
 * if it now happens to match.
 */

import type { SourcePatch } from './api';

/** The four editable fields, as one edit form carries them. */
export interface SourceEditFields {
  readonly name: string;
  readonly url: string;
  readonly licence_terms: string;
  readonly active: boolean;
}

/** The hidden input carrying what the form was rendered with. */
export const BASELINE_FIELD = 'rendered';

/**
 * Encodes the render-time values for the hidden input.
 *
 * JSON rather than one hidden input per field, because licence terms are
 * multi-line and the hidden input's own value sanitiser strips carriage
 * returns and newlines outright — a baseline that lost them would differ
 * from every submission and make the untouched field look edited on every
 * save. JSON escapes them into the attribute intact.
 */
export function renderedBaseline(fields: SourceEditFields): string {
  return JSON.stringify(fields);
}

/**
 * Newlines as the value will be compared and sent: browsers submit
 * textarea content with CRLF, while the row (and therefore the baseline)
 * holds whatever the API stored. Normalising both sides keeps a
 * line-ending difference nobody typed out of the audit stream.
 */
function normalise(value: string): string {
  return value.replace(/\r\n/g, '\n');
}

function parseBaseline(raw: string): SourceEditFields | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    return null;
  }
  const record = parsed as Record<string, unknown>;
  if (
    typeof record['name'] !== 'string' ||
    typeof record['url'] !== 'string' ||
    typeof record['licence_terms'] !== 'string' ||
    typeof record['active'] !== 'boolean'
  ) {
    return null;
  }
  return {
    name: record['name'],
    url: record['url'],
    licence_terms: normalise(record['licence_terms']),
    active: record['active'],
  };
}

/**
 * Why a submitted edit became no request at all.
 *
 * `no-change` is a finding, not a failure: every field came back as it
 * was shown, so there is nothing to record — and sending it anyway would
 * ask the API to confirm an edit that never happened.
 *
 * `no-baseline` is the form arriving without what it was showing. Nothing
 * can be diffed then, and the one alternative — send all four — is the
 * silent clobber this parser exists to prevent, so it refuses instead.
 */
export type SourceEditRefusal = 'no-source' | 'no-baseline' | 'no-change';

export type SourceEditFormResult =
  | {
      readonly ok: true;
      readonly id: string;
      /** Only the fields whose submitted value differs from the rendered one. */
      readonly patch: SourcePatch;
      readonly typed: SourceEditFields;
    }
  | {
      readonly ok: false;
      readonly refusal: SourceEditRefusal;
      /** Null only when the POST named no source. */
      readonly id: string | null;
      /**
       * What the editor had on screen, kept so a refusal can put it back
       * rather than charging them for retyping licensing details.
       */
      readonly typed: SourceEditFields;
    };

/** Parses the row edit form into the patch it asks for. */
export function parseSourceEditForm(form: FormData): SourceEditFormResult {
  const id = String(form.get('id') ?? '');
  const typed: SourceEditFields = {
    name: String(form.get('name') ?? ''),
    url: String(form.get('url') ?? '').trim(),
    licence_terms: normalise(String(form.get('licence_terms') ?? '')),
    // An unchecked checkbox is not submitted at all, which is precisely
    // "deactivate this source" — the absence is the value.
    active: form.get('active') === 'on',
  };
  const refuse = (refusal: SourceEditRefusal): SourceEditFormResult => ({
    ok: false,
    refusal,
    id: id === '' ? null : id,
    typed,
  });
  if (id === '') {
    return refuse('no-source');
  }
  const rendered = parseBaseline(String(form.get(BASELINE_FIELD) ?? ''));
  if (rendered === null) {
    return refuse('no-baseline');
  }

  const patch: {
    name?: string;
    url?: string;
    active?: boolean;
    licence_terms?: string;
  } = {};
  if (typed.name !== rendered.name) {
    patch.name = typed.name;
  }
  // The URL is compared trimmed because the API stores it trimmed; a
  // stray space is not an edit to the feed the crawler polls.
  if (typed.url !== rendered.url.trim()) {
    patch.url = typed.url;
  }
  if (typed.licence_terms !== rendered.licence_terms) {
    patch.licence_terms = typed.licence_terms;
  }
  if (typed.active !== rendered.active) {
    patch.active = typed.active;
  }
  if (Object.keys(patch).length === 0) {
    return refuse('no-change');
  }
  return { ok: true, id, patch, typed };
}
