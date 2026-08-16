import { describe, expect, it, vi } from 'vitest';

import { PLACE_CATALOG } from '../reader/axes';
import { APPROVAL_PLACES, attributionDefault, parseApprovalForm } from './approvalForm';
import { editorialStrings } from './strings';

function approvalForm(entries: readonly (readonly [string, string])[]): FormData {
  const form = new FormData();
  for (const [name, value] of entries) {
    form.append(name, value);
  }
  return form;
}

describe('APPROVAL_PLACES', () => {
  it('offers exactly the places a reader can follow', () => {
    // Same filter as register.astro: hierarchy-only entries (bavaria,
    // germany) would satisfy the API and the database, yet an article
    // tagged only to one is unreachable through the follow flow — the
    // unreachability FR-009 exists to end.
    expect(APPROVAL_PLACES).toEqual(PLACE_CATALOG.filter((place) => place.selectable));
    expect(APPROVAL_PLACES.map((place) => place.slug)).toEqual(['munich', 'greece']);
  });
});

describe('attributionDefault', () => {
  const strings = {
    originallyPublishedBy: 'Originally published by',
    publicationDateNotSupplied: 'publication date not supplied by the feed',
  };
  // A queue row carrying both dates: the default must use the declared
  // publication date, and the retrieval date must never appear.
  const item = {
    source_name: 'Münchner Tagblatt',
    retrieved_at: '2026-08-14T06:12:04Z',
    original_published_at: '2026-08-13T05:58:00Z',
  };
  const formatDate = (iso: string): string => iso.slice(0, 10);

  it('composes from the publication date the feed declared', () => {
    expect(attributionDefault(item, strings, formatDate)).toBe(
      'Originally published by Münchner Tagblatt, 2026-08-13.',
    );
  });

  it('never silently substitutes the retrieval date', () => {
    // The attribution is frozen at approval (article_guard), so a
    // substituted retrieval date would become the publication date
    // permanently. The formatter must not even be called: a fallback
    // date does not exist to format.
    const format = vi.fn(formatDate);
    const line = attributionDefault(
      { ...item, original_published_at: null },
      strings,
      format,
    );
    expect(line).toBe(
      'Originally published by Münchner Tagblatt (publication date not supplied by the feed).',
    );
    expect(line).not.toContain('2026-08-14');
    expect(format).not.toHaveBeenCalled();
  });

  it('treats an absent field like a null one — older APIs omit it', () => {
    const { original_published_at, ...withoutDate } = item;
    void original_published_at;
    const line = attributionDefault(withoutDate, strings, formatDate);
    expect(line).toContain(strings.publicationDateNotSupplied);
    expect(line).not.toContain('2026-08-14');
  });

  it('says the gap in both chrome languages', () => {
    for (const lang of ['el', 'de'] as const) {
      const t = editorialStrings(lang);
      expect(t.publicationDateNotSupplied).not.toBe('');
      const line = attributionDefault(
        { ...item, original_published_at: null },
        {
          originallyPublishedBy: 'Πηγή:',
          publicationDateNotSupplied: t.publicationDateNotSupplied,
        },
        formatDate,
      );
      expect(line).toContain(t.publicationDateNotSupplied);
    }
  });
});

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
    expect(result).toEqual({
      ok: false,
      refusal: 'no-place',
      stale: {
        sourceItemId: 'src-1',
        attribution: 'Originally published by X.',
        places: [],
      },
    });
  });

  it('treats blank place values as no place', () => {
    const withBlank = complete
      .filter(([name]) => name !== 'place')
      .concat([['place', '   ']]);
    const result = parseApprovalForm(approvalForm(withBlank));
    expect(result.ok).toBe(false);
    expect(!result.ok && result.refusal).toBe('no-place');
  });

  it('a refusal keeps what was typed and picked, so the re-render can put it back', () => {
    // The no-place refusal must not also cost the typed attribution —
    // the one thing on the form typed rather than selected — and the
    // no-attribution refusal must not cost the checked places.
    const noPlace = parseApprovalForm(
      approvalForm(complete.filter(([name]) => name !== 'place')),
    );
    expect(!noPlace.ok && noPlace.stale).toEqual({
      sourceItemId: 'src-1',
      attribution: 'Originally published by X.',
      places: [],
    });

    const noAttribution = parseApprovalForm(
      approvalForm(
        complete.map(([name, value]) =>
          name === 'attribution' ? ([name, '   '] as const) : ([name, value] as const),
        ),
      ),
    );
    expect(!noAttribution.ok && noAttribution.stale).toEqual({
      sourceItemId: 'src-1',
      attribution: null,
      places: ['munich', 'greece'],
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
    const result = parseApprovalForm(
      approvalForm(complete.filter(([name]) => name !== 'source_item_id')),
    );
    expect(result.ok).toBe(false);
    expect(!result.ok && result.refusal).toBe('no-item');
    // Nothing to re-select: the stale item is null, not ''.
    expect(!result.ok && result.stale.sourceItemId).toBe(null);
  });

  it('refuses a blank attribution — it becomes the rendered attribution block (FR-008)', () => {
    const blank = complete.map(([name, value]) =>
      name === 'attribution' ? ([name, '   '] as const) : ([name, value] as const),
    );
    const result = parseApprovalForm(approvalForm(blank));
    expect(result.ok).toBe(false);
    expect(!result.ok && result.refusal).toBe('no-attribution');
  });

  it('trims the attribution the way the API stores it', () => {
    const padded = complete.map(([name, value]) =>
      name === 'attribution' ? ([name, '  Quelle: X.  '] as const) : ([name, value] as const),
    );
    const result = parseApprovalForm(approvalForm(padded));
    expect(result.ok && result.submission.attribution).toBe('Quelle: X.');
  });
});
