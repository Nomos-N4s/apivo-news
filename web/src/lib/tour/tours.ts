/**
 * Guided tours — what they cover, and where each step lives.
 *
 * A tour is a sequence of steps that crosses PAGES. Nothing here is an
 * application: every navigation in this product is a full page load
 * (`editor/index.astro`: "the screen needs no client JavaScript"), so a
 * tour cannot be a single driver.js call that runs to completion. It is a
 * cursor persisted between loads, and this module is the map that cursor
 * moves over.
 *
 * Steps name their element by `data-tour`, never by a class. A class is a
 * styling decision and changes when the design does; a tour anchored to
 * one breaks silently at the next redesign, highlighting nothing while
 * still claiming to explain something. `data-tour` exists for no other
 * purpose, so it changes only when somebody means to move a step.
 *
 * Copy lives in `./strings`, per language, for the same reason every other
 * string in this product does: `el` and `de` are axes (FR-009), and a tour
 * hardcoded in English would be the one surface that ignores them.
 */

/** A tour a visitor can be shown. */
export type TourId = 'editor';

/** The copy keys a step may carry; `./strings` supplies one entry each. */
export type TourCopyKey =
  | 'signInIntro'
  | 'signInForm'
  | 'queueList'
  | 'queueRow'
  | 'reviewPane'
  | 'reviewOriginal'
  | 'reviewTranslation'
  | 'approveForm'
  | 'spendLedger'
  | 'sourcesNav'
  | 'sourcesAdd'
  | 'auditTrail';

export interface TourStep {
  /**
   * The page this step belongs on, as a path with `{lang}` standing in for
   * the reading language. A step is only ever shown on its own page.
   */
  readonly path: string;
  /**
   * The `data-tour` value of the element to highlight, or null for a step
   * with no anchor — driver.js centres those, which is what an opening or
   * closing step wants.
   */
  readonly anchor: string | null;
  readonly key: TourCopyKey;
}

export interface Tour {
  readonly id: TourId;
  readonly steps: readonly TourStep[];
}

/**
 * The editorial journey, end to end: sign in, read the queue, open an
 * item, compare original against translation, approve, then the two
 * screens that explain where items come from and what was decided.
 *
 * Ordered as the work is actually done rather than as the nav rail lists
 * it. Sources sits after an approval because "where do these come from"
 * is a question the queue provokes, not one anybody has before seeing it.
 */
const EDITOR_TOUR: Tour = {
  id: 'editor',
  steps: [
    { path: '/{lang}/editor/signin', anchor: null, key: 'signInIntro' },
    { path: '/{lang}/editor/signin', anchor: 'signin-form', key: 'signInForm' },
    { path: '/{lang}/editor', anchor: 'queue-list', key: 'queueList' },
    { path: '/{lang}/editor', anchor: 'queue-row', key: 'queueRow' },
    { path: '/{lang}/editor', anchor: 'review-pane', key: 'reviewPane' },
    { path: '/{lang}/editor', anchor: 'review-original', key: 'reviewOriginal' },
    { path: '/{lang}/editor', anchor: 'review-translation', key: 'reviewTranslation' },
    { path: '/{lang}/editor', anchor: 'approve-form', key: 'approveForm' },
    { path: '/{lang}/editor', anchor: 'spend-ledger', key: 'spendLedger' },
    { path: '/{lang}/editor', anchor: 'nav-sources', key: 'sourcesNav' },
    { path: '/{lang}/editor/sources', anchor: 'sources-add', key: 'sourcesAdd' },
    { path: '/{lang}/editor/audit', anchor: 'audit-trail', key: 'auditTrail' },
  ],
};

const TOURS: Readonly<Record<TourId, Tour>> = {
  editor: EDITOR_TOUR,
};

/** The tour with this id, or null. Storage is not to be trusted (see progress.ts). */
export function tourById(id: string): Tour | null {
  return Object.prototype.hasOwnProperty.call(TOURS, id) ? (TOURS[id as TourId] ?? null) : null;
}

/** A step's path with the reading language filled in. */
export function stepPath(step: TourStep, lang: string): string {
  return step.path.replace('{lang}', lang);
}
