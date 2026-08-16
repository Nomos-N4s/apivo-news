import { describe, expect, it } from 'vitest';

import {
  noticeForApproval,
  noticeForCorrection,
  noticeForSignedOut,
  noticeForSource,
  noticeForWithdrawal,
} from './notices';
import { editorialStrings } from './strings';

const t = editorialStrings('el');
const de = editorialStrings('de');
const stampDate = (iso: string): string => `[${iso}]`;

describe('noticeForApproval', () => {
  it('states publication only over a published_at the server sent', () => {
    const notice = noticeForApproval(
      {
        recorded: true,
        article_id: 'a1b2c3',
        approved_by: 'Eleni Papadaki',
        published_at: '2026-08-16T10:00:00Z',
      },
      t,
      stampDate,
    );
    expect(notice.tone).toBe('recorded');
    expect(notice.label).toBe(t.approvedTitle);
    expect(notice.body).toBe(t.approvalPublishedBody);
    expect(notice.record).toEqual([
      'approved_by = Eleni Papadaki · article a1b2c3',
      'published_at = [2026-08-16T10:00:00Z]',
    ]);
  });

  it('an explicit null is "not published yet", not silence', () => {
    const notice = noticeForApproval(
      { recorded: true, article_id: 'a1', published_at: null },
      t,
      stampDate,
    );
    expect(notice.body).toBe(t.approvalNotPublishedBody);
    expect(notice.record).toEqual(['article a1']);
  });

  it('a response that says nothing about publication claims nothing', () => {
    const notice = noticeForApproval({ recorded: true, article_id: 'a1' }, t, stampDate);
    expect(notice.body).toBe(t.approvalRecordedBody);
    expect(notice.record.join(' ')).not.toContain('published_at');
  });

  it('never shows the recorded tone over a refusal, and repeats its reason', () => {
    const notice = noticeForApproval({ recorded: false, reason: 'origin already approved' }, t, stampDate);
    expect(notice.tone).toBe('refused');
    expect(notice.label).toBe(t.notRecordedTitle);
    expect(notice.body).toBe('origin already approved');
    expect(notice.record).toEqual([]);
  });

  it('a refusal without words stays empty rather than inventing any', () => {
    const notice = noticeForApproval({ recorded: false }, t, stampDate);
    expect(notice.body).toBe('');
  });

  it('an unreadable published_at claims neither a date nor a publication', () => {
    const throwing = (iso: string): string => {
      // What Intl actually does with an unparseable date.
      if (Number.isNaN(new Date(iso).getTime())) {
        throw new RangeError('Invalid time value');
      }
      return iso;
    };
    const notice = noticeForApproval(
      { recorded: true, article_id: 'a1', published_at: 'not a date' },
      t,
      throwing,
    );
    // The approval is still confirmed — it was recorded — but a value the
    // screen cannot read is no evidence of publication.
    expect(notice.tone).toBe('recorded');
    expect(notice.body).toBe(t.approvalRecordedBody);
    expect(notice.body).not.toBe(t.approvalPublishedBody);
    expect(notice.record).toEqual(['article a1']);
  });
});

describe('noticeForWithdrawal', () => {
  it('says what withdrawal did and did not do, quoting the frozen reason', () => {
    const notice = noticeForWithdrawal(
      {
        recorded: true,
        article_id: 'art-9',
        withdrawn_at: '2026-08-16T09:00:00Z',
        withdrawn_by: 'uid-77',
        reason: 'factual error in the headline',
      },
      t,
      stampDate,
    );
    expect(notice.tone).toBe('recorded');
    expect(notice.label).toBe(t.withdraw);
    expect(notice.body).toBe(t.withdrawalRecordedBody('factual error in the headline'));
    expect(notice.body).toContain('factual error in the headline');
    expect(notice.record).toEqual([
      'withdrawn_at = [2026-08-16T09:00:00Z]',
      'withdrawn_by = uid-77',
      'article art-9',
    ]);
  });

  it('echoes only the fields the response carried', () => {
    const notice = noticeForWithdrawal(
      { recorded: true, article_id: 'art-9', reason: 'why' },
      t,
      stampDate,
    );
    expect(notice.record).toEqual(['article art-9']);
  });

  it('refuses to render an unreadable or null withdrawn_at as a frozen fact', () => {
    for (const withdrawn_at of ['not a date', null as unknown as string]) {
      const notice = noticeForWithdrawal(
        { recorded: true, article_id: 'art-9', withdrawn_at, reason: 'why' },
        t,
        (iso) => {
          if (Number.isNaN(new Date(iso).getTime())) {
            throw new RangeError('Invalid time value');
          }
          return iso;
        },
      );
      // A withdrawal that happened is still confirmed; the line that
      // would have said 1970 — or thrown — is left out.
      expect(notice.tone).toBe('recorded');
      expect(notice.record).toEqual(['article art-9']);
    }
  });

  it('a refusal keeps the not-recorded label and the refusing words', () => {
    const notice = noticeForWithdrawal({ recorded: false, reason: 'already withdrawn' }, t, stampDate);
    expect(notice.tone).toBe('refused');
    expect(notice.label).toBe(t.notRecordedTitle);
    expect(notice.body).toBe('already withdrawn');
  });

  it('holds in both chrome languages', () => {
    const recorded = noticeForWithdrawal(
      { recorded: true, article_id: 'a', reason: 'Grund' },
      de,
      stampDate,
    );
    expect(recorded.label).toBe(de.withdraw);
    expect(recorded.body).toBe(de.withdrawalRecordedBody('Grund'));
    const refused = noticeForWithdrawal({ recorded: false }, de, stampDate);
    expect(refused.label).toBe(de.notRecordedTitle);
    expect(refused.body).toBe('');
  });
});

describe('noticeForSource', () => {
  it('success states registration and echoes the id the API answered', () => {
    const notice = noticeForSource({ recorded: true, source_id: 'src-3' }, t);
    expect(notice.tone).toBe('recorded');
    expect(notice.label).toBe(t.addSource);
    expect(notice.body).toBe(t.sourceRecordedBody);
    expect(notice.record).toEqual(['source src-3']);
  });

  it('omits the id the response withheld instead of printing an empty one', () => {
    const notice = noticeForSource({ recorded: true }, t);
    expect(notice.tone).toBe('recorded');
    expect(notice.record).toEqual([]);
    expect(notice.body).toBe(t.sourceRecordedBody);
  });

  it('never announces that polling began — the 201 carries no poll state', () => {
    for (const strings of [t, de]) {
      expect(strings.sourceRecordedBody).not.toMatch(/ξεκίνησε|läuft/);
    }
  });

  it('a refusal repeats the API’s own words under the not-recorded label', () => {
    const notice = noticeForSource({ recorded: false, reason: 'duplicate feed URL' }, t);
    expect(notice.tone).toBe('refused');
    expect(notice.label).toBe(t.notRecordedTitle);
    expect(notice.body).toBe('duplicate feed URL');
  });
});

describe('noticeForSignedOut', () => {
  it('carries the typed words only when no form can', () => {
    const bare = noticeForSignedOut(t, null);
    expect(bare.tone).toBe('refused');
    expect(bare.body).toBe(t.signedOutMidPost);
    const carrying = noticeForSignedOut(t, 'the typed attribution');
    expect(carrying.body).toBe(`${t.signedOutMidPost} «the typed attribution»`);
  });
});

describe('noticeForCorrection', () => {
  it('explains the return and quotes the recorded withdrawal', () => {
    const notice = noticeForCorrection(
      {
        article_id: 'art-1',
        withdrawn_at: '2026-08-15T08:00:00Z',
        withdrawn_by: 'uid-1',
        reason: 'wrong municipality named',
      },
      t,
      stampDate,
    );
    expect(notice.tone).toBe('context');
    expect(notice.label).toBe(t.correctionTag);
    expect(notice.body).toBe(t.correctionBody);
    expect(notice.record).toEqual(['[2026-08-15T08:00:00Z] · «wrong municipality named»']);
  });

  it('states the correction fact alone when the payload carries no withdrawal', () => {
    const notice = noticeForCorrection(undefined, t, stampDate);
    expect(notice.body).toBe(t.correctionBody);
    expect(notice.record).toEqual([]);
  });
});
