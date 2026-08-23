import { describe, expect, it } from 'vitest';

import { READING_LANGUAGES } from '../reader/axes';
import { tourStrings } from './strings';
import { tourById } from './tours';

// The Record<TourCopyKey, …> type already refuses a missing key at compile
// time. What it cannot see is a key present and empty, or a step added to
// a tour with no copy behind it, or German copy pasted from Greek and
// never translated — the three ways this drifts in practice.
describe('tour strings', () => {
  for (const lang of READING_LANGUAGES) {
    describe(lang, () => {
      const s = tourStrings(lang);

      it('has a title and a body for every step of every tour', () => {
        const editor = tourById('editor');
        expect(editor).not.toBeNull();
        for (const step of editor!.steps) {
          const copy = s.steps[step.key];
          expect(copy, `${lang}: ${step.key}`).toBeDefined();
          expect(copy.title.trim(), `${lang}: ${step.key} title`).not.toBe('');
          expect(copy.body.trim(), `${lang}: ${step.key} body`).not.toBe('');
        }
      });

      it('names its own buttons', () => {
        for (const label of [s.next, s.previous, s.done, s.start, s.restart, s.startLabel]) {
          expect(label.trim()).not.toBe('');
        }
      });
    });
  }

  // Every string differing between the two languages is the cheap test for
  // "somebody copied the block and moved on". It would fail on a genuine
  // shared loanword, which is a good moment to look rather than a false
  // alarm to suppress.
  it('is actually translated, not duplicated', () => {
    const el = tourStrings('el');
    const de = tourStrings('de');
    const editor = tourById('editor');
    for (const step of editor!.steps) {
      expect(el.steps[step.key].title, `${step.key} title is identical in el and de`).not.toBe(
        de.steps[step.key].title,
      );
      expect(el.steps[step.key].body, `${step.key} body is identical in el and de`).not.toBe(
        de.steps[step.key].body,
      );
    }
  });

  // A step's anchor is a `data-tour` value, and it reaches the DOM inside
  // an attribute selector. Keeping them to a strict shape means the
  // component never has to escape one, and a typo is caught here rather
  // than as a step that silently highlights nothing.
  it('uses anchors that are safe in an attribute selector', () => {
    const editor = tourById('editor');
    for (const step of editor!.steps) {
      if (step.anchor !== null) {
        expect(step.anchor, `${step.key} anchor`).toMatch(/^[a-z][a-z0-9-]*$/);
      }
    }
  });

  it('gives every step a path carrying the language placeholder', () => {
    const editor = tourById('editor');
    for (const step of editor!.steps) {
      expect(step.path, `${step.key} path`).toMatch(/^\/\{lang\}\//);
    }
  });
});
