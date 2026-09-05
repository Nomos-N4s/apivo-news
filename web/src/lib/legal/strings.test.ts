import { describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from '../reader/axes';
import { legalStrings } from './strings';

describe('legalStrings', () => {
  it('answers in the language asked', () => {
    expect(legalStrings('el').privacyHeading).toBe('Απόρρητο');
    expect(legalStrings('de').privacyHeading).toBe('Datenschutz');
  });

  it('carries every string in every alpha language (FR-015)', () => {
    for (const lang of READING_LANGUAGES) {
      const t = legalStrings(lang);
      for (const [key, value] of Object.entries(t)) {
        if (typeof value === 'function') {
          expect(value('1.0.0'), `${lang}.${key}`).not.toBe('');
          continue;
        }
        expect(value, `${lang}.${key}`).not.toBe('');
      }
    }
  });

  it('leaves the statute citation untranslated, because it names German law', () => {
    // A Greek rendering of "TMG §5" cites no statute. The label around it
    // is ours and is translated; the citation is not.
    for (const lang of READING_LANGUAGES) {
      expect(legalStrings(lang).imprintStatute).toBe('Impressum · TMG §5');
    }
  });

  it('names the document version rather than printing it bare', () => {
    expect(legalStrings('el').privacyVersion('2026-05-04')).toContain('2026-05-04');
    expect(legalStrings('de').privacyVersion('2026-05-04')).toContain('2026-05-04');
  });

  it('states how a data right is exercised while self-service does not exist', () => {
    // The pilot has no export and no deletion button. GDPR sets a
    // one-month deadline for the answer; it does not require a button.
    // The page says which of those is true rather than drawing a control
    // that does nothing.
    expect(legalStrings('el').rightsHowBody).toContain('GDPR');
    expect(legalStrings('de').rightsHowBody).toContain('DSGVO');
  });

  it('holds no legal entity, address or support address of its own', () => {
    // Every one of those is a brand value (ADR-0004). A catalogue that
    // carried one would be wrong for every deployment but one, and would
    // fail scripts/lint-brand-literals.sh on its first hit.
    const suspicious = /@|\bAB\b|\bGmbH\b|\bUG\b|\bLtd\b|Kungsgatan|Göteborg/;
    for (const lang of READING_LANGUAGES) {
      for (const [key, value] of Object.entries(legalStrings(lang))) {
        if (typeof value === 'function') continue;
        expect(value, `${lang}.${key}`).not.toMatch(suspicious);
      }
    }
  });
});
