import { describe, expect, it } from 'vitest';

import { parseApprovalForm } from './approvalForm';

function approvalForm(entries: readonly (readonly [string, string])[]): FormData {
  const form = new FormData();
  for (const [name, value] of entries) {
    form.append(name, value);
  }
  return form;
}

describe('parseApprovalForm', () => {
  const complete: readonly (readonly [string, string])[] = [
    ['source_item_id', 'src-1'],
    ['translation_id', 'tr-1'],
    ['attribution', 'Originally published by X.'],
    ['place', 'munich'],
    ['place', 'greece'],
  ];

  it('carries a complete form as a submission', () => {
    const result = parseApprovalForm(approvalForm(complete));
    expect(result).toEqual({
      ok: true,
      submission: {
        sourceItemId: 'src-1',
        translationId: 'tr-1',
        attribution: 'Originally published by X.',
        places: ['munich', 'greece'],
      },
    });
  });

  it('cannot submit with no place checked (FR-009)', () => {
    // Unchecked checkboxes simply do not arrive in a form POST, and the
    // front page is scoped by place — an approval naming none would create
    // an article no reader could ever reach, so it must not reach the API
    // as a submission at all.
    const result = parseApprovalForm(
      approvalForm(complete.filter(([name]) => name !== 'place')),
    );
    expect(result).toEqual({ ok: false, refusal: 'no-place' });
  });

  it('treats blank place values as no place', () => {
    const withBlank = complete
      .filter(([name]) => name !== 'place')
      .concat([['place', '   ']]);
    expect(parseApprovalForm(approvalForm(withBlank))).toEqual({
      ok: false,
      refusal: 'no-place',
    });
  });

  it('collapses a repeated slug — a checkbox group cannot mean a place twice', () => {
    // The API rejects a repeated slug with 400, so the duplicate must not
    // survive parsing even if a hand-crafted POST carries one.
    const result = parseApprovalForm(approvalForm([...complete, ['place', 'munich']]));
    expect(result.ok && result.submission.places).toEqual(['munich', 'greece']);
  });

  it('maps an absent translation_id to the untranslated origin shape', () => {
    const untranslated = complete.map(([name, value]) =>
      name === 'translation_id' ? ([name, ''] as const) : ([name, value] as const),
    );
    const result = parseApprovalForm(approvalForm(untranslated));
    expect(result.ok && result.submission.translationId).toBe(null);
  });

  it('refuses a form naming no item', () => {
    expect(
      parseApprovalForm(approvalForm(complete.filter(([name]) => name !== 'source_item_id'))),
    ).toEqual({ ok: false, refusal: 'no-item' });
  });

  it('refuses a blank attribution — it becomes the rendered attribution block (FR-008)', () => {
    const blank = complete.map(([name, value]) =>
      name === 'attribution' ? ([name, '   '] as const) : ([name, value] as const),
    );
    expect(parseApprovalForm(approvalForm(blank))).toEqual({
      ok: false,
      refusal: 'no-attribution',
    });
  });

  it('trims the attribution the way the API stores it', () => {
    const padded = complete.map(([name, value]) =>
      name === 'attribution' ? ([name, '  Quelle: X.  '] as const) : ([name, value] as const),
    );
    const result = parseApprovalForm(approvalForm(padded));
    expect(result.ok && result.submission.attribution).toBe('Quelle: X.');
  });
});
