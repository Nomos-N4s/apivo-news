import { describe, expect, it } from 'vitest';

import { cashbackStrings } from '../../i18n/cashback';
import { READING_LANGUAGES } from '../reader/axes';
import { TERMS_TEXT_VERSION, termsStrings } from './terms';

/** Every sentence some cashback screen already renders, for a language. */
function shippedSentences(lang: (typeof READING_LANGUAGES)[number]): Set<string> {
  return new Set(
    Object.values(cashbackStrings(lang)).filter(
      (value): value is string => typeof value === 'string',
    ),
  );
}

/** Every clause paragraph and list item, flattened. */
function allProse(lang: (typeof READING_LANGUAGES)[number]): readonly string[] {
  return termsStrings(lang).clauses.flatMap((clause) => [...clause.body, ...clause.points]);
}

describe('termsStrings', () => {
  it('answers in the language asked', () => {
    expect(termsStrings('el').heading).toBe('Όροι για την επιστροφή χρημάτων');
    expect(termsStrings('de').heading).toBe('Bedingungen für das Cashback');
  });

  it('carries every string in every alpha language (FR-015)', () => {
    for (const lang of READING_LANGUAGES) {
      const t = termsStrings(lang);
      for (const own of [t.heading, t.intro, t.fixtureTitle, t.fixtureBody, t.driftTitle]) {
        expect(own.trim(), lang).not.toBe('');
      }
      expect(t.versionLine('0.0.0').trim(), lang).not.toBe('');
      expect(t.driftBody('1.0.0', '2.0.0').trim(), lang).not.toBe('');
      expect(t.clauses.length).toBeGreaterThan(0);
      for (const clause of t.clauses) {
        expect(clause.heading.trim(), `${lang} heading`).not.toBe('');
        expect(clause.body.length + clause.points.length, `${lang} ${clause.heading}`).toBeGreaterThan(0);
        for (const line of [...clause.body, ...clause.points]) {
          expect(line.trim(), `${lang} ${clause.heading}`).not.toBe('');
        }
      }
    }
  });

  it('has the same shape in every alpha language', () => {
    // A missing translation is a red test, not a short clause on a page.
    const shapes = READING_LANGUAGES.map((lang) =>
      termsStrings(lang).clauses.map((clause) => [clause.body.length, clause.points.length]),
    );
    for (const shape of shapes) {
      expect(shape).toEqual(shapes[0]);
    }
  });

  it('describes the product in the product’s own words, not its own', () => {
    /*
     * The rule this module exists to keep. Terms that describe the product
     * differently from the product are worse than no terms, and the only way
     * two texts cannot disagree is for there to be one text — so every clause
     * about how cashback behaves is a reference into the catalogue that
     * already renders that sentence on a screen.
     *
     * The exceptions are the statements no screen makes: that a credit can
     * be held out of every balance while somebody reviews it, that money
     * already paid is never clawed back, that a minimum exists, which
     * currency governs, what a balance IS, what leaving does, and how
     * amounts are held. They are counted rather than listed so that adding
     * an eighth has to be a deliberate edit here.
     */
    for (const lang of READING_LANGUAGES) {
      const shipped = shippedSentences(lang);
      const ownProse = allProse(lang).filter((line) => !shipped.has(line));
      expect(ownProse, `${lang} owns more prose than it should`).toHaveLength(7);
    }
  });

  it('holds no money', () => {
    // The withdrawal minimum belongs to a deployment, not to a translated
    // string (FR-071) — the clause says a minimum exists and where to read
    // it, and names no figure.
    const forbidden = /\d[.,]\d{2}|€|\bEUR\b/;
    for (const lang of READING_LANGUAGES) {
      const t = termsStrings(lang);
      for (const line of [t.heading, t.intro, ...allProse(lang)]) {
        expect(line, `${lang}: "${line}"`).not.toMatch(forbidden);
      }
      for (const clause of t.clauses) {
        expect(clause.heading, `${lang}: "${clause.heading}"`).not.toMatch(forbidden);
      }
    }
  });

  it('holds no legal entity, address or support address of its own', () => {
    // Every one of those is a brand value (ADR-0004). A catalogue carrying
    // one would be wrong for every deployment but one, and would fail
    // scripts/lint-brand-literals.sh on its first hit.
    const suspicious = /@|\bAB\b|\bGmbH\b|\bUG\b|\bLtd\b|Kungsgatan|Göteborg/;
    for (const lang of READING_LANGUAGES) {
      const t = termsStrings(lang);
      for (const line of [t.heading, t.intro, t.fixtureBody, ...allProse(lang)]) {
        expect(line, `${lang}: "${line}"`).not.toMatch(suspicious);
      }
    }
  });

  it('puts the version it is given inside the sentence', () => {
    for (const lang of READING_LANGUAGES) {
      expect(termsStrings(lang).versionLine('9.9.9')).toContain('9.9.9');
    }
  });

  it('names both versions when they disagree, so the mismatch is diagnosable', () => {
    // A member cannot act on this, but whoever deploys can — and only if the
    // page says which two numbers failed to meet.
    for (const lang of READING_LANGUAGES) {
      const said = termsStrings(lang).driftBody('0.1.0', '3.1.0');
      expect(said).toContain('0.1.0');
      expect(said).toContain('3.1.0');
    }
  });

  it('declares a version for the text itself', () => {
    // The brand carries a version and no text, so this is the only place the
    // words can say which version they are. The opt-in refuses to collect
    // consent when it and the deployment disagree.
    expect(TERMS_TEXT_VERSION.trim()).not.toBe('');
    expect(TERMS_TEXT_VERSION).toMatch(/^\d+\.\d+\.\d+$/);
  });
});
