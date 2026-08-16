import { describe, expect, it } from 'vitest';

import type { SourceRow } from './api';
import {
  activeFilters,
  filterOptions,
  filterQuery,
  filterSources,
  namedInFailures,
  NO_FILTERS,
  parseSourceFilters,
  pollHealth,
  presentValues,
  type SourceFilters,
} from './sourceFilters';

const row = (overrides: Partial<SourceRow> & { id: string }): SourceRow => ({
  name: `Feed ${overrides.id}`,
  url: `https://${overrides.id}.example/rss`,
  language: 'de',
  jurisdiction: 'DE',
  licence_terms: 'extract and link',
  usage_rule: 'extract_and_link',
  permission_evidence: null,
  active: true,
  last_polled_at: '2026-08-16T06:00:00Z',
  ...overrides,
});

const REGISTRY: readonly SourceRow[] = [
  row({ id: 'a', name: 'Münchner Tagblatt' }),
  row({ id: 'b', name: 'Isar Kurier', active: false }),
  row({ id: 'c', name: 'Πρωινός Τύπος', language: 'el', jurisdiction: 'GR' }),
  row({ id: 'd', name: 'Αιγαίο Νέα', language: 'el', jurisdiction: 'GR', last_polled_at: null }),
];
// What the API actually sends: names, straight from the same column the
// rows carry — `array_agg(name) filter (where last_poll_error is not null)`.
const FAILURES = ['Πρωινός Τύπος'];

const filters = (overrides: Partial<SourceFilters> = {}): SourceFilters => ({
  ...NO_FILTERS,
  ...overrides,
});

describe('parseSourceFilters', () => {
  it('reads every dimension out of the query string', () => {
    const parsed = parseSourceFilters(
      new URLSearchParams('view=inactive&language=el&jurisdiction=GR&health=failing&q=kurier'),
    );
    expect(parsed).toEqual({
      view: 'inactive',
      language: 'el',
      jurisdiction: 'GR',
      health: 'failing',
      search: 'kurier',
    });
  });

  it('falls back to showing more, never to an error', () => {
    const parsed = parseSourceFilters(new URLSearchParams('view=sideways&health=perfect'));
    expect(parsed.view).toBe('all');
    expect(parsed.health).toBe('any');
    expect(parseSourceFilters(new URLSearchParams())).toEqual(NO_FILTERS);
  });

  it('trims the typed values so a stray space is not a filter', () => {
    const parsed = parseSourceFilters(new URLSearchParams('q=%20%20&language=%20el%20'));
    expect(parsed.search).toBe('');
    expect(parsed.language).toBe('el');
  });
});

describe('filterQuery', () => {
  it('leaves the unnarrowed values out, so a bare list has a bare URL', () => {
    expect(filterQuery(NO_FILTERS)).toEqual({});
  });

  it('carries exactly what was chosen', () => {
    const chosen = filters({ view: 'active', health: 'never', search: 'isar' });
    expect(filterQuery(chosen)).toEqual({ view: 'active', health: 'never', q: 'isar' });
  });

  it('round-trips through the query string', () => {
    const chosen = filters({ language: 'el', jurisdiction: 'GR', search: 'νέα' });
    const params = new URLSearchParams(filterQuery(chosen));
    expect(parseSourceFilters(params)).toEqual(chosen);
  });
});

describe('namedInFailures', () => {
  it('matches the name the cycle reported', () => {
    expect(namedInFailures('Πρωινός Τύπος', FAILURES)).toBe(true);
    expect(namedInFailures('Isar Kurier', FAILURES)).toBe(false);
  });

  it('does not let one feed inherit another feed’s failure', () => {
    // The list holds names, so "Isar" is not "Isar Kurier" — matching on
    // substrings would mark a working feed broken on no evidence.
    expect(namedInFailures('Isar', ['Isar Kurier'])).toBe(false);
    expect(namedInFailures('Isar Kurier', ['Isar Kurier'])).toBe(true);
  });

  it('ignores entries that are not strings, and an empty name', () => {
    expect(namedInFailures('Feed', [null, 42, { name: 'Feed' }])).toBe(false);
    expect(namedInFailures('   ', FAILURES)).toBe(false);
  });
});

describe('pollHealth', () => {
  it('tells never-polled from failing from healthy', () => {
    expect(pollHealth(row({ id: 'x', last_polled_at: null }), FAILURES)).toBe('never');
    expect(pollHealth(row({ id: 'y', name: 'Πρωινός Τύπος' }), FAILURES)).toBe('failing');
    expect(pollHealth(row({ id: 'z' }), FAILURES)).toBe('healthy');
  });

  it('calls a paused feed unpolled, never healthy', () => {
    // The cycle is built `where active`, so a paused feed is missing from
    // the failure list because nothing polled it — not because it
    // succeeded. Its stored last_poll_error may say the opposite.
    const paused = row({ id: 'p', active: false });
    expect(pollHealth(paused, FAILURES)).toBe('unpolled');
    expect(pollHealth(paused, [])).not.toBe('healthy');
    // Never-polled still wins: the row's own field is the certain one.
    expect(pollHealth(row({ id: 'q', active: false, last_polled_at: null }), [])).toBe('never');
  });

  it('reads a never-polled feed as never, even when the cycle named it', () => {
    // Two different facts; the row's own field is the certain one.
    const never = row({ id: 'n', name: 'Πρωινός Τύπος', last_polled_at: null });
    expect(pollHealth(never, FAILURES)).toBe('never');
  });
});

describe('filterSources', () => {
  it('shows the whole registry when nothing is chosen', () => {
    expect(filterSources(REGISTRY, NO_FILTERS, FAILURES).map((source) => source.id)).toEqual([
      'c',
      'a',
      'd',
      'b',
    ]);
  });

  it('puts the feeds the last cycle could not reach at the top, paused ones last', () => {
    const shown = filterSources(REGISTRY, NO_FILTERS, FAILURES);
    expect(shown.at(0)?.name).toBe('Πρωινός Τύπος');
    expect(shown.at(-1)?.name).toBe('Isar Kurier');
  });

  it('orders by the same health the column shows, never by the raw list', () => {
    // A payload naming a feed that has never polled: the row's own field
    // is the stronger fact, so it reads 'never' — and must not be sorted
    // to the top as though the last cycle had failed on it.
    const never = row({ id: 'n', name: 'Ghost Feed', last_polled_at: null });
    const healthy = row({ id: 'h', name: 'Working Feed' });
    // Healthy first in the input: ranking off the raw failures list would
    // promote the never-polled row above it.
    const shown = filterSources([healthy, never], NO_FILTERS, ['Ghost Feed']);
    expect(pollHealth(never, ['Ghost Feed'])).toBe('never');
    expect(shown.map((source) => source.id)).toEqual(['h', 'n']);
  });

  it('composes state, language, jurisdiction, health and search', () => {
    expect(
      filterSources(REGISTRY, filters({ language: 'el' }), FAILURES).map((s) => s.id),
    ).toEqual(['c', 'd']);
    expect(
      filterSources(REGISTRY, filters({ view: 'inactive' }), FAILURES).map((s) => s.id),
    ).toEqual(['b']);
    expect(
      filterSources(REGISTRY, filters({ health: 'never' }), FAILURES).map((s) => s.id),
    ).toEqual(['d']);
    expect(
      filterSources(REGISTRY, filters({ language: 'el', health: 'failing' }), FAILURES).map(
        (s) => s.id,
      ),
    ).toEqual(['c']);
  });

  it('searches the name and the feed URL, case aside', () => {
    expect(filterSources(REGISTRY, filters({ search: 'KURIER' }), FAILURES).map((s) => s.id)).toEqual(
      ['b'],
    );
    expect(filterSources(REGISTRY, filters({ search: 'c.example' }), FAILURES).map((s) => s.id)).toEqual(
      ['c'],
    );
  });

  it('answers an impossible combination with nothing, not with everything', () => {
    expect(filterSources(REGISTRY, filters({ language: 'el', jurisdiction: 'DE' }), FAILURES)).toEqual(
      [],
    );
  });

  it('treats an absent failure list as no failures rather than guessing', () => {
    expect(filterSources(REGISTRY, filters({ health: 'failing' })).map((s) => s.id)).toEqual([]);
    // 'b' is paused: with no cycle covering it, it is unpolled rather than
    // healthy, however empty the failure list is.
    expect(filterSources(REGISTRY, filters({ health: 'healthy' })).map((s) => s.id)).toEqual([
      'a',
      'c',
    ]);
    expect(filterSources(REGISTRY, filters({ health: 'unpolled' })).map((s) => s.id)).toEqual(['b']);
  });
});

describe('filterOptions', () => {
  it('counts what each option would show with the other filters still on', () => {
    const options = filterOptions(REGISTRY, filters({ language: 'el' }), FAILURES, 'view', [
      'all',
      'active',
      'inactive',
    ]);
    // Greek feeds only: two of them, both active.
    expect(options).toEqual([
      { value: 'all', count: 2 },
      { value: 'active', count: 2 },
      { value: 'inactive', count: 0 },
    ]);
  });

  it('counts exactly what choosing the option would show', () => {
    // The count skips the ordering work the table needs; it must still
    // agree with the list the editor gets by choosing that option.
    for (const value of ['all', 'active', 'inactive'] as const) {
      const [option] = filterOptions(REGISTRY, NO_FILTERS, FAILURES, 'view', [value]);
      expect(option?.count).toBe(filterSources(REGISTRY, filters({ view: value }), FAILURES).length);
    }
  });

  it('offers an option that would show nothing rather than hiding it', () => {
    const options = filterOptions(REGISTRY, filters({ jurisdiction: 'GR' }), FAILURES, 'language', [
      '',
      'de',
      'el',
    ]);
    expect(options).toEqual([
      { value: '', count: 2 },
      { value: 'de', count: 0 },
      { value: 'el', count: 2 },
    ]);
  });
});

describe('presentValues', () => {
  it('lists what the registry actually holds, sorted and deduplicated', () => {
    expect(presentValues(REGISTRY, 'language')).toEqual(['de', 'el']);
    expect(presentValues(REGISTRY, 'jurisdiction')).toEqual(['DE', 'GR']);
    expect(presentValues([], 'language')).toEqual([]);
  });
});

describe('the paused-feed health value', () => {
  it('is selectable, and keeps paused feeds out of the healthy count', () => {
    const paused = row({ id: 'p', active: false });
    const working = row({ id: 'w' });
    const registry = [paused, working];
    expect(filterSources(registry, filters({ health: 'unpolled' }), []).map((s) => s.id)).toEqual([
      'p',
    ]);
    expect(filterSources(registry, filters({ health: 'healthy' }), []).map((s) => s.id)).toEqual([
      'w',
    ]);
    // And it survives the query string like every other value.
    expect(parseSourceFilters(new URLSearchParams('health=unpolled')).health).toBe('unpolled');
  });
});

describe('activeFilters', () => {
  it('names nothing when nothing is narrowed', () => {
    expect(activeFilters(NO_FILTERS)).toEqual([]);
  });

  it('pairs each narrowed dimension with the set that drops only it', () => {
    const chosen = filters({ view: 'active', language: 'el', search: 'νέα' });
    const chips = activeFilters(chosen);
    expect(chips.map((chip) => chip.key)).toEqual(['view', 'language', 'search']);
    // Removing the language leaves the other two exactly as they were.
    const withoutLanguage = chips[1]?.without;
    expect(withoutLanguage).toEqual({ ...chosen, language: '' });
    expect(withoutLanguage?.view).toBe('active');
    expect(withoutLanguage?.search).toBe('νέα');
  });
});
