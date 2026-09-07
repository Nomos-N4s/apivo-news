import { describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from '../reader/axes';
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

  it('names the address the link went to, rather than assembling the sentence', () => {
    // Taken as an argument so a translation can put it where its own grammar
    // wants it, instead of the page gluing a name onto a fixed phrase.
    for (const lang of READING_LANGUAGES) {
      expect(memberStrings(lang).linkSent('SAMPLE')).toContain('SAMPLE');
    }
  });

  it('offers no password, in either language', () => {
    // The board draws no password field and the shell note rules one out in
    // as many words: no password takes that route. This is the assertion
    // that used to say no copy here may describe a mechanism at all. The
    // email path describes one now; the thing worth forbidding is the
    // mechanism this product has decided never to have.
    const password = /κωδικ|passwor|passwort|kennwort/i;
    // The word is allowed in a sentence that DENIES one, and nowhere else:
    // both the shell note and the email note earn their keep by saying
    // outright that no password takes this route. A label, a placeholder or
    // an instruction would carry no negation, which is how this tells the
    // two apart without forbidding the honest use.
    const denial = /χωρίς|κανένας|καμία|ποτέ|ohne|kein|nie/i;
    for (const lang of READING_LANGUAGES) {
      for (const text of allText(lang)) {
        if (password.test(text)) {
          expect(text, `${lang} uses the word without denying one: "${text}"`).toMatch(denial);
        }
      }
    }
  });

  it('tells a member which browser to finish in', () => {
    // Not a nicety. The exchange completes against a verifier this browser
    // holds, so a link opened on a phone after being asked for on a laptop
    // cannot finish, and a member told nothing would try exactly that.
    for (const lang of READING_LANGUAGES) {
      expect(memberStrings(lang).linkSentNote.trim()).not.toBe('');
    }
  });

  it('says what is still owed is the app path, not the whole page', () => {
    // Standing beside a form that works and saying neither path does would
    // be the same dishonesty this panel exists to prevent, pointing the
    // other way.
    for (const lang of READING_LANGUAGES) {
      const owed = memberStrings(lang).notWorkingBody;
      expect(owed.trim()).not.toBe('');
      expect(owed).not.toBe(memberStrings(lang).notWorkingTitle);
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
