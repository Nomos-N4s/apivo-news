import { describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from '../reader/axes';
import { editorialStrings, languageName } from './strings';

describe('editorialStrings', () => {
  it('answers in the language asked', () => {
    expect(editorialStrings('el').editorial).toBe('Σύνταξη');
    expect(editorialStrings('de').editorial).toBe('Redaktion');
  });

  it('states the permanence of approval in both languages (FR-007)', () => {
    expect(editorialStrings('el').acknowledgement).toContain('δεν μπορεί να τροποποιηθεί');
    expect(editorialStrings('de').acknowledgement).toContain('nicht geändert werden');
  });

  it('says rejection creates no article and keeps the evidence', () => {
    expect(editorialStrings('el').rejectNote).toContain('δεν δημιουργεί άρθρο');
    expect(editorialStrings('de').rejectNote).toContain('keinen Artikel');
  });

  it('names the counts in the pipeline-hold copy (FR-006)', () => {
    expect(editorialStrings('el').queuedUntranslatedBody(3)).toContain('3');
    expect(editorialStrings('de').skippedOverCeiling(1)).toContain('1');
  });

  it('fills the ledger caption with the cap and month', () => {
    expect(editorialStrings('de').ofMonthlyCap('$25.00', '2026-08')).toContain('$25.00');
    expect(editorialStrings('el').ofMonthlyCap('$25.00', '2026-08')).toContain('2026-08');
  });

  it('covers every key in every alpha language (FR-015)', () => {
    for (const lang of READING_LANGUAGES) {
      const t = editorialStrings(lang);
      expect(t.reviewQueue).not.toBe('');
      expect(t.approveAndPublish).not.toBe('');
      expect(t.previewSession).not.toBe('');
      expect(t.notRecordedTitle).not.toBe('');
      expect(t.notSignedIn).not.toBe('');
      expect(t.signInTitle).not.toBe('');
      expect(t.signInFailed).not.toBe('');
      expect(t.roleReader).not.toBe('');
    }
  });

  it('says the sign-in names the person the approval will record (FR-007)', () => {
    expect(editorialStrings('el').signInIntro).toContain('όνομά σας');
    expect(editorialStrings('de').signInIntro).toContain('Namen');
  });
});

describe('languageName', () => {
  it('names content languages in the reading language', () => {
    expect(languageName('el', 'de')).toBe('Γερμανικά');
    expect(languageName('de', 'el')).toBe('Griechisch');
  });

  it('passes an unknown code through rather than inventing a name', () => {
    expect(languageName('el', 'fr')).toBe('fr');
    expect(languageName('de', '')).toBe('');
  });
});
