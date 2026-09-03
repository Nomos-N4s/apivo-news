import { describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from '../lib/reader/axes';
import { cashbackStrings, languageName } from './cashback';

describe('the cashback catalogue', () => {
  it('answers for every mounted reading language', () => {
    for (const lang of READING_LANGUAGES) {
      expect(cashbackStrings(lang)).toBeDefined();
    }
  });

  it('carries the same keys in every language, to the same depth', () => {
    const shape = (value: unknown): unknown => {
      if (typeof value === 'function') {
        return 'fn';
      }
      if (typeof value !== 'object' || value === null) {
        return typeof value;
      }
      return Object.fromEntries(
        Object.entries(value as Record<string, unknown>)
          .map(([key, nested]) => [key, shape(nested)] as const)
          .sort(([a], [b]) => a.localeCompare(b)),
      );
    };
    const [first, ...rest] = READING_LANGUAGES.map((lang) => shape(cashbackStrings(lang)));
    for (const other of rest) {
      expect(other).toEqual(first);
    }
  });

  it('leaves no string empty — a missing translation must fail here, not render blank', () => {
    const walk = (value: unknown, path: string): void => {
      if (typeof value === 'string') {
        expect(value.trim(), `${path} is empty`).not.toBe('');
        return;
      }
      if (typeof value === 'object' && value !== null) {
        for (const [key, nested] of Object.entries(value as Record<string, unknown>)) {
          walk(nested, `${path}.${key}`);
        }
      }
    };
    for (const lang of READING_LANGUAGES) {
      walk(cashbackStrings(lang), lang);
    }
  });

  it('names no brand, domain or support address (Rebrandability)', () => {
    // The brand-literal lint covers the repository; this covers the one file
    // most likely to acquire one, because copy is where a product name gets
    // written without anybody deciding to hardcode it.
    const forbidden = /apivo|epiloyes|\.(com|de|gr|eu)\b|@|https?:/i;
    const walk = (value: unknown, path: string): void => {
      if (typeof value === 'string') {
        expect(value, `${path} carries a brand literal`).not.toMatch(forbidden);
        return;
      }
      if (typeof value === 'object' && value !== null) {
        for (const [key, nested] of Object.entries(value as Record<string, unknown>)) {
          walk(nested, `${path}.${key}`);
        }
      }
    };
    for (const lang of READING_LANGUAGES) {
      walk(cashbackStrings(lang), lang);
    }
  });

  it('names no currency — the wallet is denominated by the API, not by the copy', () => {
    const walk = (value: unknown): void => {
      if (typeof value === 'string') {
        expect(value).not.toMatch(/€|EUR\b/);
        return;
      }
      if (typeof value === 'object' && value !== null) {
        Object.values(value as Record<string, unknown>).forEach(walk);
      }
    };
    for (const lang of READING_LANGUAGES) {
      walk(cashbackStrings(lang));
    }
  });

  it('interpolates counts and amounts through the language it was asked for', () => {
    expect(cashbackStrings('de').entryCount(3, 12)).toBe('3 von 12 Buchungen');
    expect(cashbackStrings('de').entryCount(12, 12)).toBe('12 Buchungen');
    expect(cashbackStrings('el').entryCount(3, 12)).toContain('3');
    expect(cashbackStrings('de').belowThreshold('10,00 €')).toContain('10,00 €');
  });

  it('states the whole-entries rule, because the reserved figure surprises otherwise', () => {
    for (const lang of READING_LANGUAGES) {
      expect(cashbackStrings(lang).wholeEntriesRule.length).toBeGreaterThan(40);
    }
  });

  it('says every withdrawal waits for a named approval', () => {
    for (const lang of READING_LANGUAGES) {
      expect(cashbackStrings(lang).approvalStep2).toBeTruthy();
      expect(cashbackStrings(lang).withdrawalStates.awaiting_approval).toBeTruthy();
    }
  });
});

describe('every interpolating string', () => {
  it('is exercised in both languages, so a broken template fails here', () => {
    for (const lang of READING_LANGUAGES) {
      const t = cashbackStrings(lang);
      const built = [
        t.entryCount(1, 2),
        t.entryCount(2, 2),
        t.shownInLanguage('Deutsch'),
        t.belowThreshold('10,00 \u20ac'),
        t.shortfall('2,00 \u20ac'),
        t.requestRecorded('WD-1'),
        t.reservedForRequest('18,40 \u20ac'),
      ];
      for (const line of built) {
        expect(line.trim()).not.toBe('');
        expect(line).not.toContain('undefined');
        expect(line).not.toMatch(/\$\{/);
      }
    }
  });

  it('places the value it was given inside the sentence', () => {
    const t = cashbackStrings('de');
    expect(t.shownInLanguage('Griechisch')).toContain('Griechisch');
    expect(t.requestRecorded('WD-2026-08-24-0142')).toContain('WD-2026-08-24-0142');
    expect(t.shortfall('2,00 \u20ac')).toContain('2,00 \u20ac');
  });
});

describe('languageName', () => {
  it('renders a language endonym in the reading language', () => {
    expect(languageName('el', 'de')).toBe('Γερμανικά');
    expect(languageName('de', 'el')).toBe('Griechisch');
  });

  it('falls back to the tag rather than guessing at one it does not know', () => {
    expect(languageName('de', 'zz-not-a-tag-at-all')).toBe('zz-not-a-tag-at-all');
  });
});
