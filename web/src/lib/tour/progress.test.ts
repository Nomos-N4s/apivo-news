import { describe, expect, it } from 'vitest';

import {
  advance,
  matchesPath,
  normalisePath,
  pageSteps,
  readCursor,
  resolve,
  serialise,
  storageKey,
} from './progress';
import { tourById, type Tour } from './tours';

// A stand-in tour with the two shapes that matter: steps that cross a page
// boundary, and an anchor that may not be in the document.
const TOUR: Tour = {
  id: 'editor',
  steps: [
    { path: '/{lang}/editor/signin', anchor: null, key: 'signInIntro' },
    { path: '/{lang}/editor', anchor: 'queue-list', key: 'queueList' },
    { path: '/{lang}/editor', anchor: 'spend-ledger', key: 'spendLedger' },
    { path: '/{lang}/editor/sources', anchor: 'sources-add', key: 'sourcesAdd' },
  ],
};

const everything = () => true;
const nothing = () => false;

describe('readCursor', () => {
  it('starts an unvisited tour at the beginning', () => {
    expect(readCursor(null, TOUR)).toBe(0);
  });

  it('reads a stored step', () => {
    expect(readCursor('2', TOUR)).toBe(2);
  });

  it('reports a finished tour as over', () => {
    expect(readCursor('done', TOUR)).toBeNull();
  });

  // Storage outlives the deployment that wrote it. A tour that lost steps
  // since then leaves a cursor past the end, and the alternative to
  // restarting is reading tour.steps[7] of a 4-step tour.
  it('restarts rather than pointing past the end of a shortened tour', () => {
    expect(readCursor('9', TOUR)).toBe(0);
  });

  // localStorage is user-writable. None of these should reach step maths.
  it.each(['', ' ', 'abc', '-1', '1.5', '0x2', '1e2', 'NaN', 'Infinity'])(
    'treats %o as unstarted rather than trusting it',
    (raw) => {
      expect(readCursor(raw, TOUR)).toBe(0);
    },
  );
});

describe('resolve', () => {
  it('shows the first step on its own page', () => {
    expect(resolve(TOUR, 0, '/el/editor/signin', 'el', everything)).toEqual({
      kind: 'show',
      step: 0,
    });
  });

  // The cursor advancing past the last step of a page must not drag the
  // visitor anywhere. It waits for them to arrive.
  it('waits when the next step is on another page', () => {
    expect(resolve(TOUR, 1, '/el/editor/signin', 'el', everything)).toEqual({ kind: 'wait' });
  });

  it('resumes on the page the cursor points at', () => {
    expect(resolve(TOUR, 1, '/el/editor', 'el', everything)).toEqual({ kind: 'show', step: 1 });
  });

  it('is over when the cursor is null', () => {
    expect(resolve(TOUR, null, '/el/editor', 'el', everything)).toEqual({ kind: 'done' });
  });

  // The spend ledger renders only when the API returns one, and a queue
  // row only when the queue is not empty. driver.js pointed at a selector
  // matching nothing dims the whole viewport and narrates something that
  // is not there.
  it('steps over an anchor this page does not have', () => {
    const missingLedger = (a: string) => a !== 'spend-ledger';
    expect(resolve(TOUR, 2, '/el/editor', 'el', missingLedger)).toEqual({ kind: 'wait' });
  });

  it('skips a missing anchor to reach a later one on the same page', () => {
    const onlyLedger = (a: string) => a === 'spend-ledger';
    expect(resolve(TOUR, 1, '/el/editor', 'el', onlyLedger)).toEqual({ kind: 'show', step: 2 });
  });

  // A queue with no items and no ledger has neither anchor. The tour must
  // end rather than sit forever on a step it can never show.
  it('ends when every remaining step on the last page is missing', () => {
    expect(resolve(TOUR, 3, '/el/editor/sources', 'el', nothing)).toEqual({ kind: 'done' });
  });

  it('fills the language into each step path', () => {
    expect(resolve(TOUR, 0, '/de/editor/signin', 'de', everything)).toEqual({
      kind: 'show',
      step: 0,
    });
    expect(resolve(TOUR, 0, '/de/editor/signin', 'el', everything)).toEqual({ kind: 'wait' });
  });

  it('treats a negative cursor as the beginning', () => {
    expect(resolve(TOUR, -3, '/el/editor/signin', 'el', everything)).toEqual({
      kind: 'show',
      step: 0,
    });
  });
});

describe('advance', () => {
  it('moves to the next step', () => {
    expect(advance(TOUR, 0)).toBe(1);
  });

  it('is null after the last step', () => {
    expect(advance(TOUR, TOUR.steps.length - 1)).toBeNull();
  });

  it('round-trips through storage', () => {
    expect(readCursor(serialise(advance(TOUR, 0)), TOUR)).toBe(1);
    expect(readCursor(serialise(advance(TOUR, 3)), TOUR)).toBeNull();
  });
});

describe('normalisePath', () => {
  // Selecting a queue item is a link (?item=…), so the editor page is
  // visited both with and without a query. Both are the same screen.
  it('ignores a query string', () => {
    expect(normalisePath('/el/editor?item=abc')).toBe('/el/editor');
  });

  it('ignores a fragment', () => {
    expect(normalisePath('/el/editor#review')).toBe('/el/editor');
  });

  it('ignores a trailing slash', () => {
    expect(normalisePath('/el/editor/')).toBe('/el/editor');
  });

  it('leaves the root alone', () => {
    expect(normalisePath('/')).toBe('/');
  });
});

describe('storageKey', () => {
  it('namespaces the origin', () => {
    expect(storageKey('editor')).toBe('apivo.tour.editor');
  });
});

describe('tourById', () => {
  it('finds a tour', () => {
    expect(tourById('editor')?.id).toBe('editor');
  });

  // The id comes off a data attribute in the document.
  it('rejects anything else', () => {
    expect(tourById('nope')).toBeNull();
    expect(tourById('constructor')).toBeNull();
    expect(tourById('__proto__')).toBeNull();
  });
});

describe('pageSteps', () => {
  it('returns the run of steps on this page and stops at the boundary', () => {
    const run = pageSteps(TOUR, 1, '/el/editor', 'el', everything);
    expect(run.map((p) => p.index)).toEqual([1, 2]);
  });

  it('carries the cursor index, not the position within the run', () => {
    const run = pageSteps(TOUR, 2, '/el/editor', 'el', everything);
    expect(run).toHaveLength(1);
    expect(run[0]?.index).toBe(2);
  });

  it('leaves out a step whose anchor is missing', () => {
    const noLedger = (a: string) => a !== 'spend-ledger';
    expect(pageSteps(TOUR, 1, '/el/editor', 'el', noLedger).map((p) => p.index)).toEqual([1]);
  });

  it('is empty when the run starts on another page', () => {
    expect(pageSteps(TOUR, 1, '/el/editor/signin', 'el', everything)).toEqual([]);
  });

  // Every index handed to driver.js has to survive the round trip back to
  // storage, or a resumed tour restarts a page behind.
  it('produces indexes that advance() and readCursor() agree on', () => {
    const run = pageSteps(TOUR, 1, '/el/editor', 'el', everything);
    const last = run[run.length - 1];
    expect(last).toBeDefined();
    const next = advance(TOUR, last!.index);
    expect(readCursor(serialise(next), TOUR)).toBe(3);
  });
});

// The reader's routes carry values — a front page is /{lang}/{place} and an
// article /{lang}/{place}/a/{id}. A step is about the SHAPE of a page, not
// one row of data, so those segments match whatever is in them. The
// language does not: it is an axis, and a tour running in Greek must not
// resume on the German copy of the same screen.
describe('matchesPath', () => {
  const step = (path: string) => ({ path, anchor: null, key: 'signInIntro' as const });

  it('matches a literal route', () => {
    expect(matchesPath(step('/{lang}/editor'), '/el/editor', 'el')).toBe(true);
  });

  it('matches any value in a non-language segment', () => {
    expect(matchesPath(step('/{lang}/{place}'), '/el/athina', 'el')).toBe(true);
    expect(matchesPath(step('/{lang}/{place}'), '/el/munich+bayern', 'el')).toBe(true);
  });

  it('matches a value segment deeper in the path', () => {
    expect(matchesPath(step('/{lang}/{place}/a/{id}'), '/el/athina/a/9f2c', 'el')).toBe(true);
  });

  it('refuses another language on the same screen', () => {
    expect(matchesPath(step('/{lang}/{place}'), '/de/munich', 'el')).toBe(false);
  });

  it('refuses a different depth', () => {
    expect(matchesPath(step('/{lang}/{place}'), '/el/athina/a/9f2c', 'el')).toBe(false);
    expect(matchesPath(step('/{lang}/{place}/a/{id}'), '/el/athina', 'el')).toBe(false);
  });

  it('refuses an empty value segment', () => {
    expect(matchesPath(step('/{lang}/{place}'), '/el/', 'el')).toBe(false);
  });

  it('refuses a literal segment that differs', () => {
    expect(matchesPath(step('/{lang}/editor/sources'), '/el/editor/audit', 'el')).toBe(false);
  });

  // The whole point of generalising: before this, a step on a reader route
  // compared '/{lang}/{place}' to '/el/athina' as strings and never matched,
  // so every reader step resolved as "wait" forever — the same shape as the
  // bug that stopped the editorial tour starting.
  it('resolves a step on a value-carrying route', () => {
    const readerTour = {
      id: 'editor' as const,
      steps: [step('/{lang}/{place}')],
    };
    expect(resolve(readerTour, 0, '/el/athina', 'el', everything)).toEqual({ kind: 'show', step: 0 });
  });
});
