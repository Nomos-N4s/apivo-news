import { describe, expect, it } from 'vitest';

import { profileStrings } from '../profile/strings';
import { READING_LANGUAGES } from '../reader/axes';
import { uiStrings } from '../reader/strings';
import { settingsStrings, type SettingsStrings } from './strings';

const KEYS = Object.keys(settingsStrings('el')) as (keyof SettingsStrings)[];

describe('settingsStrings', () => {
  it('answers in the language asked', () => {
    expect(settingsStrings('el').heading).toBe('Ρυθμίσεις');
    expect(settingsStrings('de').heading).toBe('Einstellungen');
  });

  it.each(READING_LANGUAGES)('carries every key, non-empty, in %s', (lang) => {
    const catalogue = settingsStrings(lang);

    for (const key of KEYS) {
      const value = catalogue[key];
      expect(typeof value, key).toBe('string');
      expect(value.trim(), key).not.toBe('');
    }
  });

  it('carries the same keys in both languages', () => {
    expect(Object.keys(settingsStrings('de')).sort()).toEqual([...KEYS].sort());
  });

  it('translates rather than copying', () => {
    for (const key of KEYS) {
      expect(settingsStrings('el')[key], key).not.toBe(settingsStrings('de')[key]);
    }
  });

  it.each(READING_LANGUAGES)('names no third party in %s', (lang) => {
    // The shell is called by what it does, never by its vendor name. The
    // product's own name is the brand-literal lint's business, not a case's.
    for (const key of KEYS) {
      const value = settingsStrings(lang)[key].toLowerCase();
      for (const name of ['evreos', 'supabase']) {
        expect(value.includes(name), `${key} names ${name}`).toBe(false);
      }
    }
  });

  it.each(READING_LANGUAGES)('re-states nothing another catalogue owns in %s', (lang) => {
    // Why this module is small. uiStrings owns the consent heading and the
    // two axis labels; profileStrings owns the payout standings. A second
    // copy of any of them here is two labels for one field, and they drift.
    const ui = uiStrings(lang);
    const profile = profileStrings(lang);
    const owned = [
      ui.consentHeading,
      ui.consentIntro,
      ui.readingLanguage,
      ui.places,
      ui.editPlaces,
      profile.payoutLabel,
      profile.payoutNotVerified,
      profile.payoutNone,
      profile.axesNote,
    ].map((value) => value.toLowerCase());

    for (const key of KEYS) {
      expect(owned.includes(settingsStrings(lang)[key].toLowerCase()), key).toBe(false);
    }
  });

  it.each(READING_LANGUAGES)('marks the sketch as a sketch, in the label, in %s', (lang) => {
    // The seam board's rule for its "Neither, yet" column: named as owed,
    // never mocked up as working. A reader who sees only the heading must
    // still not think a channel exists — so the word is in the TITLE, not
    // buried in the body where a skimmer would miss it.
    const t = settingsStrings(lang);

    expect(t.sketchTitle.toLowerCase()).toMatch(lang === 'el' ? /σκίτσο/ : /skizze/);
    expect(t.sketchNothingYet.length).toBeGreaterThan(30);
  });

  it.each(READING_LANGUAGES)('promises no notification mechanism in %s', (lang) => {
    // The copy describes a division of responsibility, never a feature a
    // member could switch on. Nothing here may read as an instruction to
    // enable something, because there is nothing to enable.
    const t = settingsStrings(lang);
    const forbidden =
      lang === 'el'
        ? [/ενεργοποίησ/i, /πάτησε/i, /επίλεξε παρακάτω/i]
        : [/aktivier/i, /schalte ein/i, /klicke hier/i];

    for (const pattern of forbidden) {
      expect(t.sketchDivision, String(pattern)).not.toMatch(pattern);
      expect(t.sketchTitle, String(pattern)).not.toMatch(pattern);
    }
  });
});
