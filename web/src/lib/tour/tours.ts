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
export type TourId = 'editor' | 'reader';

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
  | 'auditTrail'
  // The reader's first visit.
  | 'setupLanguage'
  | 'setupPlaces'
  | 'setupGo'
  | 'frontLead'
  | 'frontAttribution'
  | 'articleBody'
  | 'articleProvenance'
  | 'registerConsent';

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

/**
 * The reader's first visit: the two axes, the front page they build, and
 * what a story on it is actually made of.
 *
 * Its later steps sit on ROUTES THAT CARRY VALUES — `/{lang}/{place}` and
 * `/{lang}/{place}/a/{id}`. A step is about the shape of a screen, not one
 * row of data, so those segments match whatever is in them (matchesPath in
 * ./progress). `{lang}` is the exception: the reading language is an axis,
 * and a tour begun in Greek does not resume on the German page.
 *
 * Nobody signing in is required, and nothing here records against an
 * account, because a reader has none. Progress stays in the browser for
 * this tour, which is what the fallback in ProductTour is for.
 */
const READER_TOUR: Tour = {
  id: 'reader',
  steps: [
    { path: '/{lang}/setup', anchor: 'setup-language', key: 'setupLanguage' },
    { path: '/{lang}/setup', anchor: 'setup-places', key: 'setupPlaces' },
    { path: '/{lang}/setup', anchor: 'setup-go', key: 'setupGo' },
    { path: '/{lang}/{place}', anchor: 'front-lead', key: 'frontLead' },
    { path: '/{lang}/{place}', anchor: 'front-attribution', key: 'frontAttribution' },
    { path: '/{lang}/{place}/a/{id}', anchor: 'article-body', key: 'articleBody' },
    { path: '/{lang}/{place}/a/{id}', anchor: 'article-provenance', key: 'articleProvenance' },
    { path: '/{lang}/register', anchor: 'register-consent', key: 'registerConsent' },
  ],
};

const TOURS: Readonly<Record<TourId, Tour>> = {
  editor: EDITOR_TOUR,
  reader: READER_TOUR,
};

/** The tour with this id, or null. Storage is not to be trusted (see progress.ts). */
export function tourById(id: string): Tour | null {
  return Object.prototype.hasOwnProperty.call(TOURS, id) ? (TOURS[id as TourId] ?? null) : null;
}
