import type {
  ApprovalOutcome,
  QueueWithdrawal,
  SourceActionOutcome,
  SourceOutcome,
  WithdrawalOutcome,
} from './api';
import { approvalRecordLine, isTimestamp } from './api';
import type { EditorialStrings } from './strings';

/**
 * One shape for every moment the record behaves in a way that surprises
 * (#121): approval freezes a name forever, publication is one-way,
 * withdrawal removes an article from the reader site but not from history,
 * a withdrawn article's origin returns to the queue as a correction.
 *
 * Every builder derives its dynamic content from the response body under
 * the honest-record rule: the recorded tone never appears over a write
 * that did not happen, absent fields are omitted rather than substituted,
 * and when the server sent no detail the notice states the outcome it
 * knows and nothing more.
 */
export type NoticeTone = 'recorded' | 'refused' | 'context';

export interface RecordNoticeModel {
  readonly tone: NoticeTone;
  readonly label: string;
  readonly body: string;
  /**
   * Frozen facts echoed from the response, one line each, in the record's
   * own field names; [] when the server sent none.
   */
  readonly record: readonly string[];
}

/**
 * The banner after an approval attempt. The recorded body is keyed to what
 * the response said about publication: a `published_at` timestamp, an
 * explicit null, or nothing at all are three different facts and produce
 * three different sentences — the screen never claims a publication the
 * server did not confirm.
 */
export function noticeForApproval(
  outcome: ApprovalOutcome,
  t: Pick<
    EditorialStrings,
    | 'approvedTitle'
    | 'notRecordedTitle'
    | 'approvalPublishedBody'
    | 'approvalNotPublishedBody'
    | 'approvalRecordedBody'
  >,
  formatDate: (iso: string) => string,
): RecordNoticeModel {
  if (!outcome.recorded) {
    return {
      tone: 'refused',
      label: t.notRecordedTitle,
      body: outcome.reason ?? '',
      record: [],
    };
  }
  const record: string[] = [];
  const line = approvalRecordLine(outcome);
  if (line !== '') {
    record.push(line);
  }
  // A readable timestamp is publication stated and datable; an explicit
  // null is publication withheld. Anything else — the field absent, or a
  // value the formatter cannot read — is a publication status the screen
  // cannot report, and saying "published" over an unreadable date would
  // assert a fact on the strength of garbage.
  if (isTimestamp(outcome.published_at)) {
    record.push(`published_at = ${formatDate(outcome.published_at)}`);
  }
  return {
    tone: 'recorded',
    label: t.approvedTitle,
    body: isTimestamp(outcome.published_at)
      ? t.approvalPublishedBody
      : outcome.published_at === null
        ? t.approvalNotPublishedBody
        : t.approvalRecordedBody,
    record,
  };
}

/**
 * The banner after a withdrawal attempt. A recorded outcome carries its
 * reason by type (the client refuses a confirmation without one), so the
 * recorded body always names it; `withdrawn_at` and `withdrawn_by` are
 * echoed only when the response carried them.
 */
export function noticeForWithdrawal(
  outcome: WithdrawalOutcome,
  t: Pick<EditorialStrings, 'withdraw' | 'notRecordedTitle' | 'withdrawalRecordedBody'>,
  formatDate: (iso: string) => string,
): RecordNoticeModel {
  if (!outcome.recorded) {
    return {
      tone: 'refused',
      label: t.notRecordedTitle,
      body: outcome.reason ?? '',
      record: [],
    };
  }
  const record: string[] = [];
  // Same guard as the approval notice: a withdrawal is irreversible, and
  // an unreadable withdrawn_at must cost the editor a missing line, not
  // the confirmation that it happened. A null slips past a bare
  // undefined check and would render the 1970 epoch as a frozen fact.
  if (isTimestamp(outcome.withdrawn_at)) {
    record.push(`withdrawn_at = ${formatDate(outcome.withdrawn_at)}`);
  }
  // A type is not a validator: `withdrawn_by` is spread straight from the
  // wire, so a JSON null would satisfy "not undefined, not empty" and
  // print the word null as the person who withdrew the article. Only a
  // non-empty string names anybody.
  if (typeof outcome.withdrawn_by === 'string' && outcome.withdrawn_by !== '') {
    record.push(`withdrawn_by = ${outcome.withdrawn_by}`);
  }
  if (outcome.article_id !== '') {
    record.push(`article ${outcome.article_id}`);
  }
  return {
    tone: 'recorded',
    label: t.withdraw,
    body: t.withdrawalRecordedBody(outcome.reason),
    record,
  };
}

/**
 * The banner after registering a source. A 201 carries an id, not prose,
 * so success states itself and the id rides in the record line — omitted
 * entirely when the response withheld it, rather than interpolated as an
 * empty pair of parentheses. A refusal repeats the API's own words.
 */
export function noticeForSource(
  outcome: SourceOutcome,
  t: Pick<EditorialStrings, 'addSource' | 'notRecordedTitle' | 'sourceRecordedBody'>,
): RecordNoticeModel {
  if (!outcome.recorded) {
    return {
      tone: 'refused',
      label: t.notRecordedTitle,
      body: outcome.reason ?? '',
      record: [],
    };
  }
  return {
    tone: 'recorded',
    label: t.addSource,
    body: t.sourceRecordedBody,
    record:
      outcome.source_id === undefined || outcome.source_id === ''
        ? []
        : [`source ${outcome.source_id}`],
  };
}

/**
 * The banner after editing a source (#118). An edit that changed nothing
 * and an edit the API refused are both "nothing was recorded", and the
 * screen already words each of them; the notice carries those words
 * rather than flattening them into one failure.
 */
export function noticeForSourceEdit(
  outcome: SourceActionOutcome,
  t: Pick<EditorialStrings, 'editSource' | 'notRecordedTitle' | 'sourceUpdated'>,
): RecordNoticeModel {
  return outcome.recorded
    ? { tone: 'recorded', label: t.editSource, body: t.sourceUpdated, record: [] }
    : { tone: 'refused', label: t.notRecordedTitle, body: outcome.reason ?? '', record: [] };
}

/**
 * One row's refusal inside a bulk result (#118, #121). A delete the
 * database held back for evidence (409) gets the explanation the status
 * code alone does not carry: those retrieved items are the start of every
 * published article's provenance chain.
 *
 * The API's detail is English prose written for operators, and it names
 * the count — the fact the editor needs. Concatenating it with a Greek or
 * German sentence would switch language mid-line and say the same thing
 * twice, so it goes where the response's own words belong on every other
 * notice: the record line. The body is the explanation in the language
 * the editor is reading.
 */
export function noticeForRowRefusal(
  outcome: SourceActionOutcome,
  t: Pick<EditorialStrings, 'notRecordedTitle' | 'deleteRefusedBody'>,
): RecordNoticeModel {
  const words = outcome.reason ?? '';
  const heldByEvidence = outcome.status === 409;
  return {
    tone: 'refused',
    label: t.notRecordedTitle,
    body: heldByEvidence ? t.deleteRefusedBody : words,
    record: heldByEvidence && words !== '' ? [words] : [],
  };
}

/**
 * The summary after a bulk action (#118). Bulk work is a loop over
 * per-row endpoints, so a mixed result is the normal case and both counts
 * are always stated: any refusal at all takes the attention-seeking tone,
 * because something the editor asked for did not happen — the rows
 * themselves carry each refusal's own words.
 */
export function noticeForBulk(
  label: string,
  summary: string,
  refused: number,
  /**
   * What the action did and did not do, when that is worth saying — a
   * deactivation stops polling and keeps everything else. These sentences
   * speak about the action as a whole, so the caller passes one only when
   * the whole action succeeded: over a mixed result it would claim the
   * consequence for rows where nothing was recorded.
   */
  consequence?: string,
): RecordNoticeModel {
  return {
    tone: refused > 0 ? 'refused' : 'recorded',
    label,
    body:
      consequence === undefined || consequence === ''
        ? summary
        : `${summary} — ${consequence}`,
    record: [],
  };
}

/**
 * A bulk POST arrived with nothing selected. Nothing was done, and the
 * screen says so rather than reporting an empty success.
 */
export function noticeForNothingSelected(
  t: Pick<EditorialStrings, 'notRecordedTitle' | 'noneSelected'>,
): RecordNoticeModel {
  return { tone: 'refused', label: t.notRecordedTitle, body: t.noneSelected, record: [] };
}

/**
 * The session expired between opening a form and submitting it: nothing
 * was recorded. When the re-rendered page has no form to put the typed
 * words back into, the notice itself carries them (`stale`); when the
 * form is refilled instead, the caller passes null.
 */
export function noticeForSignedOut(
  t: Pick<EditorialStrings, 'notRecordedTitle' | 'signedOutMidPost'>,
  stale: string | null,
): RecordNoticeModel {
  return {
    tone: 'refused',
    label: t.notRecordedTitle,
    body: stale === null ? t.signedOutMidPost : `${t.signedOutMidPost} «${stale}»`,
    record: [],
  };
}

/**
 * Why a queue item is back: its previous article was withdrawn. The
 * explanation quotes the recorded withdrawal when the payload carries it;
 * for a payload that does not (an API predating the field), the notice
 * states the correction fact and no more.
 */
export function noticeForCorrection(
  withdrawal: QueueWithdrawal | undefined,
  t: Pick<EditorialStrings, 'correctionTag' | 'correctionBody'>,
  formatDate: (iso: string) => string,
): RecordNoticeModel {
  return {
    tone: 'context',
    label: t.correctionTag,
    body: t.correctionBody,
    // Guarded like its siblings, and for a sharper reason: this is the one
    // builder reading straight from the queue payload, which the client
    // never runtime-validates. An unreadable date would throw inside the
    // page render and cost the whole review queue rather than one line, and
    // an absent reason would interpolate the word "undefined" as the
    // recorded grounds for a withdrawal. The API's types make neither
    // reachable today; the record still does not rest on that.
    record:
      withdrawal === undefined ||
      !isTimestamp(withdrawal.withdrawn_at) ||
      typeof withdrawal.reason !== 'string' ||
      withdrawal.reason === ''
        ? []
        : [`${formatDate(withdrawal.withdrawn_at)} · «${withdrawal.reason}»`],
  };
}
