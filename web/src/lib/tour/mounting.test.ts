import { readFileSync } from 'node:fs';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { stepPath, tourById, type Tour } from './tours';

/**
 * The tour, checked against the pages it claims to describe.
 *
 * progress.test.ts proves the cursor's rules and can prove nothing about
 * whether the controller is on the page, or whether the elements the steps
 * name exist. Both of those were wrong when this file was written, and
 * neither showed up as a failing assertion:
 *
 *   - ProductTour was mounted only through EditorChrome, and the sign-in
 *     screen is the one editorial page with no chrome. The tour's first two
 *     steps address that route, so every other page resolved them as "wait
 *     for a page you are not on" and the tour could never start. Every unit
 *     test passed, because the model was right and only the wiring was not.
 *
 *   - An anchor a page does not carry is a step that is silently skipped.
 *     `resolve` steps over it by design — that is what keeps a tour honest
 *     when the spend ledger is absent — which also means a TYPO in an
 *     anchor name degrades into a step nobody ever sees, with nothing
 *     anywhere reporting it.
 *
 * So this reads the actual sources. It is a coarser instrument than a unit
 * test and it is the only one that can see either bug.
 */

const SRC = fileURLToPath(new URL('../..', import.meta.url));

function read(path: string): string {
  return readFileSync(`${SRC}${path}`, 'utf8');
}

/**
 * The page file a step's path resolves to. `/{lang}/editor` is a directory
 * route, so it is `editor/index.astro`; the rest are files.
 */
function pageFor(path: string): string {
  const route = stepPath({ path, anchor: null, key: 'signInIntro' }, '[lang]');
  const direct = `pages${route}.astro`;
  return existsSync(`${SRC}${direct}`) ? direct : `pages${route}/index.astro`;
}

/** Every source that could carry an anchor or mount the controller. */
const COMPONENTS = ['components/EditorChrome.astro', 'components/ProductTour.astro'];

function allTours(): Tour[] {
  const editor = tourById('editor');
  expect(editor).not.toBeNull();
  return [editor!];
}

describe('every page a tour visits can actually run it', () => {
  for (const tour of allTours()) {
    const paths = [...new Set(tour.steps.map((step) => step.path))];
    for (const path of paths) {
      it(`${tour.id}: ${path} mounts the tour controller`, () => {
        const file = pageFor(path);
        const source = read(file);
        // Directly, or through the chrome that mounts it for the pages
        // that share one. Either is fine; neither is not.
        const mounts =
          source.includes('ProductTour') ||
          (source.includes('EditorChrome') && read('components/EditorChrome.astro').includes('ProductTour'));
        expect(mounts, `${file} has tour steps but never mounts ProductTour`).toBe(true);
      });
    }
  }
});

describe('every anchor a tour names exists', () => {
  // Read once: a step's anchor may live on a shared component rather than
  // on the page whose route the step names — the spend ledger and the
  // Sources rail entry are both in EditorChrome.
  const haystack = () => {
    const files = new Set<string>(COMPONENTS);
    for (const tour of allTours()) {
      for (const step of tour.steps) {
        files.add(pageFor(step.path));
      }
    }
    return [...files].map(read).join('\n');
  };

  for (const tour of allTours()) {
    for (const step of tour.steps) {
      if (step.anchor === null) {
        continue;
      }
      it(`${tour.id}: something carries data-tour ${step.anchor}`, () => {
        expect(
          haystack().includes(`'${step.anchor}'`) || haystack().includes(`"${step.anchor}"`),
          `no source sets data-tour to ${step.anchor}; the step would be skipped in silence`,
        ).toBe(true);
      });
    }
  }
});

// The review pane exists whether or not a row is selected; its CONTENTS do
// not. An unconditional anchor there made the tour narrate "opening a row
// shows it here for review" over an empty state, then skip the original,
// translation and approval steps whose anchors live inside the selected
// branch. The review half of the tour vanished for anyone who pressed Next
// instead of picking a row first.
//
// A source assertion because the alternative is rendering the whole queue
// page with a session, an API and a selected item — and the thing being
// pinned is one attribute's conditionality.
it('the review-pane anchor is conditional, like the review it points at', () => {
  const source = read('pages/[lang]/editor/index.astro');
  expect(
    source.includes('data-tour="review-pane"'),
    'data-tour="review-pane" is unconditional: with no row selected the tour narrates an empty pane and skips the review steps',
  ).toBe(false);
  expect(source).toContain("'review-pane'");
});
