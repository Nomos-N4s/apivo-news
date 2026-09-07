import { describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from '../reader/axes';
import { uiStrings } from '../reader/strings';
import { memberStrings } from './strings';

/** Every value in a catalogue, with the functions called on a sample. */
function allText(lang: (typeof READING_LANGUAGES)[number]): readonly string[] {
  return Object.values(memberStrings(lang)).map((value) =>
    typeof value === 'function' ? value('SAMPLE') : value,
  );
}

describe('memberStrings', () => {
  it('answers in the language asked', () => {
    expect(memberStrings('el').notWorkingTitle).toBe('Δεν λειτουργεί ακόμη');
    expect(memberStrings('de').notWorkingTitle).toBe('Funktioniert noch nicht');
  });

  it('carries every string in every alpha language (FR-015)', () => {
    for (const lang of READING_LANGUAGES) {
      for (const [key, value] of Object.entries(memberStrings(lang))) {
        const text = typeof value === 'function' ? value('SAMPLE') : value;
        expect(text, `${lang}.${key}`).not.toBe('');
      }
    }
  });

  it('names the front page button by its label rather than quoting it', () => {
    // The sentence tells a reader what that control currently says. Quoting
    // the words would go stale the day the label changes; taking them as an
    // argument cannot.
    for (const lang of READING_LANGUAGES) {
      const sentence = memberStrings(lang).frontPageButtonState('SAMPLE');
      expect(sentence).toContain('SAMPLE');
    }
  });

  it('quotes the label the front page actually renders', () => {
    // The front page renders `signIn` on the button and hangs `signInPending`
    // off it as the title. A sentence that says the button "reads" something
    // has to quote the first, or it describes a tooltip.
    for (const lang of READING_LANGUAGES) {
      const label = uiStrings(lang).signIn;
      expect(memberStrings(lang).frontPageButtonState(label)).toContain(label);
    }
  });

  it('promises no mechanism', () => {
    // Nothing on this page works. A URL, a mailto: or an http reference in
    // the catalogue would be the first sign that copy had started describing
    // something the product cannot do.
    for (const lang of READING_LANGUAGES) {
      for (const text of allText(lang)) {
        expect(text, `${lang}: "${text}"`).not.toMatch(/https?:|mailto:|\/\//);
      }
    }
  });

  it('names no vendor, entity or address of its own', () => {
    // ADR-0004: the shell is described by what it does, never by whose it
    // is. The product's OWN name is left to scripts/lint-brand-literals.sh,
    // which owns that blocklist — spelling it here would put the very
    // literal being forbidden into application code, and the lint refuses
    // this file when it does. The third party's name it does not know
    // about, so that one is checked here, assembled rather than written.
    const vendor = ['evr', 'eos'].join('');
    for (const lang of READING_LANGUAGES) {
      for (const text of allText(lang)) {
        expect(text.toLowerCase(), `${lang}: "${text}"`).not.toContain(vendor);
        expect(text, `${lang}: "${text}"`).not.toMatch(/@|\bGmbH\b|\bAB\b/);
      }
    }
  });

  it('does not carry an email label of its own', () => {
    // uiStrings.emailLabel already exists; a second label for one field is
    // two things to keep in step for no gain.
    for (const lang of READING_LANGUAGES) {
      const values = Object.keys(memberStrings(lang));
      expect(values).not.toContain('emailLabel');
    }
  });
});
