import type {
  ApprovalOutcome,
  QueueWithdrawal,
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
  if (outcome.withdrawn_by !== undefined && outcome.withdrawn_by !== '') {
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
    record:
      withdrawal === undefined
        ? []
        : [`${formatDate(withdrawal.withdrawn_at)} · «${withdrawal.reason}»`],
  };
}
