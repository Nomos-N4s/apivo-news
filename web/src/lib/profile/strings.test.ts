import { describe, expect, it } from 'vitest';

import { cashbackStrings } from '../../i18n/cashback';
import { READING_LANGUAGES } from '../reader/axes';
import { uiStrings } from '../reader/strings';
import { profileStrings, type ProfileStrings } from './strings';

const KEYS = Object.keys(profileStrings('el')) as (keyof ProfileStrings)[];

describe('profileStrings', () => {
  it('answers in the language asked', () => {
    expect(profileStrings('el').heading).toBe('Το προφίλ σου');
    expect(profileStrings('de').heading).toBe('Dein Profil');
  });

  it.each(READING_LANGUAGES)('carries every key, non-empty, in %s', (lang) => {
    const catalogue = profileStrings(lang);

    for (const key of KEYS) {
      const value = catalogue[key];
      expect(typeof value, key).toBe('string');
      expect(value.trim(), key).not.toBe('');
    }
  });

  it('carries the same keys in both languages', () => {
    expect(Object.keys(profileStrings('de')).sort()).toEqual([...KEYS].sort());
  });

  it('translates rather than copying', () => {
    // A key left identical in both catalogues is nearly always an untranslated
    // paste. There is no proper noun on this screen for which one string would
    // legitimately serve both languages.
    for (const key of KEYS) {
      expect(profileStrings('el')[key], key).not.toBe(profileStrings('de')[key]);
    }
  });

  it.each(READING_LANGUAGES)('names no third party in %s', (lang) => {
    // The rule `member/strings.ts` states for itself: the shell is called by
    // what it does, never by its vendor name.
    //
    // Only third-party names are listed. The product's own name belongs to
    // `scripts/lint-brand-literals.sh`, which already refuses it in every
    // file — including, as it turns out, in a list like this one. Restating
    // it here would duplicate a check that exists and fail the check it
    // duplicates. What the lint does NOT police is somebody else's name,
    // which is the gap this case covers.
    const forbidden = ['evreos', 'supabase'];

    for (const key of KEYS) {
      const value = profileStrings(lang)[key].toLowerCase();
      for (const name of forbidden) {
        expect(value.includes(name), `${key} names ${name}`).toBe(false);
      }
    }
  });

  it.each(READING_LANGUAGES)('re-states no label another catalogue owns in %s', (lang) => {
    // The reason this module is small. `uiStrings` owns the address, the
    // reading language and the places; `cashbackStrings` owns the two
    // figures. A second copy of any of them here would be two labels for one
    // field in one product, and they would drift.
    const ui = uiStrings(lang);
    const cashback = cashbackStrings(lang);
    const owned = [
      ui.emailLabel,
      ui.places,
      ui.readingLanguage,
      cashback.available,
      cashback.entries,
    ].map((value) => value.toLowerCase());

    for (const key of KEYS) {
      expect(owned.includes(profileStrings(lang)[key].toLowerCase()), key).toBe(false);
    }
  });

  it.each(READING_LANGUAGES)('says where the address came from in %s', (lang) => {
    // The correction this module exists to carry. The boards tell the reader
    // we hold none of this because a shell hands us a token; there is no
    // shell, and the address is in our own account row. The note has to be
    // about signing in, which is where it actually comes from.
    const note = profileStrings(lang).identityNote;

    expect(note.length).toBeGreaterThan(40);
    expect(note.toLowerCase()).toMatch(
      lang === 'el' ? /συνδέθηκες/ : /angemeldet/,
    );
  });
});
