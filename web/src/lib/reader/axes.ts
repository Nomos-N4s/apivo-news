/**
 * The two reader axes — reading language and followed places — and the URL
 * scheme that carries them: `/{lang}/{place[+place…]}`.
 *
 * FR-009: language and place are independent; no combined locale value
 * exists anywhere, so the two axes travel as two separate URL segments and
 * are parsed separately. FR-015: only the alpha languages mount; anything
 * else is a 404, decided here.
 */

/** The alpha reading languages. `en` exists in the schema and is not reachable (FR-015). */
export const READING_LANGUAGES = ['el', 'de'] as const;

/** A mounted reading language. */
export type ReadingLanguage = (typeof READING_LANGUAGES)[number];

/** Reports whether a URL segment names a mounted reading language. */
export function isReadingLanguage(value: string): value is ReadingLanguage {
  return (READING_LANGUAGES as readonly string[]).includes(value);
}

/** Structural rank of a place in the hierarchy; the kicker scope label derives from it. */
export type PlaceScope = 'city' | 'region' | 'country';

/**
 * A reader-addressable place. The catalog mirrors the 0002 migration seeds
 * (Munich → Bavaria → Germany; Greece) because no public place-directory
 * endpoint exists in the HTTP contract yet — recorded in issue #53; a
 * future `GET /api/v1/places` replaces this module's data, not its shape.
 */
export interface Place {
  /** `place.slug` — the stable URL address seeded by migration 0002. */
  readonly slug: string;
  /** The place's own-language name, shown regardless of reading language (mockup 1a). */
  readonly endonym: string;
  /**
   * The language the endonym is written in, for `lang` attributes wherever
   * the endonym appears inside a page of the other reading language
   * (WCAG 3.1.2) — "München" is German on a Greek page and vice versa.
   */
  readonly endonymLang: ReadingLanguage;
  readonly scope: PlaceScope;
  /**
   * Whether the setup screen offers the place. Non-selectable places are
   * hierarchy context (Bavaria "covers München") but stay addressable in
   * the URL — the API knows their slugs and answers honestly.
   */
  readonly selectable: boolean;
  /** Endonyms of the hierarchy above, nearest first — "Bayern · Deutschland". */
  readonly parents: readonly string[];
}

/** The alpha place catalog, in display order. */
export const PLACE_CATALOG: readonly Place[] = [
  { slug: 'munich', endonym: 'München', endonymLang: 'de', scope: 'city', selectable: true, parents: ['Bayern', 'Deutschland'] },
  { slug: 'greece', endonym: 'Ελλάδα', endonymLang: 'el', scope: 'country', selectable: true, parents: [] },
  { slug: 'bavaria', endonym: 'Bayern', endonymLang: 'de', scope: 'region', selectable: false, parents: ['Deutschland'] },
  { slug: 'germany', endonym: 'Deutschland', endonymLang: 'de', scope: 'country', selectable: false, parents: [] },
];

/** Looks a place up by slug; undefined for slugs outside the catalog. */
export function findPlace(slug: string): Place | undefined {
  return PLACE_CATALOG.find((place) => place.slug === slug);
}

/**
 * Joins place slugs inside the URL's place segment: `/el/munich+greece`.
 * `+` is a literal plus in a path segment (only query strings read it as a
 * space), and slugs are lowercase ASCII words, so the segment stays plain.
 */
export const PLACE_SEPARATOR = '+';

/** Both axes, parsed and validated. */
export interface Axes {
  readonly lang: ReadingLanguage;
  /** At least one place, deduplicated, in URL order. */
  readonly places: readonly Place[];
}

/**
 * Parses the two URL segments into validated axes. Returns null — meaning
 * 404 — when the language is not mounted, any slug is unknown, or no place
 * is present. Duplicate slugs collapse to their first occurrence so every
 * axes value has one canonical URL.
 */
export function parseAxes(
  langSegment: string | undefined,
  placeSegment: string | undefined,
): Axes | null {
  if (langSegment === undefined || !isReadingLanguage(langSegment)) {
    return null;
  }
  if (placeSegment === undefined || placeSegment === '') {
    return null;
  }
  const places: Place[] = [];
  for (const slug of placeSegment.split(PLACE_SEPARATOR)) {
    const place = findPlace(slug);
    if (place === undefined) {
      return null;
    }
    if (!places.includes(place)) {
      places.push(place);
    }
  }
  if (places.length === 0) {
    return null;
  }
  return { lang: langSegment, places };
}

/** Composes the front-page path for a language and a non-empty slug list. */
export function frontPagePath(lang: ReadingLanguage, slugs: readonly string[]): string {
  if (slugs.length === 0) {
    throw new Error('frontPagePath: at least one place slug is required');
  }
  return `/${lang}/${slugs.join(PLACE_SEPARATOR)}`;
}

/**
 * The kicker for an item's place list — "München · Τοπικά" (mockup 1a):
 * the first place's endonym plus its scope label from the caller's
 * language. Slugs beyond the catalog render as themselves — the API may
 * know places before this catalog does.
 */
export function placeKicker(
  slugs: readonly string[],
  scopeLabels: Readonly<Record<PlaceScope, string>>,
): string {
  const slug = slugs.at(0);
  if (slug === undefined) {
    return '';
  }
  const place = findPlace(slug);
  if (place === undefined) {
    return slug;
  }
  return `${place.endonym} · ${scopeLabels[place.scope]}`;
}

/**
 * Composes an article path under the same axes: `/el/munich+greece/a/{id}`.
 * The article keeps both axes in its URL so the back-link and the chrome
 * language survive the navigation (FR-009).
 */
export function articlePath(
  lang: ReadingLanguage,
  slugs: readonly string[],
  articleId: string,
): string {
  return `${frontPagePath(lang, slugs)}/a/${encodeURIComponent(articleId)}`;
}

/** The reading languages named in themselves, for chrome like "Ελληνικά · München". */
export const LANGUAGE_ENDONYMS: Readonly<Record<ReadingLanguage, string>> = {
  el: 'Ελληνικά',
  de: 'Deutsch',
};

/**
 * Parses a `?places=munich%2Bgreece` query value into selectable catalog
 * places — the setup page's prefill. Query strings decode a literal `+`
 * to a space (unlike path segments), so a space is accepted as the
 * separator too: an unencoded hand-typed `?places=munich+greece` still
 * parses. Unknown and non-selectable slugs are dropped rather than
 * failing: the query is a convenience, not an address.
 */
export function parsePlacesParam(value: string | null): Place[] {
  if (value === null || value === '') {
    return [];
  }
  const places: Place[] = [];
  for (const slug of value.split(/[+ ]/)) {
    const place = findPlace(slug);
    if (place !== undefined && place.selectable && !places.includes(place)) {
      places.push(place);
    }
  }
  return places;
}

/**
 * Composes the front-page path from raw form input — the `/go` endpoint's
 * whole logic. `place` arrives repeated (`?place=munich&place=greece`).
 * Null means the input names no mounted language or no selectable place;
 * the caller decides where that lands.
 */
export function composeFrontPageTarget(
  langParam: string | null,
  placeParams: readonly string[],
): string | null {
  if (langParam === null || !isReadingLanguage(langParam)) {
    return null;
  }
  const slugs: string[] = [];
  for (const raw of placeParams) {
    const place = findPlace(raw);
    if (place !== undefined && place.selectable && !slugs.includes(place.slug)) {
      slugs.push(place.slug);
    }
  }
  if (slugs.length === 0) {
    return null;
  }
  return frontPagePath(langParam, slugs);
}

/** The US1 flagship journey — where `/` lands: Munich local + Greek national, in Greek. */
export const DEFAULT_FRONT_PAGE = frontPagePath('el', ['munich', 'greece']);
