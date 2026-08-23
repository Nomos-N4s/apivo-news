/**
 * Where a visitor is in a tour, and which step (if any) to show right now.
 *
 * Kept free of the DOM and of storage so it can be tested without either.
 * The component in `components/ProductTour.astro` supplies the current
 * path, reads and writes localStorage, and asks the document whether an
 * anchor exists; every decision about what that means is made here.
 *
 * ---------------------------------------------------------------------------
 * Two things this has to survive
 *
 * 1. STORAGE IS NOT TRUSTWORTHY. It is user-writable, it outlives the
 *    deployment that wrote it, and a tour that gained or lost steps since
 *    then leaves a cursor pointing at nothing. Every value read from it is
 *    treated as a suggestion: anything that is not a whole number inside
 *    the tour's current range restarts the tour rather than throwing, and
 *    a tour id that no longer exists is simply not a tour.
 *
 * 2. STEPS CAN BE ABSENT. Several anchors are conditional in the markup —
 *    the spend ledger renders only when the API returns one, and a queue
 *    row exists only when the queue is not empty. driver.js given a
 *    selector that matches nothing highlights the whole viewport and
 *    narrates a thing that is not on screen, which is worse than skipping.
 *    So a step whose anchor is missing is stepped over, and a tour whose
 *    remaining steps are all missing ends rather than stalling.
 */
import { type Tour, type TourStep } from './tours';

/**
 * Whether a step's path addresses the page currently open.
 *
 * Compared segment by segment rather than as strings, because the reader's
 * routes carry values: a front page is `/{lang}/{place}` and an article is
 * `/{lang}/{place}/a/{id}`. A step cannot name the place or the article it
 * will be shown on — it is about the SHAPE of the page, not one row of
 * data — so every `{…}` segment except `{lang}` matches whatever is there.
 *
 * `{lang}` is the exception because it is not free: the reading language
 * is an axis (FR-009) and a tour running in Greek must not resume on the
 * German copy of the same screen.
 */
export function matchesPath(step: TourStep, currentPath: string, lang: string): boolean {
  const want = step.path.split('/');
  const got = currentPath.split('/');
  if (want.length !== got.length) {
    return false;
  }
  return want.every((segment, i) => {
    const actual = got[i];
    if (actual === undefined) {
      return false;
    }
    if (segment === '{lang}') {
      return actual === lang;
    }
    if (segment.startsWith('{') && segment.endsWith('}')) {
      // A value segment: anything non-empty is this page.
      return actual !== '';
    }
    return segment === actual;
  });
}

/** What the controller should do on this page load. */
export type Resolution =
  /** Show this step index now. */
  | { readonly kind: 'show'; readonly step: number }
  /** The cursor is on another page. Do nothing; a later load will resume. */
  | { readonly kind: 'wait' }
  /** Nothing remains. The controller marks the tour finished. */
  | { readonly kind: 'done' };

/** The sentinel stored for a tour the visitor finished or dismissed. */
export const DONE = 'done';

/** localStorage key for a tour's cursor. Namespaced; this is a shared origin. */
export function storageKey(tourId: string): string {
  return `apivo.tour.${tourId}`;
}

/**
 * The cursor a stored value represents: a step index, or null for "this
 * tour is over". An absent, malformed, or out-of-range value means the
 * visitor has not started — restarting is the only harmless reading, and
 * it is what somebody who cleared their storage expects.
 */
export function readCursor(raw: string | null, tour: Tour): number | null {
  if (raw === DONE) {
    return null;
  }
  if (raw === null || raw === '') {
    return 0;
  }
  // Number() accepts '', ' ', '0x10' and '1e2'; none of those are a step.
  if (!/^\d+$/.test(raw)) {
    return 0;
  }
  const parsed = Number.parseInt(raw, 10);
  return parsed < tour.steps.length ? parsed : 0;
}

/**
 * Which step to show, given where the cursor sits and what this page has.
 *
 * `hasAnchor` is asked only about steps on the CURRENT page — a step
 * belonging elsewhere is never a question about this document.
 */
export function resolve(
  tour: Tour,
  cursor: number | null,
  currentPath: string,
  lang: string,
  hasAnchor: (anchor: string) => boolean,
): Resolution {
  if (cursor === null) {
    return { kind: 'done' };
  }
  let at = cursor < 0 ? 0 : cursor;
  while (at < tour.steps.length) {
    const step = tour.steps[at];
    if (step === undefined) {
      break;
    }
    if (!matchesPath(step, currentPath, lang)) {
      // The next thing to say belongs on a page the visitor is not on.
      // Parking is right: they may be on their way there, and dragging
      // them somewhere they did not ask to go is what makes product tours
      // hated.
      return { kind: 'wait' };
    }
    if (step.anchor !== null && !hasAnchor(step.anchor)) {
      at += 1;
      continue;
    }
    return { kind: 'show', step: at };
  }
  return { kind: 'done' };
}

/**
 * The cursor after finishing `step`. Past the last step is null — the
 * tour is over, and `DONE` is what gets stored.
 */
export function advance(tour: Tour, step: number): number | null {
  const next = step + 1;
  return next < tour.steps.length ? next : null;
}

/** What to write for a cursor. */
export function serialise(cursor: number | null): string {
  return cursor === null ? DONE : String(cursor);
}

/**
 * Normalises a pathname for comparison with a step's path: no trailing
 * slash, no query, no fragment. `/el/editor/` and `/el/editor?item=x` are
 * the same screen, and a tour that disagreed would resume on one and not
 * the other for reasons no reader could see.
 */
export function normalisePath(pathname: string): string {
  const withoutQuery = pathname.split('?')[0]?.split('#')[0] ?? '';
  if (withoutQuery.length > 1 && withoutQuery.endsWith('/')) {
    return withoutQuery.slice(0, -1);
  }
  return withoutQuery;
}

/** A step, with the index the cursor uses for it. */
export interface PlacedStep {
  readonly index: number;
  readonly step: TourStep;
}

/**
 * The run of steps driver.js should be given for THIS page load: starting
 * at `from`, every consecutive step that belongs to this page and whose
 * anchor is present, stopping at the first step that belongs elsewhere.
 *
 * It stops at the page boundary rather than filtering the whole tour,
 * because a later step on this same page may sit AFTER a step on another
 * page — the editorial tour returns to the queue is not a shape it has
 * today, but a tour that quietly reordered itself when it did would be a
 * bug nobody could see in the definition.
 */
export function pageSteps(
  tour: Tour,
  from: number,
  currentPath: string,
  lang: string,
  hasAnchor: (anchor: string) => boolean,
): readonly PlacedStep[] {
  const out: PlacedStep[] = [];
  for (let index = from < 0 ? 0 : from; index < tour.steps.length; index += 1) {
    const step = tour.steps[index];
    if (step === undefined || !matchesPath(step, currentPath, lang)) {
      break;
    }
    if (step.anchor !== null && !hasAnchor(step.anchor)) {
      continue;
    }
    out.push({ index, step });
  }
  return out;
}
