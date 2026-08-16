import type { SourceRow } from './api';

/**
 * The sources screen's filter layer (#120).
 *
 * Filtering happens here, over the whole walked registry, not at the API:
 * `allSources` follows the cursor to exhaustion before the screen renders,
 * so narrowing the list in memory hides nothing the page had not already
 * fetched. That is what makes it honest — the same filter applied to one
 * page of a paginated read would answer questions about data it never
 * saw. When the walk hits its bound the screen keeps saying so, because
 * then the list really is partial and every count below is a count of
 * what was loaded.
 *
 * The module is pure and synchronous, like `sourceForm`: the page parses
 * the query string through it, renders what it returns, and holds no
 * filter logic of its own.
 */

/** The three ways the table can be narrowed by state. */
export type SourceView = 'all' | 'active' | 'inactive';

/**
 * Poll health as the API actually lets a screen know it.
 *
 * `never` is a fact on the row (`last_polled_at === null`). `failing` is
 * not: no per-source error crosses the wire, and the only failure signal
 * is `cycle.failures`, a list of names describing THE LAST CYCLE. So a
 * source counts as failing when the last cycle named it, and the label
 * says exactly that rather than implying a standing condition.
 *
 * `unpolled` exists because that list is built `where active`. A paused
 * feed is missing from it for the same reason it is missing from the
 * cycle: nothing polled it. Reading that absence as success would print
 * "no failure" over a row whose stored `last_poll_error` may say
 * otherwise — deactivation clears neither the error nor the timestamp —
 * so a paused feed gets its own value instead of a verdict the payload
 * cannot support.
 *
 * The per-source `last_poll_error` all this wants is filed as issue #122;
 * until it lands, the screen surfaces the fact of failure, never its text.
 */
export type PollHealth = 'any' | 'healthy' | 'failing' | 'never' | 'unpolled';

/** Everything the query string can narrow the table by. */
export interface SourceFilters {
  readonly view: SourceView;
  /** A `SourceRow.language` value, or '' for every language. */
  readonly language: string;
  /** A `SourceRow.jurisdiction` value, or '' for every jurisdiction. */
  readonly jurisdiction: string;
  readonly health: PollHealth;
  /** Case-insensitive substring of the name or the feed URL; '' for none. */
  readonly search: string;
}

/** The unnarrowed table: what `/{lang}/editor/sources` shows bare. */
export const NO_FILTERS: SourceFilters = {
  view: 'all',
  language: '',
  jurisdiction: '',
  health: 'any',
  search: '',
};

/** The query-string keys, one per dimension. */
export const FILTER_KEYS = {
  view: 'view',
  language: 'language',
  jurisdiction: 'jurisdiction',
  health: 'health',
  search: 'q',
} as const;

function readView(raw: string | null): SourceView {
  return raw === 'active' || raw === 'inactive' ? raw : 'all';
}

function readHealth(raw: string | null): PollHealth {
  return raw === 'healthy' || raw === 'failing' || raw === 'never' || raw === 'unpolled'
    ? raw
    : 'any';
}

/**
 * Reads the filter set out of a URL. Anything unrecognised falls back to
 * the unnarrowed value rather than erroring: a hand-edited or stale link
 * should show more of the registry, never a broken screen.
 */
export function parseSourceFilters(params: URLSearchParams): SourceFilters {
  return {
    view: readView(params.get(FILTER_KEYS.view)),
    language: (params.get(FILTER_KEYS.language) ?? '').trim(),
    jurisdiction: (params.get(FILTER_KEYS.jurisdiction) ?? '').trim(),
    health: readHealth(params.get(FILTER_KEYS.health)),
    search: (params.get(FILTER_KEYS.search) ?? '').trim(),
  };
}

/**
 * The filters as query parameters, with the unnarrowed values left out so
 * the bare URL stays bare and a shared link carries only what was chosen.
 */
export function filterQuery(filters: SourceFilters): Record<string, string> {
  const query: Record<string, string> = {};
  if (filters.view !== 'all') {
    query[FILTER_KEYS.view] = filters.view;
  }
  if (filters.language !== '') {
    query[FILTER_KEYS.language] = filters.language;
  }
  if (filters.jurisdiction !== '') {
    query[FILTER_KEYS.jurisdiction] = filters.jurisdiction;
  }
  if (filters.health !== 'any') {
    query[FILTER_KEYS.health] = filters.health;
  }
  if (filters.search !== '') {
    query[FILTER_KEYS.search] = filters.search;
  }
  return query;
}

/**
 * Whether the last cycle named this source among its failures.
 *
 * The list holds names and nothing else — the query builds it as
 * `array_agg(name) filter (where last_poll_error is not null)` over the
 * active sources — so this is exact equality against the same column the
 * row's own name came from. Substring matching would be worse than
 * useless here: a feed called "Isar" would inherit "Isar Kurier"'s
 * failure and be marked broken on no evidence.
 *
 * Entries that are not strings are ignored. The client's validator checks
 * only that `failures` is an array, and a malformed payload must not get
 * to decide a source's health.
 *
 * The limit this cannot fix: `source.name` carries no unique constraint —
 * only `url` does — so two feeds registered under one masthead share a
 * name, and one of them failing marks both. The payload offers no id to
 * match on, which is the second half of what issue #122 asks for.
 */
export function namedInFailures(name: string, failures: readonly unknown[]): boolean {
  const own = name.trim();
  if (own === '') {
    return false;
  }
  return failures.some((entry) => typeof entry === 'string' && entry.trim() === own);
}

/** A source's poll health, as far as the payload can tell. */
export function pollHealth(
  row: SourceRow,
  failures: readonly unknown[],
): Exclude<PollHealth, 'any'> {
  if (row.last_polled_at === null) {
    return 'never';
  }
  // The cycle covers active feeds only, so for a paused one its silence
  // is not a clean bill of health — it is no reading at all.
  if (!row.active) {
    return 'unpolled';
  }
  return namedInFailures(row.name, failures) ? 'failing' : 'healthy';
}

/** The rows a filter set admits, in the registry's own order. */
function matching(
  items: readonly SourceRow[],
  filters: SourceFilters,
  failures: readonly unknown[],
): SourceRow[] {
  return items.filter((row) => matches(row, filters, failures));
}

function matches(row: SourceRow, filters: SourceFilters, failures: readonly unknown[]): boolean {
  if (filters.view !== 'all' && row.active !== (filters.view === 'active')) {
    return false;
  }
  if (filters.language !== '' && row.language !== filters.language) {
    return false;
  }
  if (filters.jurisdiction !== '' && row.jurisdiction !== filters.jurisdiction) {
    return false;
  }
  if (filters.health !== 'any' && pollHealth(row, failures) !== filters.health) {
    return false;
  }
  if (filters.search !== '') {
    const needle = filters.search.toLowerCase();
    const haystack = `${row.name}\n${row.url}`.toLowerCase();
    if (!haystack.includes(needle)) {
      return false;
    }
  }
  return true;
}

/**
 * The rows to render: every filter composed, then ordered.
 *
 * Order is active first — a paused feed drops below the working set
 * rather than vanishing, which is the toggle's job when asked — and
 * within the active rows the ones the last cycle could not reach come
 * first, because a failing feed is the thing an editor opened this screen
 * to find. The sort is stable, so the registry's own order survives
 * inside each group.
 */
export function filterSources(
  items: readonly SourceRow[],
  filters: SourceFilters,
  failures: readonly unknown[] = [],
): SourceRow[] {
  // Ranked through pollHealth, not through the failures list directly, so
  // the order can never disagree with the label: a never-polled feed the
  // cycle also named reads as 'never' in the health column, and must not
  // be sorted as though it had failed.
  const rank = (row: SourceRow): number => {
    if (!row.active) {
      return 2;
    }
    return pollHealth(row, failures) === 'failing' ? 0 : 1;
  };
  return matching(items, filters, failures).sort((a, b) => rank(a) - rank(b));
}

/** One filter option: what it is worth choosing, and how many it would show. */
export interface FilterOption {
  readonly value: string;
  readonly count: number;
}

/**
 * The count beside each option — how many rows that option would show
 * with the OTHER filters still applied, so the numbers describe the list
 * the editor would actually get rather than the registry in the abstract.
 * A zero-count option is still offered: hiding it would leave the editor
 * wondering where a language went.
 */
export function filterOptions(
  items: readonly SourceRow[],
  filters: SourceFilters,
  failures: readonly unknown[],
  dimension: keyof SourceFilters,
  values: readonly string[],
): FilterOption[] {
  // Counting needs the matching rows, not their order — sorting each
  // candidate list would be wasted work repeated once per option, on
  // every render of every dimension.
  return values.map((value) => ({
    value,
    count: matching(items, { ...filters, [dimension]: value }, failures).length,
  }));
}

/** The languages and jurisdictions the registry actually contains, sorted. */
export function presentValues(
  items: readonly SourceRow[],
  field: 'language' | 'jurisdiction',
): string[] {
  return [...new Set(items.map((row) => row[field]).filter((value) => value !== ''))].sort();
}

/** One narrowed dimension, and the filter set with that one dimension cleared. */
export interface ActiveFilter {
  readonly key: keyof SourceFilters;
  readonly value: string;
  readonly without: SourceFilters;
}

/**
 * The filters currently narrowing the table, each paired with the set that
 * drops it — the chips render as links to those, so removing one filter
 * never disturbs the others.
 */
export function activeFilters(filters: SourceFilters): ActiveFilter[] {
  const chips: ActiveFilter[] = [];
  if (filters.view !== 'all') {
    chips.push({ key: 'view', value: filters.view, without: { ...filters, view: 'all' } });
  }
  if (filters.language !== '') {
    chips.push({
      key: 'language',
      value: filters.language,
      without: { ...filters, language: '' },
    });
  }
  if (filters.jurisdiction !== '') {
    chips.push({
      key: 'jurisdiction',
      value: filters.jurisdiction,
      without: { ...filters, jurisdiction: '' },
    });
  }
  if (filters.health !== 'any') {
    chips.push({ key: 'health', value: filters.health, without: { ...filters, health: 'any' } });
  }
  if (filters.search !== '') {
    chips.push({ key: 'search', value: filters.search, without: { ...filters, search: '' } });
  }
  return chips;
}
