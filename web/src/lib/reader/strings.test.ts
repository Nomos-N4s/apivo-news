import { describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from './axes';
import { uiStrings } from './strings';

describe('uiStrings', () => {
  it('answers in the language asked', () => {
    expect(uiStrings('el').places).toBe('Τόποι');
    expect(uiStrings('de').places).toBe('Orte');
  });

  it('covers every scope label in every alpha language (FR-015)', () => {
    for (const lang of READING_LANGUAGES) {
      const labels = uiStrings(lang).scopeLabels;
      expect(labels.city).not.toBe('');
      expect(labels.region).not.toBe('');
      expect(labels.country).not.toBe('');
    }
  });

  it('names the place inside the empty-state sentence (US1-AC3)', () => {
    expect(uiStrings('el').emptyPlaceBody('Θεσσαλονίκη')).toContain('Θεσσαλονίκη');
    expect(uiStrings('de').emptyPlaceBody('München')).toContain('München');
  });

  it('keeps the reassurance line honest: extract-and-link, named approval', () => {
    expect(uiStrings('el').reassurance).toContain('απόσπασμα');
    expect(uiStrings('de').reassurance).toContain('Auszug');
  });

  it('sends the reader to the publisher by name (SC-008)', () => {
    expect(uiStrings('el').continueAt('kathimerini.example')).toBe(
      'Συνέχεια στο kathimerini.example ↗',
    );
    expect(uiStrings('de').continueAt('tagblatt.example')).toBe(
      'Weiter bei tagblatt.example ↗',
    );
  });

  it('titles the related rail with the place', () => {
    expect(uiStrings('el').moreFrom('München')).toBe('Περισσότερα από München');
    expect(uiStrings('de').moreFrom('München')).toBe('Mehr aus München');
  });

  it('labels the chip actions with the place (FR-009 controls)', () => {
    expect(uiStrings('el').addPlace('Ελλάδα')).toContain('Ελλάδα');
    expect(uiStrings('el').removePlace('München')).toContain('München');
    expect(uiStrings('de').addPlace('Ελλάδα')).toContain('Ελλάδα');
    expect(uiStrings('de').removePlace('München')).toContain('München');
  });

  it('speaks the setup dialog in both languages', () => {
    expect(uiStrings('el').selectedCount(2)).toBe('Επιλεγμένοι: 2');
    expect(uiStrings('de').selectedCount(2)).toBe('Ausgewählt: 2');
    expect(uiStrings('el').covers('München')).toBe('καλύπτει München');
    expect(uiStrings('de').covers('München')).toBe('umfasst München');
    for (const lang of READING_LANGUAGES) {
      const t = uiStrings(lang);
      expect(t.setupTitle).not.toBe('');
      expect(t.independenceLine).not.toBe('');
      expect(t.alphaLanguagesNote).not.toBe('');
    }
  });
});
